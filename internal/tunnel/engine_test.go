package tunnel

import (
	"bytes"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/proxyproto"
	"github.com/instantoffr/relay/internal/tunnel/wire"
)

func TestSessionControlAndStreams(t *testing.T) {
	env := newPipeEnv(t, func(t *timing) { t.ping = 30 * time.Millisecond })
	if err := env.e.Start(env.cfg, "r1"); err != nil {
		t.Fatal(err)
	}
	fg := <-env.dials
	t.Cleanup(func() { fg.sess.Close() })
	fg.pongDelay = 20 * time.Millisecond
	home := fg.handshake(gatewayHello())
	if home.Version != "home-9.9.9" || home.Proto != wire.Proto || home.MinProto != wire.MinProto {
		t.Fatalf("home hello = %+v", home)
	}

	// Routes are pushed right after the hello exchange.
	r := fg.routes()
	if r.Generation != 1 || !slices.Equal(r.Names, []string{"*.wild.test", "app.example.com"}) || !slices.Equal(r.TCP, []uint16{2222}) {
		t.Fatalf("routes = %+v", r)
	}
	st := waitState(t, env.e, "gw1", StateConnected)
	if st.Transport != "pipe" || st.ConnectedAt == nil || st.Version != "gw-1.2.3" || st.Proto != 1 ||
		!slices.Equal(st.PublicIPs, []string{"198.51.100.1", "2001:db8::1"}) || st.Generation != 1 || st.AckedGeneration != 0 {
		t.Fatalf("status = %+v", st)
	}

	// Ack with port errors; an ack for another generation is ignored.
	fg.send(&wire.Message{Ack: &wire.RoutesAck{Generation: 1, Errors: []wire.PortError{{Port: 2222, Error: "address in use"}}}})
	waitFor(t, "ack", func() bool { s, _ := gatewayStatus(env.e, "gw1"); return s.AckedGeneration == 1 })
	fg.send(&wire.Message{Ack: &wire.RoutesAck{Generation: 7}})
	fg.send(&wire.Message{Stats: &wire.Stats{ActiveConns: 3, Accepted: 10, RejectedName: 2, BytesIn: 100, BytesOut: 200}})
	waitFor(t, "stats", func() bool { s, _ := gatewayStatus(env.e, "gw1"); return s.GatewayStats != nil })
	st, _ = gatewayStatus(env.e, "gw1")
	if st.AckedGeneration != 1 || len(st.PortErrors) != 1 || st.PortErrors[0].Port != 2222 || *st.GatewayStats != (wire.Stats{ActiveConns: 3, Accepted: 10, RejectedName: 2, BytesIn: 100, BytesOut: 200}) {
		t.Fatalf("status after ack/stats = %+v", st)
	}
	if !hasLine(env.log.String(), "cannot publish port 2222: address in use") {
		t.Errorf("port error not logged:\n%s", env.log)
	}

	// The engine pings; RTT comes from the pong.
	waitFor(t, "rtt", func() bool { s, _ := gatewayStatus(env.e, "gw1"); return s.RTTMs >= 20 })

	// The gateway pings; the engine answers with the same ping.
	fg.send(&wire.Message{Ping: &wire.Ping{ID: 42, SentNs: 12345}})
	waitFor(t, "pong", func() bool {
		select {
		case m := <-fg.msgs:
			return m.Pong != nil && m.Pong.ID == 42 && m.Pong.SentNs == 12345
		default:
			return false
		}
	})

	// A published HTTPS name reaches the https socket with the client address.
	s := fg.open(httpsHeader("app.example.com", "203.0.113.7:5555"))
	got, err := roundTrip(t, s, "client hello")
	if err != nil || got != "client hello" {
		t.Fatalf("https round trip = %q, %v", got, err)
	}
	pc := env.proxy.next(t)
	if pc.socket != env.cfg.Targets.HTTPS || pc.header.Local ||
		pc.header.Src != netip.MustParseAddrPort("203.0.113.7:5555") || pc.header.Dst != netip.MustParseAddrPort("198.51.100.1:443") {
		t.Fatalf("proxy header = %+v on %s", pc.header, pc.socket)
	}
	if v, ok := pc.header.TLV(proxyproto.TLVRelayTunnel); !ok || string(v) != "gw1" {
		t.Fatalf("tunnel TLV = %q, %v", v, ok)
	}

	// HTTP with a wildcard name reaches the http socket.
	s = fg.open(wire.StreamHeader{Kind: wire.KindHTTP, Port: 80, Src: netip.MustParseAddrPort("[2001:4860::1]:6000"),
		Dst: netip.MustParseAddrPort("[2001:db8::1]:80"), Name: "deep.x.wild.test"})
	if got, err := roundTrip(t, s, "GET / HTTP/1.1\r\n\r\n"); err != nil || got != "GET / HTTP/1.1\r\n\r\n" {
		t.Fatalf("http round trip = %q, %v", got, err)
	}
	pc = env.proxy.next(t)
	if pc.socket != env.cfg.Targets.HTTP || pc.header.Src != netip.MustParseAddrPort("[2001:4860::1]:6000") {
		t.Fatalf("http proxy header = %+v on %s", pc.header, pc.socket)
	}

	// A published TCP port reaches its own socket.
	s = fg.open(wire.StreamHeader{Kind: wire.KindTCP, Port: 2222, Src: netip.MustParseAddrPort("8.8.8.8:1000"), Dst: netip.MustParseAddrPort("198.51.100.1:2222")})
	if got, err := roundTrip(t, s, "SSH-2.0"); err != nil || got != "SSH-2.0" {
		t.Fatalf("tcp round trip = %q, %v", got, err)
	}
	if pc = env.proxy.next(t); pc.socket != filepath.Join(env.dir, "tcp2222.sock") {
		t.Fatalf("tcp went to %s", pc.socket)
	}

	// Non-public client addresses are replaced by the gateway's address.
	for _, src := range []string{"192.168.1.5:4000", "127.0.0.1:4000", "100.64.3.4:4000", "[::ffff:10.1.2.3]:4000", "[fd00::1]:4000", "[fe80::1]:4000", "0.0.0.0:0"} {
		s = fg.open(httpsHeader("app.example.com", src))
		if got, err := roundTrip(t, s, "x"); err != nil || got != "x" {
			t.Fatalf("%s: round trip = %q, %v", src, got, err)
		}
		if pc = env.proxy.next(t); pc.header.Src != netip.MustParseAddrPort("198.51.100.200:0") {
			t.Fatalf("%s: proxy saw src %s", src, pc.header.Src)
		}
	}

	// Unpublished names, kinds and ports are closed without reaching the proxy.
	before, _ := gatewayStatus(env.e, "gw1")
	for _, h := range []wire.StreamHeader{
		httpsHeader("other.example.com", "203.0.113.7:1"),
		httpsHeader("wild.test", "203.0.113.7:1"),
		httpsHeader("", "203.0.113.7:1"),
		{Kind: wire.KindTCP, Port: 2223, Src: netip.MustParseAddrPort("203.0.113.7:1"), Dst: netip.MustParseAddrPort("198.51.100.1:2223")},
		{Kind: wire.KindTCP, Port: 443, Src: netip.MustParseAddrPort("203.0.113.7:1"), Dst: netip.MustParseAddrPort("198.51.100.1:443"), Name: ""},
	} {
		s = fg.open(h)
		if got, _ := roundTrip(t, s, "nope"); got != "" {
			t.Fatalf("%+v: rejected stream answered %q", h, got)
		}
	}
	waitFor(t, "rejected count", func() bool { s, _ := gatewayStatus(env.e, "gw1"); return s.Rejected == before.Rejected+5 })
	select {
	case pc := <-env.proxy.conns:
		t.Fatalf("rejected stream reached the proxy: %+v", pc)
	default:
	}

	// Garbage instead of a header is rejected too.
	raw, err := fg.sess.OpenStream(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	raw.Write([]byte("GET / HTTP/1.1\r\n\r\n"))
	if got, _ := roundTrip(t, raw, ""); got != "" {
		t.Fatalf("garbage stream answered %q", got)
	}
	waitFor(t, "rejected garbage", func() bool { s, _ := gatewayStatus(env.e, "gw1"); return s.Rejected == before.Rejected+6 })

	waitFor(t, "streams to finish", func() bool { s, _ := gatewayStatus(env.e, "gw1"); return s.ActiveStreams == 0 })
	st, _ = gatewayStatus(env.e, "gw1")
	wantIn := uint64(len("client hello") + len("GET / HTTP/1.1\r\n\r\n") + len("SSH-2.0") + 7)
	if st.Streams != 10 || st.BytesIn != wantIn || st.BytesOut != wantIn {
		t.Fatalf("counters = streams %d in %d out %d, want 10/%d/%d", st.Streams, st.BytesIn, st.BytesOut, wantIn, wantIn)
	}
}

func TestHalfClose(t *testing.T) {
	env := newPipeEnv(t, nil)
	if err := env.e.Start(env.cfg, "r1"); err != nil {
		t.Fatal(err)
	}
	fg := env.accept()
	fg.routes()

	s := fg.open(httpsHeader("app.example.com", "203.0.113.7:5555"))
	s.SetDeadline(time.Now().Add(5 * time.Second))
	// Data keeps flowing home → client while the client already sent EOF,
	// and client → home while home is still answering.
	payload := bytes.Repeat([]byte("0123456789abcdef"), 64<<10) // ~1 MiB
	go func() {
		s.Write(payload)
		s.CloseWrite()
	}()
	got, err := io.ReadAll(s)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("echo after half-close: %d bytes, %v", len(got), err)
	}
	s.Close()
}

func TestIncompatibleGateway(t *testing.T) {
	env := newPipeEnv(t, func(t *timing) { t.incompatible = time.Hour })
	if err := env.e.Start(env.cfg, "r1"); err != nil {
		t.Fatal(err)
	}
	fg := <-env.dials
	t.Cleanup(func() { fg.sess.Close() })
	fg.handshake(&wire.Hello{Proto: 9, MinProto: 7, Version: "gw-future"})
	st := waitState(t, env.e, "gw1", StateIncompatible)
	if !strings.Contains(st.LastError, "incompatible") || st.LastErrorAt == nil {
		t.Fatalf("status = %+v", st)
	}
	log := env.log.String()
	if !hasLine(log, "[error]") || !hasLine(log, "home-9.9.9 speaks 1-1, gateway gw-future speaks 7-9") {
		t.Fatalf("log:\n%s", log)
	}
	select {
	case <-fg.sess.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("session not closed")
	}
}

func TestGatewayStates(t *testing.T) {
	env := newPipeEnv(t, nil)
	disabled := env.gateway("a-disabled")
	disabled.Enabled = false
	unpaired := env.gateway("b-unpaired")
	unpaired.Pin = ""
	writeGateways(t, env.gwFile, env.gateway("gw1"), env.gateway("c-idle"), unpaired, disabled)
	cfg := env.cloneConfig()
	cfg.Routes = append(cfg.Routes, Route{GatewayID: "a-disabled", Names: []string{"a.test"}}, Route{GatewayID: "b-unpaired", Names: []string{"b.test"}},
		Route{GatewayID: "unknown", Names: []string{"u.test"}})
	if err := env.e.Start(cfg, "r1"); err != nil {
		t.Fatal(err)
	}
	fg := env.accept()
	fg.routes()
	waitState(t, env.e, "gw1", StateConnected)
	var ids, states []string
	for _, g := range env.e.Status().Gateways {
		ids = append(ids, g.ID)
		states = append(states, g.State)
	}
	if !slices.Equal(ids, []string{"a-disabled", "b-unpaired", "c-idle", "gw1"}) ||
		!slices.Equal(states, []string{StateDisabled, StateUnpaired, StateIdle, StateConnected}) {
		t.Fatalf("gateways %v states %v", ids, states)
	}
	time.Sleep(50 * time.Millisecond)
	for _, id := range []string{"a-disabled", "b-unpaired", "c-idle"} {
		if n := env.dialCount(id); n != 0 {
			t.Errorf("%s dialed %d times", id, n)
		}
	}
}

func TestReconnectAfterSessionLoss(t *testing.T) {
	env := newPipeEnv(t, func(t *timing) { t.hello = 300 * time.Millisecond })
	if err := env.e.Start(env.cfg, "r1"); err != nil {
		t.Fatal(err)
	}
	fg := env.accept()
	fg.routes()
	waitState(t, env.e, "gw1", StateConnected)
	fg.sess.Close()
	fg2 := env.accept()
	if r := fg2.routes(); r.Generation != 1 {
		t.Fatalf("routes after reconnect = %+v", r)
	}
	st := waitState(t, env.e, "gw1", StateConnected)
	if st.Reconnects != 1 || st.LastError == "" || st.AckedGeneration != 0 {
		t.Fatalf("status = %+v", st)
	}
	// A gateway that never opens the control stream is dropped.
	fg2.sess.Close()
	fg3 := <-env.dials
	t.Cleanup(func() { fg3.sess.Close() })
	select {
	case <-fg3.sess.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("silent gateway not dropped")
	}
	waitFor(t, "hello timeout error", func() bool {
		s, _ := gatewayStatus(env.e, "gw1")
		return strings.Contains(s.LastError, "control stream")
	})
}

func TestReload(t *testing.T) {
	env := newPipeEnv(t, nil)
	cfg := env.cloneConfig()
	cfg.Routes[0].Names = []string{"a.test"}
	if err := env.e.Start(cfg, "r1"); err != nil {
		t.Fatal(err)
	}
	fg := env.accept()
	if r := fg.routes(); r.Generation != 1 || !slices.Equal(r.Names, []string{"a.test"}) {
		t.Fatalf("routes = %+v", r)
	}
	fg.send(&wire.Message{Ack: &wire.RoutesAck{Generation: 1}})

	// A stream in flight.
	inflight := fg.open(httpsHeader("a.test", "203.0.113.9:9999"))
	inflight.SetDeadline(time.Now().Add(10 * time.Second))
	echo := func(s string) {
		t.Helper()
		if _, err := io.WriteString(inflight, s); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, len(s))
		if _, err := io.ReadFull(inflight, buf); err != nil || string(buf) != s {
			t.Fatalf("in-flight echo = %q, %v", buf, err)
		}
	}
	echo("before reload")

	// Only routes change: same session, generation 2, stream unaffected.
	cfg2 := env.cloneConfig()
	cfg2.Routes[0].Names = []string{"a.test", "b.test"}
	if err := env.e.Reload(cfg2, "r2"); err != nil {
		t.Fatal(err)
	}
	if r := fg.routes(); r.Generation != 2 || !slices.Equal(r.Names, []string{"a.test", "b.test"}) {
		t.Fatalf("routes after reload = %+v", r)
	}
	echo("after reload")
	if got, err := roundTrip(t, fg.open(httpsHeader("b.test", "203.0.113.9:1")), "b"); got != "b" {
		t.Fatalf("new name after reload = %q, %v", got, err)
	}
	if env.dialCount("gw1") != 1 || env.e.Hash() != "r2" {
		t.Fatalf("dials %d hash %s", env.dialCount("gw1"), env.e.Hash())
	}
	st, _ := gatewayStatus(env.e, "gw1")
	if st.State != StateConnected || st.Generation != 2 || st.AckedGeneration != 1 {
		t.Fatalf("status after routes reload = %+v", st)
	}

	// Same routes again (and a renamed gateway): nothing is pushed.
	gw := env.gateway("gw1")
	gw.Name = "Renamed"
	writeGateways(t, env.gwFile, gw)
	if err := env.e.Reload(cfg2, "r3"); err != nil {
		t.Fatal(err)
	}
	fg.send(&wire.Message{Ping: &wire.Ping{ID: 1}})
	select {
	case m := <-fg.msgs:
		if m.Pong == nil {
			t.Fatalf("unexpected message after no-op reload: %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no pong")
	}
	echo("after no-op reload")

	// A failed reload keeps everything.
	bad := env.cloneConfig()
	bad.Schema = 2
	if err := env.e.Reload(bad, "bad"); err == nil || env.e.Hash() != "r3" {
		t.Fatalf("bad reload: %v, hash %s", err, env.e.Hash())
	}

	// A new pin reconnects; the in-flight stream of the old session ends.
	gw.Pin = mustIdentity(t, "gw-new").Fingerprint()
	writeGateways(t, env.gwFile, gw)
	if err := env.e.Reload(cfg2, "r4"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fg.sess.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("old session kept after pin change")
	}
	fg2 := env.accept()
	if r := fg2.routes(); r.Generation != 2 {
		t.Fatalf("routes after reconnect = %+v", r)
	}
	st = waitState(t, env.e, "gw1", StateConnected)
	if st.Reconnects != 1 || st.AckedGeneration != 0 {
		t.Fatalf("status after pin change = %+v", st)
	}

	// A new address reconnects as well.
	gw.Address = "other.example.net:8443"
	writeGateways(t, env.gwFile, gw, env.gateway("gw2"))
	cfg3 := env.cloneConfig()
	cfg3.Routes = append(cfg2.Routes, Route{GatewayID: "gw2", Names: []string{"c.test"}})
	if err := env.e.Reload(cfg3, "r5"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fg2.sess.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("old session kept after address change")
	}
	// gw1 (new address) and gw2 (new gateway) both dial.
	byRoutes := map[string]*fakeGateway{}
	var sessions []*fakeGateway
	for range 2 {
		f := env.accept()
		byRoutes[strings.Join(f.routes().Names, ",")] = f
		sessions = append(sessions, f)
	}
	gw1, gw2 := byRoutes["a.test,b.test"], byRoutes["c.test"]
	if gw1 == nil || gw2 == nil {
		t.Fatalf("routes of new sessions: %v", byRoutes)
	}
	waitState(t, env.e, "gw2", StateConnected)
	st = waitState(t, env.e, "gw1", StateConnected)
	if st.Address != "other.example.net:8443" {
		t.Fatalf("address = %s", st.Address)
	}

	// Streams resolve against their own gateway's routes only.
	if got, _ := roundTrip(t, gw1.open(httpsHeader("c.test", "203.0.113.1:1")), "c"); got != "" {
		t.Fatalf("gw1 served gw2's name: %q", got)
	}
	if got, _ := roundTrip(t, gw2.open(httpsHeader("a.test", "203.0.113.1:1")), "a"); got != "" {
		t.Fatalf("gw2 served gw1's name: %q", got)
	}
	if got, err := roundTrip(t, gw2.open(httpsHeader("c.test", "203.0.113.1:1")), "c"); got != "c" {
		t.Fatalf("gw2 own name = %q, %v", got, err)
	}
	if got, err := roundTrip(t, gw1.open(httpsHeader("a.test", "203.0.113.1:1")), "a"); got != "a" {
		t.Fatalf("gw1 own name = %q, %v", got, err)
	}

	// Removing a gateway disconnects it; removing its route makes it idle.
	writeGateways(t, env.gwFile, gw)
	cfg4 := env.cloneConfig()
	cfg4.Routes = nil
	if err := env.e.Reload(cfg4, "r6"); err != nil {
		t.Fatal(err)
	}
	for _, f := range sessions {
		select {
		case <-f.sess.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("session kept after its gateway was removed or unpublished")
		}
	}
	gws := env.e.Status().Gateways
	if len(gws) != 1 || gws[0].ID != "gw1" || gws[0].State != StateIdle {
		t.Fatalf("gateways after removal = %+v", gws)
	}
}

func TestShutdownDrainsStreams(t *testing.T) {
	env := newPipeEnv(t, func(t *timing) { t.drain = 3 * time.Second })
	if err := env.e.Start(env.cfg, "r1"); err != nil {
		t.Fatal(err)
	}
	fg := env.accept()
	fg.routes()
	s := fg.open(httpsHeader("app.example.com", "203.0.113.7:5555"))
	s.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(s, "ping")
	buf := make([]byte, 4)
	if _, err := io.ReadFull(s, buf); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		env.e.Shutdown(t.Context())
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("shutdown did not wait for the stream in flight")
	default:
	}
	// The stream still works while draining, and shutdown ends once it is done.
	if got, err := roundTrip(t, s, "last"); err != nil || got != "last" {
		t.Fatalf("draining stream = %q, %v", got, err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	select {
	case <-fg.sess.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("session not closed at shutdown")
	}
	if _, err := os.Stat(env.cfg.RuntimeSocket); err == nil {
		t.Fatal("runtime socket not removed")
	}
}

func TestShutdownLeavesNoGoroutines(t *testing.T) {
	base := runtime.NumGoroutine()
	t.Run("pipe", func(t *testing.T) {
		env := newPipeEnv(t, nil)
		if err := env.e.Start(env.cfg, "r1"); err != nil {
			t.Fatal(err)
		}
		fg := env.accept()
		fg.routes()
		if got, err := roundTrip(t, fg.open(httpsHeader("app.example.com", "203.0.113.7:1")), "x"); got != "x" {
			t.Fatalf("round trip = %q, %v", got, err)
		}
		// A stream left open is closed by shutdown after the drain timeout.
		open := fg.open(httpsHeader("app.example.com", "203.0.113.7:2"))
		defer open.Close()
		if err := env.e.Reload(env.cloneConfig(), "r2"); err != nil {
			t.Fatal(err)
		}
		env.e.Shutdown(t.Context())
	})
	t.Run("quic and tcp", func(t *testing.T) {
		for _, tr := range []string{TransportQUIC, TransportTCP} {
			env := newDialEnv(t, tr)
			env.gw.listenQUIC()
			env.gw.listenTCP()
			if err := env.e.Start(env.cfg, "r1"); err != nil {
				t.Fatal(err)
			}
			checkStream(t, env.gw.session(t), env.proxy)
			env.e.Shutdown(t.Context())
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			buf = buf[:runtime.Stack(buf, true)]
			t.Fatalf("goroutines leaked: %d > %d\n%s", runtime.NumGoroutine(), base, buf)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
