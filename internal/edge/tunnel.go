package edge

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"

	"github.com/instantoffr/relay/internal/proxyproto"
)

// Tunnel ingress: the tunnel engine connects to unix sockets and writes a
// PROXY protocol v2 header with the client address (and the gateway id as a
// TLV) before the client's bytes. Only hosts and streams published through a
// tunnel are served there.

const proxyHeaderTimeout = 10 * time.Second

// proxyListener accepts unix connections and yields them once their PROXY
// header has been read, without blocking Accept on slow senders.
type proxyListener struct {
	ln      net.Listener
	conns   chan net.Conn
	closed  chan struct{}
	once    sync.Once
	errlog  func(format string, args ...any)
	acceptE error
}

func newProxyListener(ln net.Listener, errlog func(string, ...any)) *proxyListener {
	pl := &proxyListener{ln: ln, conns: make(chan net.Conn), closed: make(chan struct{}), errlog: errlog}
	go pl.acceptLoop()
	return pl
}

func (pl *proxyListener) acceptLoop() {
	for {
		c, err := pl.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				pl.Close()
				return
			}
			time.Sleep(20 * time.Millisecond)
			continue
		}
		go pl.handshake(c)
	}
}

func (pl *proxyListener) handshake(c net.Conn) {
	br := bufio.NewReaderSize(c, 512)
	c.SetReadDeadline(time.Now().Add(proxyHeaderTimeout))
	h, err := proxyproto.Read(br)
	c.SetReadDeadline(time.Time{})
	if err != nil {
		if pl.errlog != nil {
			pl.errlog("tunnel ingress %s: %v", pl.ln.Addr(), err)
		}
		c.Close()
		return
	}
	pc := &proxyConn{Conn: c, br: br, local: pl.ln.Addr()}
	if h.Local {
		pc.remote = &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}
	} else {
		pc.remote = net.TCPAddrFromAddrPort(netip.AddrPortFrom(h.Src.Addr().Unmap(), h.Src.Port()))
		if h.Dst.IsValid() {
			pc.local = net.TCPAddrFromAddrPort(netip.AddrPortFrom(h.Dst.Addr().Unmap(), h.Dst.Port()))
		}
	}
	if v, ok := h.TLV(proxyproto.TLVRelayTunnel); ok {
		pc.tunnel = string(v)
	}
	select {
	case pl.conns <- pc:
	case <-pl.closed:
		c.Close()
	}
}

func (pl *proxyListener) Accept() (net.Conn, error) {
	select {
	case c := <-pl.conns:
		return c, nil
	case <-pl.closed:
		return nil, net.ErrClosed
	}
}

func (pl *proxyListener) Close() error {
	var err error
	pl.once.Do(func() {
		close(pl.closed)
		err = pl.ln.Close()
	})
	return err
}

func (pl *proxyListener) Addr() net.Addr { return pl.ln.Addr() }

// proxyConn reports the addresses from the PROXY header.
type proxyConn struct {
	net.Conn
	br            *bufio.Reader
	remote, local net.Addr
	tunnel        string // gateway id
}

func (c *proxyConn) Read(p []byte) (int, error) {
	if c.br != nil {
		if c.br.Buffered() > 0 {
			return c.br.Read(p)
		}
		c.br = nil
	}
	return c.Conn.Read(p)
}

func (c *proxyConn) RemoteAddr() net.Addr { return c.remote }
func (c *proxyConn) LocalAddr() net.Addr  { return c.local }

// CloseWrite half-closes the underlying unix connection.
func (c *proxyConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// tunnelOf returns the gateway id of a connection that came through a tunnel.
func tunnelOf(c net.Conn) string {
	for c != nil {
		switch v := c.(type) {
		case *proxyConn:
			return v.tunnel
		case *trackedConn:
			c = v.Conn
		case interface{ NetConn() net.Conn }:
			c = v.NetConn()
		default:
			return ""
		}
	}
	return ""
}

type tunnelCtxKey struct{}

// tunnelConnContext stores the gateway id in the request context.
func tunnelConnContext(ctx context.Context, c net.Conn) context.Context {
	if id := tunnelOf(c); id != "" {
		return context.WithValue(ctx, tunnelCtxKey{}, id)
	}
	return ctx
}

// listenUnix binds a unix socket, replacing a stale socket file.
func listenUnix(path string) (net.Listener, error) {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
		os.Remove(path)
	}
	return net.Listen("unix", path)
}
