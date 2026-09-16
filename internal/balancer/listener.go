package balancer

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/instantoffr/relay/internal/balancer/spec"
	"github.com/instantoffr/relay/internal/proxyproto"
)

const maxHeaderBytes = 64 << 10

// limiter is the global maxconn: listeners stop accepting while it is full.
type limiter struct {
	mu   sync.Mutex
	max  int64
	cur  int64
	wake chan struct{}
}

func (l *limiter) setMax(n int) {
	l.mu.Lock()
	l.max = int64(n)
	l.broadcastLocked()
	l.mu.Unlock()
}

func (l *limiter) broadcastLocked() {
	if l.wake != nil {
		close(l.wake)
		l.wake = nil
	}
}

// acquire takes a connection slot, waiting while the limit is reached.
func (l *limiter) acquire(done <-chan struct{}) (waited, ok bool) {
	for {
		l.mu.Lock()
		if l.max <= 0 || l.cur < l.max {
			l.cur++
			l.mu.Unlock()
			return waited, true
		}
		if l.wake == nil {
			l.wake = make(chan struct{})
		}
		ch := l.wake
		l.mu.Unlock()
		waited = true
		select {
		case <-ch:
		case <-done:
			return waited, false
		}
	}
}

func (l *limiter) release() {
	l.mu.Lock()
	l.cur--
	l.broadcastLocked()
	l.mu.Unlock()
}

// feConn is an accepted client connection. With accept-proxy its addresses
// are the ones carried by the PROXY header.
type feConn struct {
	net.Conn
	br       *bufio.Reader
	src, dst netip.AddrPort
	onClose  func()
	once     sync.Once
	started  atomic.Bool // frontend session counted

	mu     sync.Mutex
	parked *sconn // private server connection (send-proxy backends)
	closed bool
}

func (c *feConn) Read(p []byte) (int, error) {
	if c.br != nil && c.br.Buffered() > 0 {
		return c.br.Read(p)
	}
	return c.Conn.Read(p)
}

func (c *feConn) RemoteAddr() net.Addr { return net.TCPAddrFromAddrPort(c.src) }
func (c *feConn) LocalAddr() net.Addr  { return net.TCPAddrFromAddrPort(c.dst) }

func (c *feConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

func (c *feConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		c.mu.Lock()
		p := c.parked
		c.parked, c.closed = nil, true
		c.mu.Unlock()
		if p != nil {
			p.close()
		}
		if c.onClose != nil {
			c.onClose()
		}
	})
	return err
}

// park keeps a private server connection for the next request of this client
// connection.
func (c *feConn) park(sc *sconn) {
	c.mu.Lock()
	if c.closed || c.parked != nil {
		c.mu.Unlock()
		sc.close()
		return
	}
	c.parked = sc
	c.mu.Unlock()
}

func (c *feConn) takeParked(srv *server) *sconn {
	c.mu.Lock()
	p := c.parked
	c.parked = nil
	c.mu.Unlock()
	if p == nil {
		return nil
	}
	if p.srv != srv {
		p.close()
		return nil
	}
	return p
}

func addrPortOf(a net.Addr) netip.AddrPort {
	if ta, ok := a.(*net.TCPAddr); ok {
		ap := ta.AddrPort()
		return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	}
	ap, _ := netip.ParseAddrPort(a.String())
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}

type connCtxKey struct{}

// boundListener is one bound socket. Reloads keep it while its mode and
// normalised bind address are unchanged and swap the frontend it serves.
type boundListener struct {
	srv  *Server
	key  string
	kind string // http | tcp | stats
	addr string
	ln   net.Listener

	fe    atomic.Pointer[frontend]
	stats atomic.Pointer[statsConf]

	closing    chan struct{}
	closeOnce  sync.Once
	acceptDone chan struct{}
	ready      chan *feConn

	hmu      sync.Mutex
	hsrv     *http.Server
	hln      *chanListener
	hOpen    *atomic.Int64 // connections of hsrv
	hTimeout time.Duration
	// retired are servers replaced after a client timeout change that still
	// own keep-alive connections (with the previous timeouts).
	retired map[*http.Server]*atomic.Int64

	cmu   sync.Mutex
	conns map[*feConn]struct{}
	wg    sync.WaitGroup
}

func (s *Server) open(ls listenSpec) (*boundListener, error) {
	ln, err := net.Listen("tcp", ls.addr)
	if err != nil {
		return nil, err
	}
	return &boundListener{
		srv: s, key: ls.key, kind: ls.kind, addr: ls.addr, ln: ln,
		closing: make(chan struct{}), acceptDone: make(chan struct{}), ready: make(chan *feConn),
		conns: map[*feConn]struct{}{},
	}, nil
}

// update points the listener at a (re)loaded frontend or stats config.
func (bl *boundListener) update(ls listenSpec) {
	if bl.kind == "stats" {
		bl.stats.Store(ls.stats)
		return
	}
	bl.fe.Store(ls.fe)
	if bl.kind != spec.ModeHTTP {
		return
	}
	bl.hmu.Lock()
	defer bl.hmu.Unlock()
	if bl.hsrv != nil && bl.hTimeout == ls.fe.clientTimeout {
		return
	}
	old, oldLn, oldOpen := bl.hsrv, bl.hln, bl.hOpen
	hl := &chanListener{ready: bl.ready, done: make(chan struct{}), addr: bl.ln.Addr()}
	open := new(atomic.Int64)
	var hs *http.Server
	hs = &http.Server{
		Handler:           &httpHandler{bl: bl},
		ReadHeaderTimeout: ls.fe.clientTimeout,
		IdleTimeout:       ls.fe.clientTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          log.New(io.Discard, "", 0),
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, connCtxKey{}, c)
		},
		ConnState: func(_ net.Conn, st http.ConnState) {
			switch st {
			case http.StateNew:
				open.Add(1)
			case http.StateClosed, http.StateHijacked:
				if open.Add(-1) == 0 {
					bl.dropRetired(hs)
				}
			}
		},
		DisableGeneralOptionsHandler: true,
	}
	hs.Protocols = new(http.Protocols)
	hs.Protocols.SetHTTP1(true)
	bl.hsrv, bl.hln, bl.hOpen, bl.hTimeout = hs, hl, open, ls.fe.clientTimeout
	go hs.Serve(hl)
	if old != nil {
		// The client timeout changed. New connections go to the new server;
		// the old one stops accepting but keeps its keep-alive connections
		// (with the previous timeouts) until they close, so the reload
		// disconnects nobody.
		oldLn.Close()
		if bl.retired == nil {
			bl.retired = map[*http.Server]*atomic.Int64{}
		}
		bl.retired[old] = oldOpen
		if oldOpen.Load() == 0 {
			delete(bl.retired, old)
			go old.Close()
		}
	}
}

// dropRetired forgets a replaced http.Server once its last connection closed.
func (bl *boundListener) dropRetired(hs *http.Server) {
	bl.hmu.Lock()
	_, ok := bl.retired[hs]
	delete(bl.retired, hs)
	bl.hmu.Unlock()
	if ok {
		go hs.Close()
	}
}

func (bl *boundListener) serve() {
	if bl.kind == "stats" {
		hs := &http.Server{Handler: http.HandlerFunc(bl.serveStats), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
		bl.hmu.Lock()
		bl.hsrv = hs
		bl.hmu.Unlock()
		go func() {
			defer close(bl.acceptDone)
			hs.Serve(bl.ln)
		}()
		return
	}
	go bl.acceptLoop()
}

func (bl *boundListener) acceptLoop() {
	defer close(bl.acceptDone)
	s := bl.srv
	tl, _ := bl.ln.(*net.TCPListener)
	deadline := false
	for {
		waited, ok := s.limiter.acquire(bl.closing)
		if !ok {
			return
		}
		if tl != nil {
			if waited {
				// Saturated: don't hold the slot on an idle listener.
				tl.SetDeadline(time.Now().Add(100 * time.Millisecond))
				deadline = true
			} else if deadline {
				tl.SetDeadline(time.Time{})
				deadline = false
			}
		}
		c, err := bl.ln.Accept()
		if err != nil {
			s.limiter.release()
			select {
			case <-bl.closing:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if !isTimeout(err) {
				time.Sleep(10 * time.Millisecond)
			}
			continue
		}
		fe := bl.fe.Load()
		fc := &feConn{Conn: c, src: addrPortOf(c.RemoteAddr()), dst: addrPortOf(c.LocalAddr())}
		s.actconn.Add(1)
		s.cumConns.Add(1)
		bl.wg.Add(1)
		bl.cmu.Lock()
		bl.conns[fc] = struct{}{}
		bl.cmu.Unlock()
		fst := fe.st
		fc.onClose = func() {
			if fc.started.Load() {
				fst.c.sessionEnd()
			}
			s.limiter.release()
			s.actconn.Add(-1)
			bl.cmu.Lock()
			delete(bl.conns, fc)
			bl.cmu.Unlock()
			bl.wg.Done()
		}
		go bl.handshake(fc, fe)
	}
}

func (bl *boundListener) handshake(fc *feConn, fe *frontend) {
	if fe.acceptProxy || (fe.mode == spec.ModeTCP && fe.needSNI) {
		size := 512
		if fe.mode == spec.ModeTCP && fe.needSNI {
			size = 32 << 10
		}
		fc.br = bufio.NewReaderSize(fc.Conn, size)
	}
	if fe.acceptProxy {
		fc.Conn.SetReadDeadline(time.Now().Add(fe.clientTimeout))
		h, err := proxyproto.Read(fc.br)
		fc.Conn.SetReadDeadline(time.Time{})
		if err != nil {
			bl.srv.logConnError(fc, fe, "Received something which does not look like a PROXY protocol header")
			fc.Close()
			return
		}
		if !h.Local {
			fc.src = netip.AddrPortFrom(h.Src.Addr().Unmap(), h.Src.Port())
			fc.dst = netip.AddrPortFrom(h.Dst.Addr().Unmap(), h.Dst.Port())
		}
	}
	now := time.Now()
	fe.st.c.connection(now)
	fe.st.c.sessionStart(now)
	fc.started.Store(true)
	if bl.kind == spec.ModeHTTP {
		select {
		case bl.ready <- fc:
		case <-bl.closing:
			fc.Close()
		}
		return
	}
	bl.handleTCP(fc, fe)
	fc.Close()
}

// shutdown stops accepting, lets requests and sessions finish until ctx
// ends, then closes what is left.
func (bl *boundListener) shutdown(ctx context.Context) {
	bl.closeOnce.Do(func() { close(bl.closing) })
	bl.hmu.Lock()
	hs := bl.hsrv
	retired := make([]*http.Server, 0, len(bl.retired))
	for r := range bl.retired {
		retired = append(retired, r)
	}
	bl.retired = nil
	bl.hmu.Unlock()
	if bl.kind == "stats" {
		if hs != nil {
			sctx, cancel := context.WithTimeout(ctx, time.Second)
			if err := hs.Shutdown(sctx); err != nil {
				hs.Close()
			}
			cancel()
		} else {
			bl.ln.Close()
		}
		return
	}
	bl.ln.Close()
	<-bl.acceptDone
	if hs != nil {
		if err := hs.Shutdown(ctx); err != nil {
			hs.Close()
		}
	}
	for _, r := range retired {
		if err := r.Shutdown(ctx); err != nil {
			r.Close()
		}
	}
	for drained := false; !drained; {
		select {
		case c := <-bl.ready:
			c.Close()
		default:
			drained = true
		}
	}
	done := make(chan struct{})
	go func() {
		bl.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return
	case <-ctx.Done():
	}
	bl.cmu.Lock()
	var left []*feConn
	for c := range bl.conns {
		left = append(left, c)
	}
	bl.cmu.Unlock()
	for _, c := range left {
		c.Close()
	}
	select {
	case <-done:
	case <-time.After(time.Second):
	}
}

// closeUnserved releases a listener that was opened but never served.
func (bl *boundListener) closeUnserved() { bl.ln.Close() }

// chanListener hands accepted (and PROXY-decoded) connections to an
// http.Server.
type chanListener struct {
	ready <-chan *feConn
	done  chan struct{}
	once  sync.Once
	addr  net.Addr
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case <-l.done:
		return nil, net.ErrClosed
	default:
	}
	select {
	case c := <-l.ready:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *chanListener) Addr() net.Addr { return l.addr }
