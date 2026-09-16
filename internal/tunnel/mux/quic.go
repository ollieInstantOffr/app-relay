package mux

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
)

const (
	quicCodeClosed  quic.ApplicationErrorCode = 0
	quicCodeAborted quic.StreamErrorCode      = 1
	quicCodeRefused quic.StreamErrorCode      = 2
)

// quicPreamble is the first byte of every stream. quic-go announces a stream
// to the peer only with its first frame, so the gateway writes it on open:
// the home can then accept a stream the gateway only reads from.
var quicPreamble = []byte{0}

// QUICConfig returns the quic-go settings for dialing or listening. Receive
// windows start at their maximum (memory is only used as data arrives, and
// ConnWindow caps the total).
func QUICConfig(cfg Config) *quic.Config {
	c := cfg.withDefaults()
	return &quic.Config{
		KeepAlivePeriod:                c.KeepAlive,
		MaxIdleTimeout:                 c.IdleTimeout,
		MaxIncomingStreams:             int64(c.MaxStreams),
		MaxIncomingUniStreams:          -1,
		InitialStreamReceiveWindow:     c.StreamWindow,
		MaxStreamReceiveWindow:         c.StreamWindow,
		InitialConnectionReceiveWindow: c.ConnWindow,
		MaxConnectionReceiveWindow:     c.ConnWindow,
	}
}

// GatewayQUIC wraps the gateway end of a QUIC tunnel connection. Streams the
// home tries to open are refused.
func GatewayQUIC(conn *quic.Conn, cfg Config) Session {
	s := &quicSession{conn: conn, cfg: cfg.withDefaults(), gateway: true}
	go s.refuseStreams()
	return s
}

// HomeQUIC wraps the home end of a QUIC tunnel connection.
func HomeQUIC(conn *quic.Conn, cfg Config) Session {
	return &quicSession{conn: conn, cfg: cfg.withDefaults()}
}

type quicSession struct {
	conn    *quic.Conn
	cfg     Config
	gateway bool
	closed  atomic.Bool
}

func (s *quicSession) refuseStreams() {
	for {
		str, err := s.conn.AcceptStream(context.Background())
		if err != nil {
			return
		}
		str.CancelRead(quicCodeRefused)
		str.CancelWrite(quicCodeRefused)
	}
}

func (s *quicSession) OpenStream(ctx context.Context) (Stream, error) {
	if !s.gateway {
		return nil, ErrWrongSide
	}
	ctx, cancel := s.cfg.openContext(ctx)
	defer cancel()
	str, err := s.conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, s.opErr(ctx, err, true)
	}
	if d, ok := ctx.Deadline(); ok {
		str.SetWriteDeadline(d)
	}
	if _, err := str.Write(quicPreamble); err != nil {
		str.CancelRead(quicCodeAborted)
		str.CancelWrite(quicCodeAborted)
		return nil, s.opErr(ctx, err, false)
	}
	str.SetWriteDeadline(time.Time{})
	return &quicStream{str: str}, nil
}

func (s *quicSession) AcceptStream(ctx context.Context) (Stream, error) {
	if s.gateway {
		return nil, ErrWrongSide
	}
	str, err := s.conn.AcceptStream(ctx)
	if err != nil {
		return nil, s.opErr(ctx, err, true)
	}
	return &quicStream{str: str, preamble: true}, nil
}

// opErr maps an error of OpenStream or AcceptStream. quic-go fails them with
// the connection's error before its context is canceled, so any error that
// is not the caller's ctx is fatal when fatal is set.
func (s *quicSession) opErr(ctx context.Context, err error, fatal bool) error {
	switch {
	case ctx.Err() != nil && s.conn.Context().Err() == nil:
		return ctx.Err()
	case s.closed.Load():
		return ErrClosed
	case fatal || s.conn.Context().Err() != nil:
		return closedErr(err)
	}
	return err
}

func (s *quicSession) Close() error {
	select {
	case <-s.conn.Context().Done():
	default:
		s.closed.Store(true)
	}
	return s.conn.CloseWithError(quicCodeClosed, "closed")
}

func (s *quicSession) Done() <-chan struct{} { return s.conn.Context().Done() }

func (s *quicSession) Err() error {
	ctx := s.conn.Context()
	if ctx.Err() == nil {
		return nil
	}
	if s.closed.Load() {
		return ErrClosed
	}
	return context.Cause(ctx)
}

func (s *quicSession) Transport() string    { return "quic" }
func (s *quicSession) LocalAddr() net.Addr  { return s.conn.LocalAddr() }
func (s *quicSession) RemoteAddr() net.Addr { return s.conn.RemoteAddr() }

// quicStream maps Stream onto a quic-go stream: CloseWrite sends FIN, Close
// cancels reading and, unless FIN was sent, writing.
type quicStream struct {
	str      *quic.Stream
	preamble bool // home side: the preamble byte is still unread
	wclosed  atomic.Bool
}

func (s *quicStream) Read(p []byte) (int, error) {
	if s.preamble {
		var b [1]byte
		if _, err := io.ReadFull(s.str, b[:]); err != nil {
			return 0, err
		}
		if b[0] != quicPreamble[0] {
			s.Close()
			return 0, errBadPreamble
		}
		s.preamble = false
	}
	return s.str.Read(p)
}

func (s *quicStream) Write(p []byte) (int, error) { return s.str.Write(p) }

func (s *quicStream) CloseWrite() error {
	s.wclosed.Store(true)
	return s.str.Close()
}

func (s *quicStream) Close() error {
	s.str.CancelRead(quicCodeAborted)
	if !s.wclosed.Load() {
		s.str.CancelWrite(quicCodeAborted)
	}
	return nil
}

func (s *quicStream) SetDeadline(t time.Time) error { return s.str.SetDeadline(t) }
