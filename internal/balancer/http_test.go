package balancer

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/balancer/spec"
	"github.com/instantoffr/relay/internal/proxyproto"
)

func singleBackendEnv(t *testing.T, mutate func(cfg *spec.Config), servers ...spec.Server) *env {
	t.Helper()
	cfg := baseConfig(t)
	cfg.Backends = []spec.Backend{httpBackend("web", servers...)}
	cfg.Frontends = []spec.Frontend{httpFrontend(t, "fe", "web")}
	if mutate != nil {
		mutate(cfg)
	}
	return startEnv(t, cfg)
}

func (e *env) waitLog(t *testing.T, substr string) {
	t.Helper()
	if !waitFor(t, 3*time.Second, func() bool { return strings.Contains(e.logs(), substr) }) {
		t.Fatalf("log lacks %q:\n%s", substr, e.logs())
	}
}

func TestForwardFor(t *testing.T) {
	u := newUpstream(t, "a", nil)
	e := singleBackendEnv(t, func(cfg *spec.Config) {
		cfg.Frontends[0].ForwardFor = true
		cfg.Frontends[0].ForwardForExcept = []string{"10.0.0.0/8"}
		cfg.Backends[0].ForwardFor = true // both: added once
	}, u.server())
	if r := get(t, e.feURL(0, "/")); !strings.HasSuffix(r.body, "xff=127.0.0.1") {
		t.Fatalf("xff: %q", r.body)
	}
	if r := get(t, e.feURL(0, "/"), "X-Forwarded-For", "203.0.113.9"); !strings.HasSuffix(r.body, "xff=203.0.113.9, 127.0.0.1") {
		t.Fatalf("appended xff: %q", r.body)
	}
	e.cfg.Frontends[0].ForwardForExcept = []string{"127.0.0.0/8"}
	e.cfg.Backends[0].ForwardFor = false
	if err := e.reload("h2"); err != nil {
		t.Fatal(err)
	}
	if r := get(t, e.feURL(0, "/"), "X-Forwarded-For", "203.0.113.9"); !strings.HasSuffix(r.body, "xff=203.0.113.9") {
		t.Fatalf("except: %q", r.body)
	}
	e.cfg.Frontends[0].ForwardFor = false
	e.cfg.Backends[0].ForwardFor = true
	if err := e.reload("h3"); err != nil {
		t.Fatal(err)
	}
	if r := get(t, e.feURL(0, "/")); !strings.HasSuffix(r.body, "xff=127.0.0.1") {
		t.Fatalf("backend forwardfor: %q", r.body)
	}
}

func TestHeadersAndBodies(t *testing.T) {
	u := newUpstream(t, "a", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Seen", fmt.Sprintf("host=%s conn=%q custom=%q ua=%q te=%q len=%d", r.Host, r.Header.Get("Connection"), r.Header.Get("X-Custom"), r.Header.Get("User-Agent"), r.TransferEncoding, len(body)))
		w.Header().Set("Connection", "X-Hop")
		w.Header().Set("X-Hop", "1")
		w.WriteHeader(201)
		io.WriteString(w, "ok")
	})
	e := singleBackendEnv(t, nil, u.server())
	req, _ := http.NewRequest("POST", e.feURL(0, "/p"), strings.NewReader(strings.Repeat("x", 100000)))
	req.Host = "Site.Example:8080"
	req.Header.Set("Connection", "X-Custom")
	req.Header.Set("X-Custom", "secret")
	r := do(t, req)
	if r.StatusCode != 201 || r.body != "ok" || r.Header.Get("X-Hop") != "" {
		t.Fatalf("%d %q %v", r.StatusCode, r.body, r.Header)
	}
	if got := r.Header.Get("X-Seen"); got != `host=Site.Example:8080 conn="" custom="" ua="Go-http-client/1.1" te=[] len=100000` {
		t.Fatalf("upstream saw %s", got)
	}
	// Chunked request body.
	pr, pw := io.Pipe()
	go func() {
		for range 5 {
			pw.Write(bytes.Repeat([]byte("y"), 1000))
		}
		pw.Close()
	}()
	req, _ = http.NewRequest("PUT", e.feURL(0, "/chunked"), pr)
	r = do(t, req)
	if !strings.Contains(r.Header.Get("X-Seen"), `te=["chunked"] len=5000`) {
		t.Fatalf("chunked: %s", r.Header.Get("X-Seen"))
	}
	st := e.stat("web", "a")
	if st["hrsp_2xx"] != "2" || st["stot"] != "2" {
		t.Fatalf("server stats: %v", st)
	}
	if fe := e.stat("fe", "FRONTEND"); fe["req_tot"] != "2" || fe["bin"] == "0" {
		t.Fatalf("frontend stats: %v", fe)
	}
}

func TestAcceptProxy(t *testing.T) {
	u := newUpstream(t, "a", nil)
	other := newUpstream(t, "proxied", nil)
	e := singleBackendEnv(t, func(cfg *spec.Config) {
		fe := &cfg.Frontends[0]
		fe.AcceptProxy, fe.ForwardFor = true, true
		cfg.Backends = append(cfg.Backends, httpBackend("proxied", other.server()))
		fe.Rules = []spec.Rule{{Backend: "proxied", Conditions: []spec.Condition{{Type: spec.CondSrc, Values: []string{"192.0.2.0/24"}}}}}
	}, u.server())
	send := func(header []byte) string {
		c, err := net.Dial("tcp", e.cfg.Frontends[0].Bind)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(3 * time.Second))
		c.Write(header)
		io.WriteString(c, "GET /pp HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
		b, _ := io.ReadAll(c)
		return string(b)
	}
	v1 := send([]byte("PROXY TCP4 192.0.2.10 198.51.100.1 51234 443\r\n"))
	if !strings.Contains(v1, "proxied /pp xff=192.0.2.10") {
		t.Fatalf("v1: %q", v1)
	}
	v2 := send(proxyproto.AppendV2(nil, netip.MustParseAddrPort("[2001:db8::7]:4000"), netip.MustParseAddrPort("[2001:db8::1]:443")))
	if !strings.Contains(v2, "a /pp xff=2001:db8::7") {
		t.Fatalf("v2: %q", v2)
	}
	local := send([]byte("\r\n\r\n\x00\r\nQUIT\n\x20\x00\x00\x00"))
	if !strings.Contains(local, "a /pp xff=127.0.0.1") {
		t.Fatalf("v2 LOCAL: %q", local)
	}
	if bad := send(nil); bad != "" {
		t.Fatalf("missing header must close: %q", bad)
	}
	e.waitLog(t, `192.0.2.10:51234 [`)
	e.waitLog(t, `2001:db8::7:4000 [`)
	e.waitLog(t, "Received something which does not look like a PROXY protocol header")
}

// proxyEchoServer reads a PROXY v2 header and answers HTTP requests with the
// addresses; it counts connections.
func proxyEchoServer(t *testing.T, conns *atomic.Int64) int {
	return tcpServer(t, func(c net.Conn) {
		conns.Add(1)
		br := bufio.NewReader(c)
		h, err := proxyproto.Read(br)
		if err != nil {
			return
		}
		for {
			req, err := http.ReadRequest(br)
			if err != nil {
				return
			}
			io.Copy(io.Discard, req.Body)
			body := fmt.Sprintf("src=%s dst=%s local=%v", h.Src, h.Dst, h.Local)
			fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
		}
	})
}

func TestSendProxyV2(t *testing.T) {
	var conns atomic.Int64
	port := proxyEchoServer(t, &conns)
	e := singleBackendEnv(t, func(cfg *spec.Config) { cfg.Backends[0].SendProxy = true },
		spec.Server{Name: "pp", Address: "127.0.0.1", Port: port, Weight: 1, State: spec.StateReady})
	bind := e.cfg.Frontends[0].Bind
	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr}
	var first string
	for i := range 3 {
		res, err := client.Get("http://" + bind + "/")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if i == 0 {
			first = string(b)
		}
		if !strings.HasPrefix(string(b), "src=127.0.0.1:") || !strings.Contains(string(b), "dst="+bind) || string(b) != first {
			t.Fatalf("request %d: %q (first %q)", i, b, first)
		}
	}
	if conns.Load() != 1 {
		t.Fatalf("private server connection reused per client connection: %d connections", conns.Load())
	}
	// Another client connection gets its own server connection.
	res, err := (&http.Client{Transport: &http.Transport{}}).Get("http://" + bind + "/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if conns.Load() != 2 {
		t.Fatalf("connections %d", conns.Load())
	}

	// tcp mode
	tport := tcpServer(t, func(c net.Conn) {
		h, err := proxyproto.Read(bufio.NewReader(c))
		if err == nil {
			fmt.Fprintf(c, "src=%s\n", h.Src)
		}
	})
	cfg := baseConfig(t)
	b := tcpBackend("t", spec.Server{Name: "t", Address: "127.0.0.1", Port: tport, Weight: 1, State: spec.StateReady})
	b.SendProxy = true
	cfg.Backends = []spec.Backend{b}
	cfg.Frontends = []spec.Frontend{tcpFrontend(t, "t", "t")}
	e2 := startEnv(t, cfg)
	c, err := net.Dial("tcp", e2.cfg.Frontends[0].Bind)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	line, _ := bufio.NewReader(c).ReadString('\n')
	if want := "src=" + c.LocalAddr().String() + "\n"; line != want {
		t.Fatalf("tcp send-proxy: %q want %q", line, want)
	}
}

func TestTLSToServers(t *testing.T) {
	var sni, alpn atomic.Value
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "secure") }))
	ts.TLS = &tls.Config{GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
		sni.Store(h.ServerName)
		alpn.Store(strings.Join(h.SupportedProtos, ","))
		return nil, nil
	}}
	ts.StartTLS()
	defer ts.Close()
	_, p, _ := net.SplitHostPort(ts.Listener.Addr().String())
	port, _ := strconv.Atoi(p)
	dir := t.TempDir()
	goodCA := filepath.Join(dir, "good.pem")
	os.WriteFile(goodCA, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw}), 0o644)
	badCA := filepath.Join(dir, "bad.pem")
	os.WriteFile(badCA, selfSignedPEM(t), 0o644)

	srv := spec.Server{Name: "s", Address: "127.0.0.1", Port: port, Weight: 1, State: spec.StateReady}
	for _, tc := range []struct {
		name         string
		verify       bool
		ca           string
		status       int
		wantBodyPart string
	}{
		{"no verify", false, "", 200, "secure"},
		{"verify ok (chain only, host name ignored)", true, goodCA, 200, "secure"},
		{"verify fails", true, badCA, 503, "No server is available"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := singleBackendEnv(t, func(cfg *spec.Config) {
				cfg.Backends[0].TLS, cfg.Backends[0].TLSVerify = true, tc.verify
				cfg.CAFile = tc.ca
			}, srv)
			r := get(t, e.feURL(0, "/"))
			if r.StatusCode != tc.status || !strings.Contains(r.body, tc.wantBodyPart) {
				t.Fatalf("%d %q", r.StatusCode, r.body)
			}
			if sni.Load() != "" || alpn.Load() != "" {
				t.Fatalf("sent SNI %q / ALPN %q", sni.Load(), alpn.Load())
			}
		})
	}
}

func TestRetriesRedispatchAndConnectErrors(t *testing.T) {
	alive := newUpstream(t, "alive", nil)
	dead := spec.Server{Name: "dead", Address: "127.0.0.1", Port: closedPort(t), Weight: 1, State: spec.StateReady}
	e := singleBackendEnv(t, func(cfg *spec.Config) {
		b := &cfg.Backends[0]
		b.Algorithm, b.Retries, b.Redispatch = spec.AlgoFirst, 2, true
		b.Timeouts.ConnectMs = 40
	}, dead, alive.server())
	start := time.Now()
	r := get(t, e.feURL(0, "/"))
	if r.StatusCode != 200 || !strings.HasPrefix(r.body, "alive") {
		t.Fatalf("redispatch: %d %q", r.StatusCode, r.body)
	}
	if d := time.Since(start); d < 60*time.Millisecond {
		t.Errorf("turn-around timer not applied between retries (%s)", d)
	}
	be, ds := e.stat("web", "BACKEND"), e.stat("web", "dead")
	if be["wretr"] != "2" || be["wredis"] != "1" || be["econ"] != "0" || ds["wretr"] != "2" || ds["wredis"] != "1" {
		t.Fatalf("counters backend %s/%s/%s dead %s/%s", be["wretr"], be["wredis"], be["econ"], ds["wretr"], ds["wredis"])
	}
	e.waitLog(t, "fe web/alive 0/0/")
	e.waitLog(t, " ---- 1/1/1/1/+2 0/0 ")

	e.cfg.Backends[0].Redispatch = false
	e.cfg.Backends[0].Retries = 1
	if err := e.reload("h2"); err != nil {
		t.Fatal(err)
	}
	r = get(t, e.feURL(0, "/"))
	if r.StatusCode != 503 || !strings.Contains(r.body, "503 Service Unavailable") {
		t.Fatalf("connect failure: %d %q", r.StatusCode, r.body)
	}
	if be := e.stat("web", "BACKEND"); be["econ"] != "1" || be["wretr"] != "3" {
		t.Fatalf("econ %s wretr %s", be["econ"], be["wretr"])
	}
	e.waitLog(t, "fe web/dead 0/0/-1/-1/")
	e.waitLog(t, " 503 ")
	e.waitLog(t, " SC-- ")
}

func TestServerErrors(t *testing.T) {
	slow := newUpstream(t, "slow", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		io.WriteString(w, "late")
	})
	garbage := tcpServer(t, func(c net.Conn) {
		bufio.NewReader(c).ReadString('\n')
		io.WriteString(c, "NOT HTTP AT ALL\r\n\r\n")
	})
	cut := tcpServer(t, func(c net.Conn) {
		http.ReadRequest(bufio.NewReader(c))
		io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\npartial")
	})
	cfg := baseConfig(t)
	b1 := httpBackend("slow", slow.server())
	b1.Timeouts.ServerMs = 100
	cfg.Backends = []spec.Backend{b1,
		httpBackend("garbage", spec.Server{Name: "g", Address: "127.0.0.1", Port: garbage, Weight: 1, State: spec.StateReady}),
		httpBackend("cut", spec.Server{Name: "c", Address: "127.0.0.1", Port: cut, Weight: 1, State: spec.StateReady}),
	}
	fe := httpFrontend(t, "fe", "slow")
	fe.Rules = []spec.Rule{
		{Backend: "garbage", Conditions: []spec.Condition{{Type: spec.CondPath, Values: []string{"/garbage"}}}},
		{Backend: "cut", Conditions: []spec.Condition{{Type: spec.CondPath, Values: []string{"/cut"}}}},
	}
	cfg.Frontends = []spec.Frontend{fe}
	e := startEnv(t, cfg)
	if r := get(t, e.feURL(0, "/")); r.StatusCode != 504 || !strings.Contains(r.body, "504 Gateway Time-out") {
		t.Fatalf("timeout: %d %q", r.StatusCode, r.body)
	}
	if r := get(t, e.feURL(0, "/garbage")); r.StatusCode != 502 || !strings.Contains(r.body, "502 Bad Gateway") {
		t.Fatalf("bad response: %d %q", r.StatusCode, r.body)
	}
	if _, err := testClient.Get(e.feURL(0, "/cut")); err == nil {
		// headers arrive; the body read must fail
		res, _ := testClient.Get(e.feURL(0, "/cut"))
		if res != nil {
			if _, err := io.ReadAll(res.Body); err == nil {
				t.Fatal("truncated response delivered as complete")
			}
		}
	}
	if st := e.stat("slow", "BACKEND"); st["eresp"] != "1" {
		t.Fatalf("eresp %v", st["eresp"])
	}
	e.waitLog(t, " 504 ")
	e.waitLog(t, " sH-- ")
	e.waitLog(t, " SH-- ")
	e.waitLog(t, " SD-- ")
}

func TestCompression(t *testing.T) {
	big := strings.Repeat("hello compression ", 200)
	u := newUpstream(t, "a", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/png":
			w.Header().Set("Content-Type", "image/png")
		case "/encoded":
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Content-Encoding", "br")
		case "/partial":
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(206)
		default:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("ETag", `"v1"`)
		}
		io.WriteString(w, big)
	})
	e := singleBackendEnv(t, func(cfg *spec.Config) { cfg.Frontends[0].Compression = true }, u.server())
	r := get(t, e.feURL(0, "/html"), "Accept-Encoding", "br;q=1, gzip;q=0.5")
	if r.Header.Get("Content-Encoding") != "gzip" || r.Header.Get("Vary") != "Accept-Encoding" || r.Header.Get("Etag") != `W/"v1"` || (r.Header.Get("Content-Length") != "" && r.Header.Get("Content-Length") != strconv.Itoa(len(r.body))) {
		t.Fatalf("compressed headers: %v", r.Header)
	}
	zr, err := gzip.NewReader(strings.NewReader(r.body))
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(zr)
	if string(plain) != big || len(r.body) >= len(big) {
		t.Fatalf("gzip body: %d → %d bytes", len(big), len(r.body))
	}
	for _, tc := range []struct{ path, ae string }{
		{"/html", ""}, {"/html", "gzip;q=0"}, {"/png", "gzip"}, {"/encoded", "gzip"}, {"/partial", "gzip"},
	} {
		r := get(t, e.feURL(0, tc.path), "Accept-Encoding", tc.ae)
		if r.Header.Get("Content-Encoding") == "gzip" || len(r.body) != len(big) {
			t.Errorf("%s ae=%q compressed: %v", tc.path, tc.ae, r.Header)
		}
	}
	req, _ := http.NewRequest("HEAD", e.feURL(0, "/html"), nil)
	req.Header.Set("Accept-Encoding", "gzip")
	if r := do(t, req); r.Header.Get("Content-Encoding") != "" {
		t.Error("HEAD compressed")
	}
	if fe := e.stat("fe", "FRONTEND"); fe["comp_rsp"] != "1" || fe["comp_in"] != strconv.Itoa(len(big)) || fe["comp_out"] == "0" || fe["comp_byp"] != "1" {
		t.Errorf("compression counters: rsp %s in %s out %s byp %s", fe["comp_rsp"], fe["comp_in"], fe["comp_out"], fe["comp_byp"])
	}
	// Disabled on the frontend.
	e.cfg.Frontends[0].Compression = false
	e.reload("h2")
	if r := get(t, e.feURL(0, "/html"), "Accept-Encoding", "gzip"); r.Header.Get("Content-Encoding") != "" {
		t.Error("compressed while disabled")
	}
}

func TestWebSocketTunnel(t *testing.T) {
	u := newUpstream(t, "ws", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" || !strings.EqualFold(r.Header.Get("Connection"), "upgrade") {
			http.Error(w, "not an upgrade", 400)
			return
		}
		c, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\nhello\n")
		for {
			line, err := brw.ReadString('\n')
			if err != nil {
				return
			}
			io.WriteString(c, "echo "+line)
		}
	})
	e := singleBackendEnv(t, nil, u.server())
	c, err := net.Dial("tcp", e.cfg.Frontends[0].Bind)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(c, "GET /chat HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: keep-alive, Upgrade\r\n\r\nearly\n")
	br := bufio.NewReader(c)
	res, err := http.ReadResponse(br, nil)
	if err != nil || res.StatusCode != 101 || res.Header.Get("Upgrade") != "websocket" {
		t.Fatalf("upgrade: %v %+v", err, res)
	}
	if l, _ := br.ReadString('\n'); l != "hello\n" {
		t.Fatalf("server data: %q", l)
	}
	if l, _ := br.ReadString('\n'); l != "echo early\n" {
		t.Fatalf("buffered client data: %q", l)
	}
	for i := range 3 {
		fmt.Fprintf(c, "msg %d\n", i)
		if l, _ := br.ReadString('\n'); l != fmt.Sprintf("echo msg %d\n", i) {
			t.Fatalf("echo: %q", l)
		}
	}
	c.Close()
	e.waitLog(t, ` 101 `)
	e.waitLog(t, `"GET /chat HTTP/1.1"`)
}

func TestMaxConn(t *testing.T) {
	port := tcpServer(t, echoTCP("srv"))
	cfg := baseConfig(t)
	cfg.MaxConn = 1
	cfg.Backends = []spec.Backend{tcpBackend("t", spec.Server{Name: "t", Address: "127.0.0.1", Port: port, Weight: 1, State: spec.StateReady})}
	cfg.Frontends = []spec.Frontend{tcpFrontend(t, "t", "t")}
	e := startEnv(t, cfg)
	talk := func(c net.Conn, br *bufio.Reader, wait time.Duration) (string, error) {
		io.WriteString(c, "hi\n")
		c.SetReadDeadline(time.Now().Add(wait))
		return br.ReadString('\n')
	}
	c1, _ := net.Dial("tcp", cfg.Frontends[0].Bind)
	defer c1.Close()
	br1 := bufio.NewReader(c1)
	if l, err := talk(c1, br1, 2*time.Second); err != nil || l != "srv:hi\n" {
		t.Fatalf("first: %q %v", l, err)
	}
	c2, err := net.Dial("tcp", cfg.Frontends[0].Bind)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	br2 := bufio.NewReader(c2)
	if l, err := talk(c2, br2, 200*time.Millisecond); err == nil {
		t.Fatalf("second connection served beyond maxconn: %q", l)
	}
	if info := e.srv.Command("show info"); !strings.Contains(info, "CurrConns: 1\n") || !strings.Contains(info, "Maxconn: 1\n") {
		t.Fatalf("show info:\n%s", info)
	}
	c1.Close()
	c2.SetReadDeadline(time.Now().Add(3 * time.Second))
	if l, err := br2.ReadString('\n'); err != nil || l != "srv:hi\n" {
		t.Fatalf("second after first closed: %q %v", l, err)
	}
}

func cookieJarless(t *testing.T, url string, cookie string) result {
	t.Helper()
	if cookie == "" {
		return get(t, url)
	}
	return get(t, url, "Cookie", cookie)
}

func TestStickyInsert(t *testing.T) {
	var mu sync.Mutex
	seen := map[string][]string{}
	mk := func(name string) *upstream {
		return newUpstream(t, name, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen[name] = append(seen[name], r.Header.Get("Cookie"))
			mu.Unlock()
			http.SetCookie(w, &http.Cookie{Name: "SRVID", Value: "server-set"})
			io.WriteString(w, name)
		})
	}
	a, b := mk("a"), mk("b")
	sa, sb := a.server(), b.server()
	sa.Cookie, sb.Cookie = "a", "b"
	e := singleBackendEnv(t, func(cfg *spec.Config) {
		cfg.Backends[0].Sticky = &spec.Sticky{Mode: spec.StickyInsert, Cookie: "SRVID"}
	}, sa, sb)
	url := e.feURL(0, "/")
	r := cookieJarless(t, url, "")
	first := r.body
	if sc := r.Header.Values("Set-Cookie"); len(sc) != 1 || sc[0] != "SRVID="+first+"; path=/" || r.Header.Get("Cache-Control") != "private" {
		t.Fatalf("insert: %v %v", sc, r.Header)
	}
	for range 4 {
		r = cookieJarless(t, url, "other=1; SRVID="+first)
		if r.body != first || len(r.Header.Values("Set-Cookie")) != 0 {
			t.Fatalf("persistence: %q %v", r.body, r.Header.Values("Set-Cookie"))
		}
	}
	mu.Lock()
	last := seen[first][len(seen[first])-1]
	mu.Unlock()
	if last != "other=1" {
		t.Fatalf("indirect: server saw cookie %q", last)
	}
	if r = cookieJarless(t, url, "SRVID=zzz"); r.Header.Get("Set-Cookie") == "" {
		t.Fatal("invalid cookie not replaced")
	}
	otherName := map[string]string{"a": "b", "b": "a"}[first]
	// drain: persistent clients stay.
	e.cmd("set server web/" + first + " state drain")
	if r = cookieJarless(t, url, "SRVID="+first); r.body != first {
		t.Fatalf("drain must honour persistence: %q", r.body)
	}
	if r = cookieJarless(t, url, ""); r.body != otherName {
		t.Fatalf("drain must not get new clients: %q", r.body)
	}
	// maint / DOWN: redispatch and a new cookie.
	for _, cmd := range []string{"state maint", "health down"} {
		e.cmd("set server web/" + first + " state ready")
		e.cmd("set server web/" + first + " health up")
		e.cmd("set server web/" + first + " " + cmd)
		r = cookieJarless(t, url, "SRVID="+first)
		if r.body != otherName || r.Header.Get("Set-Cookie") != "SRVID="+otherName+"; path=/" {
			t.Fatalf("%s: %q %v", cmd, r.body, r.Header.Values("Set-Cookie"))
		}
	}
	e.waitLog(t, " --VD ")
	e.waitLog(t, " --NI ")
	e.waitLog(t, " --DI ")
}

func TestStickyPrefix(t *testing.T) {
	var got atomic.Value
	u := newUpstream(t, "a", func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.Header.Get("Cookie"))
		w.Header().Add("Set-Cookie", "JSESSIONID=abc123; Path=/; HttpOnly")
		w.Header().Add("Set-Cookie", "theme=dark")
		io.WriteString(w, "a")
	})
	s := u.server()
	s.Cookie = "web1"
	e := singleBackendEnv(t, func(cfg *spec.Config) {
		cfg.Backends[0].Sticky = &spec.Sticky{Mode: spec.StickyPrefix, Cookie: "JSESSIONID"}
	}, s)
	r := get(t, e.feURL(0, "/"))
	sc := r.Header.Values("Set-Cookie")
	if len(sc) != 2 || sc[0] != "JSESSIONID=web1~abc123; Path=/; HttpOnly" || sc[1] != "theme=dark" || r.Header.Get("Cache-Control") != "private" {
		t.Fatalf("prefix response: %v %v", sc, r.Header)
	}
	get(t, e.feURL(0, "/"), "Cookie", "a=1; JSESSIONID=web1~abc123; b=2")
	if got.Load() != "a=1; JSESSIONID=abc123; b=2" {
		t.Fatalf("server saw %q", got.Load())
	}
	e.waitLog(t, " --VR ")
}

func TestStickySource(t *testing.T) {
	a, b := newUpstream(t, "a", nil), newUpstream(t, "b", nil)
	e := singleBackendEnv(t, func(cfg *spec.Config) {
		cfg.Backends[0].Sticky = &spec.Sticky{Mode: spec.StickySource, ExpireMs: 60000, TableSize: 100}
	}, a.server(), b.server())
	first := strings.Fields(get(t, e.feURL(0, "/")).body)[0]
	for range 5 {
		if got := strings.Fields(get(t, e.feURL(0, "/")).body)[0]; got != first {
			t.Fatalf("source persistence broke: %s then %s", first, got)
		}
	}
	e.cmd("set server web/" + first + " state maint")
	moved := strings.Fields(get(t, e.feURL(0, "/")).body)[0]
	if moved == first {
		t.Fatal("persistence to a maint server")
	}
	e.cmd("set server web/" + first + " state ready")
	if got := strings.Fields(get(t, e.feURL(0, "/")).body)[0]; got != moved {
		t.Fatalf("table not updated after redispatch: %s", got)
	}
	if e.srv.rt.Load().backends[0].st.stick.Load().len() != 1 {
		t.Fatal("table size")
	}

	// tcp mode
	pa, pb := tcpServer(t, echoTCP("A")), tcpServer(t, echoTCP("B"))
	cfg := baseConfig(t)
	tb := tcpBackend("t", spec.Server{Name: "A", Address: "127.0.0.1", Port: pa, Weight: 1, State: spec.StateReady}, spec.Server{Name: "B", Address: "127.0.0.1", Port: pb, Weight: 1, State: spec.StateReady})
	tb.Sticky = &spec.Sticky{Mode: spec.StickySource}
	cfg.Backends = []spec.Backend{tb}
	cfg.Frontends = []spec.Frontend{tcpFrontend(t, "t", "t")}
	startEnv(t, cfg)
	var names []string
	for range 4 {
		c, err := net.Dial("tcp", cfg.Frontends[0].Bind)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(c, "x\n")
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		l, _ := bufio.NewReader(c).ReadString('\n')
		c.Close()
		names = append(names, l)
	}
	if names[0] == "" || names[0] != names[1] || names[1] != names[2] || names[2] != names[3] {
		t.Fatalf("tcp source persistence: %q", names)
	}
}

func TestTCPProxyHalfCloseAndLog(t *testing.T) {
	port := tcpServer(t, func(c net.Conn) {
		b, _ := io.ReadAll(c) // until the client half-closes
		io.WriteString(c, "got "+strconv.Itoa(len(b)))
	})
	cfg := baseConfig(t)
	cfg.Backends = []spec.Backend{tcpBackend("t", spec.Server{Name: "t1", Address: "127.0.0.1", Port: port, Weight: 1, State: spec.StateReady})}
	cfg.Frontends = []spec.Frontend{tcpFrontend(t, "tcpfe", "t")}
	e := startEnv(t, cfg)
	c, err := net.Dial("tcp", cfg.Frontends[0].Bind)
	if err != nil {
		t.Fatal(err)
	}
	c.Write(bytes.Repeat([]byte("z"), 70000))
	c.(*net.TCPConn).CloseWrite()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	b, _ := io.ReadAll(c)
	c.Close()
	if string(b) != "got 70000" {
		t.Fatalf("half close: %q", b)
	}
	e.waitLog(t, "] tcpfe t/t1 0/")
	e.waitLog(t, " 9 -- 1/1/")
	if st := e.stat("t", "t1"); st["bin"] != "70000" || st["bout"] != "9" {
		t.Fatalf("bytes %s/%s", st["bin"], st["bout"])
	}

	// Idle timeout closes the session.
	idle := tcpServer(t, func(c net.Conn) { io.Copy(io.Discard, c) })
	e.cfg.Backends[0].Servers[0].Port = idle
	e.cfg.Timeouts.ClientMs, e.cfg.Timeouts.ServerMs = 150, 150
	if err := e.reload("h2"); err != nil {
		t.Fatal(err)
	}
	c, _ = net.Dial("tcp", cfg.Frontends[0].Bind)
	defer c.Close()
	io.WriteString(c, "x")
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	if _, err := c.Read(make([]byte, 1)); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("idle session not closed: %v after %s", err, time.Since(start))
	}
	if !waitFor(t, 3*time.Second, func() bool { l := e.logs(); return strings.Contains(l, " cD ") || strings.Contains(l, " sD ") }) {
		t.Fatalf("idle timeout not logged:\n%s", e.logs())
	}
}

func selfSignedPEM(t *testing.T) []byte {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "Other CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageCertSign, IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
