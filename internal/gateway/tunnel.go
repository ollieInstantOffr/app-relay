package gateway

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/instantoffr/relay/internal/tunnel/mux"
	"github.com/instantoffr/relay/internal/tunnel/pair"
)

// listenTunnel binds the tunnel port on TCP and UDP. With port 0 the UDP
// socket takes the port TCP got, retrying when that one is taken on UDP.
func (s *Server) listenTunnel(ctx context.Context, lc net.ListenConfig) (net.Listener, *quic.Listener, error) {
	host, port, err := net.SplitHostPort(s.opts.TunnelAddr)
	if err != nil {
		return nil, nil, err
	}
	qtls := pair.ServerConfig(s.id, s.HomePin, mux.ALPNQUIC)
	for attempt := 0; ; attempt++ {
		ln, err := lc.Listen(ctx, "tcp", s.opts.TunnelAddr)
		if err != nil {
			return nil, nil, err
		}
		bound := strconv.Itoa(int(addrPort(ln.Addr()).Port()))
		qln, err := quic.ListenAddr(net.JoinHostPort(host, bound), qtls, mux.QUICConfig(s.opts.Mux))
		if err == nil {
			return ln, qln, nil
		}
		ln.Close()
		if port != "0" || attempt >= 20 {
			return nil, nil, err
		}
	}
}

// acceptTunnel admits a TCP tunnel connection and handshakes it.
func (s *Server) acceptTunnel(c net.Conn) {
	// An unpaired gateway serves nothing but pairing, so a source over the
	// pairing failure limit is dropped before spending a handshake on it.
	if ip := addrPort(c.RemoteAddr()).Addr(); s.HomePin() == "" && !s.limiter.Allow(ip, time.Now()) {
		c.Close()
		return
	}
	ip, ok := s.admit(c, slotHandshake)
	if !ok {
		c.Close()
		return
	}
	go s.handleTunnel(c, ip)
}

func (s *Server) handleTunnel(c net.Conn, ip netip.Addr) {
	attached := false
	defer func() {
		if !attached {
			c.Close()
		}
		s.release(c, ip, slotHandshake)
	}()
	remote := c.RemoteAddr().String()
	tc := tls.Server(c, pair.ServerConfig(s.id, s.HomePin, mux.ALPNH2, pair.ALPN))
	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	defer cancel()
	if err := tc.HandshakeContext(ctx); err != nil {
		if s.HomePin() == "" {
			s.limiter.Failed(ip, time.Now())
		}
		s.log.Debug("tunnel handshake failed", "remote", remote, "err", err)
		return
	}
	cs := tc.ConnectionState()
	switch cs.NegotiatedProtocol {
	case pair.ALPN:
		s.pairConn(tc, ip)
	case mux.ALPNH2:
		pin, err := pair.PeerFingerprint(cs)
		if err != nil {
			return
		}
		sess, err := mux.GatewayTCP(tc, s.opts.Mux)
		if err != nil {
			s.log.Warn("tunnel session failed", "remote", remote, "err", err)
			return
		}
		attached = true // the session owns the connection now
		s.attach(sess, pin)
	}
}

// serveQUIC accepts QUIC tunnel connections. Their handshake already required
// the paired home's key (an unpaired gateway cannot negotiate ALPNQUIC).
func (s *Server) serveQUIC(ln *quic.Listener) {
	defer s.wg.Done()
	for {
		conn, err := ln.Accept(context.Background())
		if err != nil {
			if !errors.Is(err, quic.ErrServerClosed) {
				s.log.Warn("QUIC accept failed", "err", err)
			}
			return
		}
		cs := conn.ConnectionState().TLS
		pin, err := pair.PeerFingerprint(cs)
		if err != nil || cs.NegotiatedProtocol != mux.ALPNQUIC {
			conn.CloseWithError(0, "refused")
			continue
		}
		s.attach(mux.GatewayQUIC(conn, s.opts.Mux), pin)
	}
}

// pairConn runs the pairing handshake on a connection that negotiated
// pair.ALPN, and persists the home's pin on success.
func (s *Server) pairConn(tc *tls.Conn, ip netip.Addr) {
	remote := tc.RemoteAddr().String()
	s.mu.Lock()
	paired, tok, used := s.HomePin() != "", s.token, s.tokenUsed
	s.mu.Unlock()
	switch {
	case paired:
		s.log.Info("pairing attempt refused: already paired (run `relay gateway reset` to pair again)", "remote", remote)
		return
	case tok == nil || used:
		s.log.Warn("pairing attempt refused: no pairing token (start the gateway with a new pairing token)", "remote", remote)
		return
	case !s.limiter.Allow(ip, time.Now()):
		s.log.Warn("pairing attempt refused: too many failed attempts", "remote", remote)
		return
	}
	s.log.Info("pairing attempt", "remote", remote)
	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	defer cancel()
	pin, err := pair.Gateway(ctx, tc, *tok)
	if err != nil {
		s.limiter.Failed(ip, time.Now())
		s.log.Warn("pairing failed", "remote", remote, "err", err)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed:
		return
	case s.HomePin() != "" || s.tokenUsed:
		s.log.Warn("pairing refused: the gateway was paired by a concurrent attempt", "remote", remote, "home", pin)
		return
	}
	p := &Pairing{Schema: 1, HomePin: pin, PairedAt: time.Now().UTC().Truncate(time.Second)}
	if err := savePairing(s.dir, p); err != nil {
		s.log.Error("pairing succeeded but could not be saved", "remote", remote, "home", pin, "err", err)
		return
	}
	s.tokenUsed = true
	s.pairing.Store(p)
	s.log.Info("paired with home; the pairing token is used up and can be removed from the command line", "remote", remote, "home", pin)
}
