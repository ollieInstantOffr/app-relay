package edge

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// taggedUpstream answers "tag uri".
func taggedUpstream(t *testing.T, tag string) *upstream {
	return newUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, tag+" "+r.RequestURI)
	}))
}

func TestServerSelection(t *testing.T) {
	cfg, dir := newConfig(t)
	a, b, dup := taggedUpstream(t, "a"), taggedUpstream(t, "b"), taggedUpstream(t, "dup")
	cfg.Hosts = []Host{
		proxyHost("a", []string{"a.test", "*.wild.test"}, a.ref()),
		proxyHost("b", []string{"b.test", "*.x.wild.test"}, b.ref()),
		proxyHost("dup", []string{"a.test"}, dup.ref()),
	}
	e := startEnv(t, cfg, dir)
	tests := map[string]string{
		"a.test": "a /p", "A.TEST:1234": "a /p", "b.test": "b /p", "one.wild.test": "a /p",
		"k.x.wild.test": "b /p", "x.wild.test": "a /p",
	}
	for host, want := range tests {
		if res := e.get("http://edge/p", "Host", host); res.body != want {
			t.Errorf("Host %s: %d %q, want %q", host, res.StatusCode, res.body, want)
		}
	}
	res := e.get("http://edge/p", "Host", "unknown.test")
	if res.StatusCode != 404 || res.body != notFoundPage {
		t.Errorf("unknown host: %d %q", res.StatusCode, res.body)
	}
}

func TestForceHTTPSAndHTTPOnlyHosts(t *testing.T) {
	cfg, dir := newConfig(t)
	up := taggedUpstream(t, "up")
	secure := proxyHost("s", []string{"s.test"}, up.ref())
	secure.Cert = writeCert(t, dir, "s", "s.test")
	secure.ForceHTTPS = true
	secure.HSTS = "max-age=63072000"
	both := proxyHost("both", []string{"both.test"}, up.ref())
	both.Cert = writeCert(t, dir, "both", "both.test")
	both.HSTS = "max-age=1"
	both.HTTP3 = true
	plain := proxyHost("p", []string{"plain.test"}, up.ref())
	cfg.Hosts = []Host{secure, both, plain}
	os.MkdirAll(filepath.Join(dir, "acme", ".well-known", "acme-challenge"), 0o755)
	os.WriteFile(filepath.Join(dir, "acme", ".well-known", "acme-challenge", "tok"), []byte("proof"), 0o644)
	e := startEnv(t, cfg, dir)

	res := e.get("http://edge/x/y?z=1", "Host", "s.test")
	wantLoc := fmt.Sprintf("https://s.test:%d/x/y?z=1", cfg.HTTPSPort)
	if res.StatusCode != 301 || res.Header.Get("Location") != wantLoc {
		t.Errorf("force https: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	if res.Header.Get("Strict-Transport-Security") != "" {
		t.Error("HSTS on the HTTP redirect")
	}
	if res := e.get("http://edge/.well-known/acme-challenge/tok", "Host", "s.test"); res.StatusCode != 200 || res.body != "proof" {
		t.Errorf("acme on force-https HTTP server: %d %q", res.StatusCode, res.body)
	}
	res = e.get("https://s.test/x")
	if res.StatusCode != 200 || res.body != "up /x" || res.Header.Get("Strict-Transport-Security") != "max-age=63072000" {
		t.Errorf("https: %d %q hsts=%q", res.StatusCode, res.body, res.Header.Get("Strict-Transport-Security"))
	}
	if res.Header.Get("Alt-Svc") != "" {
		t.Error("Alt-Svc without HTTP3")
	}
	res = e.get("http://edge/x", "Host", "both.test")
	if res.body != "up /x" || res.Header.Get("Strict-Transport-Security") != "" {
		t.Errorf("both over http: %q hsts=%q", res.body, res.Header.Get("Strict-Transport-Security"))
	}
	res = e.get("https://both.test/x")
	if got := res.Header.Get("Alt-Svc"); got != fmt.Sprintf(`h3=":%d"; ma=86400`, cfg.HTTPSPort) {
		t.Errorf("Alt-Svc = %q", got)
	}
	if res := e.get("http://edge/x", "Host", "plain.test"); res.body != "up /x" {
		t.Errorf("plain over http: %q", res.body)
	}
	res = e.get("https://plain.test/x")
	if res.StatusCode != 404 || res.body != notFoundPage || res.Header.Get("Strict-Transport-Security") != "" {
		t.Errorf("HTTP-only host over https must hit the default server: %d %q", res.StatusCode, res.body)
	}
}

func TestDefaultServerActions(t *testing.T) {
	up := taggedUpstream(t, "default-host")
	tests := []struct {
		name   string
		def    DefaultServer
		check  func(t *testing.T, e *testEnv)
		status string
	}{
		{"close", DefaultServer{Action: "close"}, func(t *testing.T, e *testEnv) {
			_, err := e.client("http").Get("http://unknown.test/")
			if err == nil {
				t.Fatal("close: expected the connection to be aborted")
			}
		}, "444"},
		{"404", DefaultServer{Action: "404"}, func(t *testing.T, e *testEnv) {
			res := e.get("http://unknown.test/")
			if res.StatusCode != 404 || res.body != notFoundPage || res.Header.Get("Content-Type") != "text/html" {
				t.Fatalf("%d %q %q", res.StatusCode, res.Header.Get("Content-Type"), res.body)
			}
		}, "404"},
		{"redirect", DefaultServer{Action: "redirect", RedirectTo: "https://www.example.com/$host"}, func(t *testing.T, e *testEnv) {
			res := e.get("http://unknown.test/x")
			if res.StatusCode != 302 || res.Header.Get("Location") != "https://www.example.com/unknown.test" {
				t.Fatalf("%d %q", res.StatusCode, res.Header.Get("Location"))
			}
		}, "302"},
		{"host", DefaultServer{Action: "host", Host: &Host{ID: "dh", HSTS: "max-age=1", Locations: []Location{{Path: "/", Kind: "proxy", Upstream: up.ref()}}}}, func(t *testing.T, e *testEnv) {
			for _, scheme := range []string{"http", "https"} {
				res := e.get(scheme + "://unknown.test/y")
				if res.StatusCode != 200 || res.body != "default-host /y" || res.Header.Get("Strict-Transport-Security") != "" || res.Header.Get("Alt-Svc") != "" {
					t.Fatalf("%s: %d %q %v", scheme, res.StatusCode, res.body, res.Header)
				}
			}
		}, "200"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, dir := newConfig(t)
			cfg.Default = tt.def
			e := startEnv(t, cfg, dir)
			tt.check(t, e)
			line := waitAccessLine(t, cfg.LogDir, func(m map[string]string) bool { return m["host"] == "unknown.test" })
			if line["status"] != tt.status || line["host_id"] != "" {
				t.Fatalf("access log status=%q host_id=%q, want %s", line["status"], line["host_id"], tt.status)
			}
			if tt.name == "close" && strings.Contains(e.stderr.String(), "panic") {
				t.Fatalf("abort logged as a crash: %s", e.stderr.String())
			}
		})
	}
}

func handshake(t *testing.T, port int, serverName string, conf *tls.Config) (tls.ConnectionState, error) {
	t.Helper()
	if conf == nil {
		conf = &tls.Config{}
	}
	conf.ServerName = serverName
	conf.InsecureSkipVerify = true
	if conf.NextProtos == nil {
		conf.NextProtos = []string{"h2", "http/1.1"}
	}
	d := &net.Dialer{Timeout: 5 * time.Second}
	c, err := tls.DialWithDialer(d, "tcp", "127.0.0.1:"+strconv.Itoa(port), conf)
	if err != nil {
		return tls.ConnectionState{}, err
	}
	defer c.Close()
	return c.ConnectionState(), nil
}

func TestSNISelectionALPNAndProfiles(t *testing.T) {
	cfg, dir := newConfig(t)
	up := taggedUpstream(t, "up")
	exact := proxyHost("exact", []string{"exact.test"}, up.ref())
	exact.Cert = writeCert(t, dir, "exact", "exact.test")
	exact.HTTP2 = true
	wild := proxyHost("wild", []string{"*.wild.test"}, up.ref())
	wild.Cert = writeCert(t, dir, "wild", "*.wild.test")
	modern := proxyHost("modern", []string{"modern.test"}, up.ref())
	modern.Cert = writeCert(t, dir, "modern", "modern.test")
	modern.CipherProfile = "modern"
	httpOnly := proxyHost("plain", []string{"plain.test"}, up.ref())
	cfg.Hosts = []Host{exact, wild, modern, httpOnly}
	e := startEnv(t, cfg, dir)

	tests := []struct {
		sni, wantName, wantProto string
	}{
		{"exact.test", "exact.test", "h2"},
		{"EXACT.test", "exact.test", "h2"},
		{"a.b.wild.test", "*.wild.test", "http/1.1"},
		{"unknown.test", "localhost", "h2"},
		{"plain.test", "localhost", "h2"},
		{"", "localhost", "h2"},
	}
	for _, tt := range tests {
		st, err := handshake(t, cfg.HTTPSPort, tt.sni, nil)
		if err != nil {
			t.Fatalf("%q: %v", tt.sni, err)
		}
		if got := st.PeerCertificates[0].DNSNames[0]; got != tt.wantName {
			t.Errorf("SNI %q: certificate for %q, want %q", tt.sni, got, tt.wantName)
		}
		if st.NegotiatedProtocol != tt.wantProto {
			t.Errorf("SNI %q: ALPN %q, want %q", tt.sni, st.NegotiatedProtocol, tt.wantProto)
		}
	}
	if _, err := handshake(t, cfg.HTTPSPort, "modern.test", &tls.Config{MaxVersion: tls.VersionTLS12}); err == nil {
		t.Error("modern profile accepted TLS 1.2")
	}
	st, err := handshake(t, cfg.HTTPSPort, "exact.test", &tls.Config{MaxVersion: tls.VersionTLS12})
	if err != nil || st.Version != tls.VersionTLS12 {
		t.Errorf("intermediate profile over TLS 1.2: %v", err)
	}
	res := e.get("https://exact.test/h2")
	if res.ProtoMajor != 2 || res.body != "up /h2" {
		t.Errorf("h2 request: proto %s body %q", res.Proto, res.body)
	}
	line := waitAccessLine(t, cfg.LogDir, func(m map[string]string) bool { return m["uri"] == "/h2" })
	if line["protocol"] != "HTTP/2.0" || line["ssl_protocol"] != "TLSv1.3" || line["scheme"] != "https" || line["host_id"] != "exact" {
		t.Errorf("h2 access line: %v", line)
	}
}

func TestCertificateHotReload(t *testing.T) {
	cfg, dir := newConfig(t)
	up := taggedUpstream(t, "up")
	h := proxyHost("h", []string{"h.test"}, up.ref())
	h.Cert = writeCert(t, dir, "h", "h.test")
	cfg.Hosts = []Host{h}
	srv, err := NewServer(Options{BindHost: "127.0.0.1", CertPollInterval: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(cfg, dir, "r1"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { shutdownQuick(srv) })
	serial := func() string {
		st, err := handshake(t, cfg.HTTPSPort, "h.test", nil)
		if err != nil {
			t.Fatal(err)
		}
		return st.PeerCertificates[0].SerialNumber.String()
	}
	before := serial()
	time.Sleep(20 * time.Millisecond) // distinct mtime
	writeCert(t, dir, "h", "h.test")
	if !waitFor(t, 5*time.Second, func() bool { return serial() != before }) {
		t.Fatal("renewed certificate not picked up")
	}
	if srv.metrics.certReloads.Load() == 0 {
		t.Error("certificate reload not counted")
	}
}
