package balancer

import (
	"bufio"
	"crypto/tls"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/balancer/spec"
	"github.com/instantoffr/relay/internal/sniff"
)

func TestHTTPRules(t *testing.T) {
	names := []string{"host", "suffix", "pathbeg", "path", "regex", "hdrfound", "hdrexact", "src", "negsrc", "def", "never", "multi"}
	cfg := baseConfig(t)
	for _, n := range names {
		u := newUpstream(t, n, nil)
		cfg.Backends = append(cfg.Backends, httpBackend(n, u.server()))
	}
	rule := func(be string, conds ...spec.Condition) spec.Rule { return spec.Rule{Backend: be, Conditions: conds} }
	fe := httpFrontend(t, "fe", "def")
	fe.Rules = []spec.Rule{
		rule("never", spec.Condition{Type: spec.CondSNI, Values: []string{"a.lan"}}), // tcp-only: never matches in http
		rule("host", spec.Condition{Type: spec.CondHost, Match: spec.MatchExact, Values: []string{"app.lan"}}),
		rule("suffix", spec.Condition{Type: spec.CondHost, Match: spec.MatchSuffix, Values: []string{".s3.lan"}}),
		rule("multi",
			spec.Condition{Type: spec.CondPathBeg, Values: []string{"/multi"}},
			spec.Condition{Type: spec.CondHeader, Name: "x-env", Match: spec.MatchExact, Values: []string{"staging"}},
			spec.Condition{Type: spec.CondPathReg, Values: []string{"forbidden"}, Negate: true}),
		rule("pathbeg", spec.Condition{Type: spec.CondPathBeg, Values: []string{"/api/", "/v2/"}}),
		rule("path", spec.Condition{Type: spec.CondPath, Values: []string{"/exact"}}),
		rule("regex", spec.Condition{Type: spec.CondPathReg, Values: []string{`^/img/[0-9]+\.png$`}}),
		rule("hdrfound", spec.Condition{Type: spec.CondHeader, Name: "X-Debug", Match: spec.MatchFound}),
		rule("hdrexact", spec.Condition{Type: spec.CondHeader, Name: "X-Tenant", Match: spec.MatchExact, Values: []string{"acme"}}),
		rule("src", spec.Condition{Type: spec.CondPathBeg, Values: []string{"/src"}}, spec.Condition{Type: spec.CondSrc, Values: []string{"127.0.0.0/8", "::1"}}),
		rule("negsrc", spec.Condition{Type: spec.CondPathBeg, Values: []string{"/neg"}}, spec.Condition{Type: spec.CondSrc, Values: []string{"10.0.0.0/8"}, Negate: true}),
	}
	cfg.Frontends = []spec.Frontend{fe}
	e := startEnv(t, cfg)
	tests := []struct {
		path    string
		headers []string
		want    string
	}{
		{"/", []string{"Host", "APP.lan:8080"}, "host"},
		{"/", []string{"Host", "bucket.s3.lan"}, "suffix"},
		{"/", []string{"Host", "s3.lan"}, "def"},
		{"/api/x", nil, "pathbeg"},
		{"/v2/x", nil, "pathbeg"},
		{"/exact", nil, "path"},
		{"/exact?q=1", nil, "path"},
		{"/exact/", nil, "def"},
		{"/img/42.png", nil, "regex"},
		{"/img/%34%32.png", nil, "def"}, // raw path, not decoded
		{"/", []string{"X-Debug", ""}, "hdrfound"},
		{"/", []string{"X-Tenant", "other, acme"}, "hdrexact"},
		{"/", []string{"X-Tenant", "ACME"}, "def"},
		{"/src", nil, "src"},
		{"/neg", nil, "negsrc"},
		{"/multi", []string{"X-Env", "staging"}, "multi"},
		{"/multi/forbidden", []string{"X-Env", "staging"}, "def"},
		{"/multi", []string{"X-Env", "prod"}, "def"},
	}
	for _, tt := range tests {
		r := get(t, e.feURL(0, tt.path), tt.headers...)
		if got := strings.Fields(r.body)[0]; got != tt.want {
			t.Errorf("%s %v → %s (%d), want %s", tt.path, tt.headers, got, r.StatusCode, tt.want)
		}
	}

	// No default backend: 503 with HAProxy's page and <NOSRV> in the log.
	e.cfg.Frontends[0].DefaultBackend = ""
	if err := e.reload("h2"); err != nil {
		t.Fatal(err)
	}
	r := get(t, e.feURL(0, "/nothing"))
	if r.StatusCode != 503 || !strings.Contains(r.body, "No server is available to handle this request.") || r.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("no backend: %d %q", r.StatusCode, r.body)
	}
	if !waitFor(t, time.Second, func() bool { return strings.Contains(e.logs(), " fe fe/<NOSRV> 0/-1/-1/-1/") }) {
		t.Fatalf("log:\n%s", e.logs())
	}
}

// clientHello returns the first flight of a TLS client for serverName.
func clientHello(t *testing.T, serverName string) []byte {
	t.Helper()
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	go tls.Client(c1, &tls.Config{ServerName: serverName, InsecureSkipVerify: true}).Handshake()
	c2.SetReadDeadline(time.Now().Add(2 * time.Second))
	var data []byte
	buf := make([]byte, 4096)
	for {
		n, err := c2.Read(buf)
		data = append(data, buf[:n]...)
		if _, done := sniff.ClientHelloSNI(data); done || err != nil {
			return data
		}
	}
}

// nameServer answers every connection with its name after reading some bytes.
func nameServer(t *testing.T, name string, readFirst bool) spec.Server {
	port := tcpServer(t, func(c net.Conn) {
		if readFirst {
			c.SetReadDeadline(time.Now().Add(2 * time.Second))
			c.Read(make([]byte, 1))
		}
		io.WriteString(c, name+"\n")
	})
	return spec.Server{Name: name, Address: "127.0.0.1", Port: port, Weight: 1, State: spec.StateReady}
}

func TestTCPSNIAndSrcRules(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Backends = []spec.Backend{
		tcpBackend("exact", nameServer(t, "exact", true)),
		tcpBackend("suffix", nameServer(t, "suffix", true)),
		tcpBackend("local", nameServer(t, "local", false)),
		tcpBackend("def", nameServer(t, "def", false)),
	}
	fe := tcpFrontend(t, "tls", "def")
	fe.InspectDelayMs = 300
	fe.Rules = []spec.Rule{
		{Backend: "exact", Conditions: []spec.Condition{{Type: spec.CondSNI, Match: spec.MatchExact, Values: []string{"redis.home.lan"}}}},
		{Backend: "suffix", Conditions: []spec.Condition{{Type: spec.CondSNI, Match: spec.MatchSuffix, Values: []string{".apps.lan"}}}},
		{Backend: "never", Conditions: []spec.Condition{{Type: spec.CondHost, Values: []string{"x"}}}},
		{Backend: "local", Conditions: []spec.Condition{{Type: spec.CondSrc, Values: []string{"127.0.0.1"}}, {Type: spec.CondSNI, Values: []string{"local.lan"}, Negate: true}}},
	}
	fe.Rules[2].Backend = "def"
	cfg.Frontends = []spec.Frontend{fe}
	e := startEnv(t, cfg)
	dial := func(payload []byte) (string, time.Duration) {
		t.Helper()
		start := time.Now()
		c, err := net.Dial("tcp", fe.Bind)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		if payload != nil {
			c.Write(payload)
		}
		line, _ := bufio.NewReader(c).ReadString('\n')
		return strings.TrimSpace(line), time.Since(start)
	}
	if got, _ := dial(clientHello(t, "Redis.Home.Lan")); got != "exact" {
		t.Errorf("exact sni → %q", got)
	}
	if got, _ := dial(clientHello(t, "grafana.apps.lan")); got != "suffix" {
		t.Errorf("suffix sni → %q", got)
	}
	// No SNI match: the src rule (negated sni) picks "local".
	if got, _ := dial(clientHello(t, "other.lan")); got != "local" {
		t.Errorf("src rule → %q", got)
	}
	if got, _ := dial(clientHello(t, "local.lan")); got != "def" {
		t.Errorf("negated sni → %q", got)
	}
	// Non-TLS data is decided at once.
	if got, d := dial([]byte("PING\r\n")); got != "local" || d > 250*time.Millisecond {
		t.Errorf("plain data → %q after %s", got, d)
	}
	// A silent client waits for the inspect delay.
	if got, d := dial(nil); got != "local" || d < 250*time.Millisecond {
		t.Errorf("silent client → %q after %s", got, d)
	}
	_ = e
}
