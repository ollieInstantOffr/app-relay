package tunnel_test

// End to end: a client talks to a real gateway, which forwards over a real
// tunnel session (QUIC and HTTP/2-over-TCP) to the tunnel engine, which hands
// the connection to Relay Edge's tunnel ingress, which proxies to an app.

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/edge"
	"github.com/instantoffr/relay/internal/gateway"
	"github.com/instantoffr/relay/internal/tunnel"
	"github.com/instantoffr/relay/internal/tunnel/pair"
)

func TestEndToEnd(t *testing.T) {
	for _, transport := range []string{"quic", "tcp"} {
		t.Run(transport, func(t *testing.T) { runEndToEnd(t, transport) })
	}
}

func runEndToEnd(t *testing.T, transport string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	socks, err := os.MkdirTemp("/tmp", "e2etun")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socks)

	// The app behind Relay.
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "app host=%s path=%s real-ip=%s", r.Host, r.URL.Path, r.Header.Get("X-Real-Ip"))
	}))
	defer app.Close()
	appHost, appPortStr, _ := net.SplitHostPort(app.Listener.Addr().String())
	var appPort int
	fmt.Sscan(appPortStr, &appPort)

	// A TCP service for the published stream.
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, _ := bufio.NewReader(c).ReadString('\n')
				io.WriteString(c, "echo "+line)
			}()
		}
	}()
	echoPort := echo.Addr().(*net.TCPAddr).Port
	streamPort := freeTCPPort(t)

	// Relay Edge with tunnel ingress sockets.
	certFile, keyFile := writeCert(t, dir, "pub.test")
	httpsSock, httpSock := filepath.Join(socks, "edge-https.sock"), filepath.Join(socks, "edge-http.sock")
	streamSock := filepath.Join(socks, "edge-stream.sock")
	up := edge.Upstream{Scheme: "http", Host: appHost, Port: appPort}
	ecfg := &edge.Config{
		// Only the tunnel ingress: no public HTTP/HTTPS listeners.
		Schema: 1, Default: edge.DefaultServer{Action: "close"},
		Tunnel: &edge.TunnelIngress{HTTPSocket: httpSock, HTTPSSocket: httpsSock},
		Hosts: []edge.Host{
			{ID: "pub", Domains: []string{"pub.test"}, Tunnel: true, Cert: &edge.CertRef{ID: "pub", CertFile: certFile, KeyFile: keyFile},
				Locations: []edge.Location{{Path: "/", Kind: "proxy", Upstream: up}}},
			{ID: "secret", Domains: []string{"secret.test"}, Locations: []edge.Location{{Path: "/", Kind: "proxy", Upstream: up}}},
		},
		// The home side of the stream listens on ::1 so it doesn't collide with
		// the gateway's public port on 127.0.0.1 (different hosts in real life).
		Streams: []edge.Stream{{ID: "s1", Name: "echo", TCP: true, ListenAddr: "::1", ListenLo: streamPort, ListenHi: streamPort,
			ForwardHost: "127.0.0.1", ForwardLo: echoPort, ForwardHi: echoPort,
			TunnelSockets: []edge.TunnelSocket{{Port: streamPort, Socket: streamSock}}}},
	}
	esrv, err := edge.NewServer(edge.Options{BindHost: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := esrv.Start(ecfg, dir, "e1"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		sctx, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		esrv.Shutdown(sctx)
		esrv.Close()
	}()

	// The gateway with a pairing token.
	tok, err := pair.NewToken(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	gw, err := gateway.New(gateway.Options{
		DataDir: filepath.Join(dir, "gw"), TunnelAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", HTTPSAddr: "127.0.0.1:0",
		BindHost: "127.0.0.1", AllowPorts: "1024-65535", PairToken: tok.String(), Version: "e2e",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gw.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer gw.Close()
	addrs := gw.Addrs()

	// Pair like the Relay app does.
	home, err := pair.NewIdentity("relay-home")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := net.Dial("tcp", addrs.Tunnel.String())
	if err != nil {
		t.Fatal(err)
	}
	pctx, pcancel := context.WithTimeout(ctx, 10*time.Second)
	pin, err := pair.Home(pctx, tls.Client(raw, pair.ClientConfig(home, "", pair.ALPN)), tok)
	pcancel()
	raw.Close()
	if err != nil {
		t.Fatalf("pairing: %v", err)
	}
	// The gateway commits once it has read the home's confirmation.
	waitFor(3*time.Second, func() bool { return gw.HomePin() != "" })
	// The gateway commits once it has read the home's confirmation.
	waitFor(3*time.Second, func() bool { return gw.HomePin() != "" })
	if pin != gw.Fingerprint() || gw.HomePin() != home.Fingerprint() {
		t.Fatalf("pins: %s / %s", pin, gw.HomePin())
	}

	// The tunnel engine's files.
	certPEM, keyPEM, _ := home.MarshalPEM()
	homeCert, homeKey := filepath.Join(dir, "home.crt"), filepath.Join(dir, "home.key")
	os.WriteFile(homeCert, certPEM, 0o644)
	os.WriteFile(homeKey, keyPEM, 0o600)
	gwFile := filepath.Join(dir, "gateways.json")
	writeJSON(t, gwFile, tunnel.Gateways{Schema: 1, Gateways: []tunnel.Gateway{{
		ID: "gw1", Name: "vps", Address: addrs.Tunnel.String(), Transport: transport, Enabled: true, Pin: pin, CertFile: homeCert, KeyFile: homeKey,
	}}})
	// The gateway's public stream port maps to the stream's ingress socket.
	cfg := &tunnel.Config{
		Schema: 1, GatewaysFile: gwFile, RuntimeSocket: filepath.Join(socks, "rt.sock"),
		Targets: tunnel.Targets{HTTP: httpSock, HTTPS: httpsSock},
		Routes:  []tunnel.Route{{GatewayID: "gw1", Names: []string{"pub.test"}, TCP: []tunnel.TCPRoute{{Port: uint16(streamPort), Socket: streamSock}}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	eng := tunnel.New(tunnel.Options{Version: "e2e"})
	if err := eng.Start(cfg, "r1"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		sctx, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		eng.Shutdown(sctx)
	}()

	var st tunnel.GatewayStatus
	if !waitFor(10*time.Second, func() bool {
		s := eng.Status()
		if len(s.Gateways) == 1 {
			st = s.Gateways[0]
		}
		return st.State == tunnel.StateConnected && st.AckedGeneration == st.Generation && st.Generation > 0
	}) {
		t.Fatalf("not connected: %+v", st)
	}
	if st.Transport != transport || st.Version != "e2e" {
		t.Errorf("status %+v", st)
	}

	// HTTPS by SNI through gateway → tunnel → engine → Relay Edge → app.
	httpsClient := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addrs.HTTPS.String())
		},
	}}
	res, err := httpsClient.Get("https://pub.test/hello")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	// The client connects from 127.0.0.1 to a gateway on loopback: its
	// address is not trusted and must not look local.
	if res.StatusCode != 200 || !strings.Contains(string(body), "app host=pub.test path=/hello real-ip=0.0.0.0") {
		t.Fatalf("https: %d %q", res.StatusCode, body)
	}

	// Plain HTTP by Host.
	httpClient := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addrs.HTTP.String())
		},
	}}
	res, err = httpClient.Get("http://pub.test/plain")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(body), "path=/plain") {
		t.Fatalf("http: %d %q", res.StatusCode, body)
	}

	// A name that isn't published: the gateway drops it.
	if res, err := httpClient.Get("http://secret.test/"); err == nil {
		res.Body.Close()
		t.Errorf("unpublished host through the gateway: %d", res.StatusCode)
	}
	// Published SNI but the Host of an unpublished host: Relay refuses it.
	req, _ := http.NewRequest(http.MethodGet, "https://pub.test/", nil)
	req.Host = "secret.test"
	if res, err := httpsClient.Do(req); err == nil {
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if strings.Contains(string(b), "app host=secret.test") {
			t.Errorf("unpublished Host served through the tunnel: %q", b)
		}
	}

	// A published TCP stream port on the gateway.
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", streamPort), 3*time.Second)
	if err != nil {
		t.Fatalf("stream port: %v", err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(c, "ping\n")
	line, err := bufio.NewReader(c).ReadString('\n')
	c.Close()
	if err != nil || line != "echo ping\n" {
		t.Fatalf("stream: %q %v", line, err)
	}

	// Unpublishing the name takes effect on reload without dropping the session.
	cfg.Routes[0].Names = []string{"other.test"}
	if err := eng.Reload(cfg, "r2"); err != nil {
		t.Fatal(err)
	}
	if !waitFor(5*time.Second, func() bool {
		s := eng.Status().Gateways[0]
		return s.AckedGeneration == s.Generation && s.Generation > st.Generation
	}) {
		t.Fatalf("routes not re-acked: %+v", eng.Status().Gateways[0])
	}
	if res, err := httpsClient.Get("https://pub.test/hello"); err == nil {
		res.Body.Close()
		t.Errorf("unpublished name still served: %d", res.StatusCode)
	}
	if s := eng.Status().Gateways[0]; s.Reconnects != 0 || s.State != tunnel.StateConnected {
		t.Errorf("reload reconnected: %+v", s)
	}
}

func freeTCPPort(t *testing.T) int {
	for range 50 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		p := ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		if ln2, err := net.Listen("tcp", fmt.Sprintf("[::1]:%d", p)); err == nil {
			ln2.Close()
			return p
		}
	}
	t.Fatal("no free port")
	return 0
}

func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

func writeJSON(t *testing.T, path string, v any) {
	data, _ := json.Marshal(v)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeCert(t *testing.T, dir string, names ...string) (certFile, keyFile string) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(key)
	certFile, keyFile = filepath.Join(dir, "pub.crt"), filepath.Join(dir, "pub.key")
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600)
	return certFile, keyFile
}
