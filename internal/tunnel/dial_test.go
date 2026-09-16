package tunnel

import (
	"bufio"
	"context"
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/instantoffr/relay/internal/tunnel/mux"
	"github.com/instantoffr/relay/internal/tunnel/pair"
	"github.com/instantoffr/relay/internal/tunnel/wire"
)

// testGateway is a minimal gateway: it accepts pinned tunnel connections,
// runs the hello exchange, acknowledges routes and forwards one test stream
// on demand.
type testGateway struct {
	t        *testing.T
	id       pair.Identity
	homePin  string
	addr     string // 127.0.0.1:port, shared by UDP and TCP
	sessions chan mux.Session

	mu    sync.Mutex
	close []func()
}

func newTestGateway(t *testing.T, homePin string) *testGateway {
	gw := &testGateway{t: t, id: mustIdentity(t, "gateway"), homePin: homePin, sessions: make(chan mux.Session, 8)}
	t.Cleanup(gw.stop)
	return gw
}

// freeAddr returns a loopback port free for both TCP and UDP.
func freeAddr(t *testing.T) string {
	t.Helper()
	for range 20 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		pc, err := net.ListenPacket("udp", addr)
		ln.Close()
		if err == nil {
			pc.Close()
			return addr
		}
	}
	t.Fatal("no free port")
	return ""
}

func (gw *testGateway) onClose(f func()) {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	gw.close = append(gw.close, f)
}

// stop closes the listeners and every session.
func (gw *testGateway) stop() {
	gw.mu.Lock()
	fs := gw.close
	gw.close = nil
	gw.mu.Unlock()
	for _, f := range fs {
		f()
	}
}

func (gw *testGateway) listenQUIC() {
	gw.t.Helper()
	ln, err := quic.ListenAddr(gw.addr, pair.ServerConfig(gw.id, func() string { return gw.homePin }, mux.ALPNQUIC), mux.QUICConfig(mux.Config{}))
	if err != nil {
		gw.t.Fatal(err)
	}
	gw.onClose(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept(context.Background())
			if err != nil {
				return
			}
			go gw.serve(mux.GatewayQUIC(c, mux.Config{}))
		}
	}()
}

func (gw *testGateway) listenTCP() {
	gw.t.Helper()
	ln, err := tls.Listen("tcp", gw.addr, pair.ServerConfig(gw.id, func() string { return gw.homePin }, mux.ALPNH2))
	if err != nil {
		gw.t.Fatal(err)
	}
	gw.onClose(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				tc := c.(*tls.Conn)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := tc.HandshakeContext(ctx); err != nil {
					tc.Close()
					return
				}
				sess, err := mux.GatewayTCP(tc, mux.Config{})
				if err != nil {
					tc.Close()
					return
				}
				gw.serve(sess)
			}()
		}
	}()
}

// serve runs the gateway side of the control protocol.
func (gw *testGateway) serve(sess mux.Session) {
	gw.onClose(func() { sess.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctl, err := sess.OpenStream(ctx)
	if err != nil {
		sess.Close()
		return
	}
	if wire.WriteMessage(ctl, &wire.Message{Hello: wire.LocalHello("gw-test")}) != nil {
		sess.Close()
		return
	}
	br := bufio.NewReader(ctl)
	var once sync.Once
	for {
		m, err := wire.ReadMessage(br)
		if err != nil {
			sess.Close()
			return
		}
		switch {
		case m.Routes != nil:
			wire.WriteMessage(ctl, &wire.Message{Ack: &wire.RoutesAck{Generation: m.Routes.Generation}})
			once.Do(func() { gw.sessions <- sess })
		case m.Ping != nil:
			wire.WriteMessage(ctl, &wire.Message{Pong: m.Ping})
		}
	}
}

func (gw *testGateway) session(t *testing.T) mux.Session {
	t.Helper()
	select {
	case s := <-gw.sessions:
		return s
	case <-time.After(10 * time.Second):
		t.Fatal("home did not connect to the gateway")
		return nil
	}
}

// dialEnv is an engine dialing real gateways over loopback.
type dialEnv struct {
	e     *Engine
	log   *syncBuffer
	cfg   *Config
	home  pair.Identity
	gw    *testGateway
	proxy *fakeProxy
	def   Gateway
	dir   string
}

func newDialEnv(t *testing.T, transport string) *dialEnv {
	t.Helper()
	dir := shortDir(t)
	home := mustIdentity(t, "home")
	certPEM, keyPEM, err := home.MarshalPEM()
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile := filepath.Join(dir, "home.crt"), filepath.Join(dir, "home.key")
	writeFile(t, certFile, certPEM)
	writeFile(t, keyFile, keyPEM)
	gw := newTestGateway(t, home.Fingerprint())
	gw.addr = freeAddr(t)
	env := &dialEnv{
		log: &syncBuffer{}, home: home, gw: gw, dir: dir,
		def: Gateway{ID: "gw1", Address: gw.addr, Transport: transport, Enabled: true, Pin: gw.id.Fingerprint(), CertFile: certFile, KeyFile: keyFile},
		cfg: &Config{
			Schema: 1, GatewaysFile: filepath.Join(dir, "gateways.json"),
			Targets: Targets{HTTP: filepath.Join(dir, "http.sock"), HTTPS: filepath.Join(dir, "https.sock")},
			Routes:  []Route{{GatewayID: "gw1", Names: []string{"app.test"}}},
		},
	}
	env.proxy = newFakeProxy(t, env.cfg.Targets.HTTPS)
	writeGateways(t, env.cfg.GatewaysFile, env.def)
	env.e = New(Options{Stderr: env.log, timing: timing{backoffMin: 50 * time.Millisecond, backoffMax: 200 * time.Millisecond, quicAuto: 500 * time.Millisecond, dial: 2 * time.Second}})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		env.e.Shutdown(ctx)
	})
	return env
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// checkStream sends a client connection through the gateway session.
func checkStream(t *testing.T, sess mux.Session, proxy *fakeProxy) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := sess.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := wire.WriteStreamHeader(st, httpsHeader("app.test", "203.0.113.50:1234")); err != nil {
		t.Fatal(err)
	}
	if got, err := roundTrip(t, st, "tls client hello"); err != nil || got != "tls client hello" {
		t.Fatalf("stream through %s = %q, %v", sess.Transport(), got, err)
	}
	if pc := proxy.next(t); pc.header.Src.String() != "203.0.113.50:1234" {
		t.Fatalf("proxy header = %+v", pc.header)
	}
}

func TestDialTransports(t *testing.T) {
	for _, tc := range []struct {
		name, transport string
		quic, tcp       bool
		want            string
	}{
		{"quic", TransportQUIC, true, true, "quic"},
		{"tcp", TransportTCP, true, true, "tcp"},
		{"auto prefers quic", TransportAuto, true, true, "quic"},
		{"auto falls back to tcp", TransportAuto, false, true, "tcp"},
		{"empty transport is auto", "", true, false, "quic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newDialEnv(t, tc.transport)
			if tc.quic {
				env.gw.listenQUIC()
			}
			if tc.tcp {
				env.gw.listenTCP()
			}
			if err := env.e.Start(env.cfg, "r1"); err != nil {
				t.Fatal(err)
			}
			sess := env.gw.session(t)
			st := waitState(t, env.e, "gw1", StateConnected)
			if st.Transport != tc.want || sess.Transport() != tc.want || st.Version != "gw-test" {
				t.Fatalf("transport = %s (gateway saw %s), status %+v", st.Transport, sess.Transport(), st)
			}
			waitFor(t, "ack", func() bool { s, _ := gatewayStatus(env.e, "gw1"); return s.AckedGeneration == 1 })
			checkStream(t, sess, env.proxy)
			// The session's remote address is the gateway's.
			if ip := addrIP(sess.RemoteAddr()); ip.String() != "127.0.0.1" {
				t.Fatalf("gateway saw home at %v", sess.RemoteAddr())
			}
		})
	}
}

func TestDialAutoRemembersQUICFailure(t *testing.T) {
	env := newDialEnv(t, TransportAuto)
	env.gw.listenTCP()
	if err := env.e.Start(env.cfg, "r1"); err != nil {
		t.Fatal(err)
	}
	sess := env.gw.session(t)
	waitState(t, env.e, "gw1", StateConnected)

	// QUIC becomes available, but within the penalty TCP goes first.
	env.gw.listenQUIC()
	sess.Close()
	sess = env.gw.session(t)
	if sess.Transport() != "tcp" {
		t.Fatalf("reconnected over %s during the QUIC penalty", sess.Transport())
	}
	env.e.mu.Lock()
	g := env.e.gws["gw1"]
	env.e.mu.Unlock()
	g.mu.Lock()
	g.quicFail = time.Now().Add(-time.Hour) // penalty over
	g.mu.Unlock()
	sess.Close()
	if sess = env.gw.session(t); sess.Transport() != "quic" {
		t.Fatalf("reconnected over %s after the QUIC penalty", sess.Transport())
	}
}

func TestDialReconnectsAfterGatewayRestart(t *testing.T) {
	env := newDialEnv(t, TransportQUIC)
	env.gw.listenQUIC()
	if err := env.e.Start(env.cfg, "r1"); err != nil {
		t.Fatal(err)
	}
	env.gw.session(t)
	waitState(t, env.e, "gw1", StateConnected)

	env.gw.stop()
	st := waitState(t, env.e, "gw1", StateDisconnected)
	if st.LastError == "" || st.LastErrorAt == nil || st.ConnectedAt != nil {
		t.Fatalf("status after gateway stop = %+v", st)
	}
	time.Sleep(300 * time.Millisecond) // a few failed attempts
	env.gw.listenQUIC()
	sess := env.gw.session(t)
	st = waitState(t, env.e, "gw1", StateConnected)
	if st.Reconnects != 1 {
		t.Fatalf("reconnects = %d", st.Reconnects)
	}
	checkStream(t, sess, env.proxy)
	if !hasLine(env.log.String(), "gateway gw1: disconnected after") {
		t.Fatalf("log:\n%s", env.log)
	}
}

func TestDialErrors(t *testing.T) {
	t.Run("pin mismatch", func(t *testing.T) {
		env := newDialEnv(t, TransportTCP)
		env.gw.listenTCP()
		env.def.Pin = mustIdentity(t, "impostor").Fingerprint()
		writeGateways(t, env.cfg.GatewaysFile, env.def)
		if err := env.e.Start(env.cfg, "r1"); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "pin error", func() bool {
			s, _ := gatewayStatus(env.e, "gw1")
			return s.State == StateDisconnected && strings.Contains(s.LastError, "pin")
		})
		if !hasLine(env.log.String(), "[warn] gateway gw1: cannot connect to "+env.gw.addr) {
			t.Fatalf("log:\n%s", env.log)
		}
	})
	t.Run("gateway does not know the home", func(t *testing.T) {
		env := newDialEnv(t, TransportQUIC)
		env.gw.homePin = mustIdentity(t, "other-home").Fingerprint()
		env.gw.listenQUIC()
		if err := env.e.Start(env.cfg, "r1"); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "handshake error", func() bool {
			s, _ := gatewayStatus(env.e, "gw1")
			return s.State == StateDisconnected && s.LastError != ""
		})
	})
	t.Run("missing identity", func(t *testing.T) {
		env := newDialEnv(t, TransportTCP)
		env.def.KeyFile = filepath.Join(env.dir, "nope.key")
		writeGateways(t, env.cfg.GatewaysFile, env.def)
		if err := env.e.Start(env.cfg, "r1"); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "identity error", func() bool {
			s, _ := gatewayStatus(env.e, "gw1")
			return strings.HasPrefix(s.LastError, "identity: ")
		})
	})
	t.Run("unreachable", func(t *testing.T) {
		env := newDialEnv(t, TransportAuto)
		if err := env.e.Start(env.cfg, "r1"); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "both transports failing", func() bool {
			s, _ := gatewayStatus(env.e, "gw1")
			return strings.Contains(s.LastError, "quic: ") && strings.Contains(s.LastError, "tcp: ")
		})
	})
}

func TestBackoffJitter(t *testing.T) {
	for range 1000 {
		d := jitter(10 * time.Second)
		if d < 8*time.Second || d > 12*time.Second {
			t.Fatalf("jitter(10s) = %s", d)
		}
	}
}
