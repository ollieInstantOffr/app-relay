package balancer

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// idleConn applies an inactivity timeout to a server connection: every read
// and write pushes the deadline (throttled to 1/16 of the timeout).
type idleConn struct {
	net.Conn
	timeout time.Duration
	last    atomic.Int64
	off     atomic.Bool // deadlines managed by the caller
}

var aLongTimeAgo = time.Unix(1, 0)

func (c *idleConn) extend() {
	if c.timeout <= 0 || c.off.Load() {
		return
	}
	now := time.Now()
	n := now.UnixNano()
	if n-c.last.Load() < int64(c.timeout/16) {
		return
	}
	c.last.Store(n)
	c.Conn.SetDeadline(now.Add(c.timeout))
}

func (c *idleConn) Read(p []byte) (int, error) {
	c.extend()
	return c.Conn.Read(p)
}

func (c *idleConn) Write(p []byte) (int, error) {
	c.extend()
	return c.Conn.Write(p)
}

// kick unblocks pending reads and writes.
func (c *idleConn) kick() {
	c.last.Store(0)
	c.Conn.SetDeadline(aLongTimeAgo)
}

// rearm restores the inactivity deadline after kick or connect.
func (c *idleConn) rearm() {
	c.last.Store(0)
	c.off.Store(false)
	if c.timeout > 0 {
		c.extend()
	} else {
		c.Conn.SetDeadline(time.Time{})
	}
}

func (c *idleConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// sconn is a connection to a server.
type sconn struct {
	conn net.Conn // raw or TLS on top of raw
	raw  *idleConn
	br   *bufio.Reader
	bw   *bufio.Writer
	srv  *server

	taken     bool // guarded by pool.mu
	watchDone chan struct{}
	watchBad  bool
}

func (sc *sconn) close() { sc.conn.Close() }

// closeWrite half-closes the connection to the server.
func (sc *sconn) closeWrite() {
	if tc, ok := sc.conn.(*tls.Conn); ok {
		tc.CloseWrite()
		return
	}
	sc.raw.CloseWrite()
}

// connectError marks failures to establish a server connection (retried).
type connectError struct{ err error }

func (e *connectError) Error() string { return e.err.Error() }
func (e *connectError) Unwrap() error { return e.err }

// dial connects to the server: TCP, PROXY v2 header, TLS, all within the
// connect timeout.
func (s *server) dial(ctx context.Context, src, dst netip.AddrPort) (*sconn, error) {
	be := s.be
	deadline := time.Now().Add(be.connectTimeout)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	d := net.Dialer{KeepAlive: 30 * time.Second}
	raw, err := d.DialContext(ctx, "tcp", s.dialAddr)
	if err != nil {
		return nil, &connectError{err}
	}
	ic := &idleConn{Conn: raw, timeout: be.serverTimeout}
	ic.off.Store(true)
	raw.SetDeadline(deadline)
	var c net.Conn = ic
	if be.sendProxy {
		if _, err := raw.Write(appendProxyV2(make([]byte, 0, 52), src, dst)); err != nil {
			raw.Close()
			return nil, &connectError{err}
		}
	}
	if be.tlsConf != nil {
		tc := tls.Client(ic, be.tlsConf)
		if err := tc.HandshakeContext(ctx); err != nil {
			raw.Close()
			return nil, &connectError{err}
		}
		c = tc
	}
	ic.rearm()
	return &sconn{conn: c, raw: ic, srv: s}, nil
}

func (sc *sconn) buffers() {
	if sc.br == nil {
		sc.br = bufio.NewReaderSize(sc.conn, 16<<10)
		sc.bw = bufio.NewWriterSize(sc.conn, 16<<10)
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

// isClosedByPeer reports errors of a reused connection the server closed
// before answering.
func isClosedByPeer(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, net.ErrClosed)
}

// ---------------------------------------------------------------- pool

const maxIdlePerServer = 128

// connPool keeps idle keep-alive HTTP connections to one server. Idle
// connections are watched: data or EOF from the server removes them.
type connPool struct {
	mu     sync.Mutex
	idle   []*sconn
	closed bool
}

func newConnPool() *connPool { return &connPool{} }

func (p *connPool) get() *sconn {
	for {
		p.mu.Lock()
		n := len(p.idle)
		if n == 0 {
			p.mu.Unlock()
			return nil
		}
		sc := p.idle[n-1]
		p.idle = p.idle[:n-1]
		sc.taken = true
		p.mu.Unlock()
		sc.raw.kick()
		<-sc.watchDone
		if sc.watchBad {
			sc.close()
			continue
		}
		sc.raw.rearm()
		return sc
	}
}

func (p *connPool) put(sc *sconn) {
	p.mu.Lock()
	if p.closed || len(p.idle) >= maxIdlePerServer {
		p.mu.Unlock()
		sc.close()
		return
	}
	sc.taken, sc.watchBad = false, false
	sc.watchDone = make(chan struct{})
	p.idle = append(p.idle, sc)
	p.mu.Unlock()
	go p.watch(sc)
}

func (p *connPool) watch(sc *sconn) {
	_, err := sc.br.Peek(1)
	p.mu.Lock()
	taken := sc.taken
	if !taken {
		for i, c := range p.idle {
			if c == sc {
				p.idle = append(p.idle[:i], p.idle[i+1:]...)
				break
			}
		}
	}
	p.mu.Unlock()
	if taken {
		sc.watchBad = err == nil || !isTimeout(err)
	} else {
		sc.watchBad = true
		sc.close()
	}
	close(sc.watchDone)
}

func (p *connPool) close() {
	p.mu.Lock()
	p.closed = true
	idle := p.idle
	p.idle = nil
	for _, sc := range idle {
		sc.taken = true
	}
	p.mu.Unlock()
	for _, sc := range idle {
		sc.close()
	}
}

func (p *connPool) idleCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.idle)
}

// ---------------------------------------------------------------- pipe

var bufPool = sync.Pool{New: func() any {
	b := make([]byte, 32<<10)
	return &b
}}

// pipeEnd is one side of a tunnel.
type pipeEnd struct {
	conn    net.Conn
	r       io.Reader
	timeout time.Duration
	closeW  func()
}

// pipeResult reports a finished tunnel.
type pipeResult struct {
	up, down int64 // client → server, server → client
	term     byte  // termination cause: '-', 'C', 'S', 'c', 's'
}

// pipe copies both directions until both are done. EOF on one side
// half-closes the other; errors abort both. Each side's read times out after
// its idle timeout unless the other direction was active meanwhile.
func pipe(client, server pipeEnd, onUp, onDown func(int)) pipeResult {
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	var aborted atomic.Bool
	var term atomic.Int32
	term.Store('-')
	abort := func(t byte) {
		if aborted.CompareAndSwap(false, true) {
			term.Store(int32(t))
			client.conn.Close()
			server.conn.Close()
		}
	}
	copyDir := func(src, dst pipeEnd, n *int64, readTerm, readTimeout, writeTerm byte, count func(int)) {
		bp := bufPool.Get().(*[]byte)
		defer bufPool.Put(bp)
		buf := *bp
		for !aborted.Load() {
			if src.timeout > 0 {
				src.conn.SetReadDeadline(time.Now().Add(src.timeout))
			}
			nr, err := src.r.Read(buf)
			if nr > 0 {
				now := time.Now()
				last.Store(now.UnixNano())
				if dst.timeout > 0 {
					dst.conn.SetWriteDeadline(now.Add(dst.timeout))
				}
				nw, werr := dst.conn.Write(buf[:nr])
				*n += int64(nw)
				if count != nil && nw > 0 {
					count(nw)
				}
				if werr != nil {
					abort(writeTerm)
					return
				}
			}
			if err == nil {
				continue
			}
			if errors.Is(err, io.EOF) {
				if dst.closeW != nil {
					dst.closeW()
				}
				return
			}
			if isTimeout(err) && src.timeout > 0 && !aborted.Load() && time.Since(time.Unix(0, last.Load())) < src.timeout {
				continue
			}
			if isTimeout(err) {
				abort(readTimeout)
			} else {
				abort(readTerm)
			}
			return
		}
	}
	var res pipeResult
	var wg sync.WaitGroup
	wg.Go(func() { copyDir(client, server, &res.up, 'C', 'c', 'S', onUp) })
	copyDir(server, client, &res.down, 'S', 's', 'C', onDown)
	wg.Wait()
	res.term = byte(term.Load())
	return res
}
