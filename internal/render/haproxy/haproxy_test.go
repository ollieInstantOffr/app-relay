package haproxy

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/render"
)

var update = flag.Bool("update", false, "rewrite golden files")

func settings() model.HAProxySettings {
	return model.HAProxySettings{
		TimeoutConnect: "5s", TimeoutClient: "50s", TimeoutServer: "50s", MaxConn: 20000,
		CheckInterval: "2s", Rise: 2, Fall: 3, SeamlessReload: true, StatsEnabled: true,
		StatsBind: "127.0.0.1:8404", StatsAccessList: "al1", Prometheus: true, ExposePortStart: 10080,
	}
}

func sampleSnapshot() *model.Snapshot {
	web := model.Backend{
		Meta: model.Meta{ID: "b1"}, Name: "web-app", Mode: "http", Algorithm: "roundrobin",
		Servers: []model.Server{
			{ID: "s1", Name: "", Address: "10.0.0.71", Port: 8080, Weight: 100, Role: "active", Check: true, State: "ready"},
			{ID: "s2", Name: "", Address: "10.0.0.72", Port: 8080, Weight: 100, Role: "active", Check: true, State: "drain"},
			{ID: "s3", Name: "web-3", Address: "10.0.0.73", Port: 8080, Weight: 50, Role: "backup", Check: true, State: "maint"},
		},
		HealthCheck:     model.HealthCheck{Type: "http", Method: "GET", Path: "/health", ExpectStatus: "200", Host: "app.home.lan", Interval: "5s"},
		Sticky:          model.Sticky{Enabled: true, Mode: "insert", CookieName: "SRVID"},
		ForwardClientIP: true, Retries: 3,
		TLSReencrypt: true, TLSVerify: true,
		Timeouts: model.BackendTimeouts{Connect: "3s", Server: "30s", Queue: "10s"},
	}
	pg := model.Backend{
		Meta: model.Meta{ID: "b2"}, Name: "postgres-ro", Mode: "tcp", Algorithm: "leastconn",
		Servers: []model.Server{
			{ID: "p1", Name: "pg-1", Address: "10.0.0.31", Port: 5432, Weight: 100, Role: "active", Check: true},
			{ID: "p2", Name: "pg-2", Address: "fd00::32", Port: 5432, Weight: 100, Role: "active", Check: false},
		},
		HealthCheck: model.HealthCheck{Type: "pgsql", Rise: 1, Fall: 2},
		Sticky:      model.Sticky{Enabled: true, Mode: "insert"},
		SendProxy:   true,
	}
	minio := model.Backend{
		Meta: model.Meta{ID: "b3"}, Name: "minio", Mode: "http", Algorithm: "source",
		Servers:     []model.Server{{ID: "m1", Name: "minio-1", Address: "minio.lan", Port: 9000, Weight: 10, Check: true}},
		HealthCheck: model.HealthCheck{Type: "http", Method: "HEAD", Path: "/minio/health/live", ExpectStatus: "2xx"},
	}
	redis := model.Backend{
		Meta: model.Meta{ID: "b4"}, Name: "redis", Mode: "tcp", Algorithm: "first",
		Servers:     []model.Server{{ID: "r1", Address: "10.0.0.50", Port: 6379, Weight: 1, Check: true}},
		HealthCheck: model.HealthCheck{Type: "redis"},
	}
	return &model.Snapshot{
		AccessLists: []model.AccessList{{
			Meta: model.Meta{ID: "al1"}, Name: "admin-vpn",
			Rules: []model.IPRule{
				{Action: "deny", CIDR: "10.8.0.99"},
				{Action: "allow", CIDR: "10.8.0.0/24"},
				{Action: "allow", CIDR: "127.0.0.1"},
			},
		}},
		Backends: []model.Backend{web, pg, minio, redis},
		Frontends: []model.Frontend{
			{
				Meta: model.Meta{ID: "f1"}, Name: "http-in", Mode: "http", Bind: "127.0.0.1:10080", Enabled: true,
				HostID: "h1", Compression: true, DefaultBackendID: "b1",
				Rules: []model.FrontendRule{
					{ID: "r1", BackendID: "b3", Conditions: []model.Condition{
						{Type: model.CondHost, Value: "s3.home.lan"},
						{Type: model.CondPathBeg, Value: "/v1"},
						{Type: model.CondPathRegex, Value: `^/api/(a|b) x$`, Negate: true},
					}},
					{ID: "r2", BackendID: "b3", Conditions: []model.Condition{
						{Type: model.CondHost, Value: "*.s3.home.lan"},
						{Type: model.CondHeader, Name: "X-Debug", Value: ""},
						{Type: model.CondSrc, Value: "10.0.0.0/8, 192.168.1.0/24"},
						{Type: model.CondPath, Value: "/exact"},
						{Type: model.CondHeader, Name: "X-Env", Value: "staging"},
					}},
				},
			},
			{
				Meta: model.Meta{ID: "f2"}, Name: "tls-passthrough", Mode: "tcp", Bind: "0.0.0.0:8443", Enabled: true,
				AcceptProxy: true, DefaultBackendID: "b2",
				Rules: []model.FrontendRule{
					{ID: "r3", BackendID: "b4", Conditions: []model.Condition{{Type: model.CondSNI, Value: "redis.home.lan"}}},
				},
			},
			{Meta: model.Meta{ID: "f3"}, Name: "disabled-fe", Mode: "http", Bind: "127.0.0.1:10081", Enabled: false, DefaultBackendID: "b1"},
		},
		HAProxy: settings(),
	}
}

func env() render.Env { return render.DefaultEnv("/data", "/run/relay", "/var/log/relay") }

func TestRenderGolden(t *testing.T) {
	out, err := Render(sampleSnapshot(), env())
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "full.cfg")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(out), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run go test -update): %v", err)
	}
	if string(want) != out {
		t.Errorf("render mismatch (go test ./internal/render/haproxy -update to accept)\n--- got ---\n%s", out)
	}
	// determinism
	again, _ := Render(sampleSnapshot(), env())
	if again != out {
		t.Error("render is not deterministic")
	}
}

func TestRenderDetails(t *testing.T) {
	out, err := Render(sampleSnapshot(), env())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"stats socket /run/relay/haproxy-runtime.sock mode 660 level admin expose-fd listeners",
		"default-server init-addr libc,none inter 2s rise 2 fall 3",
		"http-request use-service prometheus-exporter if { path /metrics }",
		"http-request deny if acl_rule_1\n",
		"http-request deny if !acl_rule_2 !acl_rule_3",
		"server web-app-1 10.0.0.71:8080 check weight 100 cookie web-app-1 ssl verify required ca-file /etc/ssl/certs/ca-certificates.crt",
		"server web-3 10.0.0.73:8080 check weight 50 backup cookie web-3 ssl verify required ca-file /etc/ssl/certs/ca-certificates.crt disabled",
		"http-check send meth GET uri /health ver HTTP/1.1 hdr Host app.home.lan",
		"http-check expect status 200",
		"http-check expect rstatus ^2",
		"default-server inter 5s rise 2 fall 3",
		"option pgsql-check user relay",
		"default-server inter 2s rise 1 fall 2",
		"stick on src",
		"server pg-2 [fd00::32]:5432 weight 100 send-proxy-v2",
		"option redis-check",
		"server redis-1 10.0.0.50:6379 check weight 1",
		"use_backend minio if r1_host_1 r1_path_beg_2 !r1_path_reg_3",
		"acl r1_path_reg_3 path_reg '^/api/(a|b) x$'",
		"acl r2_host_1 req.hdr(host),host_only -m end -i .s3.home.lan",
		"acl r2_header_2 req.hdr(X-Debug) -m found",
		"acl r2_src_3 src 10.0.0.0/8 192.168.1.0/24",
		"option forwardfor except 127.0.0.0/8",
		"compression algo gzip",
		"tcp-request content accept if { req_ssl_hello_type 1 }",
		"acl r1_sni_1 req.ssl_sni -m str -i redis.home.lan",
		"bind 0.0.0.0:8443 accept-proxy",
		"default_backend postgres-ro",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(out, "disabled-fe") {
		t.Error("disabled frontend rendered")
	}
}

func TestRenderMissingBackend(t *testing.T) {
	snap := sampleSnapshot()
	snap.Frontends[0].DefaultBackendID = "nope"
	if _, err := Render(snap, env()); err == nil {
		t.Fatal("expected error for missing default backend")
	}
	// Previews still render with an inline comment.
	if s := RenderFrontend(snap, &snap.Frontends[0]); !strings.Contains(s, "# error:") {
		t.Errorf("preview should flag error, got\n%s", s)
	}
}

func TestAccessRules(t *testing.T) {
	cases := []struct {
		rules []model.IPRule
		want  []string
	}{
		{nil, nil},
		{[]model.IPRule{{Action: "allow", CIDR: "all"}}, nil},
		{[]model.IPRule{{Action: "deny", CIDR: "all"}}, []string{"http-request deny"}},
		{
			[]model.IPRule{{Action: "allow", CIDR: "10.0.0.0/8"}, {Action: "deny", CIDR: "all"}},
			[]string{"acl acl_rule_1 src 10.0.0.0/8", "http-request deny if !acl_rule_1"},
		},
		{
			[]model.IPRule{{Action: "deny", CIDR: "1.2.3.4"}, {Action: "allow", CIDR: "all"}},
			[]string{"acl acl_rule_1 src 1.2.3.4", "http-request deny if acl_rule_1"},
		},
	}
	for i, c := range cases {
		got := AccessRules(c.rules)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("case %d: got %q want %q", i, got, c.want)
		}
	}
}

func TestExpectStatus(t *testing.T) {
	for in, want := range map[string]string{
		"": "rstatus ^2", "200": "status 200", "3xx": "rstatus ^3", "200-399": "status 200-399",
		"200,204": "status 200,204", "bogus": "rstatus ^2",
	} {
		if got := expectStatus(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestQuoteArg(t *testing.T) {
	for in, want := range map[string]string{
		"/api": "/api", "a b": "'a b'", "#x": "'#x'", `"q"`: `'"q"'`, "${HOME}": "'${HOME}'", "^/v[0-9]+$": "^/v[0-9]+$",
	} {
		if got := quoteArg(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestStatsDisabledAndDefaults(t *testing.T) {
	snap := &model.Snapshot{Backends: []model.Backend{{Meta: model.Meta{ID: "x"}, Name: "x"}}}
	out, err := Render(snap, render.Env{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "frontend stats") {
		t.Error("stats rendered while disabled")
	}
	for _, want := range []string{"maxconn 20000", "timeout connect 5s", "backend x\n    mode http\n    balance roundrobin\n", "stats socket /run/relay/haproxy-runtime.sock mode 660 level admin\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}
