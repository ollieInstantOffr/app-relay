package gateway

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/instantoffr/relay/internal/tunnel/mux"
	"github.com/instantoffr/relay/internal/tunnel/wire"
)

// tunnelSession is one session with the home and its control stream.
type tunnelSession struct {
	sess mux.Session
	done chan struct{} // closed when the control loop has ended

	wmu sync.Mutex
	ctl mux.Stream
}

// send writes one control message; writes are serialized.
func (ts *tunnelSession) send(m *wire.Message) error {
	ts.wmu.Lock()
	defer ts.wmu.Unlock()
	return wire.WriteMessage(ts.ctl, m)
}

// AttachSession makes sess the session with the home, replacing (and closing)
// the current one, and runs its control stream. The caller vouches that sess
// is authenticated; the tunnel listeners attach sessions of the paired home.
func (s *Server) AttachSession(sess mux.Session) {
	s.attachSession(sess, "")
}

// attach attaches a session from a tunnel listener whose peer key has
// fingerprint pin, provided it is still the paired home.
func (s *Server) attach(sess mux.Session, pin string) {
	if pin == "" {
		sess.Close()
		return
	}
	s.attachSession(sess, pin)
}

func (s *Server) attachSession(sess mux.Session, pin string) {
	ts := &tunnelSession{sess: sess, done: make(chan struct{})}
	s.mu.Lock()
	if s.closed || (pin != "" && pin != s.HomePin()) {
		s.mu.Unlock()
		sess.Close()
		return
	}
	old := s.session
	s.session = ts
	s.active.Store(nil)
	s.wg.Add(1)
	s.mu.Unlock()
	if old != nil {
		s.log.Info("tunnel session replaced by a new connection", "transport", old.sess.Transport(), "remote", addrString(old.sess.RemoteAddr()))
		old.sess.Close()
	}
	go s.runSession(ts)
}

// runSession runs the control stream until it or the session ends.
func (s *Server) runSession(ts *tunnelSession) {
	defer s.wg.Done()
	log := s.log.With("transport", ts.sess.Transport(), "remote", addrString(ts.sess.RemoteAddr()))
	err := s.control(ts, log)
	close(ts.done)
	if ts.ctl != nil {
		ts.ctl.Close()
	}
	ts.sess.Close()

	s.mu.Lock()
	current := s.session == ts
	if current {
		s.session = nil
	}
	s.active.CompareAndSwap(ts, nil)
	s.mu.Unlock()
	if current {
		if serr := ts.sess.Err(); serr != nil && !errors.Is(serr, mux.ErrClosed) {
			err = serr
		}
		log.Warn("tunnel session closed; refusing client connections until the home reconnects", "err", err)
	}
}

// control opens the control stream, exchanges hellos and processes messages.
func (s *Server) control(ts *tunnelSession, log *slog.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), helloTimeout)
	ctl, err := ts.sess.OpenStream(ctx)
	cancel()
	if err != nil {
		return err
	}
	ts.ctl = ctl

	// The session is closed when the hello does not complete in time.
	timer := time.AfterFunc(helloTimeout, func() { ts.sess.Close() })
	hello := wire.LocalHello(s.opts.Version)
	hello.PublicIPs = s.publicIPs
	if len(hello.PublicIPs) == 0 {
		hello.PublicIPs = interfacePublicIPs()
	}
	if err := ts.send(&wire.Message{Hello: hello}); err != nil {
		timer.Stop()
		return err
	}
	br := bufio.NewReader(ctl)
	m, err := wire.ReadMessage(br)
	if !timer.Stop() {
		return errors.New("no hello from the home within " + helloTimeout.String())
	}
	if err != nil {
		return err
	}
	if m.Hello == nil {
		return errors.New("home did not start with a hello")
	}
	proto, err := wire.Negotiate(hello, m.Hello)
	if err != nil {
		log.Warn("tunnel session refused", "homeVersion", m.Hello.Version, "err", err)
		return err
	}

	s.mu.Lock()
	if s.session != ts {
		s.mu.Unlock()
		return mux.ErrClosed
	}
	s.active.Store(ts)
	s.mu.Unlock()
	log.Info("tunnel session established", "homeVersion", m.Hello.Version, "proto", proto)

	s.wg.Add(1)
	go s.sendStats(ts)
	for {
		m, err := wire.ReadMessage(br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return errors.New("control stream closed by the home")
			}
			return err
		}
		switch {
		case m.Routes != nil:
			ack, ok := s.applyRoutes(ts, m.Routes)
			if !ok {
				return mux.ErrClosed
			}
			if err := ts.send(&wire.Message{Ack: ack}); err != nil {
				return err
			}
			stats := s.Stats()
			if err := ts.send(&wire.Message{Stats: &stats}); err != nil {
				return err
			}
		case m.Ping != nil:
			if err := ts.send(&wire.Message{Pong: &wire.Ping{ID: m.Ping.ID, SentNs: m.Ping.SentNs}}); err != nil {
				return err
			}
		case m.Unpair != nil:
			s.unpair(ts)
			return errors.New("unpaired by the home")
		}
	}
}

// sendStats sends the counters periodically while the session lives.
func (s *Server) sendStats(ts *tunnelSession) {
	defer s.wg.Done()
	t := time.NewTicker(s.statsEvery)
	defer t.Stop()
	for {
		select {
		case <-ts.done:
			return
		case <-t.C:
			stats := s.Stats()
			if err := ts.send(&wire.Message{Stats: &stats}); err != nil {
				ts.sess.Close()
				return
			}
		}
	}
}

// applyRoutes replaces the routing table and reconciles the TCP port
// listeners: new ports are bound, unpublished ones closed, unchanged ones
// kept. ok is false when ts is no longer the current session.
func (s *Server) applyRoutes(ts *tunnelSession, r *wire.Routes) (ack *wire.RoutesAck, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.session != ts {
		return nil, false
	}
	ack = &wire.RoutesAck{Generation: r.Generation}
	tcp := map[uint16]bool{}
	seen := map[uint16]bool{}
	var opened []uint16
	for _, p := range r.TCP {
		if seen[p] {
			continue
		}
		seen[p] = true
		if p == 0 || !s.portAllowed(p) {
			ack.Errors = append(ack.Errors, wire.PortError{Port: p, Error: "port not allowed on this gateway"})
			continue
		}
		if s.ports[p] == nil {
			addr := net.JoinHostPort(s.opts.BindHost, strconv.Itoa(int(p)))
			lc := listenConfig()
			ln, err := lc.Listen(context.Background(), "tcp", addr)
			if err != nil {
				s.log.Warn("cannot listen on published port", "addr", addr, "err", err)
				ack.Errors = append(ack.Errors, wire.PortError{Port: p, Error: "cannot listen: " + bindReason(err)})
				continue
			}
			s.ports[p] = ln
			opened = append(opened, p)
		}
		tcp[p] = true
	}
	for p, ln := range s.ports {
		if !tcp[p] {
			ln.Close()
			delete(s.ports, p)
			s.log.Info("closed unpublished port", "port", p)
		}
	}
	s.routes.Store(&routeTable{generation: r.Generation, names: wire.NewNames(r.Names), tcp: tcp})
	for _, p := range opened {
		port := p
		s.wg.Add(1)
		go s.serve(s.ports[port], func(c net.Conn) { s.acceptClient(c, func(c net.Conn) { s.serveTCPPort(c, port) }) })
		s.log.Info("listening on published port", "addr", s.ports[port].Addr().String())
	}
	slices.SortFunc(ack.Errors, func(a, b wire.PortError) int { return cmp.Compare(a.Port, b.Port) })
	s.log.Info("routes updated", "generation", r.Generation, "names", len(r.Names), "tcpPorts", len(tcp), "portErrors", len(ack.Errors))
	return ack, true
}

// unpair forgets the home: the pairing state is deleted and its routes and
// port listeners dropped. The identity is kept.
func (s *Server) unpair(ts *tunnelSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session != ts {
		return
	}
	home := s.HomePin()
	if _, err := deletePairing(s.dir); err != nil {
		s.log.Error("could not delete the pairing state", "err", err)
	}
	s.pairing.Store(nil)
	for p, ln := range s.ports {
		ln.Close()
		delete(s.ports, p)
	}
	s.routes.Store(&routeTable{names: wire.NewNames(nil)})
	s.active.Store(nil)
	s.log.Warn("unpaired by the home: pairing state deleted; start the gateway with a new pairing token to pair again", "home", home)
}

func addrString(a net.Addr) string {
	if a == nil {
		return ""
	}
	return a.String()
}
