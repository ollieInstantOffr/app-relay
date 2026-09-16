package mux

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// pipeBuffer is how much a Pipe stream buffers per direction before Write
// blocks.
const pipeBuffer = 64 << 10

var errStreamReset = errors.New("mux: stream reset by peer")

// Pipe returns the two ends of an in-memory session, for tests of higher
// layers. Closing either end closes both. OpenStream blocks once MaxStreams
// streams wait to be accepted.
func Pipe(cfg Config) (gateway, home Session) {
	cfg = cfg.withDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	c := &pipeCore{ctx: ctx, cancel: cancel, cfg: cfg, accept: make(chan *pipeStream, cfg.MaxStreams)}
	return &pipeSession{c, true}, &pipeSession{c, false}
}

type pipeCore struct {
	ctx    context.Context
	cancel context.CancelFunc
	cfg    Config
	accept chan *pipeStream
}

type pipeSession struct {
	*pipeCore
	gateway bool
}

func (s *pipeSession) OpenStream(ctx context.Context) (Stream, error) {
	if !s.gateway {
		return nil, ErrWrongSide
	}
	if s.ctx.Err() != nil {
		return nil, ErrClosed
	}
	ctx, cancel := s.cfg.openContext(ctx)
	defer cancel()
	a, b := newPipeStreams(s.ctx)
	select {
	case s.accept <- b:
		return a, nil
	case <-ctx.Done():
		a.Close()
		b.Close()
		return nil, ctx.Err()
	case <-s.ctx.Done():
		return nil, ErrClosed
	}
}

func (s *pipeSession) AcceptStream(ctx context.Context) (Stream, error) {
	if s.gateway {
		return nil, ErrWrongSide
	}
	select {
	case <-s.ctx.Done():
		return nil, ErrClosed
	default:
	}
	select {
	case st := <-s.accept:
		return st, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.ctx.Done():
		return nil, ErrClosed
	}
}

func (s *pipeSession) Close() error {
	s.cancel()
	return nil
}

func (s *pipeSession) Done() <-chan struct{} { return s.ctx.Done() }

func (s *pipeSession) Err() error {
	if s.ctx.Err() != nil {
		return ErrClosed
	}
	return nil
}

func (s *pipeSession) Transport() string    { return "pipe" }
func (s *pipeSession) LocalAddr() net.Addr  { return pipeAddr{} }
func (s *pipeSession) RemoteAddr() net.Addr { return pipeAddr{} }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

func newPipeStreams(ctx context.Context) (*pipeStream, *pipeStream) {
	ab, ba := newHalfPipe(), newHalfPipe()
	a := &pipeStream{r: ba, w: ab, rd: newPipeDeadline(), wd: newPipeDeadline()}
	b := &pipeStream{r: ab, w: ba, rd: newPipeDeadline(), wd: newPipeDeadline()}
	a.stop = context.AfterFunc(ctx, a.kill)
	b.stop = context.AfterFunc(ctx, b.kill)
	return a, b
}

type pipeStream struct {
	r, w   *halfPipe
	rd, wd pipeDeadline
	stop   func() bool
}

func (s *pipeStream) Read(p []byte) (int, error)  { return s.r.read(p, &s.rd) }
func (s *pipeStream) Write(p []byte) (int, error) { return s.w.write(p, &s.wd) }

func (s *pipeStream) CloseWrite() error {
	s.w.update(func() { s.w.wclosed = true })
	return nil
}

func (s *pipeStream) Close() error {
	s.stop()
	s.r.update(func() { s.r.rclosed = true })
	s.w.update(func() {
		if !s.w.wclosed {
			s.w.wreset = true
		}
	})
	return nil
}

// kill tears the stream down when the session dies.
func (s *pipeStream) kill() {
	s.r.update(func() { s.r.rclosed = true })
	s.w.update(func() { s.w.wreset = true })
}

func (s *pipeStream) SetDeadline(t time.Time) error {
	s.rd.set(t)
	s.wd.set(t)
	return nil
}

// halfPipe is one direction of a pipe stream.
type halfPipe struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	wake    chan struct{} // closed and replaced on every state change
	rclosed bool          // reader closed: writes fail
	wclosed bool          // writer sent EOF
	wreset  bool          // writer aborted: reads fail
}

func newHalfPipe() *halfPipe {
	return &halfPipe{wake: make(chan struct{})}
}

func (h *halfPipe) update(f func()) {
	h.mu.Lock()
	f()
	close(h.wake)
	h.wake = make(chan struct{})
	h.mu.Unlock()
}

func (h *halfPipe) read(p []byte, d *pipeDeadline) (int, error) {
	for {
		h.mu.Lock()
		switch {
		case h.rclosed:
			h.mu.Unlock()
			return 0, net.ErrClosed
		case h.wreset:
			h.mu.Unlock()
			return 0, errStreamReset
		case d.expired():
			h.mu.Unlock()
			return 0, os.ErrDeadlineExceeded
		case h.buf.Len() > 0:
			n, _ := h.buf.Read(p)
			close(h.wake)
			h.wake = make(chan struct{})
			h.mu.Unlock()
			return n, nil
		case h.wclosed:
			h.mu.Unlock()
			return 0, io.EOF
		case len(p) == 0:
			h.mu.Unlock()
			return 0, nil
		}
		wake := h.wake
		h.mu.Unlock()
		select {
		case <-wake:
		case <-d.wait():
		}
	}
}

func (h *halfPipe) write(p []byte, d *pipeDeadline) (int, error) {
	n := 0
	for {
		h.mu.Lock()
		switch {
		case h.wreset:
			h.mu.Unlock()
			return n, net.ErrClosed
		case h.wclosed:
			h.mu.Unlock()
			return n, errWriteClosed
		case h.rclosed:
			h.mu.Unlock()
			return n, errStreamReset
		case d.expired():
			h.mu.Unlock()
			return n, os.ErrDeadlineExceeded
		}
		if room := pipeBuffer - h.buf.Len(); room > 0 {
			m := min(room, len(p)-n)
			h.buf.Write(p[n : n+m])
			n += m
			close(h.wake)
			h.wake = make(chan struct{})
			if n == len(p) {
				h.mu.Unlock()
				return n, nil
			}
		}
		wake := h.wake
		h.mu.Unlock()
		select {
		case <-wake:
		case <-d.wait():
		}
	}
}

// pipeDeadline is a resettable deadline, as in net.Pipe: wait returns a
// channel that is closed when the deadline passes.
type pipeDeadline struct {
	mu     *sync.Mutex
	timer  *time.Timer
	cancel chan struct{}
}

func newPipeDeadline() pipeDeadline {
	return pipeDeadline{mu: new(sync.Mutex), cancel: make(chan struct{})}
}

func (d *pipeDeadline) set(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.timer != nil && !d.timer.Stop() {
		<-d.cancel // wait for the timer callback to close cancel
	}
	d.timer = nil
	closed := isClosedChan(d.cancel)
	if t.IsZero() {
		if closed {
			d.cancel = make(chan struct{})
		}
		return
	}
	if dur := time.Until(t); dur > 0 {
		if closed {
			d.cancel = make(chan struct{})
		}
		cancel := d.cancel
		d.timer = time.AfterFunc(dur, func() { close(cancel) })
		return
	}
	if !closed {
		close(d.cancel)
	}
}

func (d *pipeDeadline) wait() chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cancel
}

func (d *pipeDeadline) expired() bool { return isClosedChan(d.wait()) }

func isClosedChan(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}
