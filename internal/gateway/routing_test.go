package gateway

import (
	"bytes"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/tunnel/mux"
	"github.com/instantoffr/relay/internal/tunnel/wire"
)

func TestControl(t *testing.T) {
	srv := newServer(t, Options{PublicIPs: []string{"203.0.113.7"}}, func(s *Server) { s.statsEvery = 50 * time.Millisecond })
	h := pipeHome(t, srv)

	if h.hello.Version != "test" || h.hello.Proto != wire.Proto || len(h.hello.PublicIPs) != 1 || h.hello.PublicIPs[0] != "203.0.113.7" {
		t.Fatalf("gateway hello = %+v", h.hello)
	}

	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	busyPort := addrPort(busy.Addr()).Port()
	free := freePort(t)
	tunnelPort := addrPort(srv.Addrs().Tunnel).Port()

	ack := h.publish(1, []string{"app.example.com"}, free, 22, busyPort, tunnelPort, 80, free)
	errs := map[uint16]string{}
	for _, e := range ack.Errors {
		errs[e.Port] = e.Error
	}
	if len(errs) != 4 || len(ack.Errors) != 4 {
		t.Fatalf("ack errors = %+v", ack.Errors)
	}
	for _, p := range []uint16{22, tunnelPort, 80} {
		if !strings.Contains(errs[p], "not allowed") {
			t.Errorf("port %d: error %q, want not allowed", p, errs[p])
		}
	}
	if !strings.Contains(errs[busyPort], "cannot listen") {
		t.Errorf("busy port: error %q", errs[busyPort])
	}
	if _, bad := errs[free]; bad {
		t.Errorf("free port reported: %q", errs[free])
	}
	// Stats follow each ack.
	h.expect("stats after ack", func(m *wire.Message) bool { return m.Stats != nil })

	h.send(&wire.Message{Ping: &wire.Ping{ID: 42, SentNs: 12345}})
	m := h.expect("pong", func(m *wire.Message) bool { return m.Pong != nil })
	if m.Pong.ID != 42 || m.Pong.SentNs != 12345 {
		t.Fatalf("pong = %+v", m.Pong)
	}

	// Unknown and empty messages are ignored; the periodic stats keep coming.
	h.send(&wire.Message{})
	h.send(&wire.Message{Pong: &wire.Ping{ID: 1}})
	for range 2 {
		h.expect("periodic stats", func(m *wire.Message) bool { return m.Stats != nil })
	}
	h.send(&wire.Message{Ping: &wire.Ping{ID: 43}})
	h.expect("pong after unknown messages", func(m *wire.Message) bool { return m.Pong != nil && m.Pong.ID == 43 })
}

func TestHelloIncompatible(t *testing.T) {
	srv := newServer(t, Options{})
	gw, home := mux.Pipe(mux.Config{})
	defer home.Close()
	srv.AttachSession(gw)
	ctl, err := home.AcceptStream(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := wire.WriteMessage(ctl, &wire.Message{Hello: &wire.Hello{Proto: 99, MinProto: 99}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-home.Done():
	case <-time.After(testTimeout):
		t.Fatal("incompatible session was not closed")
	}
	if srv.active.Load() != nil {
		t.Fatal("incompatible session became active")
	}
}

func TestHTTPSRouting(t *testing.T) {
	srv := newServer(t, Options{})
	h := pipeHome(t, srv)
	h.publish(1, []string{"app.example.com", "*.wild.example.com"})
	httpsAddr := srv.Addrs().HTTPS.String()
	httpsPort := addrPort(srv.Addrs().HTTPS).Port()

	hello := clientHello(t, "app.example.com")
	c := dial(t, httpsAddr)
	if _, err := c.Write(hello); err != nil {
		t.Fatal(err)
	}
	st, hdr := h.mustAccept()
	if hdr.Kind != wire.KindHTTPS || hdr.Name != "app.example.com" || hdr.Port != httpsPort {
		t.Fatalf("header = %+v", hdr)
	}
	if hdr.Src != addrPort(c.LocalAddr()) || hdr.Dst != addrPort(c.RemoteAddr()) {
		t.Fatalf("header src/dst = %v/%v, want %v/%v", hdr.Src, hdr.Dst, c.LocalAddr(), c.RemoteAddr())
	}
	st.SetDeadline(time.Now().Add(testTimeout))
	if got := readN(t, st, len(hello)); !bytes.Equal(got, hello) {
		t.Fatal("stream does not start with the ClientHello")
	}

	// Both directions, then half-closes in both directions.
	if _, err := st.Write([]byte("from home")); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(testTimeout))
	if got := readN(t, c, 9); string(got) != "from home" {
		t.Fatalf("client read %q", got)
	}
	if _, err := c.Write([]byte("from client")); err != nil {
		t.Fatal(err)
	}
	if got := readN(t, st, 11); string(got) != "from client" {
		t.Fatalf("home read %q", got)
	}
	c.CloseWrite()
	if rest, err := io.ReadAll(st); err != nil || len(rest) != 0 {
		t.Fatalf("home after client half-close: %q, %v", rest, err)
	}
	if _, err := st.Write([]byte("bye")); err != nil {
		t.Fatalf("write after client half-close: %v", err)
	}
	st.CloseWrite()
	if rest, err := io.ReadAll(c); err != nil || string(rest) != "bye" {
		t.Fatalf("client after home half-close: %q, %v", rest, err)
	}
	waitFor(t, "connection end", func() bool { return srv.Stats().ActiveConns == 0 })
	stats := srv.Stats()
	if stats.Accepted != 1 || stats.BytesIn != uint64(len(hello)+11) || stats.BytesOut != 12 {
		t.Fatalf("stats = %+v", stats)
	}

	// Wildcards match, unpublished names are closed without a stream.
	c = dial(t, httpsAddr)
	c.Write(clientHello(t, "a.b.wild.example.com"))
	if _, hdr := h.mustAccept(); hdr.Name != "a.b.wild.example.com" {
		t.Fatalf("wildcard header = %+v", hdr)
	}
	for _, data := range [][]byte{clientHello(t, "other.example.com"), clientHello(t, "wild.example.com"), []byte("GET / HTTP/1.1\r\nHost: app.example.com\r\n\r\n")} {
		c := dial(t, httpsAddr)
		c.Write(data)
		expectClosed(t, c)
	}
	h.expectNoStream()
	if got := srv.Stats().RejectedName; got != 3 {
		t.Fatalf("RejectedName = %d, want 3", got)
	}
}

func TestHTTPRouting(t *testing.T) {
	srv := newServer(t, Options{})
	h := pipeHome(t, srv)
	h.publish(1, []string{"app.example.com"})
	httpAddr := srv.Addrs().HTTP.String()

	req := "GET /path HTTP/1.1\r\nHost: App.Example.com:80\r\nUser-Agent: test\r\n\r\n"
	c := dial(t, httpAddr)
	// Split writes: the gateway waits for the end of the headers.
	c.Write([]byte(req[:10]))
	time.Sleep(20 * time.Millisecond)
	c.Write([]byte(req[10:]))
	st, hdr := h.mustAccept()
	if hdr.Kind != wire.KindHTTP || hdr.Name != "app.example.com" || hdr.Port != addrPort(srv.Addrs().HTTP).Port() {
		t.Fatalf("header = %+v", hdr)
	}
	st.SetDeadline(time.Now().Add(testTimeout))
	if got := readN(t, st, len(req)); string(got) != req {
		t.Fatalf("stream data %q", got)
	}
	st.Write([]byte("HTTP/1.1 204 No Content\r\n\r\n"))
	st.CloseWrite()
	c.SetReadDeadline(time.Now().Add(testTimeout))
	if resp, err := io.ReadAll(c); err != nil || !strings.HasPrefix(string(resp), "HTTP/1.1 204") {
		t.Fatalf("response %q, %v", resp, err)
	}

	for _, r := range []string{
		"GET / HTTP/1.1\r\nUser-Agent: test\r\n\r\n",        // no Host
		"GET / HTTP/1.1\r\nHost: other.example.com\r\n\r\n", // unpublished
		"PRI * HTTP/2.0\r\n\r\n",                            // not HTTP/1
	} {
		c := dial(t, httpAddr)
		c.Write([]byte(r))
		expectClosed(t, c)
	}
	h.expectNoStream()
	if got := srv.Stats().RejectedName; got != 3 {
		t.Fatalf("RejectedName = %d, want 3", got)
	}
}

func TestTCPPorts(t *testing.T) {
	srv := newServer(t, Options{})
	h := pipeHome(t, srv)
	port := freePort(t)
	if ack := h.publish(1, nil, port); len(ack.Errors) != 0 {
		t.Fatalf("ack errors: %+v", ack.Errors)
	}
	c := dial(t, loopback(port))
	c.Write([]byte("ping"))
	st, hdr := h.mustAccept()
	if hdr.Kind != wire.KindTCP || hdr.Port != port || hdr.Name != "" || hdr.Src != addrPort(c.LocalAddr()) {
		t.Fatalf("header = %+v", hdr)
	}
	st.SetDeadline(time.Now().Add(testTimeout))
	if got := readN(t, st, 4); string(got) != "ping" {
		t.Fatalf("home read %q", got)
	}
	st.Write([]byte("pong"))
	c.SetReadDeadline(time.Now().Add(testTimeout))
	if got := readN(t, c, 4); string(got) != "pong" {
		t.Fatalf("client read %q", got)
	}

	// Republishing keeps the listener (and its established connection).
	srv.mu.Lock()
	ln := srv.ports[port]
	srv.mu.Unlock()
	h.publish(2, []string{"app.example.com"}, port)
	srv.mu.Lock()
	same := srv.ports[port] == ln
	srv.mu.Unlock()
	if !same {
		t.Fatal("listener was replaced on republish")
	}
	st.Write([]byte("still"))
	if got := readN(t, c, 5); string(got) != "still" {
		t.Fatalf("client read %q", got)
	}

	// Unpublishing closes the listener.
	h.publish(3, []string{"app.example.com"})
	if c, err := net.DialTimeout("tcp", loopback(port), time.Second); err == nil {
		c.Close()
		t.Fatal("unpublished port still accepts connections")
	}
}

func TestNoSession(t *testing.T) {
	srv := newServer(t, Options{})
	// Before any session.
	for _, addr := range []string{srv.Addrs().HTTP.String(), srv.Addrs().HTTPS.String()} {
		c := dial(t, addr)
		expectClosed(t, c)
	}
	if got := srv.Stats().RejectedPort; got != 2 {
		t.Fatalf("RejectedPort = %d, want 2", got)
	}

	// After the session is gone the routes and listeners stay, but clients
	// are refused.
	gw, home := mux.Pipe(mux.Config{})
	srv.AttachSession(gw)
	h := connectHome(t, home)
	waitActive(t, srv, gw)
	port := freePort(t)
	h.publish(1, []string{"app.example.com"}, port)
	home.Close()
	waitFor(t, "session end", func() bool { return srv.active.Load() == nil })

	c := dial(t, loopback(port))
	c.Write([]byte("hello"))
	expectClosed(t, c)
	c = dial(t, srv.Addrs().HTTP.String())
	c.Write([]byte("GET / HTTP/1.1\r\nHost: app.example.com\r\n\r\n"))
	expectClosed(t, c)
	if got := srv.Stats().RejectedPort; got != 4 {
		t.Fatalf("RejectedPort = %d, want 4", got)
	}

	// A new session serves the kept routes right away.
	h = pipeHome(t, srv)
	c = dial(t, loopback(port))
	c.Write([]byte("hello"))
	if _, hdr := h.mustAccept(); hdr.Port != port {
		t.Fatalf("header = %+v", hdr)
	}
}

func TestSessionReplaced(t *testing.T) {
	srv := newServer(t, Options{})
	h1 := pipeHome(t, srv)
	h2 := pipeHome(t, srv)
	select {
	case <-h1.sess.Done():
	case <-time.After(testTimeout):
		t.Fatal("first session not closed")
	}
	h2.send(&wire.Message{Ping: &wire.Ping{ID: 1}})
	h2.expect("pong", func(m *wire.Message) bool { return m.Pong != nil })
}

func TestUnpair(t *testing.T) {
	dir := t.TempDir()
	home := mustIdentity(t, "home")
	if err := savePairing(stateDir(dir), &Pairing{Schema: 1, HomePin: home.Fingerprint(), PairedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	srv := newServer(t, Options{DataDir: dir})
	if srv.HomePin() != home.Fingerprint() {
		t.Fatal("pairing state not loaded")
	}
	h := pipeHome(t, srv)
	port := freePort(t)
	h.publish(1, []string{"app.example.com"}, port)
	h.send(&wire.Message{Unpair: &wire.Unpair{}})
	select {
	case <-h.sess.Done():
	case <-time.After(testTimeout):
		t.Fatal("session not closed after unpair")
	}
	if srv.HomePin() != "" {
		t.Fatal("still paired in memory")
	}
	if p, err := loadPairing(stateDir(dir)); err != nil || p != nil {
		t.Fatalf("pairing state after unpair: %+v, %v", p, err)
	}
	if _, err := loadIdentity(stateDir(dir)); err != nil {
		t.Fatalf("identity removed: %v", err)
	}
	if c, err := net.DialTimeout("tcp", loopback(port), time.Second); err == nil {
		c.Close()
		t.Fatal("port still published after unpair")
	}
}

func TestClientLimits(t *testing.T) {
	srv := newServer(t, Options{MaxConnsPerIP: 2})
	h := pipeHome(t, srv)
	h.publish(1, []string{"app.example.com"})
	addr := srv.Addrs().HTTP.String()

	// Two slow clients hold their slots while the gateway waits for headers.
	c1, c2 := dial(t, addr), dial(t, addr)
	c1.Write([]byte("GET / HTTP/1.1\r\n"))
	c2.Write([]byte("GET / HTTP/1.1\r\n"))
	waitFor(t, "two admitted clients", func() bool {
		srv.connMu.Lock()
		defer srv.connMu.Unlock()
		return srv.clients == 2
	})
	expectClosed(t, dial(t, addr))
	expectOpen(t, c1)

	c1.Close()
	waitFor(t, "released slot", func() bool {
		srv.connMu.Lock()
		defer srv.connMu.Unlock()
		return srv.clients == 1
	})
	c3 := dial(t, addr)
	expectOpen(t, c3)
	c3.Write([]byte("GET / HTTP/1.1\r\nHost: app.example.com\r\n\r\n"))
	h.mustAccept()
}
