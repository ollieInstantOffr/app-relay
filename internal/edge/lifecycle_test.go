package edge

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

func TestReloadKeepsInFlightAndSwitchesRouting(t *testing.T) {
	cfg, dir := newConfig(t)
	started, release := make(chan struct{}, 1), make(chan struct{})
	v1 := newUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			started <- struct{}{}
			select {
			case <-release:
			case <-time.After(5 * time.Second):
			}
			io.WriteString(w, "slow-done")
			return
		}
		io.WriteString(w, "v1")
	}))
	v2 := taggedUpstream(t, "v2")
	cfg.Hosts = []Host{proxyHost("h", []string{"h.test"}, v1.ref())}
	e := startEnv(t, cfg, dir)

	type result struct {
		status int
		body   string
		err    error
	}
	done := make(chan result, 1)
	client := e.client("http")
	go func() {
		res, err := client.Get("http://h.test/slow")
		if err != nil {
			done <- result{err: err}
			return
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		done <- result{status: res.StatusCode, body: string(b)}
	}()
	<-started

	next := *cfg
	next.Hosts = []Host{proxyHost("h", []string{"h.test"}, v2.ref()), proxyHost("n", []string{"n.test"}, v2.ref())}
	e.cfg = &next
	if err := e.srv.Reload(e.cfg, dir, "r2"); err != nil {
		t.Fatal(err)
	}
	if res := e.get("http://h.test/fast"); res.body != "v2 /fast" {
		t.Fatalf("after reload: %q", res.body)
	}
	if res := e.get("http://n.test/x"); res.body != "v2 /x" {
		t.Fatalf("new host after reload: %q", res.body)
	}
	close(release)
	if r := <-done; r.err != nil || r.status != 200 || r.body != "slow-done" {
		t.Fatalf("in-flight request across reload: %+v", r)
	}
	if e.srv.Hash() != "r2" || e.srv.metrics.reloadsOK.Load() != 1 {
		t.Fatalf("hash %q reloads %d", e.srv.Hash(), e.srv.metrics.reloadsOK.Load())
	}

	// Moving the HTTP port binds the new one and closes the old one.
	oldPort := next.HTTPPort
	moved := next
	moved.HTTPPort = freePort(t, "tcp")
	e.cfg = &moved
	if err := e.srv.Reload(e.cfg, dir, "r3"); err != nil {
		t.Fatal(err)
	}
	if res := e.get("http://h.test/moved"); res.body != "v2 /moved" {
		t.Fatalf("new port: %q", res.body)
	}
	if !waitFor(t, 5*time.Second, func() bool {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(oldPort), time.Second)
		if err == nil {
			c.Close()
		}
		return err != nil
	}) {
		t.Fatal("old HTTP port still accepting")
	}
}

func TestFailedReloadKeepsOldConfig(t *testing.T) {
	cfg, dir := newConfig(t)
	v1, v2 := taggedUpstream(t, "v1"), taggedUpstream(t, "v2")
	cfg.Hosts = []Host{proxyHost("h", []string{"h.test"}, v1.ref())}
	e := startEnv(t, cfg, dir)

	badCert := *cfg
	h := proxyHost("h", []string{"h.test"}, v2.ref())
	h.Cert = &CertRef{ID: "gone", CertFile: "missing.crt", KeyFile: "missing.key"}
	badCert.Hosts = []Host{h}
	if err := e.srv.Reload(&badCert, dir, "bad-cert"); err == nil || !strings.Contains(err.Error(), "missing.crt") {
		t.Fatalf("bad certificate reload: %v", err)
	}

	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	busyPort := busy.Addr().(*net.TCPAddr).Port
	newHTTP := freePort(t, "tcp")
	portInUse := *cfg
	portInUse.HTTPPort = newHTTP
	portInUse.Hosts = []Host{proxyHost("h", []string{"h.test"}, v2.ref())}
	portInUse.Streams = []Stream{{ID: "s", TCP: true, ListenAddr: "127.0.0.1", ListenLo: busyPort, ListenHi: busyPort, ForwardHost: "127.0.0.1", ForwardLo: 9, ForwardHi: 9}}
	if err := e.srv.Reload(&portInUse, dir, "busy"); err == nil || !strings.Contains(err.Error(), "bind") {
		t.Fatalf("port in use reload: %v", err)
	}
	if res := e.get("http://h.test/x"); res.body != "v1 /x" {
		t.Fatalf("old config must keep serving: %q", res.body)
	}
	if e.srv.Hash() != filepath.Base(dir) || e.srv.metrics.reloadsFailed.Load() != 2 {
		t.Fatalf("hash %q failed reloads %d", e.srv.Hash(), e.srv.metrics.reloadsFailed.Load())
	}
	// The listener opened before the bind failure was released.
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(newHTTP))
	if err != nil {
		t.Fatalf("port opened by the failed reload is still bound: %v", err)
	}
	ln.Close()
}

func readStreamLines(t *testing.T, logDir string, pred func(map[string]string) bool) map[string]string {
	t.Helper()
	var found map[string]string
	if !waitFor(t, 5*time.Second, func() bool {
		data, _ := os.ReadFile(filepath.Join(logDir, "stream-access.log"))
		for _, l := range strings.Split(string(data), "\n") {
			var m map[string]string
			if json.Unmarshal([]byte(l), &m) == nil && pred(m) {
				found = m
				return true
			}
		}
		return false
	}) {
		data, _ := os.ReadFile(filepath.Join(logDir, "stream-access.log"))
		t.Fatalf("no matching stream log line in:\n%s", data)
	}
	return found
}

func TestStreams(t *testing.T) {
	cfg, dir := newConfig(t)
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	headers := make(chan string, 4)
	go func() {
		for {
			c, err := backend.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				line, _ := br.ReadString('\n')
				headers <- line
				io.Copy(c, br)
			}()
		}
	}()
	udpBackend, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udpBackend.Close()
	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := udpBackend.ReadFrom(buf)
			if err != nil {
				return
			}
			udpBackend.WriteTo(buf[:n], addr)
		}
	}()
	tcpPort, udpPort, deadPort := freePort(t, "tcp"), freePort(t, "udp"), freePort(t, "tcp")
	backendPort := backend.Addr().(*net.TCPAddr).Port
	cfg.Streams = []Stream{
		{ID: "tcp1", Name: "echo", TCP: true, ListenAddr: "127.0.0.1", ListenLo: tcpPort, ListenHi: tcpPort, ForwardHost: "localhost", ForwardLo: backendPort, ForwardHi: backendPort, ProxyProtocol: true},
		{ID: "udp1", UDP: true, ListenAddr: "127.0.0.1", ListenLo: udpPort, ListenHi: udpPort, ForwardHost: "127.0.0.1", ForwardLo: udpBackend.LocalAddr().(*net.UDPAddr).Port, ForwardHi: udpBackend.LocalAddr().(*net.UDPAddr).Port, IdleTimeoutMs: 200},
		{ID: "dead", TCP: true, ListenAddr: "127.0.0.1", ListenLo: deadPort, ListenHi: deadPort, ForwardHost: "127.0.0.1", ForwardLo: freePort(t, "tcp"), ForwardHi: 0, ConnectTimeoutMs: 1000},
	}
	cfg.Streams[2].ForwardHi = cfg.Streams[2].ForwardLo
	startEnv(t, cfg, dir)

	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(tcpPort), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte("hello"))
	buf := make([]byte, 5)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("tcp echo: %q %v", buf, err)
	}
	wantHeader := fmt.Sprintf("PROXY TCP4 127.0.0.1 127.0.0.1 %d %d\r\n", c.LocalAddr().(*net.TCPAddr).Port, tcpPort)
	if got := <-headers; got != wantHeader {
		t.Fatalf("PROXY header %q, want %q", got, wantHeader)
	}
	c.Close()
	line := readStreamLines(t, cfg.LogDir, func(m map[string]string) bool { return m["stream_id"] == "tcp1" })
	if line["status"] != "200" || line["protocol"] != "TCP" || line["bytes_received"] != "5" || line["bytes_sent"] != "5" ||
		line["server_port"] != strconv.Itoa(tcpPort) || line["remote_addr"] != "127.0.0.1" || !strings.HasSuffix(line["upstream_addr"], ":"+strconv.Itoa(backendPort)) {
		t.Errorf("tcp stream log: %v", line)
	}

	uc, err := net.Dial("udp", "127.0.0.1:"+strconv.Itoa(udpPort))
	if err != nil {
		t.Fatal(err)
	}
	defer uc.Close()
	uc.SetDeadline(time.Now().Add(5 * time.Second))
	uc.Write([]byte("ping"))
	ubuf := make([]byte, 16)
	n, err := uc.Read(ubuf)
	if err != nil || string(ubuf[:n]) != "ping" {
		t.Fatalf("udp echo: %q %v", ubuf[:n], err)
	}
	line = readStreamLines(t, cfg.LogDir, func(m map[string]string) bool { return m["stream_id"] == "udp1" })
	if line["status"] != "200" || line["protocol"] != "UDP" || line["bytes_received"] != "4" || line["bytes_sent"] != "4" {
		t.Errorf("udp stream log: %v", line)
	}

	dc, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(deadPort), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	dc.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := dc.Read(buf); err == nil {
		t.Fatal("connection to a dead upstream must be closed")
	}
	dc.Close()
	line = readStreamLines(t, cfg.LogDir, func(m map[string]string) bool { return m["stream_id"] == "dead" })
	if line["status"] != "502" || line["upstream_connect_time"] != "" {
		t.Errorf("dead stream log: %v", line)
	}
}

func writeConfig(t *testing.T, dir string, cfg *Config) {
	t.Helper()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, "edge.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheck(t *testing.T) {
	base := func(t *testing.T) (*Config, string) {
		dir := t.TempDir()
		cfg := &Config{Schema: 1, HTTPPort: 8080, HTTPSPort: 8443, StatusAddr: "127.0.0.1:18081", Default: DefaultServer{Action: "close"}}
		h := proxyHost("h", []string{"h.test"}, Upstream{Scheme: "http", Host: "127.0.0.1", Port: 3000})
		h.Cert = writeCert(t, dir, "h", "h.test")
		h.Locations[0].AccessListID = "al"
		cfg.AccessLists = map[string]AccessList{"al": {BasicAuth: &BasicAuth{UsersFile: "htpasswd/al"}}}
		os.MkdirAll(filepath.Join(dir, "htpasswd"), 0o755)
		os.WriteFile(filepath.Join(dir, "htpasswd", "al"), []byte("u:{PLAIN}p\n"), 0o644)
		cfg.Hosts = []Host{h}
		cfg.Streams = []Stream{{ID: "s", TCP: true, UDP: true, ListenLo: 5000, ListenHi: 5010, ForwardHost: "db", ForwardLo: 6000, ForwardHi: 6010}}
		return cfg, dir
	}
	tests := []struct {
		name   string
		mutate func(cfg *Config)
		want   []string // substrings; nil = valid
	}{
		{"valid", func(*Config) {}, nil},
		{"bootstrap without certificates", func(c *Config) {
			*c = Config{Schema: 1, HTTPPort: 80, HTTPSPort: 443, Default: DefaultServer{Action: "close"}}
		}, nil},
		{"tcp and udp on one port", func(c *Config) {
			c.Streams = append(c.Streams, Stream{ID: "u", UDP: true, ListenLo: 8080, ListenHi: 8080, ForwardHost: "x", ForwardLo: 1, ForwardHi: 1})
		}, nil},
		{"schema", func(c *Config) { c.Schema = 2 }, []string{"unsupported schema 2"}},
		{"unknown header variable", func(c *Config) {
			c.Hosts[0].Locations[0].Headers = []Header{{Name: "X-A", Value: "$nope"}}
		}, []string{`unknown "nope" variable`}},
		{"unknown redirect variable", func(c *Config) {
			c.Redirects = []RedirectGroup{{Domains: []string{"r.test"}, Whole: &Redirect{To: "https://$nosuch/", Code: 301}}}
		}, []string{`unknown "nosuch" variable`}},
		{"missing cert", func(c *Config) { c.Hosts[0].Cert.CertFile = "certs/missing.crt" }, []string{"missing.crt"}},
		{"missing htpasswd", func(c *Config) { c.AccessLists["al"].BasicAuth.UsersFile = "htpasswd/none" }, []string{"htpasswd/none"}},
		{"unknown access list", func(c *Config) { c.Hosts[0].Locations[0].AccessListID = "zz" }, []string{`access list "zz" does not exist`}},
		{"stream on HTTP port", func(c *Config) { c.Streams[0].ListenLo, c.Streams[0].ListenHi = 8075, 8085 }, []string{"conflicts with HTTP port 8080"}},
		{"stream on status port", func(c *Config) {
			c.Streams = []Stream{{ID: "x", TCP: true, ListenAddr: "127.0.0.1", ListenLo: 18081, ListenHi: 18081, ForwardHost: "x", ForwardLo: 1, ForwardHi: 1}}
		}, []string{"conflicts with status address"}},
		{"overlapping streams", func(c *Config) {
			c.Streams = append(c.Streams, Stream{ID: "o", Name: "other", UDP: true, ListenLo: 5010, ListenHi: 5020, ForwardHost: "x", ForwardLo: 1, ForwardHi: 1})
		}, []string{`stream "other" port 5010/udp conflicts with stream "s" port 5010/udp`}},
		{"http equals https", func(c *Config) { c.HTTPSPort = 8080 }, []string{"both use port 8080"}},
		{"range size", func(c *Config) { c.Streams[0].ForwardHi = 6001 }, []string{"differ in size"}},
		{"bad blocklist", func(c *Config) { c.Blocklist = []string{"10.0.0.300"} }, []string{"blocklist"}},
		{"several errors at once", func(c *Config) {
			c.Hosts[0].Locations[0].Headers = []Header{{Name: "X-A", Value: "$nope"}}
			c.Hosts[0].Cert.KeyFile = "certs/missing.key"
		}, []string{`unknown "nope" variable`, "missing.key"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, dir := base(t)
			tt.mutate(cfg)
			writeConfig(t, dir, cfg)
			var out bytes.Buffer
			err := RunCLI(context.Background(), []string{"check", dir}, io.Discard, &out)
			if tt.want == nil {
				if err != nil || out.String() != "[notice] configuration is valid\n" {
					t.Fatalf("valid config rejected: %v\n%s", err, out.String())
				}
				return
			}
			if err == nil {
				t.Fatalf("invalid config accepted:\n%s", out.String())
			}
			for _, w := range tt.want {
				if !strings.Contains(out.String(), "[emerg] ") || !strings.Contains(out.String(), w) {
					t.Errorf("output lacks %q:\n%s", w, out.String())
				}
			}
		})
	}
}

func TestRunCLISignalsAndMarkers(t *testing.T) {
	root := t.TempDir()
	logDir := filepath.Join(root, "logs")
	statusAddr := "127.0.0.1:" + strconv.Itoa(freePort(t, "tcp"))
	release := func(name string, cfg *Config) {
		writeConfig(t, filepath.Join(root, "releases", name), cfg)
	}
	good := &Config{Schema: 1, StatusAddr: statusAddr, LogDir: logDir, Default: DefaultServer{Action: "404"}}
	release("r1", good)
	release("r2", good)
	release("r3", &Config{Schema: 1, StatusAddr: statusAddr, Default: DefaultServer{Action: "wat"}})
	current := filepath.Join(root, "current")
	point := func(name string) {
		os.Remove(current)
		if err := os.Symlink(filepath.Join("releases", name), current); err != nil {
			t.Fatal(err)
		}
	}
	point("r1")

	stderr := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		errc <- RunCLI(ctx, []string{"run", "--config", filepath.Join(current, "edge.json")}, io.Discard, stderr)
	}()
	wait := func(marker string) {
		t.Helper()
		if !waitFor(t, 10*time.Second, func() bool { return strings.Contains(stderr.String(), marker) }) {
			t.Fatalf("marker %q not seen in:\n%s", marker, stderr.String())
		}
	}
	wait("[notice] started hash=r1")
	healthz := func() string {
		res, err := http.Get("http://" + statusAddr + "/healthz")
		if err != nil {
			return err.Error()
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return string(b)
	}
	if h := healthz(); !strings.Contains(h, `"hash":"r1"`) {
		t.Fatalf("healthz: %s", h)
	}
	if !waitFor(t, 5*time.Second, func() bool {
		b, _ := os.ReadFile(filepath.Join(logDir, "error.log"))
		return strings.Contains(string(b), "[notice] started hash=r1")
	}) {
		t.Fatal("started marker missing from error.log")
	}

	point("r2")
	syscall.Kill(os.Getpid(), syscall.SIGHUP)
	wait("[notice] config loaded hash=r2")
	point("r3")
	syscall.Kill(os.Getpid(), syscall.SIGHUP)
	wait("[emerg] reload failed: default server: unknown action")
	if h := healthz(); !strings.Contains(h, `"hash":"r2"`) {
		t.Fatalf("failed reload changed the config: %s", h)
	}
	lineRe := regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} \[(debug|info|notice|warn|error|crit|alert|emerg)\] \S`)
	for _, l := range strings.Split(strings.TrimSpace(stderr.String()), "\n") {
		if !lineRe.MatchString(l) {
			t.Errorf("malformed error log line %q", l)
		}
	}
	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(25 * time.Second):
		t.Fatal("run did not stop")
	}
}

func TestStatusEndpoints(t *testing.T) {
	cfg, dir := newConfig(t)
	cfg.StatusAddr = "127.0.0.1:" + strconv.Itoa(freePort(t, "tcp"))
	up := taggedUpstream(t, "a")
	cfg.Hosts = []Host{proxyHost("a", []string{"a.test"}, up.ref())}
	e := startEnv(t, cfg, dir)
	e.get("http://a.test/")
	get := func(path string) (int, string) {
		res, err := http.Get("http://" + cfg.StatusAddr + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	if code, body := get("/healthz"); code != 200 || !strings.Contains(body, `"hash":"`+filepath.Base(dir)+`"`) {
		t.Errorf("healthz: %d %s", code, body)
	}
	stub := regexp.MustCompile(`^Active connections: \d+ \nserver accepts handled requests\n \d+ \d+ [1-9]\d* \nReading: \d+ Writing: \d+ Waiting: \d+ \n$`)
	if code, body := get("/stub_status"); code != 200 || !stub.MatchString(body) {
		t.Errorf("stub_status: %d %q", code, body)
	}
	_, metrics := get("/metrics")
	for _, want := range []string{
		`relay_edge_http_requests_total{host_id="a",code="2xx"} 1`,
		`relay_edge_http_request_duration_seconds_count{host_id="a"} 1`,
		`relay_edge_config_reloads_total{result="success"} 0`,
		`# TYPE relay_edge_connections_active gauge`,
		`relay_edge_asset_cache_bytes 0`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	if code, _ := get("/nope"); code != 404 {
		t.Errorf("unknown status path: %d", code)
	}
}

func TestAccessLogReopenedAfterRotation(t *testing.T) {
	cfg, dir := newConfig(t)
	up := taggedUpstream(t, "a")
	cfg.Hosts = []Host{proxyHost("a", []string{"a.test"}, up.ref())}
	e := startEnv(t, cfg, dir)
	e.get("http://a.test/before")
	waitAccessLine(t, cfg.LogDir, func(m map[string]string) bool { return m["uri"] == "/before" })
	path := filepath.Join(cfg.LogDir, "access.log")
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 5*time.Second, func() bool {
		e.get("http://a.test/after")
		b, _ := os.ReadFile(path)
		return strings.Contains(string(b), `"uri":"/after"`)
	}) {
		t.Fatal("access.log not reopened after rotation")
	}
}

func TestHTTP3(t *testing.T) {
	cfg, dir := newConfig(t)
	cfg.HTTP3 = true
	up := taggedUpstream(t, "h3")
	h := proxyHost("h3", []string{"h3.test"}, up.ref())
	h.Cert = writeCert(t, dir, "h3", "h3.test")
	h.HTTP3 = true
	h.HTTP2 = true
	cfg.Hosts = []Host{h}
	e := startEnv(t, cfg, dir)
	if res := e.get("https://h3.test/"); res.Header.Get("Alt-Svc") == "" {
		t.Fatal("no Alt-Svc over TCP")
	}
	tr := &http3.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		Dial: func(ctx context.Context, _ string, tlsCfg *tls.Config, qcfg *quic.Config) (*quic.Conn, error) {
			return quic.DialAddrEarly(ctx, "127.0.0.1:"+strconv.Itoa(cfg.HTTPSPort), tlsCfg, qcfg)
		},
	}
	defer tr.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://h3.test/quic", nil)
	res, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || string(body) != "h3 /quic" || res.ProtoMajor != 3 {
		t.Fatalf("h3: %d %q %s", res.StatusCode, body, res.Proto)
	}
	line := waitAccessLine(t, cfg.LogDir, func(m map[string]string) bool { return m["uri"] == "/quic" })
	if line["protocol"] != "HTTP/3.0" || line["ssl_protocol"] != "TLSv1.3" || line["host_id"] != "h3" {
		t.Errorf("h3 access line: %v", line)
	}
}
