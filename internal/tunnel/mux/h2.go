package mux

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2"
)

// HTTP/2 sessions reverse the roles of the TCP connection: the home dials and
// runs the HTTP/2 server, the gateway runs the client and opens one CONNECT
// request per stream. Gateway-to-home data is the request body; its EOF is the
// request's END_STREAM. Home-to-gateway data is framed in the response body
// as [uint32 length][data] chunks with a zero length marking EOF, because an
// HTTP/2 server handler cannot end its response while still reading the
// request.
//
// Flow control: the home's receive windows are http2.Server's
// MaxUploadBufferPerStream/PerConnection, the gateway's are the
// http.HTTP2Config MaxReceiveBuffer* of the underlying http.Transport (the
// x/net Transport has no window fields). Both are static and capped at
// 2^31-1.

const (
	h2Authority  = "tunnel"
	h2MaxChunk   = 1 << 20
	h2ForceClose = 250 * time.Millisecond // bound for closing a tls.Conn to a silent peer
)

var (
	errWriteClosed = errors.New("mux: write after CloseWrite")
	aLongTimeAgo   = time.Unix(1, 0)
)

func checkH2Conn(conn *tls.Conn) error {
	st := conn.ConnectionState()
	if !st.HandshakeComplete {
		return errors.New("mux: TLS handshake not complete")
	}
	if st.NegotiatedProtocol != ALPNH2 {
		return fmt.Errorf("mux: negotiated protocol %q, want %q", st.NegotiatedProtocol, ALPNH2)
	}
	return nil
}

// h2PingTimeout makes KeepAlive plus the ping timeout add up to IdleTimeout.
func (c Config) h2PingTimeout() time.Duration {
	if d := c.IdleTimeout - c.KeepAlive; d >= c.KeepAlive {
		return d
	}
	return c.KeepAlive
}

// h2Conn reports when net/http closes the tunnel connection, which it does
// whenever the connection dies (read error, lost ping, write timeout), and
// remembers why.
type h2Conn struct {
	*tls.Conn
	onClose func(error)
	once    sync.Once
	mu      sync.Mutex
	err     error
}

func (c *h2Conn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if err != nil {
		c.fail(err)
	}
	return n, err
}

func (c *h2Conn) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
}

func (c *h2Conn) cause() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *h2Conn) Close() error {
	c.fail(errConnLost)
	c.once.Do(func() { c.onClose(c.cause()) })
	// tls.Conn.Close blocks while sending close_notify to a peer that does
	// not read; net/http force-closes only a bare *tls.Conn.
	t := time.AfterFunc(h2ForceClose, func() { c.NetConn().Close() })
	defer t.Stop()
	return c.Conn.Close()
}

type h2Base struct {
	conn *h2Conn
	ctx  context.Context
	end  context.CancelCauseFunc
}

func newH2Base(conn *tls.Conn) h2Base {
	ctx, end := context.WithCancelCause(context.Background())
	return h2Base{conn: &h2Conn{Conn: conn, onClose: end}, ctx: ctx, end: end}
}

func (b *h2Base) Done() <-chan struct{} { return b.ctx.Done() }

func (b *h2Base) Err() error {
	if b.ctx.Err() == nil {
		return nil
	}
	return context.Cause(b.ctx)
}

func (b *h2Base) Transport() string    { return "tcp" }
func (b *h2Base) LocalAddr() net.Addr  { return b.conn.LocalAddr() }
func (b *h2Base) RemoteAddr() net.Addr { return b.conn.RemoteAddr() }

// GatewayTCP runs the HTTP/2 client over conn, the gateway end of a
// TLS tunnel connection negotiated with ALPNH2.
func GatewayTCP(conn *tls.Conn, cfg Config) (Session, error) {
	if err := checkH2Conn(conn); err != nil {
		return nil, err
	}
	cfg = cfg.withDefaults()
	s := &h2Gateway{h2Base: newH2Base(conn), cfg: cfg}
	t1 := &http.Transport{HTTP2: &http.HTTP2Config{
		StrictMaxConcurrentRequests:   true,
		MaxReceiveBufferPerStream:     int(window(cfg.StreamWindow)),
		MaxReceiveBufferPerConnection: int(window(cfg.ConnWindow)),
		SendPingTimeout:               cfg.KeepAlive,
		PingTimeout:                   cfg.h2PingTimeout(),
		WriteByteTimeout:              cfg.IdleTimeout,
	}}
	tr, err := http2.ConfigureTransports(t1)
	if err != nil {
		return nil, err
	}
	tr.StrictMaxConcurrentStreams = true
	tr.ReadIdleTimeout = cfg.KeepAlive
	tr.PingTimeout = cfg.h2PingTimeout()
	tr.WriteByteTimeout = cfg.IdleTimeout
	s.cc, err = tr.NewClientConn(s.conn)
	if err != nil {
		s.end(err)
		conn.Close()
		return nil, err
	}
	return s, nil
}

type h2Gateway struct {
	h2Base
	cfg Config
	cc  *http2.ClientConn
}

func (s *h2Gateway) OpenStream(ctx context.Context) (Stream, error) {
	if s.ctx.Err() != nil {
		return nil, closedErr(s.Err())
	}
	ctx, cancel := s.cfg.openContext(ctx)
	defer cancel()

	sctx, scancel := context.WithCancel(s.ctx)
	pr, pw := io.Pipe()
	wrote := make(chan struct{})
	var wroteOnce sync.Once
	trace := &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) {
		wroteOnce.Do(func() { close(wrote) })
	}}
	req := (&http.Request{
		Method:        http.MethodConnect,
		URL:           &url.URL{Scheme: "https", Host: h2Authority},
		Host:          h2Authority,
		Proto:         "HTTP/2.0",
		ProtoMajor:    2,
		Header:        make(http.Header),
		Body:          pr,
		ContentLength: -1,
	}).WithContext(httptrace.WithClientTrace(sctx, trace))

	stop := context.AfterFunc(ctx, scancel)
	resp, err := s.cc.RoundTrip(req)
	canceled := !stop()
	if err == nil && (canceled || resp.StatusCode != http.StatusOK) {
		resp.Body.Close()
		if !canceled {
			err = fmt.Errorf("mux: stream refused: %s", resp.Status)
		}
	}
	if canceled || err != nil {
		scancel()
		pw.CloseWithError(net.ErrClosed)
		switch {
		case s.ctx.Err() != nil:
			return nil, closedErr(s.Err())
		case canceled:
			return nil, ctx.Err()
		}
		return nil, err
	}
	st := &h2GatewayStream{
		sess:    s,
		ctx:     sctx,
		cancel:  scancel,
		body:    resp.Body,
		pw:      pw,
		wrote:   wrote,
		aborted: make(chan struct{}),
	}
	context.AfterFunc(sctx, func() { st.abort(closedErr(s.Err())) })
	return st, nil
}

func (s *h2Gateway) AcceptStream(context.Context) (Stream, error) { return nil, ErrWrongSide }

func (s *h2Gateway) Close() error {
	s.end(ErrClosed)
	return s.cc.Close()
}

// h2GatewayStream is one CONNECT request. Its context is canceled when the
// stream is aborted or the session dies.
type h2GatewayStream struct {
	sess   *h2Gateway
	ctx    context.Context
	cancel context.CancelFunc
	body   io.ReadCloser
	pw     *io.PipeWriter
	wrote  chan struct{} // the request body was fully sent (or failed)

	left uint32 // unread bytes of the current chunk
	fin  bool
	hdr  [4]byte

	wclosed   atomic.Bool
	closed    atomic.Bool
	closeOnce sync.Once
	abortOnce sync.Once
	aborted   chan struct{}
	err       error
	timer     abortTimer
}

func (st *h2GatewayStream) abort(err error) {
	st.abortOnce.Do(func() {
		st.err = err
		close(st.aborted)
		st.timer.stop()
		st.cancel()
		st.pw.CloseWithError(err)
		st.body.Close()
	})
}

func (st *h2GatewayStream) localErr() error {
	select {
	case <-st.aborted:
		return st.err
	default:
	}
	if st.closed.Load() {
		return net.ErrClosed
	}
	if st.sess.ctx.Err() != nil {
		return closedErr(st.sess.Err())
	}
	return nil
}

func (st *h2GatewayStream) Read(p []byte) (int, error) {
	if err := st.localErr(); err != nil {
		return 0, err
	}
	if st.fin {
		return 0, io.EOF
	}
	if st.left == 0 {
		if _, err := io.ReadFull(st.body, st.hdr[:]); err != nil {
			return 0, st.readErr(err)
		}
		st.left = binary.BigEndian.Uint32(st.hdr[:])
		if st.left == 0 {
			st.fin = true
			return 0, io.EOF
		}
	}
	if uint64(len(p)) > uint64(st.left) {
		p = p[:st.left]
	}
	n, err := st.body.Read(p)
	st.left -= uint32(n)
	if err == io.EOF && n > 0 {
		err = nil
	}
	if err != nil {
		err = st.readErr(err)
	}
	return n, err
}

func (st *h2GatewayStream) readErr(err error) error {
	if lerr := st.localErr(); lerr != nil {
		return lerr
	}
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return errNoFIN
	}
	return err
}

func (st *h2GatewayStream) Write(p []byte) (int, error) {
	if err := st.localErr(); err != nil {
		return 0, err
	}
	n, err := st.pw.Write(p)
	if err != nil {
		switch lerr := st.localErr(); {
		case lerr != nil:
			err = lerr
		case st.wclosed.Load():
			err = errWriteClosed
		default:
			err = errStreamReset
		}
	}
	return n, err
}

func (st *h2GatewayStream) CloseWrite() error {
	if st.wclosed.Swap(true) {
		return nil
	}
	return st.pw.Close()
}

// Close aborts the request. After CloseWrite it first lets the request body
// finish sending (bounded by IdleTimeout); a Read pending meanwhile returns
// when the abort happens.
func (st *h2GatewayStream) Close() error {
	st.closeOnce.Do(func() {
		st.closed.Store(true)
		if !st.wclosed.Load() || isClosedChan(st.wrote) {
			st.abort(net.ErrClosed)
			return
		}
		go func() {
			t := time.NewTimer(st.sess.cfg.IdleTimeout)
			defer t.Stop()
			select {
			case <-st.wrote:
			case <-st.ctx.Done():
			case <-t.C:
			}
			st.abort(net.ErrClosed)
		}()
	})
	return nil
}

func (st *h2GatewayStream) SetDeadline(t time.Time) error {
	st.timer.set(t, func() { st.abort(os.ErrDeadlineExceeded) })
	return nil
}

// HomeTCP runs the HTTP/2 server over conn, the home end of a TLS tunnel
// connection negotiated with ALPNH2.
func HomeTCP(conn *tls.Conn, cfg Config) (Session, error) {
	if err := checkH2Conn(conn); err != nil {
		return nil, err
	}
	cfg = cfg.withDefaults()
	s := &h2Home{h2Base: newH2Base(conn), accept: make(chan *h2HomeStream)}
	srv := &http2.Server{
		MaxConcurrentStreams:         uint32(min(cfg.MaxStreams, math.MaxUint32)),
		MaxUploadBufferPerStream:     window(cfg.StreamWindow),
		MaxUploadBufferPerConnection: window(cfg.ConnWindow),
		ReadIdleTimeout:              cfg.KeepAlive,
		PingTimeout:                  cfg.h2PingTimeout(),
		WriteByteTimeout:             cfg.IdleTimeout,
	}
	opts := &http2.ServeConnOpts{
		Context:    s.ctx,
		BaseConfig: &http.Server{ErrorLog: log.New(io.Discard, "", 0)},
		Handler:    http.HandlerFunc(s.serve),
	}
	go func() {
		srv.ServeConn(s.conn, opts)
		s.conn.Close()
	}()
	return s, nil
}

type h2Home struct {
	h2Base
	accept chan *h2HomeStream
}

func (s *h2Home) OpenStream(context.Context) (Stream, error) { return nil, ErrWrongSide }

func (s *h2Home) AcceptStream(ctx context.Context) (Stream, error) {
	if s.ctx.Err() != nil {
		return nil, closedErr(s.Err())
	}
	select {
	case st := <-s.accept:
		return st, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.ctx.Done():
		return nil, closedErr(s.Err())
	}
}

func (s *h2Home) Close() error {
	s.end(ErrClosed)
	return s.conn.Close()
}

// serve runs one stream. The handler outlives the stream's use: it returns
// only once no Read or Write is in progress, normally after both EOFs, else
// by resetting the stream.
func (s *h2Home) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	rc := http.NewResponseController(w)
	w.WriteHeader(http.StatusOK)
	if rc.Flush() != nil {
		return
	}
	st := &h2HomeStream{
		sess:     s,
		w:        w,
		rc:       rc,
		body:     r.Body,
		finished: make(chan struct{}),
		readDone: make(chan struct{}),
		aborted:  make(chan struct{}),
	}
	select {
	case s.accept <- st:
	case <-r.Context().Done():
		return
	}

	peerGone := r.Context().Done()
	var readDone chan struct{}
	for !isClosedChan(st.finished) {
		select {
		case <-st.finished:
			continue
		case <-st.aborted:
		case <-s.ctx.Done():
			st.abort(closedErr(s.Err()))
		case <-peerGone:
			// The gateway reset the stream: data it sent before stays
			// readable, so wait for the reader to drain it (or Close).
			peerGone, readDone = nil, st.readDone
			continue
		case <-readDone:
			st.abort(errStreamReset)
		}
		break
	}
	graceful := isClosedChan(st.finished)
	if !graceful {
		st.abort(net.ErrClosed)
		rc.SetReadDeadline(aLongTimeAgo)
		rc.SetWriteDeadline(aLongTimeAgo)
	}
	st.wmu.Lock()
	st.rmu.Lock()
	st.gone = true
	st.rmu.Unlock()
	st.wmu.Unlock()
	if !graceful {
		panic(http.ErrAbortHandler)
	}
}

type h2HomeStream struct {
	sess *h2Home
	w    http.ResponseWriter
	rc   *http.ResponseController
	body io.ReadCloser

	rmu      sync.Mutex
	wmu      sync.Mutex
	gone     bool // the handler returned; guarded by rmu and wmu
	hdr      [4]byte
	sawEOF   atomic.Bool
	wclosed  atomic.Bool
	finished chan struct{} // both directions saw EOF
	finOnce  sync.Once
	readDone chan struct{} // a Read returned an error
	rdOnce   sync.Once

	abortOnce sync.Once
	aborted   chan struct{}
	err       error
	timer     abortTimer
}

func (st *h2HomeStream) abort(err error) {
	st.abortOnce.Do(func() {
		st.err = err
		close(st.aborted)
		st.timer.stop()
	})
}

func (st *h2HomeStream) abortErr() error {
	select {
	case <-st.aborted:
		return st.err
	default:
		return nil
	}
}

func (st *h2HomeStream) maybeFinish() {
	if st.sawEOF.Load() && st.wclosed.Load() {
		st.finOnce.Do(func() { close(st.finished) })
	}
}

func (st *h2HomeStream) Read(p []byte) (int, error) {
	st.rmu.Lock()
	defer st.rmu.Unlock()
	switch {
	case st.abortErr() != nil:
		return 0, st.abortErr()
	case st.sawEOF.Load():
		return 0, io.EOF
	case st.gone:
		return 0, net.ErrClosed
	}
	n, err := st.body.Read(p)
	if err != nil {
		if err == io.EOF {
			st.sawEOF.Store(true)
			st.maybeFinish()
		} else if aerr := st.abortErr(); aerr != nil {
			err = aerr
		}
		st.rdOnce.Do(func() { close(st.readDone) })
	}
	return n, err
}

func (st *h2HomeStream) writable() error {
	switch {
	case st.abortErr() != nil:
		return st.abortErr()
	case st.gone:
		return net.ErrClosed
	case st.wclosed.Load():
		return errWriteClosed
	}
	return nil
}

func (st *h2HomeStream) writeErr(err error) error {
	if aerr := st.abortErr(); aerr != nil {
		return aerr
	}
	return err
}

func (st *h2HomeStream) Write(p []byte) (int, error) {
	st.wmu.Lock()
	defer st.wmu.Unlock()
	if err := st.writable(); err != nil {
		return 0, err
	}
	n := 0
	for n < len(p) {
		chunk := p[n:min(len(p), n+h2MaxChunk)]
		binary.BigEndian.PutUint32(st.hdr[:], uint32(len(chunk)))
		if _, err := st.w.Write(st.hdr[:]); err != nil {
			return n, st.writeErr(err)
		}
		m, err := st.w.Write(chunk)
		n += m
		if err != nil {
			return n, st.writeErr(err)
		}
	}
	if err := st.rc.Flush(); err != nil {
		return n, st.writeErr(err)
	}
	return n, nil
}

// CloseWrite sends the EOF marker. The response itself ends when the request
// body has also been read to EOF.
func (st *h2HomeStream) CloseWrite() error {
	st.wmu.Lock()
	defer st.wmu.Unlock()
	if err := st.writable(); err != nil {
		if err == errWriteClosed {
			return nil
		}
		return err
	}
	clear(st.hdr[:])
	st.w.Write(st.hdr[:])
	if err := st.rc.Flush(); err != nil {
		return st.writeErr(err)
	}
	st.wclosed.Store(true)
	st.maybeFinish()
	return nil
}

func (st *h2HomeStream) Close() error {
	st.abort(net.ErrClosed)
	return nil
}

func (st *h2HomeStream) SetDeadline(t time.Time) error {
	st.timer.set(t, func() { st.abort(os.ErrDeadlineExceeded) })
	return nil
}

// abortTimer calls fire when a deadline passes.
type abortTimer struct {
	mu      sync.Mutex
	t       *time.Timer
	stopped bool
}

func (a *abortTimer) set(t time.Time, fire func()) {
	a.mu.Lock()
	if a.t != nil {
		a.t.Stop()
		a.t = nil
	}
	if a.stopped || t.IsZero() {
		a.mu.Unlock()
		return
	}
	if d := time.Until(t); d > 0 {
		a.t = time.AfterFunc(d, fire)
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()
	fire()
}

func (a *abortTimer) stop() {
	a.mu.Lock()
	a.stopped = true
	if a.t != nil {
		a.t.Stop()
	}
	a.mu.Unlock()
}
