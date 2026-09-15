package edge

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// freePort reserves an ephemeral port and releases it for the server.
func freePort(t testing.TB, network string) int {
	t.Helper()
	for range 20 {
		switch network {
		case "udp":
			pc, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			p := pc.LocalAddr().(*net.UDPAddr).Port
			pc.Close()
			return p
		default:
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			p := ln.Addr().(*net.TCPAddr).Port
			ln.Close()
			// Also make sure the UDP side is free (QUIC shares the port).
			if pc, err := net.ListenPacket("udp", "127.0.0.1:"+strconv.Itoa(p)); err == nil {
				pc.Close()
				return p
			}
		}
	}
	t.Fatal("no free port")
	return 0
}

// writeCert writes a self-signed certificate for names into dir and returns
// a CertRef with paths relative to dir.
func writeCert(t testing.TB, dir, id string, names ...string) *CertRef {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: names[0]},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: names,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(dir, "certs"), 0o755)
	certFile, keyFile := filepath.Join("certs", id+".crt"), filepath.Join("certs", id+".key")
	if err := os.WriteFile(filepath.Join(dir, certFile), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, keyFile), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		t.Fatal(err)
	}
	return &CertRef{ID: id, CertFile: certFile, KeyFile: keyFile}
}

// syncBuffer collects stderr output safely.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
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

// testEnv is an edge server plus the directory its config lives in.
type testEnv struct {
	t      testing.TB
	dir    string
	cfg    *Config
	srv    *Server
	stderr *syncBuffer
}

// newConfig returns a minimal valid config on free loopback ports.
func newConfig(t testing.TB) (*Config, string) {
	dir := t.TempDir()
	cfg := &Config{
		Schema:      1,
		HTTPPort:    freePort(t, "tcp"),
		HTTPSPort:   freePort(t, "tcp"),
		LogDir:      filepath.Join(dir, "logs"),
		ACMEWebroot: "acme",
		Default:     DefaultServer{Action: "404"},
	}
	return cfg, dir
}

func startEnv(t testing.TB, cfg *Config, dir string) *testEnv {
	t.Helper()
	e := &testEnv{t: t, dir: dir, cfg: cfg, stderr: &syncBuffer{}}
	srv, err := NewServer(Options{Stderr: e.stderr, BindHost: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(cfg, dir, filepath.Base(dir)); err != nil {
		t.Fatalf("start: %v", err)
	}
	e.srv = srv
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		srv.Close()
	})
	return e
}

// client returns an HTTP client whose connections always go to the edge
// listener for scheme, whatever host the URL names.
func (e *testEnv) client(scheme string) *http.Client {
	port := e.cfg.HTTPPort
	if scheme == "https" {
		port = e.cfg.HTTPSPort
	}
	addr := "127.0.0.1:" + strconv.Itoa(port)
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		TLSClientConfig:    &tls.Config{InsecureSkipVerify: true},
		DisableCompression: true,
		ForceAttemptHTTP2:  true,
	}
	return &http.Client{
		Transport:     tr,
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

type response struct {
	*http.Response
	body string
}

func (e *testEnv) do(req *http.Request) response {
	e.t.Helper()
	res, err := e.client(req.URL.Scheme).Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return response{res, string(b)}
}

func (e *testEnv) get(url string, headers ...string) response {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if strings.EqualFold(headers[i], "Host") {
			req.Host = headers[i+1]
			continue
		}
		req.Header.Add(headers[i], headers[i+1])
	}
	return e.do(req)
}

// reload applies e.cfg again.
func (e *testEnv) reload() error {
	return e.srv.Reload(e.cfg, e.dir, filepath.Base(e.dir))
}

// upstream is a test backend that echoes request details.
type upstream struct {
	*httptest.Server
	host string
	port int
}

func newUpstream(t testing.TB, h http.Handler) *upstream {
	t.Helper()
	if h == nil {
		h = echoHandler()
	}
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	host, p, _ := net.SplitHostPort(s.Listener.Addr().String())
	port, _ := strconv.Atoi(p)
	return &upstream{Server: s, host: host, port: port}
}

func (u *upstream) ref() Upstream { return Upstream{Scheme: "http", Host: u.host, Port: u.port} }

// echoHandler answers with the upstream request line and selected headers.
func echoHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.Copy(io.Discard, r.Body)
		var b strings.Builder
		b.WriteString("uri=" + r.RequestURI + "\n")
		b.WriteString("host=" + r.Host + "\n")
		for _, k := range []string{"X-Real-Ip", "X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host", "X-Forwarded-Port",
			"X-Request-Id", "Remote-User", "Remote-Groups", "X_under", "X-Custom", "X-Uri", "Upgrade", "Connection"} {
			if v, ok := r.Header[k]; ok {
				b.WriteString(strings.ToLower(k) + "=" + strings.Join(v, ",") + "\n")
			}
		}
		io.WriteString(w, b.String())
	})
}

// field returns "key=value" lines from an echo body.
func field(body, key string) (string, bool) {
	for _, l := range strings.Split(body, "\n") {
		if k, v, ok := strings.Cut(l, "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

func proxyHost(id string, domains []string, up Upstream) Host {
	return Host{ID: id, Domains: domains, Locations: []Location{{Path: "/", Kind: "proxy", Upstream: up}}}
}

// waitFor polls cond until it holds or the timeout expires.
func waitFor(t testing.TB, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}
