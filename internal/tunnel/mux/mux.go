// Package mux turns an established, authenticated tunnel connection into a
// stream multiplexer. The gateway opens streams and the home accepts them;
// QUIC and HTTP/2-over-TLS (with reversed client/server roles) share one
// Session interface, and Pipe provides an in-memory pair for tests.
package mux

import (
	"context"
	"errors"
	"io"
	"math"
	"net"
	"time"
)

const (
	ALPNQUIC = "relay-tunnel/1"    // QUIC sessions
	ALPNH2   = "relay-tunnel-h2/1" // HTTP/2-over-TLS sessions
	ALPNPair = "relay-pair/1"      // pairing
)

// Config tunes a session. Zero values mean the defaults below.
type Config struct {
	MaxStreams   int           // concurrent streams the home accepts; default 10000
	KeepAlive    time.Duration // default 15s (QUIC keepalive / HTTP/2 ping after this much read silence)
	IdleTimeout  time.Duration // default 45s: a silent peer is declared dead after about this long
	OpenTimeout  time.Duration // default 10s, bound for OpenStream when the caller's ctx has no deadline
	StreamWindow uint64        // receive window per stream, default 16 MiB
	ConnWindow   uint64        // receive window per connection, default 256 MiB
}

func (c Config) withDefaults() Config {
	if c.MaxStreams <= 0 {
		c.MaxStreams = 10000
	}
	if c.KeepAlive <= 0 {
		c.KeepAlive = 15 * time.Second
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = 45 * time.Second
	}
	if c.OpenTimeout <= 0 {
		c.OpenTimeout = 10 * time.Second
	}
	if c.StreamWindow == 0 {
		c.StreamWindow = 16 << 20
	}
	if c.ConnWindow == 0 {
		c.ConnWindow = 256 << 20
	}
	return c
}

// openContext bounds ctx by OpenTimeout unless it already has a deadline.
func (c Config) openContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, c.OpenTimeout)
}

// window clamps a flow control window to HTTP/2's 2^31-1 limit.
func window(v uint64) int32 {
	return int32(min(v, math.MaxInt32))
}

// Stream is one bidirectional byte stream.
//
// CloseWrite sends EOF to the peer while reading continues. Close aborts the
// stream and unblocks pending Read and Write calls; after CloseWrite, data
// already written is still delivered (only reading is abandoned).
type Stream interface {
	io.Reader
	io.Writer
	CloseWrite() error
	Close() error
	// SetDeadline sets the read and write deadline. On QUIC and Pipe it
	// behaves like net.Conn. On HTTP/2 an expired deadline aborts the stream:
	// pending and later calls fail with os.ErrDeadlineExceeded.
	SetDeadline(t time.Time) error
}

// Session is a multiplexed tunnel session.
type Session interface {
	// OpenStream opens a stream to the home. Gateway side only.
	OpenStream(ctx context.Context) (Stream, error)
	// AcceptStream waits for the next stream from the gateway. Home side only.
	AcceptStream(ctx context.Context) (Stream, error)
	Close() error
	// Done is closed when the session is dead (peer gone, idle timeout, Close).
	Done() <-chan struct{}
	// Err reports why the session died; nil while alive, ErrClosed after Close.
	Err() error
	Transport() string // "quic", "tcp" or "pipe"
	LocalAddr() net.Addr
	RemoteAddr() net.Addr
}

var (
	ErrWrongSide = errors.New("mux: operation not available on this side of the tunnel")
	ErrClosed    = errors.New("mux: session closed")

	errConnLost    = errors.New("mux: tunnel connection lost")
	errBadPreamble = errors.New("mux: bad stream preamble")
	errNoFIN       = errors.New("mux: stream ended without EOF marker")
)

// closedError is returned by calls on a dead session: it matches ErrClosed
// and unwraps to the reason the session died.
type closedError struct{ cause error }

func (e closedError) Error() string        { return ErrClosed.Error() + ": " + e.cause.Error() }
func (e closedError) Is(target error) bool { return target == ErrClosed }
func (e closedError) Unwrap() error        { return e.cause }

func closedErr(cause error) error {
	if cause == nil || errors.Is(cause, ErrClosed) {
		return ErrClosed
	}
	return closedError{cause}
}
