package tunnel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/instantoffr/relay/internal/proxyproto"
	"github.com/instantoffr/relay/internal/tunnel/mux"
	"github.com/instantoffr/relay/internal/tunnel/wire"
)

var errControlClosed = errors.New("gateway closed the control stream")

// conn serves one established session with a gateway: the control stream
// and the data streams.
type conn struct {
	l      *loop
	g      *gateway
	e      *Engine
	sess   mux.Session
	remote netip.Addr

	ctx    context.Context // canceled when the connection ends
	cancel context.CancelCauseFunc

	ctl     mux.Stream
	br      *bufio.Reader  // control stream reader
	kickCh  chan struct{}  // routes changed (coalesced)
	pongCh  chan wire.Ping // pings from the gateway to answer
	sentGen atomic.Uint64  // latest Routes generation written
}

func newConn(l *loop, sess mux.Session) *conn {
	ctx, cancel := context.WithCancelCause(l.ctx)
	return &conn{
		l: l, g: l.g, e: l.g.e, sess: sess,
		remote: addrIP(sess.RemoteAddr()),
		ctx:    ctx, cancel: cancel,
		kickCh: make(chan struct{}, 1),
		pongCh: make(chan wire.Ping, 16),
	}
}

// kick schedules a Routes push.
func (c *conn) kick() {
	select {
	case c.kickCh <- struct{}{}:
	default:
	}
}

func (c *conn) fail(err error) { c.cancel(err) }

// serve runs the connection until the session, the control stream or the
// loop ends.
func (c *conn) serve() connectResult {
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		select {
		case <-c.sess.Done():
			err := c.sess.Err()
			if err == nil {
				err = mux.ErrClosed
			}
			c.fail(err)
		case <-c.ctx.Done():
		}
		c.sess.Close()
	}()
	defer func() {
		c.cancel(nil)
		<-watched // the session is closed
	}()

	peer, err := c.hello()
	if err != nil {
		if cause := context.Cause(c.ctx); cause != nil && !errors.Is(cause, context.Canceled) {
			err = cause
		}
		return connectResult{err: err}
	}
	local := wire.LocalHello(c.e.opts.Version)
	proto, err := wire.Negotiate(local, peer)
	if err != nil {
		c.e.log.errorf("gateway %s: incompatible tunnel protocol: this Relay %s speaks %d-%d, gateway %s speaks %d-%d; update the older side (retrying every %s)",
			c.g.id, local.Version, local.MinProto, local.Proto, peer.Version, peer.MinProto, peer.Proto, c.e.t.incompatible)
		return connectResult{err: err, incompatible: true}
	}

	connectedAt := time.Now()
	c.g.mu.Lock()
	if c.g.loop != c.l {
		c.g.mu.Unlock()
		return connectResult{err: context.Canceled}
	}
	st := &c.g.st
	if c.g.connected {
		st.Reconnects++
	}
	c.g.connected = true
	st.State = StateConnected
	st.Transport = c.sess.Transport()
	st.ConnectedAt = &connectedAt
	st.Version, st.Proto, st.PublicIPs = peer.Version, proto, slices.Clone(peer.PublicIPs)
	st.AckedGeneration, st.PortErrors, st.RTTMs = 0, nil, 0
	c.g.lastLog = ""
	c.l.conn = c
	c.g.mu.Unlock()
	c.e.log.noticef("gateway %s: connected to %s over %s (gateway %s, protocol %d)", c.g.id, c.sess.RemoteAddr(), c.sess.Transport(), peer.Version, proto)

	c.kick()
	var wg sync.WaitGroup
	wg.Go(c.writeLoop)
	wg.Go(c.readLoop)
	wg.Go(c.acceptLoop)
	<-c.ctx.Done()
	c.sess.Close()
	wg.Wait()

	c.g.mu.Lock()
	if c.l.conn == c {
		c.l.conn = nil
	}
	c.g.mu.Unlock()
	err = context.Cause(c.ctx)
	if errors.Is(err, context.Canceled) {
		err = nil
	}
	return connectResult{err: err, lived: max(time.Since(connectedAt), time.Nanosecond)}
}

// hello accepts the control stream and exchanges Hello messages.
func (c *conn) hello() (*wire.Hello, error) {
	deadline := time.Now().Add(c.e.t.hello)
	ctx, cancel := context.WithDeadline(c.ctx, deadline)
	ctl, err := c.sess.AcceptStream(ctx)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("waiting for the control stream: %w", err)
	}
	c.ctl = ctl
	ctl.SetDeadline(deadline)
	if err := wire.WriteMessage(ctl, &wire.Message{Hello: wire.LocalHello(c.e.opts.Version)}); err != nil {
		return nil, fmt.Errorf("sending hello: %w", err)
	}
	br := bufio.NewReader(ctl)
	m, err := wire.ReadMessage(br)
	if err != nil {
		return nil, fmt.Errorf("reading hello: %w", err)
	}
	if m.Hello == nil {
		return nil, errors.New("gateway did not start with hello")
	}
	ctl.SetDeadline(time.Time{})
	c.br = br
	return m.Hello, nil
}

func (c *conn) writeLoop() {
	ping := time.NewTicker(c.e.t.ping)
	defer ping.Stop()
	var pingID uint64
	for {
		var m *wire.Message
		select {
		case <-c.ctx.Done():
			return
		case <-c.kickCh:
			rt := c.g.routes.Load()
			if rt == nil {
				continue
			}
			m = rt.message()
			c.sentGen.Store(rt.gen)
			c.g.update(c.l, func(st *GatewayStatus) { st.Generation = rt.gen })
		case p := <-c.pongCh:
			m = &wire.Message{Pong: &p}
		case <-ping.C:
			pingID++
			m = &wire.Message{Ping: &wire.Ping{ID: pingID, SentNs: time.Now().UnixNano()}}
		}
		if err := wire.WriteMessage(c.ctl, m); err != nil {
			c.fail(fmt.Errorf("control stream: %w", err))
			return
		}
	}
}

func (c *conn) readLoop() {
	for {
		m, err := wire.ReadMessage(c.br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = errControlClosed
			} else {
				err = fmt.Errorf("control stream: %w", err)
			}
			c.fail(err)
			return
		}
		switch {
		case m.Ack != nil:
			ack := m.Ack
			if ack.Generation != c.sentGen.Load() {
				continue
			}
			c.g.update(c.l, func(st *GatewayStatus) {
				st.AckedGeneration = ack.Generation
				st.PortErrors = slices.Clone(ack.Errors)
			})
			for _, pe := range ack.Errors {
				c.e.log.warnf("gateway %s: cannot publish port %d: %s", c.g.id, pe.Port, pe.Error)
			}
		case m.Pong != nil:
			rtt := time.Duration(time.Now().UnixNano() - m.Pong.SentNs)
			if rtt < 0 || rtt > time.Minute {
				continue
			}
			c.g.update(c.l, func(st *GatewayStatus) { st.RTTMs = float64(rtt) / float64(time.Millisecond) })
		case m.Stats != nil:
			s := *m.Stats
			c.g.update(c.l, func(st *GatewayStatus) { st.GatewayStats = &s })
		case m.Ping != nil:
			select {
			case c.pongCh <- *m.Ping:
			default: // a peer pinging faster than we answer gets fewer pongs
			}
		}
	}
}

func (c *conn) acceptLoop() {
	for {
		st, err := c.sess.AcceptStream(c.ctx)
		if err != nil {
			c.fail(err)
			return
		}
		c.e.inflight.Add(1)
		go c.handleStream(st)
	}
}

// ---------------------------------------------------------------- data streams

// handleStream routes one client connection to the proxy engine.
func (c *conn) handleStream(st mux.Stream) {
	e, g := c.e, c.g
	defer e.inflight.Add(-1)
	if e.draining.Load() {
		st.Close()
		return
	}
	st.SetDeadline(time.Now().Add(e.t.header))
	h, err := wire.ReadStreamHeader(st)
	if err != nil {
		st.Close()
		g.rejected.Add(1)
		g.logReject("malformed stream header: %v", err)
		return
	}
	st.SetDeadline(time.Time{})

	target := g.routes.Load().resolve(h)
	src, okSrc := sanitizeSource(h.Src, c.remote)
	dst := sanitizeDest(h.Dst, src, c.remote, h.Port)
	switch {
	case target == "" && h.Kind == wire.KindTCP:
		st.Close()
		g.rejected.Add(1)
		g.logReject("stream for port %d rejected: not published through this gateway", h.Port)
		return
	case target == "":
		st.Close()
		g.rejected.Add(1)
		g.logReject("%s stream for %q rejected: not published through this gateway", h.Kind, h.Name)
		return
	case !okSrc:
		st.Close()
		g.rejected.Add(1)
		g.logReject("%s stream rejected: no usable client address", h.Kind)
		return
	}

	g.streams.Add(1)
	g.active.Add(1)
	defer g.active.Add(-1)

	dctx, cancel := context.WithTimeout(c.ctx, e.t.target)
	nc, err := (&net.Dialer{}).DialContext(dctx, "unix", target)
	cancel()
	if err != nil {
		st.Close()
		if ok, n := g.targetLog.allow(30 * time.Second); ok {
			more := ""
			if n > 0 {
				more = fmt.Sprintf(" (%d similar errors suppressed)", n)
			}
			e.log.warnf("gateway %s: %s stream: proxy engine unavailable: %v%s", g.id, h.Kind, err, more)
		}
		return
	}
	uc := nc.(*net.UnixConn)
	stop := context.AfterFunc(c.ctx, func() {
		st.Close()
		uc.Close()
	})
	defer stop()

	hdr := proxyproto.AppendV2(nil, src, dst, proxyproto.TLV{Type: proxyproto.TLVRelayTunnel, Value: []byte(g.id)})
	uc.SetWriteDeadline(time.Now().Add(e.t.target))
	if _, err := uc.Write(hdr); err != nil {
		st.Close()
		uc.Close()
		return
	}
	uc.SetWriteDeadline(time.Time{})
	pipe(st, uc, &g.bytesIn, &g.bytesOut)
}

func (g *gateway) logReject(format string, args ...any) {
	ok, n := g.rejectLog.allow(time.Minute)
	if !ok {
		return
	}
	msg := fmt.Sprintf(format, args...)
	if n > 0 {
		msg += fmt.Sprintf(" (%d more rejected streams not logged)", n)
	}
	g.e.log.warnf("gateway %s: %s", g.id, msg)
}

var copyBufs = sync.Pool{New: func() any { b := make([]byte, 32<<10); return &b }}

// countWriter adds the bytes written to n.
type countWriter struct {
	w io.Writer
	n *atomic.Uint64
}

func (cw countWriter) Write(p []byte) (int, error) {
	k, err := cw.w.Write(p)
	cw.n.Add(uint64(k))
	return k, err
}

func copyCount(dst io.Writer, src io.Reader, n *atomic.Uint64) error {
	buf := copyBufs.Get().(*[]byte)
	defer copyBufs.Put(buf)
	_, err := io.CopyBuffer(countWriter{dst, n}, struct{ io.Reader }{src}, *buf)
	return err
}

// pipe copies both directions with half-close: EOF from one side becomes
// CloseWrite on the other; an error aborts both.
func pipe(st mux.Stream, uc *net.UnixConn, in, out *atomic.Uint64) {
	abort := sync.OnceFunc(func() {
		st.Close()
		uc.Close()
	})
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := copyCount(uc, st, in); err != nil {
			abort()
			return
		}
		uc.CloseWrite()
	})
	wg.Go(func() {
		if err := copyCount(st, uc, out); err != nil {
			abort()
			return
		}
		st.CloseWrite()
	})
	wg.Wait()
	st.Close()
	uc.Close()
}
