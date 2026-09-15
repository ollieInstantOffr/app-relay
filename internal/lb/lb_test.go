package lb

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// testdata/showstat.csv is real `show stat` output from haproxy 3.0 with
// servers UP, DOWN (L4TOUT, L4CON), MAINT (disabled) and DRAIN.
func loadLive(t *testing.T) *liveStats {
	t.Helper()
	b, err := os.ReadFile("testdata/showstat.csv")
	if err != nil {
		t.Fatal(err)
	}
	ls, err := parseLive(string(b), time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	return ls
}

func TestParseLive(t *testing.T) {
	ls := loadLive(t)
	if len(ls.Frontends) != 1 || ls.Frontends[0].Name != "http-in" {
		t.Fatalf("frontends (stats must be excluded): %+v", ls.Frontends)
	}
	if len(ls.Backends) != 2 {
		t.Fatalf("backends: %+v", ls.Backends)
	}
	web := ls.Servers["web"]
	if len(web) != 5 {
		t.Fatalf("web servers: %d", len(web))
	}
	want := []string{"UP", "DOWN", "DOWN", "MAINT", "DRAIN"}
	for i, s := range web {
		if s.Status != want[i] {
			t.Errorf("server %s status %s want %s", s.Server, s.Status, want[i])
		}
	}
	if web[1].CheckStatus != "L4TOUT" || web[1].CheckFall != 2 || web[1].Addr != "10.255.255.1:80" {
		t.Errorf("web-2 parsed wrong: %+v", web[1])
	}
	if !web[2].Backup {
		t.Error("web-3 should be backup")
	}
	if d := checkDetail(web[1]); d != "L4 timeout · 2/2 failed · 7s" {
		t.Errorf("detail %q", d)
	}
	if d := checkDetail(web[2]); d != "connection refused · 2/2 failed · 8s" {
		t.Errorf("detail %q", d)
	}
	if d := checkDetail(web[4]); d != "draining · idle" {
		t.Errorf("detail %q", d)
	}
}

func TestParseInfo(t *testing.T) {
	b, _ := os.ReadFile("testdata/showinfo.txt")
	info := parseInfo(string(b))
	if info["Pid"] != "7" || !strings.HasPrefix(info["Version"], "3.0") {
		t.Errorf("info: %v", info)
	}
}

func TestNormStatus(t *testing.T) {
	for in, want := range map[string]string{
		"UP": "UP", "UP 1/3": "UP", "DOWN 1/2": "DOWN", "no check": "NOCHECK", "MAINT (via web/x)": "MAINT",
		"MAINT (resolution)": "MAINT", "DRAIN": "DRAIN", "NOLB": "NOLB", "OPEN": "OPEN",
	} {
		if got := normStatus(in); got != want {
			t.Errorf("%q → %q want %q", in, got, want)
		}
	}
}

func TestBuildStats(t *testing.T) {
	ls := loadLive(t)
	ls.Servers["web"][0].Lbtot = 30
	ls.Servers["web"][4].Lbtot = 10
	backends := []model.Backend{{
		Meta: model.Meta{ID: "b-web"}, Name: "web",
		Servers: []model.Server{
			{ID: "s1", Name: "web-1"}, {ID: "s2", Name: "web-2"}, {ID: "s3", Name: "web-3", Role: "backup"},
			{ID: "s4", Name: "web-4"}, {ID: "s5", Name: "web-5"},
		},
	}}
	st := buildStats(ls, backends, []model.Frontend{{Meta: model.Meta{ID: "f1"}, Name: "http-in"}}, func(string, string) int64 { return 42 })
	if !st.Running || len(st.Backends) != 2 {
		t.Fatalf("stats: %+v", st)
	}
	web := st.Backends[0]
	if web.ID != "b-web" || web.Status != "DEGRADED" {
		t.Errorf("web backend: id=%q status=%q", web.ID, web.Status)
	}
	if web.Servers[0].ID != "s1" || web.Servers[0].SharePct != 75 || web.Servers[4].SharePct != 25 {
		t.Errorf("share/id: %+v", web.Servers)
	}
	if web.Servers[0].RespP95Ms != 42 || web.Servers[1].DownSec != 7 {
		t.Errorf("p95/downsec: %+v", web.Servers[:2])
	}
	if pg := st.Backends[1]; pg.Status != "DOWN" || pg.ID != "" {
		t.Errorf("pg backend: %+v", pg)
	}
	if st.Frontends[0].ID != "f1" {
		t.Errorf("frontends: %+v", st.Frontends)
	}
}

func TestPercentile(t *testing.T) {
	vals := []int64{}
	for i := int64(1); i <= 100; i++ {
		vals = append(vals, i)
	}
	if p := percentile(vals, 95); p != 95 {
		t.Errorf("p95 = %d", p)
	}
	if percentile(nil, 95) != 0 {
		t.Error("empty")
	}
}

func TestSamplerRecordAndTransitions(t *testing.T) {
	s := newSampler(&Service{})
	ls := loadLive(t)
	s.record(ls)
	if tr := s.detectTransitions(ls, ls.At, true); len(tr) != 0 {
		t.Fatalf("first sight must not emit: %+v", tr)
	}
	// web-1 goes down, web-2 comes back.
	ls2 := loadLive(t)
	ls2.At = ls.At.Add(2 * time.Second)
	ls2.Servers["web"][0].Status = "DOWN"
	ls2.Servers["web"][1].Status = "UP"
	ls2.Backends[0].Econ += 5
	s.record(ls2)
	tr := s.detectTransitions(ls2, ls2.At, false)
	if len(tr) != 2 || tr[0].Server != "web-1" || tr[0].To != "down" || tr[1].Server != "web-2" || tr[1].To != "up" {
		t.Fatalf("transitions: %+v", tr)
	}
	pts := s.points["web"]
	if len(pts) != 2 || pts[1].Errors != 5 {
		t.Errorf("points: %+v", pts)
	}
	// During the post-reload quiet period flaps are ignored.
	s.quietUntil = ls2.At.Add(time.Minute)
	ls3 := loadLive(t)
	ls3.At = ls2.At.Add(2 * time.Second)
	if tr := s.detectTransitions(ls3, ls3.At, false); len(tr) != 0 {
		t.Errorf("quiet period emitted: %+v", tr)
	}
}

func TestDelta(t *testing.T) {
	if delta(10, 4) != 6 || delta(3, 10) != 3 {
		t.Error("delta")
	}
}

func TestStatusMatches(t *testing.T) {
	cases := []struct {
		expect string
		code   int
		ok     bool
	}{
		{"", 200, true}, {"", 301, false}, {"200", 200, true}, {"200", 204, false}, {"2xx", 204, true},
		{"3xx", 302, true}, {"200-399", 301, true}, {"200,204", 204, true}, {"200,204", 201, false},
	}
	for _, c := range cases {
		if statusMatches(c.expect, c.code) != c.ok {
			t.Errorf("%q %d", c.expect, c.code)
		}
	}
}

func TestPortInSpec(t *testing.T) {
	if !portInSpec("2456-2458", 2457) || portInSpec("2456-2458", 2459) || !portInSpec("80, 443", 443) || portInSpec("", 80) {
		t.Error("portInSpec")
	}
}

func TestPortOwner(t *testing.T) {
	snap := &model.Snapshot{
		General: store.DefaultGeneral(),
		HAProxy: store.DefaultHAProxy(),
		Frontends: []model.Frontend{
			{Meta: model.Meta{ID: "f1"}, Name: "a", Bind: "127.0.0.1:10080", Enabled: true},
			{Meta: model.Meta{ID: "f2"}, Name: "b", Bind: "0.0.0.0:9000", Enabled: true},
			{Meta: model.Meta{ID: "f3"}, Name: "off", Bind: "127.0.0.1:10081", Enabled: false},
		},
		Streams: []model.Stream{{Name: "valheim", Protocol: "tcp", ListenAddress: "0.0.0.0", ListenPorts: "2456-2458", Enabled: true}},
	}
	cases := []struct {
		self, addr string
		port       int
		want       string
	}{
		{"", "127.0.0.1", 10080, "frontend a"},
		{"f1", "127.0.0.1", 10080, ""},
		{"", "10.0.0.1", 10080, ""},
		{"", "127.0.0.1", 9000, "frontend b"},
		{"", "127.0.0.1", 10081, ""},
		{"", "127.0.0.1", 2457, "stream valheim"},
		{"", "", 443, "nginx (HTTPS)"},
		{"", "127.0.0.1", 8404, "the load balancer stats endpoint"},
		{"", "127.0.0.1", 8181, "the Relay admin UI"},
	}
	for _, c := range cases {
		if got := portOwner(nil, snap, c.self, c.addr, c.port); got != c.want {
			t.Errorf("%s:%d self=%s → %q want %q", c.addr, c.port, c.self, got, c.want)
		}
	}
}

func TestNormalizeBackend(t *testing.T) {
	s := store.DefaultHAProxy()
	prev := &model.Backend{Servers: []model.Server{{ID: "keep", State: "drain"}}}
	b := &model.Backend{Name: "api", Servers: []model.Server{
		{ID: "keep", Address: " 10.0.0.1 ", Port: 80},
		{Address: "10.0.0.2", Port: 80, Name: "api-1"},
		{Address: "10.0.0.3", Port: 80},
	}}
	normalizeBackend(s, prev, b)
	if b.Mode != "http" || b.Algorithm != "roundrobin" || b.HealthCheck.Type != "http" || b.HealthCheck.Path != "/" ||
		b.HealthCheck.Rise != 2 || b.HealthCheck.Fall != 3 || b.Sticky.CookieName != "SRVID" || b.Source != "manual" {
		t.Errorf("defaults: %+v", b)
	}
	if b.Servers[0].State != "drain" || b.Servers[0].Address != "10.0.0.1" || b.Servers[0].Weight != 100 || b.Servers[0].Role != "active" {
		t.Errorf("server 0: %+v", b.Servers[0])
	}
	if b.Servers[0].Name != "api-2" || b.Servers[2].Name != "api-3" || b.Servers[2].ID == "" {
		t.Errorf("names: %q %q %q", b.Servers[0].Name, b.Servers[1].Name, b.Servers[2].Name)
	}
	if err := b.Validate(); err != nil {
		t.Errorf("normalized backend invalid: %v", err)
	}
}

func TestValidateFrontend(t *testing.T) {
	f := &model.Frontend{Name: "stats", Mode: "http", Bind: "localhost:80", Rules: []model.FrontendRule{
		{BackendID: "x", Conditions: []model.Condition{{Type: model.CondSNI, Value: "a.b"}, {Type: model.CondSrc, Value: "10.0.0.0/33"}}},
	}}
	err := f.Validate()
	ve, ok := err.(*model.ValidationError)
	if !ok {
		t.Fatalf("expected validation error, got %v", err)
	}
	for _, k := range []string{"name", "bind", "rules.0.conditions.0.type", "rules.0.conditions.1.value"} {
		if _, ok := ve.Fields[k]; !ok {
			t.Errorf("missing field error %s in %v", k, ve.Fields)
		}
	}
}

func TestCertCovers(t *testing.T) {
	if !certCovers([]string{"*.example.com"}, "app.example.com") || certCovers([]string{"*.example.com"}, "a.b.example.com") ||
		!certCovers([]string{"app.example.com"}, "app.example.com") || certCovers([]string{"*.example.com"}, "example.com") {
		t.Error("certCovers")
	}
}
