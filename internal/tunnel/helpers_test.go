package tunnel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/proxyproto"
	"github.com/instantoffr/relay/internal/tunnel/mux"
	"github.com/instantoffr/relay/internal/tunnel/pair"
	"github.com/instantoffr/relay/internal/tunnel/wire"
)

// shortDir returns a directory in /tmp (unix socket paths are length-limited).
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "rtun")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func mustIdentity(t *testing.T, cn string) pair.Identity {
	t.Helper()
	id, err := pair.NewIdentity(cn)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func writeGateways(t *testing.T, path string, gws ...Gateway) {
	t.Helper()
	writeJSON(t, path, Gateways{Schema: 1, Gateways: gws})
}

// waitFor polls cond for up to 5 seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func gatewayStatus(e *Engine, id string) (GatewayStatus, bool) {
	for _, g := range e.Status().Gateways {
		if g.ID == id {
			return g, true
		}
	}
	return GatewayStatus{}, false
}

func waitState(t *testing.T, e *Engine, id, state string) GatewayStatus {
	t.Helper()
	var st GatewayStatus
	waitFor(t, "gateway "+id+" "+state, func() bool {
		st, _ = gatewayStatus(e, id)
		return st.State == state
	})
	return st
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// ---------------------------------------------------------------- fake proxy engine

// proxyConn is one connection received by a fake proxy engine socket.
type proxyConn struct {
	socket string
	header proxyproto.Header
}

// fakeProxy listens on unix sockets like the proxy engine's tunnel ingress:
// it requires a PROXY header, reports it, then echoes the data and
// half-closes after the client's EOF.
type fakeProxy struct {
	conns chan proxyConn
}

func newFakeProxy(t *testing.T, sockets ...string) *fakeProxy {
	t.Helper()
	fp := &fakeProxy{conns: make(chan proxyConn, 64)}
	for _, path := range sockets {
		ln, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go fp.serve(path, c.(*net.UnixConn))
			}
		}()
	}
	return fp
}

func (fp *fakeProxy) serve(path string, c *net.UnixConn) {
	defer c.Close()
	br := bufio.NewReader(c)
	h, err := proxyproto.Read(br)
	if err != nil {
		return
	}
	fp.conns <- proxyConn{socket: path, header: h}
	if _, err := io.Copy(c, br); err == nil {
		c.CloseWrite()
	}
	io.Copy(io.Discard, c)
}

func (fp *fakeProxy) next(t *testing.T) proxyConn {
	t.Helper()
	select {
	case pc := <-fp.conns:
		return pc
	case <-time.After(5 * time.Second):
		t.Fatal("no connection reached the proxy engine")
		return proxyConn{}
	}
}

// ---------------------------------------------------------------- fake gateway

// fakeGateway is the gateway end of a tunnel session driven by a test.
type fakeGateway struct {
	t    *testing.T
	sess mux.Session
	ctl  mux.Stream
	wmu  sync.Mutex
	msgs chan *wire.Message
	// pongDelay > 0 answers the home's pings after this delay.
	pongDelay time.Duration
}

func newFakeGateway(t *testing.T, sess mux.Session) *fakeGateway {
	return &fakeGateway{t: t, sess: sess, msgs: make(chan *wire.Message, 64)}
}

// handshake opens the control stream and exchanges hellos; it returns the
// home's hello and then reads control messages in the background.
func (f *fakeGateway) handshake(hello *wire.Hello) *wire.Hello {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctl, err := f.sess.OpenStream(ctx)
	if err != nil {
		f.t.Fatalf("open control stream: %v", err)
	}
	f.ctl = ctl
	f.send(&wire.Message{Hello: hello})
	br := bufio.NewReader(ctl)
	ctl.SetDeadline(time.Now().Add(5 * time.Second))
	m, err := wire.ReadMessage(br)
	if err != nil || m.Hello == nil {
		f.t.Fatalf("home hello: %v %+v", err, m)
	}
	ctl.SetDeadline(time.Time{})
	go func() {
		defer close(f.msgs)
		for {
			m, err := wire.ReadMessage(br)
			if err != nil {
				return
			}
			if m.Ping != nil {
				if f.pongDelay > 0 {
					p := *m.Ping
					time.AfterFunc(f.pongDelay, func() { f.sendRaw(&wire.Message{Pong: &p}) })
				}
				continue
			}
			f.msgs <- m
		}
	}()
	return m.Hello
}

func gatewayHello() *wire.Hello {
	h := wire.LocalHello("gw-1.2.3")
	h.PublicIPs = []string{"198.51.100.1", "2001:db8::1"}
	return h
}

func (f *fakeGateway) send(m *wire.Message) {
	f.t.Helper()
	if err := f.sendRaw(m); err != nil {
		f.t.Errorf("gateway send: %v", err)
	}
}

func (f *fakeGateway) sendRaw(m *wire.Message) error {
	f.wmu.Lock()
	defer f.wmu.Unlock()
	return wire.WriteMessage(f.ctl, m)
}

// routes waits for the next Routes message.
func (f *fakeGateway) routes() *wire.Routes {
	f.t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case m, ok := <-f.msgs:
			if !ok {
				f.t.Fatal("control stream ended while waiting for routes")
			}
			if m.Routes != nil {
				return m.Routes
			}
		case <-timeout:
			f.t.Fatal("no routes received")
		}
	}
}

// open opens a data stream with header h.
func (f *fakeGateway) open(h wire.StreamHeader) mux.Stream {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := f.sess.OpenStream(ctx)
	if err != nil {
		f.t.Fatalf("open stream: %v", err)
	}
	if err := wire.WriteStreamHeader(st, h); err != nil {
		f.t.Fatalf("write stream header: %v", err)
	}
	return st
}

// roundTrip writes data, half-closes and reads the answer to EOF.
func roundTrip(t *testing.T, st mux.Stream, data string) (string, error) {
	t.Helper()
	st.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(st, data); err != nil {
		return "", err
	}
	if err := st.CloseWrite(); err != nil {
		return "", err
	}
	b, err := io.ReadAll(st)
	return string(b), err
}

func httpsHeader(name, src string) wire.StreamHeader {
	return wire.StreamHeader{Kind: wire.KindHTTPS, Port: 443, Src: netip.MustParseAddrPort(src),
		Dst: netip.MustParseAddrPort("198.51.100.1:443"), Name: name}
}

// addrSession overrides a pipe session's remote address.
type addrSession struct {
	mux.Session
	remote net.Addr
}

func (s addrSession) RemoteAddr() net.Addr { return s.remote }

var gatewayRemote = &net.UDPAddr{IP: net.ParseIP("198.51.100.200"), Port: 40000}

// pipeEnv is an engine whose gateways are dialed over mux.Pipe.
type pipeEnv struct {
	t      *testing.T
	dir    string // short: sockets
	e      *Engine
	log    *syncBuffer
	gwFile string
	cfg    *Config
	proxy  *fakeProxy
	dials  chan *fakeGateway
	dialed map[string]int
	mu     sync.Mutex
	pin    string
}

func newPipeEnv(t *testing.T, tune func(*timing)) *pipeEnv {
	t.Helper()
	dir := shortDir(t)
	env := &pipeEnv{
		t: t, dir: dir, log: &syncBuffer{},
		gwFile: filepath.Join(dir, "gateways.json"),
		dials:  make(chan *fakeGateway, 16),
		dialed: map[string]int{},
		pin:    mustIdentity(t, "gw").Fingerprint(),
	}
	env.cfg = &Config{
		Schema:        1,
		GatewaysFile:  env.gwFile,
		RuntimeSocket: filepath.Join(dir, "rt.sock"),
		Targets:       Targets{HTTP: filepath.Join(dir, "http.sock"), HTTPS: filepath.Join(dir, "https.sock")},
		Routes: []Route{{
			GatewayID: "gw1",
			Names:     []string{"app.example.com", "*.wild.test"},
			TCP:       []TCPRoute{{Port: 2222, Socket: filepath.Join(dir, "tcp2222.sock")}},
		}},
	}
	env.proxy = newFakeProxy(t, env.cfg.Targets.HTTP, env.cfg.Targets.HTTPS, filepath.Join(dir, "tcp2222.sock"))
	writeGateways(t, env.gwFile, env.gateway("gw1"))
	tm := timing{backoffMin: 20 * time.Millisecond, backoffMax: 100 * time.Millisecond, hello: 2 * time.Second, drain: time.Second}
	if tune != nil {
		tune(&tm)
	}
	env.e = New(Options{
		Stderr:  env.log,
		Version: "home-9.9.9",
		dial: func(ctx context.Context, gw Gateway) (mux.Session, error) {
			gwSide, home := mux.Pipe(mux.Config{})
			env.mu.Lock()
			env.dialed[gw.ID]++
			env.mu.Unlock()
			select {
			case env.dials <- newFakeGateway(t, gwSide):
			case <-ctx.Done():
				gwSide.Close()
				return nil, ctx.Err()
			}
			return addrSession{home, gatewayRemote}, nil
		},
		timing: tm,
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		env.e.Shutdown(ctx)
		for {
			select {
			case fg := <-env.dials:
				fg.sess.Close()
			default:
				return
			}
		}
	})
	return env
}

func (env *pipeEnv) gateway(id string) Gateway {
	return Gateway{ID: id, Name: "Gateway " + id, Address: "gw.example.net:443", Transport: "auto", Enabled: true, Pin: env.pin}
}

func (env *pipeEnv) dialCount(id string) int {
	env.mu.Lock()
	defer env.mu.Unlock()
	return env.dialed[id]
}

// accept waits for the engine to dial and completes the handshake.
func (env *pipeEnv) accept() *fakeGateway {
	env.t.Helper()
	select {
	case fg := <-env.dials:
		env.t.Cleanup(func() { fg.sess.Close() })
		fg.handshake(gatewayHello())
		return fg
	case <-time.After(5 * time.Second):
		env.t.Fatal("engine did not dial")
		return nil
	}
}

func (env *pipeEnv) cloneConfig() *Config {
	b, _ := json.Marshal(env.cfg)
	var c Config
	json.Unmarshal(b, &c)
	return &c
}

func hasLine(log, substr string) bool {
	for _, l := range strings.Split(log, "\n") {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}
