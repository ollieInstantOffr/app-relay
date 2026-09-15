package apply

import (
	"strings"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/model"
)

func baseSnap() *model.Snapshot {
	s := defaultSnapshot()
	s.Hosts = []model.ProxyHost{{Meta: model.Meta{ID: "h1", UpdatedAt: time.Unix(1, 0)}, Domains: []string{"grafana.home.lan"}, Enabled: true, Upstream: model.Upstream{Scheme: "http", Host: "10.0.0.21", Port: 3000}}}
	s.Backends = []model.Backend{{Meta: model.Meta{ID: "b1"}, Name: "api", Servers: []model.Server{{ID: "s1", Address: "10.0.0.81", Port: 9000}}}}
	s.AccessLists = []model.AccessList{{Meta: model.Meta{ID: "a1"}, Name: "lan-only", Rules: []model.IPRule{{ID: "r1", Action: "allow", CIDR: "192.168.0.0/16"}}}}
	s.Certificates = []model.Certificate{{Meta: model.Meta{ID: "c1"}, Name: "*.home.lan", Status: model.CertStatusValid, NotAfter: ptrTime(time.Unix(100, 0))}}
	return s
}

func ptrTime(t time.Time) *time.Time { return &t }

func TestPendingNothingApplied(t *testing.T) {
	items := computePending(baseSnap(), nil)
	if len(items) != 4 {
		t.Fatalf("items = %+v", items)
	}
	for _, it := range items {
		if it.Action != core.ActionCreated {
			t.Errorf("expected created, got %+v", it)
		}
	}
	// Default settings are not pending on a fresh install.
	if got := computePending(defaultSnapshot(), nil); len(got) != 0 {
		t.Fatalf("fresh install pending = %+v", got)
	}
}

func TestPendingDetectsRealChangesOnly(t *testing.T) {
	live := baseSnap()
	cur := baseSnap()
	// Timestamps, descriptions, cert expiry and last-used times don't count.
	cur.Hosts[0].UpdatedAt = time.Now()
	cur.AccessLists[0].Description = "changed"
	cur.Certificates[0].NotAfter = ptrTime(time.Now())
	now := time.Now()
	cur.AccessLists[0].BasicAuth.Users = nil
	live.AccessLists[0].BasicAuth.Users = nil
	cur.General.InstanceName = "Other"
	cur.Certificates[0].Status = model.CertStatusFailed
	cur.Certificates[0].AutoRenew = !live.Certificates[0].AutoRenew
	if got := computePending(cur, live); len(got) != 0 {
		t.Fatalf("expected no pending, got %+v", got)
	}
	_ = now

	cur.Hosts[0].Websockets = true
	cur.Backends[0].Servers = append(cur.Backends[0].Servers, model.Server{ID: "s2", Address: "10.0.0.82", Port: 9000})
	cur.AccessLists[0].Rules = append(cur.AccessLists[0].Rules, model.IPRule{ID: "r2", Action: "allow", CIDR: "10.0.0.0/8"})
	cur.TLS.CipherProfile = "modern"
	cur.Redirects = []model.Redirect{{Meta: model.Meta{ID: "rd"}, Domains: []string{"www.example.com"}, To: "https://example.com", Enabled: true}}
	live.Streams = []model.Stream{{Meta: model.Meta{ID: "st"}, Name: "valheim"}}

	items := computePending(cur, live)
	var got []string
	for _, it := range items {
		got = append(got, it.Kind+":"+it.Name+":"+it.Action)
	}
	want := "hosts:grafana.home.lan:updated,redirects:www.example.com:created,streams:valheim:deleted,access_lists:lan-only:updated,backends:api:updated,settings:Default TLS settings:updated"
	if strings.Join(got, ",") != want {
		t.Fatalf("items\n got %s\nwant %s", strings.Join(got, ","), want)
	}

	summary := summarize(items, cur, live)
	if !strings.HasPrefix(summary, "grafana.home.lan edited · New redirect www.example.com · stream valheim removed · +3 more") {
		t.Errorf("summary = %q", summary)
	}
	short := []core.PendingItem{items[0], items[4], items[3]}
	if s := summarize(short, cur, live); s != "grafana.home.lan edited · backend api: server added · lan-only: rule added" {
		t.Errorf("short summary = %q", s)
	}
}

func TestProbeTargets(t *testing.T) {
	live := baseSnap()
	cur := baseSnap()
	cur.Hosts = append(cur.Hosts, model.ProxyHost{Meta: model.Meta{ID: "h2"}, Domains: []string{"app.example.com"}, Enabled: true, Upstream: model.Upstream{Host: "127.0.0.1", Port: 10080, BackendID: "b1"}})
	live.Hosts = append(live.Hosts, cur.Hosts[1])
	cur.Hosts = append(cur.Hosts, model.ProxyHost{Meta: model.Meta{ID: "h3"}, Domains: []string{"new.home.lan"}, Enabled: true})
	cur.Hosts[0].Websockets = true
	cur.Backends[0].Algorithm = "leastconn"
	s := &Service{}
	got := s.probeTargets(cur, live, computePending(cur, live))
	if strings.Join(got, ",") != "h1,h2" {
		t.Fatalf("targets = %v", got)
	}
	if s.probeTargets(cur, nil, nil) != nil {
		t.Fatal("no targets without a live version")
	}
}

func TestRedactFiles(t *testing.T) {
	in := map[string]string{"htpasswd/a": "# family\njonas:$2a$10$abc\nmira:$2y$05$x\n", "nginx.conf": "user nginx;\n"}
	out := redactFiles(in)
	if out["htpasswd/a"] != "# family\njonas:<bcrypt hash>\nmira:<bcrypt hash>\n" || out["nginx.conf"] != "user nginx;\n" {
		t.Fatalf("redacted = %q", out)
	}
	if in["htpasswd/a"] == out["htpasswd/a"] {
		t.Fatal("input mutated or not redacted")
	}
}

func TestPendingProxyEngineSwitch(t *testing.T) {
	live := baseSnap()
	live.General.ProxyEngine = "" // versions applied before Relay Edge existed
	cur := baseSnap()
	if got := computePending(cur, live); len(got) != 0 {
		t.Fatalf("empty proxy engine must equal nginx: %+v", got)
	}
	cur.General.ProxyEngine = "edge"
	items := computePending(cur, live)
	if len(items) != 1 || items[0].Kind != "settings" || items[0].ID != model.SettingsGeneral {
		t.Fatalf("items = %+v", items)
	}
	if s := summarize(items, cur, live); s != "Proxy engine: nginx → Relay Edge" {
		t.Fatalf("summary = %q", s)
	}
	cur.General.HTTP3 = true
	if s := summarize(items, cur, live); s != "General settings changed (proxy engine: nginx → Relay Edge)" {
		t.Fatalf("summary = %q", s)
	}
}

func TestPendingLBEngineSwitch(t *testing.T) {
	live := baseSnap()
	live.General.LBEngine = "" // versions applied before Relay Balancer existed
	cur := baseSnap()
	cur.General.LBEngine = "haproxy"
	if got := computePending(cur, live); len(got) != 0 {
		t.Fatalf("empty lb engine must equal haproxy: %+v", got)
	}
	cur.General.LBEngine = "balancer"
	items := computePending(cur, live)
	if len(items) != 1 || items[0].Kind != "settings" || items[0].ID != model.SettingsGeneral {
		t.Fatalf("items = %+v", items)
	}
	if s := summarize(items, cur, live); s != "Load balancer engine: HAProxy → Relay Balancer" {
		t.Fatalf("summary = %q", s)
	}
	cur.General.ProxyEngine = "edge"
	if s := summarize(items, cur, live); s != "Proxy engine: nginx → Relay Edge; load balancer engine: HAProxy → Relay Balancer" {
		t.Fatalf("summary = %q", s)
	}
	cur.General.HTTP3 = true
	if s := summarize(items, cur, live); s != "General settings changed (proxy engine: nginx → Relay Edge; load balancer engine: HAProxy → Relay Balancer)" {
		t.Fatalf("summary = %q", s)
	}
	if settingsLabels[model.SettingsHAProxy] != "Load balancer settings" {
		t.Fatal("settings label")
	}
}
