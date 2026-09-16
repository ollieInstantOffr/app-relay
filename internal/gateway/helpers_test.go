package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/sniff"
	"github.com/instantoffr/relay/internal/tunnel/mux"
	"github.com/instantoffr/relay/internal/tunnel/pair"
	"github.com/instantoffr/relay/internal/tunnel/wire"
)

const testTimeout = 5 * time.Second

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func testCtx(t testing.TB) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func waitFor(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// freePort returns a TCP port that was free a moment ago.
func freePort(t testing.TB) uint16 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return addrPort(ln.Addr()).Port()
}

func mustToken(t testing.TB) pair.Token {
	t.Helper()
	tok, err := pair.NewToken(10 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func mustIdentity(t testing.TB, cn string) pair.Identity {
	t.Helper()
	id, err := pair.NewIdentity(cn)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// newServer starts a gateway on loopback with ephemeral ports. The logs are
// printed when the test fails.
func newServer(t testing.TB, opts Options, tweak ...func(*Server)) *Server {
	t.Helper()
	if opts.DataDir == "" {
		opts.DataDir = t.TempDir()
	}
	if opts.TunnelAddr == "" {
		opts.TunnelAddr = "127.0.0.1:0"
	}
	if opts.HTTPAddr == "" {
		opts.HTTPAddr = "127.0.0.1:0"
	}
	if opts.HTTPSAddr == "" {
		opts.HTTPSAddr = "127.0.0.1:0"
	}
	if opts.BindHost == "" {
		opts.BindHost = "127.0.0.1"
	}
	if opts.Version == "" {
		opts.Version = "test"
	}
	logs := &syncBuffer{}
	opts.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range tweak {
		f(srv)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		srv.Close()
		if t.Failed() {
			t.Logf("gateway logs:\n%s", logs.String())
		}
	})
	return srv
}

// fakeHome is the home end of a session, speaking the control protocol.
type fakeHome struct {
	t     testing.TB
	sess  mux.Session
	ctl   mux.Stream
	hello *wire.Hello // the gateway's
	msgs  chan *wire.Message
}

// connectHome runs the home side of the hello on sess.
func connectHome(t testing.TB, sess mux.Session) *fakeHome {
	t.Helper()
	t.Cleanup(func() { sess.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	ctl, err := sess.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("accept control stream: %v", err)
	}
	if err := wire.WriteMessage(ctl, &wire.Message{Hello: wire.LocalHello("home-test")}); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(ctl)
	m, err := wire.ReadMessage(br)
	if err != nil {
		t.Fatalf("read gateway hello: %v", err)
	}
	if m.Hello == nil {
		t.Fatalf("first message is not a hello: %+v", m)
	}
	h := &fakeHome{t: t, sess: sess, ctl: ctl, hello: m.Hello, msgs: make(chan *wire.Message, 256)}
	go func() {
		defer close(h.msgs)
		for {
			m, err := wire.ReadMessage(br)
			if err != nil {
				return
			}
			h.msgs <- m
		}
	}()
	return h
}

// pipeHome attaches an in-memory session to srv and waits until it is active.
func pipeHome(t testing.TB, srv *Server) *fakeHome {
	t.Helper()
	gw, home := mux.Pipe(mux.Config{})
	srv.AttachSession(gw)
	h := connectHome(t, home)
	waitActive(t, srv, gw)
	return h
}

func waitActive(t testing.TB, srv *Server, sess mux.Session) {
	t.Helper()
	waitFor(t, "active session", func() bool {
		ts := srv.active.Load()
		return ts != nil && (sess == nil || ts.sess == sess)
	})
}

func (h *fakeHome) send(m *wire.Message) {
	h.t.Helper()
	if err := wire.WriteMessage(h.ctl, m); err != nil {
		h.t.Fatalf("send control message: %v", err)
	}
}

// expect returns the next message matching pred, skipping others.
func (h *fakeHome) expect(what string, pred func(*wire.Message) bool) *wire.Message {
	h.t.Helper()
	timer := time.NewTimer(testTimeout)
	defer timer.Stop()
	for {
		select {
		case m, ok := <-h.msgs:
			if !ok {
				h.t.Fatalf("control stream closed while waiting for %s", what)
			}
			if pred(m) {
				return m
			}
		case <-timer.C:
			h.t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// publish pushes routes and returns the gateway's ack.
func (h *fakeHome) publish(gen uint64, names []string, tcp ...uint16) *wire.RoutesAck {
	h.t.Helper()
	h.send(&wire.Message{Routes: &wire.Routes{Generation: gen, Names: names, TCP: tcp}})
	m := h.expect("routes ack", func(m *wire.Message) bool { return m.Ack != nil && m.Ack.Generation == gen })
	return m.Ack
}

// accept waits for a data stream and reads its header.
func (h *fakeHome) accept(timeout time.Duration) (mux.Stream, wire.StreamHeader, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	st, err := h.sess.AcceptStream(ctx)
	if err != nil {
		return nil, wire.StreamHeader{}, err
	}
	st.SetDeadline(time.Now().Add(testTimeout))
	hdr, err := wire.ReadStreamHeader(st)
	if err != nil {
		st.Close()
		return nil, hdr, err
	}
	st.SetDeadline(time.Time{})
	return st, hdr, nil
}

func (h *fakeHome) mustAccept() (mux.Stream, wire.StreamHeader) {
	h.t.Helper()
	st, hdr, err := h.accept(testTimeout)
	if err != nil {
		h.t.Fatalf("accept data stream: %v", err)
	}
	h.t.Cleanup(func() { st.Close() })
	return st, hdr
}

// expectNoStream fails when a data stream arrives within a short time.
func (h *fakeHome) expectNoStream() {
	h.t.Helper()
	if st, hdr, err := h.accept(200 * time.Millisecond); err == nil {
		st.Close()
		h.t.Fatalf("unexpected stream %+v", hdr)
	}
}

// clientHello returns the first flight of a TLS client for sni.
func clientHello(t testing.TB, sni string) []byte {
	t.Helper()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go tls.Client(a, &tls.Config{ServerName: sni, InsecureSkipVerify: true}).Handshake()
	var data []byte
	buf := make([]byte, 4096)
	b.SetReadDeadline(time.Now().Add(testTimeout))
	for {
		n, err := b.Read(buf)
		data = append(data, buf[:n]...)
		if name, done := sniff.ClientHelloSNI(data); done {
			if name != sni {
				t.Fatalf("generated ClientHello has SNI %q", name)
			}
			return data
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func dial(t testing.TB, addr string) *net.TCPConn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, testTimeout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c.(*net.TCPConn)
}

func loopback(port uint16) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))) }

// expectClosed checks that the gateway closes c without sending anything.
func expectClosed(t testing.TB, c net.Conn) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(testTimeout))
	n, err := io.Copy(io.Discard, c)
	if err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Fatal("connection was not closed")
		}
	}
	if n != 0 {
		t.Fatalf("read %d unexpected bytes", n)
	}
}

// expectOpen checks that c stays open for a short while.
func expectOpen(t testing.TB, c net.Conn) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	_, err := c.Read(make([]byte, 1))
	if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
		t.Fatalf("connection closed: %v", err)
	}
	c.SetReadDeadline(time.Time{})
}

func readN(t testing.TB, r io.Reader, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("read %d bytes: %v", n, err)
	}
	return buf
}
