package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/instantoffr/relay/internal/tunnel/mux"
	"github.com/instantoffr/relay/internal/tunnel/pair"
	"github.com/instantoffr/relay/internal/tunnel/wire"
)

// pairHome runs the home side of pairing against srv.
func pairHome(t testing.TB, srv *Server, id pair.Identity, tok pair.Token) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	d := tls.Dialer{Config: pair.ClientConfig(id, "", pair.ALPN)}
	c, err := d.DialContext(ctx, "tcp", srv.Addrs().Tunnel.String())
	if err != nil {
		return "", err
	}
	defer c.Close()
	return pair.Home(ctx, c.(*tls.Conn), tok)
}

// dialTCPHome opens an HTTP/2-over-TLS tunnel session as the home.
func dialTCPHome(t testing.TB, srv *Server, id pair.Identity, gatewayPin string) (mux.Session, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	d := tls.Dialer{Config: pair.ClientConfig(id, gatewayPin, mux.ALPNH2)}
	c, err := d.DialContext(ctx, "tcp", srv.Addrs().Tunnel.String())
	if err != nil {
		return nil, err
	}
	sess, err := mux.HomeTCP(c.(*tls.Conn), mux.Config{})
	if err != nil {
		c.Close()
		return nil, err
	}
	return sess, nil
}

// dialQUICHome opens a QUIC tunnel session as the home.
func dialQUICHome(t testing.TB, srv *Server, id pair.Identity, gatewayPin string) (mux.Session, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	conn, err := quic.DialAddr(ctx, srv.Addrs().TunnelQUIC.String(), pair.ClientConfig(id, gatewayPin, mux.ALPNQUIC), mux.QUICConfig(mux.Config{}))
	if err != nil {
		return nil, err
	}
	return mux.HomeQUIC(conn, mux.Config{}), nil
}

// expectNoSession checks that sess never delivers the control stream.
func expectNoSession(t testing.TB, sess mux.Session, dialErr error) {
	t.Helper()
	if dialErr != nil {
		return
	}
	defer sess.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if st, err := sess.AcceptStream(ctx); err == nil {
		st.Close()
		t.Fatal("unauthorized home got a control stream")
	}
}

func sendHTTP(t testing.TB, srv *Server, h *fakeHome, host string) {
	t.Helper()
	req := "GET / HTTP/1.1\r\nHost: " + host + "\r\n\r\n"
	c := dial(t, srv.Addrs().HTTP.String())
	c.Write([]byte(req))
	st, hdr := h.mustAccept()
	if hdr.Kind != wire.KindHTTP || hdr.Name != host {
		t.Fatalf("header = %+v", hdr)
	}
	st.SetDeadline(time.Now().Add(testTimeout))
	if got := readN(t, st, len(req)); string(got) != req {
		t.Fatalf("stream data %q", got)
	}
	st.Write([]byte("ok"))
	c.SetReadDeadline(time.Now().Add(testTimeout))
	if got := readN(t, c, 2); string(got) != "ok" {
		t.Fatalf("client read %q", got)
	}
}

func TestEndToEnd(t *testing.T) {
	dir := t.TempDir()
	tok := mustToken(t)
	srv := newServer(t, Options{DataDir: dir, PairToken: tok.String()})
	gwPin := srv.Fingerprint()
	home := mustIdentity(t, "home")

	// Unpaired: no sessions on either transport, even with the right pin.
	sess, err := dialTCPHome(t, srv, home, gwPin)
	expectNoSession(t, sess, err)
	sess, err = dialQUICHome(t, srv, home, gwPin)
	expectNoSession(t, sess, err)

	// Pairing.
	pin, err := pairHome(t, srv, home, tok)
	if err != nil {
		t.Fatalf("pairing: %v", err)
	}
	if pin != gwPin {
		t.Fatalf("home pinned %s, gateway is %s", pin, gwPin)
	}
	waitFor(t, "pairing commit", func() bool { return srv.HomePin() == home.Fingerprint() })
	p, err := loadPairing(stateDir(dir))
	if err != nil || p == nil || p.HomePin != home.Fingerprint() || p.PairedAt.IsZero() {
		t.Fatalf("persisted pairing = %+v, %v", p, err)
	}
	if fi, err := os.Stat(filepath.Join(stateDir(dir), PairFileName)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("pairing file mode: %v, %v", fi, err)
	}
	if fi, err := os.Stat(filepath.Join(stateDir(dir), KeyFileName)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode: %v, %v", fi, err)
	}

	// The token is used up: neither another home nor the same one can pair again.
	if _, err := pairHome(t, srv, mustIdentity(t, "intruder"), tok); err == nil {
		t.Fatal("second pairing succeeded")
	}
	if _, err := pairHome(t, srv, home, tok); err == nil {
		t.Fatal("repeated pairing succeeded")
	}
	if srv.HomePin() != home.Fingerprint() {
		t.Fatal("pin changed")
	}

	// TCP session.
	tcpSess, err := dialTCPHome(t, srv, home, gwPin)
	if err != nil {
		t.Fatal(err)
	}
	tcpHome := connectHome(t, tcpSess)
	if tcpHome.hello.Version != "test" {
		t.Fatalf("hello = %+v", tcpHome.hello)
	}
	waitActive(t, srv, nil)
	tcpHome.publish(1, []string{"app.example.com"})
	sendHTTP(t, srv, tcpHome, "app.example.com")

	// QUIC session takes over.
	quicSess, err := dialQUICHome(t, srv, home, gwPin)
	if err != nil {
		t.Fatal(err)
	}
	quicHome := connectHome(t, quicSess)
	select {
	case <-tcpSess.Done():
	case <-time.After(testTimeout):
		t.Fatal("TCP session not replaced by the QUIC session")
	}
	waitFor(t, "QUIC session active", func() bool {
		ts := srv.active.Load()
		return ts != nil && ts.sess.Transport() == "quic"
	})
	quicHome.publish(2, []string{"app.example.com"})
	sendHTTP(t, srv, quicHome, "app.example.com")

	// Wrong keys get nothing and leave the session alone.
	intruder := mustIdentity(t, "intruder")
	sess, err = dialTCPHome(t, srv, intruder, gwPin)
	expectNoSession(t, sess, err)
	sess, err = dialQUICHome(t, srv, intruder, gwPin)
	expectNoSession(t, sess, err)
	// A home that pins another gateway key refuses the gateway.
	if sess, err := dialTCPHome(t, srv, home, intruder.Fingerprint()); err == nil {
		expectNoSession(t, sess, nil)
	}
	quicHome.send(&wire.Message{Ping: &wire.Ping{ID: 9}})
	quicHome.expect("pong", func(m *wire.Message) bool { return m.Pong != nil && m.Pong.ID == 9 })

	// reset allows pairing again with a new token; the identity is kept.
	srv.Close()
	var out, errOut bytes.Buffer
	if err := RunCLI(context.Background(), []string{"reset", "--data-dir", dir}, &out, &errOut); err != nil {
		t.Fatalf("reset: %v (%s)", err, errOut.String())
	}
	out.Reset()
	if err := RunCLI(context.Background(), []string{"info", "--data-dir", dir}, &out, &errOut); err != nil {
		t.Fatalf("info: %v", err)
	}
	if !strings.Contains(out.String(), gwPin) || !strings.Contains(out.String(), "paired:      no") {
		t.Fatalf("info after reset:\n%s", out.String())
	}

	tok2 := mustToken(t)
	srv2 := newServer(t, Options{DataDir: dir, PairToken: tok2.String()})
	if srv2.Fingerprint() != gwPin {
		t.Fatal("identity changed across restarts")
	}
	home2 := mustIdentity(t, "home2")
	if _, err := pairHome(t, srv2, home2, tok); err == nil {
		t.Fatal("paired with the old token")
	}
	if _, err := pairHome(t, srv2, home2, tok2); err != nil {
		t.Fatalf("pairing after reset: %v", err)
	}
	waitFor(t, "pairing commit", func() bool { return srv2.HomePin() == home2.Fingerprint() })
	out.Reset()
	RunCLI(context.Background(), []string{"info", "--data-dir", dir}, &out, &errOut)
	if !strings.Contains(out.String(), "paired:      yes") || !strings.Contains(out.String(), home2.Fingerprint()) {
		t.Fatalf("info after pairing:\n%s", out.String())
	}
}

func TestPairingRateLimit(t *testing.T) {
	tok := mustToken(t)
	srv := newServer(t, Options{PairToken: tok.String()}, func(s *Server) {
		s.limiter = pair.NewLimiter(2, 0, time.Minute)
	})
	home := mustIdentity(t, "home")
	for range 2 {
		if _, err := pairHome(t, srv, home, mustToken(t)); !errors.Is(err, pair.ErrBadProof) {
			t.Fatalf("pairing with a wrong token: %v", err)
		}
	}
	waitFor(t, "limiter", func() bool { return !srv.limiter.Allow(netip.MustParseAddr("127.0.0.1"), time.Now()) })
	if _, err := pairHome(t, srv, home, tok); err == nil {
		t.Fatal("pairing allowed over the failure limit")
	}
	if srv.HomePin() != "" {
		t.Fatal("paired over the failure limit")
	}
}

func TestPairingWithoutToken(t *testing.T) {
	srv := newServer(t, Options{})
	if _, err := pairHome(t, srv, mustIdentity(t, "home"), mustToken(t)); err == nil {
		t.Fatal("paired without a token")
	}
}

func TestTunnelHandshakeLimit(t *testing.T) {
	srv := newServer(t, Options{})
	addr := srv.Addrs().Tunnel.String()
	// Idle connections hold pending handshake slots up to the per-IP cap.
	for range maxHandshakesPerIP {
		dial(t, addr)
	}
	waitFor(t, "pending handshakes", func() bool {
		srv.connMu.Lock()
		defer srv.connMu.Unlock()
		return srv.handshakes == maxHandshakesPerIP
	})
	expectClosed(t, dial(t, addr))
}

func TestParsePortRanges(t *testing.T) {
	r, err := parsePortRanges("1024-2000, 3000,4000-4000")
	if err != nil || len(r) != 3 || r[0] != (portRange{1024, 2000}) || r[1] != (portRange{3000, 3000}) {
		t.Fatalf("ranges = %v, %v", r, err)
	}
	for _, bad := range []string{"0", "10-5", "70000", "a-b", "1-"} {
		if _, err := parsePortRanges(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	srv := &Server{allow: r, reserved: map[uint16]bool{22: true, 1500: true}}
	for p, want := range map[uint16]bool{1024: true, 1500: false, 2000: true, 2001: false, 3000: true, 22: false} {
		if srv.portAllowed(p) != want {
			t.Errorf("portAllowed(%d) = %v", p, !want)
		}
	}
}

func TestPublicAddr(t *testing.T) {
	for s, want := range map[string]bool{
		"203.0.113.7": true, "2a01:4f8::1": true, "10.0.0.1": false, "192.168.1.1": false,
		"100.64.1.1": false, "127.0.0.1": false, "fe80::1": false, "fd00::1": false, "::ffff:8.8.8.8": true,
	} {
		if got := publicAddr(netip.MustParseAddr(s)); got != want {
			t.Errorf("publicAddr(%s) = %v", s, got)
		}
	}
}
