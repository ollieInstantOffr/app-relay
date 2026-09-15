package balancer

import (
	"encoding/json"
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/instantoffr/relay/internal/balancer/spec"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/render"
	"github.com/instantoffr/relay/internal/render/haproxy"
)

var update = flag.Bool("update", false, "rewrite golden files")

// settings and sampleSnapshot are copied from internal/render/haproxy's tests
// so testdata/full.json and haproxy's testdata/full.cfg describe the same config.
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

// renderCfg renders snap and decodes balancer.json.
func renderCfg(t *testing.T, snap *model.Snapshot) (*spec.Config, error) {
	t.Helper()
	files, err := Render(snap, env())
	if files == nil {
		t.Fatalf("no files (err %v)", err)
	}
	if len(files) != 1 || files[ConfigFile] == "" {
		t.Fatalf("want only %s, got %v", ConfigFile, keys(files))
	}
	var cfg spec.Config
	if jerr := json.Unmarshal([]byte(files[ConfigFile]), &cfg); jerr != nil {
		t.Fatalf("invalid JSON: %v", jerr)
	}
	return &cfg, err
}

func mustRender(t *testing.T, snap *model.Snapshot) *spec.Config {
	t.Helper()
	cfg, err := renderCfg(t, snap)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func backendByName(t *testing.T, cfg *spec.Config, name string) spec.Backend {
	t.Helper()
	for _, b := range cfg.Backends {
		if b.Name == name {
			return b
		}
	}
	t.Fatalf("backend %s not rendered", name)
	return spec.Backend{}
}

func frontendByName(t *testing.T, cfg *spec.Config, name string) spec.Frontend {
	t.Helper()
	for _, f := range cfg.Frontends {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("frontend %s not rendered", name)
	return spec.Frontend{}
}

// oneBackend is a minimal snapshot around a single backend.
func oneBackend(b model.Backend) *model.Snapshot {
	if b.ID == "" {
		b.ID = "bx"
	}
	if b.Name == "" {
		b.Name = "bx"
	}
	return &model.Snapshot{Backends: []model.Backend{b}, HAProxy: settings()}
}

// ---------------------------------------------------------------- golden

func TestRenderGolden(t *testing.T) {
	files, err := Render(sampleSnapshot(), env())
	if err != nil {
		t.Fatal(err)
	}
	out := files[ConfigFile]
	golden := filepath.Join("testdata", "full.json")
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
		t.Errorf("render mismatch (go test ./internal/render/balancer -update to accept)\n--- got ---\n%s", out)
	}
}

func TestDeterministicAndFormatting(t *testing.T) {
	a, _ := Render(sampleSnapshot(), env())
	for i := 0; i < 5; i++ {
		b, _ := Render(sampleSnapshot(), env())
		if a[ConfigFile] != b[ConfigFile] {
			t.Fatal("render is not deterministic")
		}
	}
	out := a[ConfigFile]
	if !strings.HasSuffix(out, "}\n") || strings.HasSuffix(out, "\n\n") {
		t.Error("want exactly one trailing newline")
	}
	if !strings.Contains(out, "\n  \"schema\": 1,") {
		t.Error("want 2-space indentation")
	}
	snap := sampleSnapshot()
	snap.Backends[0].HealthCheck.Path = "/health?a=1&b=<x>"
	files, _ := Render(snap, env())
	if !strings.Contains(files[ConfigFile], `"/health?a=1&b=<x>"`) {
		t.Error("HTML characters must not be escaped")
	}
}

// TestParityWithHAProxy cross-checks names, cookies and flags against the
// haproxy.cfg rendered for the same snapshot.
func TestParityWithHAProxy(t *testing.T) {
	snap := sampleSnapshot()
	cfgText, err := haproxy.Render(snap, env())
	if err != nil {
		t.Fatal(err)
	}
	cfg := mustRender(t, snap)
	serverRe := regexp.MustCompile(`(?m)^    server (\S+) \S+(.*)$`)
	var hapServers []string
	for _, m := range serverRe.FindAllStringSubmatch(cfgText, -1) {
		line := m[1]
		opts := " " + m[2] + " "
		if strings.Contains(opts, " check ") {
			line += " check"
		}
		if strings.Contains(opts, " backup ") {
			line += " backup"
		}
		if c := regexp.MustCompile(` cookie (\S+) `).FindStringSubmatch(opts); c != nil {
			line += " cookie=" + c[1]
		}
		if strings.Contains(opts, " disabled ") {
			line += " maint"
		}
		hapServers = append(hapServers, line)
	}
	var ours []string
	for _, b := range cfg.Backends {
		for _, s := range b.Servers {
			line := s.Name
			if s.Check {
				line += " check"
			}
			if s.Backup {
				line += " backup"
			}
			if s.Cookie != "" {
				line += " cookie=" + s.Cookie
			}
			if s.State == spec.StateMaint {
				line += " maint"
			}
			ours = append(ours, line)
		}
	}
	if strings.Join(hapServers, "\n") != strings.Join(ours, "\n") {
		t.Errorf("servers differ\nhaproxy:\n%s\nbalancer:\n%s", strings.Join(hapServers, "\n"), strings.Join(ours, "\n"))
	}
	if n := strings.Count(cfgText, "\nfrontend ") - 1; n != len(cfg.Frontends) { // minus stats
		t.Errorf("frontends: haproxy %d, balancer %d", n, len(cfg.Frontends))
	}
	if n := strings.Count(cfgText, "\nbackend "); n != len(cfg.Backends) {
		t.Errorf("backends: haproxy %d, balancer %d", n, len(cfg.Backends))
	}
	if n := strings.Count(cfgText, "use_backend "); n != len(cfg.Frontends[0].Rules)+len(cfg.Frontends[1].Rules) {
		t.Errorf("rules: haproxy %d", n)
	}
}

// ---------------------------------------------------------------- globals

func TestConfigGlobals(t *testing.T) {
	cfg := mustRender(t, sampleSnapshot())
	if cfg.Schema != spec.SchemaVersion {
		t.Errorf("schema %d", cfg.Schema)
	}
	if cfg.RuntimeSocket != "/run/relay/balancer-runtime.sock" {
		t.Errorf("runtime socket %q", cfg.RuntimeSocket)
	}
	if cfg.MaxConn != 20000 {
		t.Errorf("maxconn %d", cfg.MaxConn)
	}
	if want := (spec.Timeouts{ConnectMs: 5000, ClientMs: 50000, ServerMs: 50000}); cfg.Timeouts != want {
		t.Errorf("timeouts %+v", cfg.Timeouts)
	}
	if want := (spec.CheckDefaults{IntervalMs: 2000, Rise: 2, Fall: 3}); cfg.Check != want {
		t.Errorf("check defaults %+v", cfg.Check)
	}
	if cfg.CAFile != haproxy.CAFile {
		t.Errorf("ca file %q", cfg.CAFile)
	}
	if len(cfg.Notes) != 0 {
		t.Errorf("unexpected notes %q", cfg.Notes)
	}
	if RuntimeSocketPath("") != "/run/relay/balancer-runtime.sock" || RuntimeSocketPath("/x") != "/x/balancer-runtime.sock" {
		t.Error("RuntimeSocketPath")
	}
}

func TestSettingsDefaultsAndNotes(t *testing.T) {
	snap := &model.Snapshot{Backends: []model.Backend{{Meta: model.Meta{ID: "x"}, Name: "x"}}}
	files, err := Render(snap, render.Env{})
	if err != nil {
		t.Fatal(err)
	}
	var cfg spec.Config
	if err := json.Unmarshal([]byte(files[ConfigFile]), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.MaxConn != 20000 || cfg.Timeouts.ConnectMs != 5000 || cfg.Timeouts.ClientMs != 50000 || cfg.Timeouts.ServerMs != 50000 {
		t.Errorf("defaults: %+v", cfg)
	}
	if cfg.Check != (spec.CheckDefaults{IntervalMs: 2000, Rise: 2, Fall: 3}) {
		t.Errorf("check defaults %+v", cfg.Check)
	}
	if cfg.RuntimeSocket != "/run/relay/balancer-runtime.sock" {
		t.Errorf("runtime socket %q", cfg.RuntimeSocket)
	}
	if cfg.Stats != nil {
		t.Error("stats rendered while disabled")
	}
	if cfg.CAFile != "" {
		t.Error("CA file without verified TLS backends")
	}
	// SeamlessReload false → informational note only.
	if len(cfg.Notes) != 1 || !strings.Contains(cfg.Notes[0], "seamless reload") {
		t.Errorf("notes %q", cfg.Notes)
	}
	b := cfg.Backends[0]
	if b.Mode != spec.ModeHTTP || b.Algorithm != spec.AlgoRoundRobin || b.Retries != 3 || b.Check != nil || len(b.Servers) != 0 {
		t.Errorf("default backend %+v", b)
	}
}

func TestEmptySnapshot(t *testing.T) {
	snap := &model.Snapshot{HAProxy: settings()}
	if HasBackends(snap) {
		t.Error("HasBackends on empty snapshot")
	}
	if !HasBackends(sampleSnapshot()) {
		t.Error("HasBackends on sample")
	}
	files, err := Render(snap, env())
	if err != nil {
		t.Fatal(err)
	}
	out := files[ConfigFile]
	for _, want := range []string{`"schema": 1`, `"runtimeSocket": "/run/relay/balancer-runtime.sock"`, `"frontends": []`, `"backends": []`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
}

// ---------------------------------------------------------------- durations

func TestParseDurationMs(t *testing.T) {
	ok := map[string]int64{
		"0": 0, "500": 500, "500ms": 500, "5s": 5000, " 5s ": 5000, "2m": 120000, "1h": 3600000, "1d": 86400000,
		"1us": 1, "999us": 1, "1000us": 1, "1001us": 2, "0us": 0, "24d": 24 * 86400000,
		"2147483647": 2147483647, "2147483s": 2147483000, "000005s": 5000,
	}
	for in, want := range ok {
		got, err := ParseDurationMs(in)
		if err != nil || got != want {
			t.Errorf("%q: got %d, %v want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "5x", "s", "-1s", "1.5s", "5 s", "5S", "25d", "2147483648", "2147484s", "99999999999999999999d", "9999999999999us"} {
		if _, err := ParseDurationMs(in); err == nil {
			t.Errorf("%q: expected error", in)
		}
	}
}

func TestTimeoutErrorsAndNotes(t *testing.T) {
	snap := sampleSnapshot()
	snap.HAProxy.TimeoutClient = "5x"
	if _, err := renderCfg(t, snap); err == nil || !strings.Contains(err.Error(), "timeout client") {
		t.Errorf("want timeout client error, got %v", err)
	}

	snap = sampleSnapshot()
	snap.Backends[0].Timeouts.Queue = "10years"
	if _, err := renderCfg(t, snap); err == nil || !strings.Contains(err.Error(), "backend web-app timeout queue") {
		t.Errorf("want backend timeout error, got %v", err)
	}

	snap = sampleSnapshot()
	snap.HAProxy.CheckInterval = "0s"
	if _, err := renderCfg(t, snap); err == nil || !strings.Contains(err.Error(), "check interval: must be greater than 0") {
		t.Errorf("want check interval error, got %v", err)
	}

	snap = sampleSnapshot()
	snap.Backends[2].HealthCheck.Interval = "0"
	if _, err := renderCfg(t, snap); err == nil || !strings.Contains(err.Error(), "backend minio check interval") {
		t.Errorf("want backend interval error, got %v", err)
	}

	snap = sampleSnapshot()
	snap.HAProxy.TimeoutServer = "0"
	cfg, err := renderCfg(t, snap)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timeouts.ServerMs != 0 || len(cfg.Notes) != 1 || !strings.Contains(cfg.Notes[0], "timeout server is 0") {
		t.Errorf("zero timeout: %d notes %q", cfg.Timeouts.ServerMs, cfg.Notes)
	}
}

// ---------------------------------------------------------------- stats

func TestStats(t *testing.T) {
	cfg := mustRender(t, sampleSnapshot())
	want := &spec.Stats{Bind: "127.0.0.1:8404", Prometheus: true, Access: []spec.IPRule{
		{Allow: false, CIDR: "10.8.0.99"},
		{Allow: true, CIDR: "10.8.0.0/24"},
		{Allow: true, CIDR: "127.0.0.1"},
		{Allow: false, CIDR: "all"},
	}}
	if !reflect.DeepEqual(cfg.Stats, want) {
		t.Errorf("stats %+v", cfg.Stats)
	}

	snap := sampleSnapshot()
	snap.HAProxy.Prometheus = false
	snap.HAProxy.StatsAccessList = "missing"
	snap.HAProxy.StatsBind = "*:8404"
	cfg = mustRender(t, snap)
	if cfg.Stats == nil || cfg.Stats.Prometheus || cfg.Stats.Access != nil || cfg.Stats.Bind != ":8404" {
		t.Errorf("stats %+v", cfg.Stats)
	}

	snap = sampleSnapshot()
	snap.HAProxy.StatsBind = ""
	if cfg := mustRender(t, snap); cfg.Stats != nil {
		t.Error("stats without bind")
	}
	snap = sampleSnapshot()
	snap.HAProxy.StatsEnabled = false
	if cfg := mustRender(t, snap); cfg.Stats != nil {
		t.Error("stats while disabled")
	}

	snap = sampleSnapshot()
	snap.AccessLists[0].Rules = append(snap.AccessLists[0].Rules, model.IPRule{Action: "allow", CIDR: "bogus"})
	if _, err := renderCfg(t, snap); err == nil || !strings.Contains(err.Error(), "stats access list admin-vpn") {
		t.Errorf("want invalid CIDR error, got %v", err)
	}
}

// haproxyAllows evaluates haproxy.AccessRules output for one client address.
func haproxyAllows(t *testing.T, lines []string, ip netip.Addr) bool {
	t.Helper()
	acls := map[string]netip.Prefix{}
	for _, l := range lines {
		f := strings.Fields(l)
		switch {
		case f[0] == "acl":
			acls[f[1]] = parsePrefix(t, f[3])
		case len(f) >= 2 && f[0] == "http-request" && f[1] == "deny":
			match := true
			if len(f) > 2 {
				if f[2] != "if" {
					t.Fatalf("unexpected line %q", l)
				}
				for _, term := range f[3:] {
					name := strings.TrimPrefix(term, "!")
					p, ok := acls[name]
					if !ok {
						t.Fatalf("unknown acl %s", name)
					}
					m := p.Contains(ip)
					if term != name {
						m = !m
					}
					match = match && m
				}
			}
			if match {
				return false
			}
		default:
			t.Fatalf("unexpected line %q", l)
		}
	}
	return true
}

// specAllows evaluates Stats.Access (first match wins, unmatched allowed).
func specAllows(t *testing.T, rules []spec.IPRule, ip netip.Addr) bool {
	t.Helper()
	for _, r := range rules {
		if r.CIDR == "all" || parsePrefix(t, r.CIDR).Contains(ip) {
			return r.Allow
		}
	}
	return true
}

func parsePrefix(t *testing.T, s string) netip.Prefix {
	t.Helper()
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatal(err)
	}
	return netip.PrefixFrom(a, a.BitLen())
}

func TestAccessRulesEquivalence(t *testing.T) {
	R := func(action, cidr string) model.IPRule { return model.IPRule{Action: action, CIDR: cidr} }
	sets := [][]model.IPRule{
		nil,
		{R("allow", "all")},
		{R("deny", "all")},
		{R("allow", "10.0.0.0/8")},
		{R("deny", "10.0.0.0/8")},
		{R("allow", "10.0.0.0/8"), R("deny", "all")},
		{R("deny", "1.2.3.4"), R("allow", "all")},
		{R("deny", "10.8.0.99"), R("allow", "10.8.0.0/24"), R("allow", "127.0.0.1")},
		{R("allow", "10.1.2.3"), R("deny", "10.0.0.0/8"), R("allow", "192.168.0.0/16"), R("deny", "192.168.1.0/24")},
		{R("allow", "10.1.2.3"), R("deny", "10.0.0.0/8"), R("deny", "all"), R("allow", "ALL"), R("deny", "10.1.2.3/32")},
		{R("deny", "10.0.0.0/8"), R("allow", "10.1.0.0/16"), R("allow", " all "), R("deny", "192.168.1.1")},
		{R("allow", ""), R("deny", "10.0.0.1"), R("maybe", "10.0.0.2"), R("allow", "10.0.0.0/24")},
		{R("allow", "fd00::/8"), R("deny", "::1"), R("allow", "2001:db8::1")},
		{R("deny", "10.0.0.0/8"), R("deny", "192.168.0.0/16")},
		{R("allow", "10.0.0.5/24"), R("deny", "10.0.1.0/24"), R("allow", "10.0.0.0/8")},
	}
	var ips []netip.Addr
	for _, s := range []string{
		"10.0.0.1", "10.0.0.2", "10.0.0.5", "10.0.1.7", "10.1.2.3", "10.1.2.4", "10.8.0.99", "10.8.0.1", "10.9.0.1",
		"1.2.3.4", "1.2.3.5", "127.0.0.1", "192.168.1.1", "192.168.2.1", "8.8.8.8", "::1", "fd00::5", "2001:db8::1", "2001:db8::2",
	} {
		ips = append(ips, netip.MustParseAddr(s))
	}
	for i, set := range sets {
		lines := haproxy.AccessRules(set)
		rules, err := accessRules(set)
		if err != nil {
			t.Fatalf("set %d: %v", i, err)
		}
		for _, ip := range ips {
			if h, s := haproxyAllows(t, lines, ip), specAllows(t, rules, ip); h != s {
				t.Errorf("set %d ip %s: haproxy allows=%v, balancer allows=%v\nhaproxy: %q\nbalancer: %+v", i, ip, h, s, lines, rules)
			}
		}
	}
}

// ---------------------------------------------------------------- frontends

func TestConditions(t *testing.T) {
	cases := []struct {
		in   model.Condition
		want spec.Condition
	}{
		{model.Condition{Type: model.CondHost, Value: " App.Home.LAN "}, spec.Condition{Type: "host", Match: "exact", Values: []string{"app.home.lan"}}},
		{model.Condition{Type: model.CondHost, Value: "*.Home.lan", Negate: true}, spec.Condition{Type: "host", Match: "suffix", Values: []string{".home.lan"}, Negate: true}},
		{model.Condition{Type: model.CondHost, Value: ".home.lan"}, spec.Condition{Type: "host", Match: "exact", Values: []string{".home.lan"}}},
		{model.Condition{Type: model.CondSNI, Value: "Redis.home.lan"}, spec.Condition{Type: "sni", Match: "exact", Values: []string{"redis.home.lan"}}},
		{model.Condition{Type: model.CondSNI, Value: "*.home.lan", Negate: true}, spec.Condition{Type: "sni", Match: "suffix", Values: []string{".home.lan"}, Negate: true}},
		{model.Condition{Type: model.CondPathBeg, Value: "/v1"}, spec.Condition{Type: "path_beg", Values: []string{"/v1"}}},
		{model.Condition{Type: model.CondPath, Value: " /exact ", Negate: true}, spec.Condition{Type: "path", Values: []string{"/exact"}, Negate: true}},
		{model.Condition{Type: model.CondPathRegex, Value: `^/api/(a|b) x$`}, spec.Condition{Type: "path_reg", Values: []string{`^/api/(a|b) x$`}}},
		{model.Condition{Type: model.CondHeader, Name: " X-Debug ", Value: ""}, spec.Condition{Type: "header", Name: "X-Debug", Match: "found"}},
		{model.Condition{Type: model.CondHeader, Name: "X-Env", Value: " Staging ", Negate: true}, spec.Condition{Type: "header", Name: "X-Env", Match: "exact", Values: []string{"Staging"}, Negate: true}},
		{model.Condition{Type: model.CondSrc, Value: "10.0.0.0/8, 192.168.1.7 fd00::1/64"}, spec.Condition{Type: "src", Values: []string{"10.0.0.0/8", "192.168.1.7", "fd00::/64"}}},
	}
	for i, c := range cases {
		got, known, err := condition(c.in)
		if err != nil || !known {
			t.Errorf("case %d: known=%v err=%v", i, known, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("case %d: got %+v want %+v", i, got, c.want)
		}
	}
	if _, known, _ := condition(model.Condition{Type: "geo"}); known {
		t.Error("unknown type reported as known")
	}
	if _, _, err := condition(model.Condition{Type: model.CondSrc, Value: "10.0.0.0/8, nas.lan"}); err == nil {
		t.Error("want error for non-IP src")
	}
}

func TestSampleFrontends(t *testing.T) {
	cfg := mustRender(t, sampleSnapshot())
	if len(cfg.Frontends) != 2 || cfg.Frontends[0].Name != "http-in" || cfg.Frontends[1].Name != "tls-passthrough" {
		t.Fatalf("frontends %+v", cfg.Frontends)
	}
	web := cfg.Frontends[0]
	want := spec.Frontend{
		ID: "f1", Name: "http-in", Mode: "http", Bind: "127.0.0.1:10080",
		ForwardFor: true, ForwardForExcept: []string{"127.0.0.0/8"}, Compression: true,
		Rules: []spec.Rule{
			{Backend: "b3", Conditions: []spec.Condition{
				{Type: "host", Match: "exact", Values: []string{"s3.home.lan"}},
				{Type: "path_beg", Values: []string{"/v1"}},
				{Type: "path_reg", Values: []string{`^/api/(a|b) x$`}, Negate: true},
			}},
			{Backend: "b3", Conditions: []spec.Condition{
				{Type: "host", Match: "suffix", Values: []string{".s3.home.lan"}},
				{Type: "header", Name: "X-Debug", Match: "found"},
				{Type: "src", Values: []string{"10.0.0.0/8", "192.168.1.0/24"}},
				{Type: "path", Values: []string{"/exact"}},
				{Type: "header", Name: "X-Env", Match: "exact", Values: []string{"staging"}},
			}},
		},
		DefaultBackend: "b1",
	}
	if !reflect.DeepEqual(web, want) {
		t.Errorf("http-in\ngot  %+v\nwant %+v", web, want)
	}
	tls := cfg.Frontends[1]
	if tls.Mode != "tcp" || tls.Bind != "0.0.0.0:8443" || !tls.AcceptProxy || tls.InspectDelayMs != 5000 ||
		tls.ForwardFor || tls.Compression || tls.DefaultBackend != "b2" || len(tls.Rules) != 1 || tls.Rules[0].Backend != "b4" {
		t.Errorf("tls-passthrough %+v", tls)
	}
}

func TestFrontendFlags(t *testing.T) {
	sni := []model.FrontendRule{{BackendID: "b4", Conditions: []model.Condition{{Type: model.CondSNI, Value: "x.lan"}}}}
	src := []model.FrontendRule{{BackendID: "b4", Conditions: []model.Condition{{Type: model.CondSrc, Value: "10.0.0.1"}}}}
	cases := []struct {
		name string
		f    model.Frontend
		chk  func(spec.Frontend) bool
	}{
		{"http host forwardfor", model.Frontend{Mode: "http", HostID: "h"}, func(f spec.Frontend) bool {
			return f.ForwardFor && reflect.DeepEqual(f.ForwardForExcept, []string{"127.0.0.0/8"})
		}},
		{"http no host", model.Frontend{Mode: "http"}, func(f spec.Frontend) bool { return !f.ForwardFor && f.ForwardForExcept == nil }},
		{"tcp host", model.Frontend{Mode: "tcp", HostID: "h"}, func(f spec.Frontend) bool { return !f.ForwardFor && f.ForwardForExcept == nil }},
		{"empty mode is http", model.Frontend{Mode: "", HostID: "h", Compression: true}, func(f spec.Frontend) bool {
			return f.Mode == "http" && f.ForwardFor && f.Compression
		}},
		{"compression tcp ignored", model.Frontend{Mode: "tcp", Compression: true}, func(f spec.Frontend) bool { return !f.Compression }},
		{"compression off", model.Frontend{Mode: "http"}, func(f spec.Frontend) bool { return !f.Compression }},
		{"inspect delay tcp sni", model.Frontend{Mode: "tcp", Rules: sni}, func(f spec.Frontend) bool { return f.InspectDelayMs == 5000 }},
		{"no inspect delay tcp src", model.Frontend{Mode: "tcp", Rules: src}, func(f spec.Frontend) bool { return f.InspectDelayMs == 0 }},
		{"no inspect delay http sni", model.Frontend{Mode: "http", Rules: sni}, func(f spec.Frontend) bool { return f.InspectDelayMs == 0 }},
		{"inspect delay from rule with missing backend too", model.Frontend{Mode: "tcp", DefaultBackendID: "b2", Rules: []model.FrontendRule{
			{BackendID: "b4", Conditions: []model.Condition{{Type: model.CondSrc, Value: "10.0.0.1"}}},
			{BackendID: "b4"}, // no conditions: skipped
			{BackendID: "b4", Conditions: []model.Condition{{Type: model.CondSNI, Value: "a.lan"}}},
		}}, func(f spec.Frontend) bool { return f.InspectDelayMs == 5000 && len(f.Rules) == 2 }},
		{"bind all interfaces", model.Frontend{Mode: "tcp", Bind: "*:9000"}, func(f spec.Frontend) bool { return f.Bind == ":9000" }},
		{"bind ipv6", model.Frontend{Mode: "tcp", Bind: "[::]:443"}, func(f spec.Frontend) bool { return f.Bind == "[::]:443" }},
		{"accept proxy", model.Frontend{Mode: "http", AcceptProxy: true}, func(f spec.Frontend) bool { return f.AcceptProxy }},
	}
	for _, c := range cases {
		snap := sampleSnapshot()
		f := c.f
		f.Meta = model.Meta{ID: "fx"}
		f.Name = "fx"
		f.Enabled = true
		if f.Bind == "" {
			f.Bind = "127.0.0.1:19000"
		}
		if f.DefaultBackendID == "" {
			f.DefaultBackendID = "b1"
		}
		snap.Frontends = []model.Frontend{f}
		cfg := mustRender(t, snap)
		if len(cfg.Frontends) != 1 || !c.chk(cfg.Frontends[0]) {
			t.Errorf("%s: %+v", c.name, cfg.Frontends)
		}
	}

	snap := sampleSnapshot()
	snap.Frontends[0].Bind = "localhost:80"
	if _, err := renderCfg(t, snap); err == nil || !strings.Contains(err.Error(), "frontend http-in: bind") {
		t.Errorf("want bind error, got %v", err)
	}
}

func TestMissingBackend(t *testing.T) {
	snap := sampleSnapshot()
	snap.Frontends[0].DefaultBackendID = "nope"
	snap.Frontends[1].Rules[0].BackendID = "gone"
	cfg, err := renderCfg(t, snap)
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{`frontend http-in: default backend "nope" does not exist`, `frontend tls-passthrough rule 1: backend "gone" does not exist`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
	if _, hapErr := haproxy.Render(snap, env()); hapErr == nil {
		t.Error("haproxy renders this snapshot; parity expects an error there too")
	}
	// Partial config: the bad references are left out.
	if cfg.Frontends[0].DefaultBackend != "" || len(cfg.Frontends[1].Rules) != 0 || len(cfg.Backends) != 4 {
		t.Errorf("partial config %+v", cfg.Frontends)
	}
	preview := RenderFrontend(snap, &snap.Frontends[0])
	first, rest, _ := strings.Cut(preview, "\n")
	if !strings.HasPrefix(first, "// error: ") || !strings.Contains(first, "does not exist") {
		t.Errorf("preview first line %q", first)
	}
	var f spec.Frontend
	if err := json.Unmarshal([]byte(rest), &f); err != nil || f.Name != "http-in" {
		t.Errorf("preview body: %v\n%s", err, rest)
	}
}

func TestBadRegex(t *testing.T) {
	snap := sampleSnapshot()
	snap.Frontends[0].Rules[1].Conditions = append(snap.Frontends[0].Rules[1].Conditions,
		model.Condition{Type: model.CondPathRegex, Value: `^/(?!admin)`})
	cfg, err := renderCfg(t, snap)
	if err == nil {
		t.Fatal("expected error for PCRE-only regex")
	}
	if msg := err.Error(); !strings.Contains(msg, "frontend http-in rule 2 condition 6") || !strings.Contains(msg, "RE2") || !strings.Contains(msg, "(?!admin)") {
		t.Errorf("error %q", msg)
	}
	if len(cfg.Frontends[0].Rules) != 1 {
		t.Errorf("bad rule should be left out: %+v", cfg.Frontends[0].Rules)
	}
}

func TestUnknownConditionType(t *testing.T) {
	snap := sampleSnapshot()
	fe := &snap.Frontends[0]
	fe.Rules = []model.FrontendRule{
		{BackendID: "b3", Conditions: []model.Condition{{Type: model.CondPath, Value: "/a"}, {Type: "geo", Value: "DE"}}},
		{BackendID: "b3", Conditions: []model.Condition{{Type: model.CondPath, Value: "/b"}, {Type: "geo", Value: "DE", Negate: true}}},
		{BackendID: "b3", Conditions: []model.Condition{{Type: "geo", Value: "DE", Negate: true}}},
	}
	cfg := mustRender(t, snap)
	want := []spec.Rule{
		{Backend: "b3", Conditions: []spec.Condition{{Type: "path", Values: []string{"/b"}}}},
		{Backend: "b3", Conditions: []spec.Condition{{Type: "src", Values: []string{"0.0.0.0/0", "::/0"}}}},
	}
	if !reflect.DeepEqual(cfg.Frontends[0].Rules, want) {
		t.Errorf("rules %+v", cfg.Frontends[0].Rules)
	}
	if len(cfg.Notes) != 1 || !strings.Contains(cfg.Notes[0], "frontend http-in rule 1") {
		t.Errorf("notes %q", cfg.Notes)
	}
}

// ---------------------------------------------------------------- backends

func TestAlgorithms(t *testing.T) {
	cases := []struct{ mode, algo, want string }{
		{"http", "roundrobin", "roundrobin"},
		{"http", "static-rr", "static-rr"},
		{"http", "leastconn", "leastconn"},
		{"tcp", "source", "source"},
		{"http", "uri", "uri"},
		{"tcp", "uri", "roundrobin"},
		{"http", "random", "random"},
		{"tcp", "first", "first"},
		{"http", "", "roundrobin"},
		{"http", "hdr(host)", "roundrobin"},
		{"", "uri", "uri"}, // empty mode is http
	}
	for _, c := range cases {
		cfg := mustRender(t, oneBackend(model.Backend{Mode: c.mode, Algorithm: c.algo}))
		if got := cfg.Backends[0].Algorithm; got != c.want {
			t.Errorf("%s/%s: got %s want %s", c.mode, c.algo, got, c.want)
		}
	}
}

func TestServers(t *testing.T) {
	cfg := mustRender(t, sampleSnapshot())
	web := backendByName(t, cfg, "web-app")
	want := []spec.Server{
		{ID: "s1", Name: "web-app-1", Address: "10.0.0.71", Port: 8080, Weight: 100, Check: true, State: "ready", Cookie: "web-app-1"},
		{ID: "s2", Name: "web-app-2", Address: "10.0.0.72", Port: 8080, Weight: 100, Check: true, State: "drain", Cookie: "web-app-2"},
		{ID: "s3", Name: "web-3", Address: "10.0.0.73", Port: 8080, Weight: 50, Backup: true, Check: true, State: "maint", Cookie: "web-3"},
	}
	if !reflect.DeepEqual(web.Servers, want) {
		t.Errorf("web-app servers\ngot  %+v\nwant %+v", web.Servers, want)
	}
	pg := backendByName(t, cfg, "postgres-ro")
	wantPG := []spec.Server{
		{ID: "p1", Name: "pg-1", Address: "10.0.0.31", Port: 5432, Weight: 100, Check: true, State: "ready"},
		{ID: "p2", Name: "pg-2", Address: "fd00::32", Port: 5432, Weight: 100, State: "ready"},
	}
	if !reflect.DeepEqual(pg.Servers, wantPG) {
		t.Errorf("postgres servers %+v", pg.Servers)
	}
	if r := backendByName(t, cfg, "redis").Servers[0]; r.Name != "redis-1" || r.Weight != 1 {
		t.Errorf("redis server %+v", r)
	}

	cfg = mustRender(t, oneBackend(model.Backend{
		Name: "x", HealthCheck: model.HealthCheck{Type: "none"},
		Servers: []model.Server{
			{Address: " [fd00::1] ", Port: 80, Check: true, State: "bogus"},
			{Name: "  ", Address: "host.lan", Port: 81, Weight: 0, Role: "backup"},
		},
	}))
	got := cfg.Backends[0].Servers
	wantX := []spec.Server{
		{Name: "x-1", Address: "fd00::1", Port: 80, Weight: 100, State: "ready"},
		{Name: "x-2", Address: "host.lan", Port: 81, Weight: 100, Backup: true, State: "ready"},
	}
	if !reflect.DeepEqual(got, wantX) {
		t.Errorf("servers without checks\ngot  %+v\nwant %+v", got, wantX)
	}

	if _, err := renderCfg(t, oneBackend(model.Backend{Servers: []model.Server{{Address: "a", Port: 1, Weight: 300}}})); err == nil {
		t.Error("want weight error")
	}
}

func TestHealthChecks(t *testing.T) {
	cases := []struct {
		name string
		hc   model.HealthCheck
		want *spec.HealthCheck
	}{
		{"none", model.HealthCheck{Type: "none"}, nil},
		{"empty", model.HealthCheck{}, nil},
		{"tcp", model.HealthCheck{Type: "tcp"}, &spec.HealthCheck{Type: "tcp", IntervalMs: 2000, Rise: 2, Fall: 3}},
		{"unknown is tcp", model.HealthCheck{Type: "smtp"}, &spec.HealthCheck{Type: "tcp", IntervalMs: 2000, Rise: 2, Fall: 3}},
		{"http defaults", model.HealthCheck{Type: "http"}, &spec.HealthCheck{Type: "http", Method: "GET", Path: "/", Expect: "2xx", IntervalMs: 2000, Rise: 2, Fall: 3}},
		{"http full", model.HealthCheck{Type: "http", Method: " head ", Path: " /live ", Host: " app.lan ", ExpectStatus: "204", Interval: "500ms", Rise: 4, Fall: 5},
			&spec.HealthCheck{Type: "http", Method: "HEAD", Path: "/live", Host: "app.lan", Expect: "204", IntervalMs: 500, Rise: 4, Fall: 5}},
		{"pgsql", model.HealthCheck{Type: "pgsql", Rise: 1, Fall: 2}, &spec.HealthCheck{Type: "pgsql", User: "relay", IntervalMs: 2000, Rise: 1, Fall: 2}},
		{"mysql", model.HealthCheck{Type: "mysql", Interval: "1m"}, &spec.HealthCheck{Type: "mysql", IntervalMs: 60000, Rise: 2, Fall: 3}},
		{"redis", model.HealthCheck{Type: "redis"}, &spec.HealthCheck{Type: "redis", IntervalMs: 2000, Rise: 2, Fall: 3}},
	}
	for _, c := range cases {
		cfg := mustRender(t, oneBackend(model.Backend{HealthCheck: c.hc}))
		if got := cfg.Backends[0].Check; !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %+v want %+v", c.name, got, c.want)
		}
	}

	// Inherited interval/rise/fall follow the settings.
	snap := oneBackend(model.Backend{HealthCheck: model.HealthCheck{Type: "tcp"}})
	snap.HAProxy.CheckInterval, snap.HAProxy.Rise, snap.HAProxy.Fall = "3s", 7, 8
	if c := mustRender(t, snap).Backends[0].Check; c.IntervalMs != 3000 || c.Rise != 7 || c.Fall != 8 {
		t.Errorf("inherited %+v", c)
	}
}

func TestExpectStatus(t *testing.T) {
	// Same inputs as haproxy's TestExpectStatus: status N → "N", rstatus ^N → "Nxx".
	for in, want := range map[string]string{
		"": "2xx", "200": "200", "3xx": "3xx", "3XX": "3xx", "200-399": "200-399",
		"200,204": "200,204", "200-299,404": "200-299,404", "bogus": "2xx", "600": "2xx", " 204 ": "204",
	} {
		if got := expectStatus(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestSticky(t *testing.T) {
	cases := []struct {
		name string
		mode string
		st   model.Sticky
		want *spec.Sticky
		srv  string // server cookie
	}{
		{"disabled", "http", model.Sticky{Mode: "insert", CookieName: "X"}, nil, ""},
		{"insert default cookie", "http", model.Sticky{Enabled: true, Mode: "insert"}, &spec.Sticky{Mode: "insert", Cookie: "SRVID"}, "bx-1"},
		{"insert custom", "http", model.Sticky{Enabled: true, Mode: "insert", CookieName: "APP"}, &spec.Sticky{Mode: "insert", Cookie: "APP"}, "bx-1"},
		{"unknown mode is insert", "http", model.Sticky{Enabled: true, Mode: "weird"}, &spec.Sticky{Mode: "insert", Cookie: "SRVID"}, "bx-1"},
		{"prefix", "http", model.Sticky{Enabled: true, Mode: "prefix", CookieName: "JSESSIONID"}, &spec.Sticky{Mode: "prefix", Cookie: "JSESSIONID"}, "bx-1"},
		{"source", "http", model.Sticky{Enabled: true, Mode: "source", CookieName: "IGNORED"}, &spec.Sticky{Mode: "source", ExpireMs: 1800000, TableSize: 200000}, ""},
		{"tcp insert becomes source", "tcp", model.Sticky{Enabled: true, Mode: "insert"}, &spec.Sticky{Mode: "source", ExpireMs: 1800000, TableSize: 200000}, ""},
	}
	for _, c := range cases {
		cfg := mustRender(t, oneBackend(model.Backend{Mode: c.mode, Sticky: c.st, Servers: []model.Server{{Address: "10.0.0.1", Port: 80}}}))
		b := cfg.Backends[0]
		if !reflect.DeepEqual(b.Sticky, c.want) || b.Servers[0].Cookie != c.srv {
			t.Errorf("%s: sticky %+v cookie %q", c.name, b.Sticky, b.Servers[0].Cookie)
		}
	}
}

func TestBackendFlags(t *testing.T) {
	cfg := mustRender(t, sampleSnapshot())
	web := backendByName(t, cfg, "web-app")
	if !web.ForwardFor || web.SendProxy || !web.TLS || !web.TLSVerify || web.Retries != 3 || !web.Redispatch ||
		web.Timeouts != (spec.Timeouts{ConnectMs: 3000, ServerMs: 30000, QueueMs: 10000}) {
		t.Errorf("web-app %+v", web)
	}
	pg := backendByName(t, cfg, "postgres-ro")
	if pg.ForwardFor || !pg.SendProxy || pg.TLS || !pg.Redispatch || pg.Timeouts != (spec.Timeouts{}) {
		t.Errorf("postgres-ro %+v", pg)
	}
	if m := backendByName(t, cfg, "minio"); m.Redispatch {
		t.Error("redispatch with a single server")
	}

	cases := []struct {
		name string
		b    model.Backend
		chk  func(spec.Backend) bool
	}{
		{"forwardfor tcp ignored", model.Backend{Mode: "tcp", ForwardClientIP: true}, func(b spec.Backend) bool { return !b.ForwardFor }},
		{"tls no verify", model.Backend{TLSReencrypt: true}, func(b spec.Backend) bool { return b.TLS && !b.TLSVerify }},
		{"verify needs tls", model.Backend{TLSVerify: true}, func(b spec.Backend) bool { return !b.TLS && !b.TLSVerify }},
		{"retries default", model.Backend{}, func(b spec.Backend) bool { return b.Retries == 3 }},
		{"retries explicit", model.Backend{Retries: 7}, func(b spec.Backend) bool { return b.Retries == 7 }},
		{"retries 1", model.Backend{Retries: 1}, func(b spec.Backend) bool { return b.Retries == 1 }},
		{"redispatch two servers", model.Backend{Servers: []model.Server{{Address: "a", Port: 1}, {Address: "b", Port: 1}}}, func(b spec.Backend) bool { return b.Redispatch }},
		{"no redispatch", model.Backend{Servers: []model.Server{{Address: "a", Port: 1}}}, func(b spec.Backend) bool { return !b.Redispatch }},
		{"timeouts", model.Backend{Timeouts: model.BackendTimeouts{Connect: " 250ms ", Queue: "1m"}}, func(b spec.Backend) bool {
			return b.Timeouts == spec.Timeouts{ConnectMs: 250, QueueMs: 60000}
		}},
	}
	for _, c := range cases {
		cfg := mustRender(t, oneBackend(c.b))
		if !c.chk(cfg.Backends[0]) {
			t.Errorf("%s: %+v", c.name, cfg.Backends[0])
		}
		if (cfg.CAFile != "") != cfg.Backends[0].TLSVerify {
			t.Errorf("%s: CA file %q with tlsVerify=%v", c.name, cfg.CAFile, cfg.Backends[0].TLSVerify)
		}
	}
}

// ---------------------------------------------------------------- previews

func TestRenderBackendPreview(t *testing.T) {
	snap := sampleSnapshot()
	out := RenderBackend(snap, &snap.Backends[0])
	var b spec.Backend
	if err := json.Unmarshal([]byte(out), &b); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	cfg := mustRender(t, snap)
	if !reflect.DeepEqual(b, cfg.Backends[0]) {
		t.Errorf("preview differs from rendered backend\n%s", out)
	}
	if !strings.HasSuffix(out, "}\n") || !strings.HasPrefix(out, "{\n  \"id\": \"b1\"") {
		t.Errorf("formatting:\n%s", out)
	}

	bad := snap.Backends[0]
	bad.Timeouts.Server = "30 s"
	out = RenderBackend(snap, &bad)
	first, rest, _ := strings.Cut(out, "\n")
	if !strings.HasPrefix(first, "// error: backend web-app timeout server") {
		t.Errorf("first line %q", first)
	}
	if err := json.Unmarshal([]byte(rest), &b); err != nil {
		t.Errorf("body not JSON: %v", err)
	}
}

func TestRenderFrontendPreview(t *testing.T) {
	snap := sampleSnapshot()
	// Previews render disabled frontends too.
	out := RenderFrontend(snap, &snap.Frontends[2])
	if strings.HasPrefix(out, "//") {
		t.Fatalf("unexpected error:\n%s", out)
	}
	var f spec.Frontend
	if err := json.Unmarshal([]byte(out), &f); err != nil {
		t.Fatal(err)
	}
	if f.Name != "disabled-fe" || f.DefaultBackend != "b1" {
		t.Errorf("preview %+v", f)
	}
	out = RenderFrontend(snap, &snap.Frontends[0])
	cfg := mustRender(t, snap)
	if err := json.Unmarshal([]byte(out), &f); err != nil || !reflect.DeepEqual(f, cfg.Frontends[0]) {
		t.Errorf("preview differs from rendered frontend (%v)\n%s", err, out)
	}
}
