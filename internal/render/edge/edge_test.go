package edge

import (
	"bytes"
	"encoding/json"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/agent"
	edgecfg "github.com/instantoffr/relay/internal/edge"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/render"
)

func boolp(b bool) *bool { return &b }

func timep(t time.Time) *time.Time { return &t }

func testEnv() render.Env {
	env := render.DefaultEnv("/data", "/run/relay", "/var/log/relay")
	env.Modules = map[string]bool{"stream": true, "http_v3": true, "auth_request": true, "ipv6": true}
	return env
}

// richSnapshot mirrors the nginx renderer's rich snapshot (without the custom
// snippet, which Relay Edge rejects) plus cases specific to resolution rules.
func richSnapshot() *model.Snapshot {
	snap := &model.Snapshot{
		General: model.GeneralSettings{HTTPPort: 80, HTTPSPort: 443, HTTP3: true},
		TLS: model.TLSSettings{
			CipherProfile: "intermediate", OCSPStapling: true,
			HSTS: model.HSTSSettings{Enabled: true, MaxAgeSeconds: 15768000, IncludeSubdomains: true},
		},
		DefaultHost: model.DefaultHostSettings{Action: "404"},
	}
	snap.Blocklist.Entries = []model.BlockEntry{{CIDR: "203.0.113.88"}, {CIDR: "198.51.100.0/24"}, {CIDR: "not-an-ip"}, {CIDR: " 203.0.113.88 "}}
	snap.Certificates = []model.Certificate{
		{Meta: model.Meta{ID: "certwild"}, Name: "*.home.lan", Domains: []string{"*.home.lan"}, Provider: model.CertLetsEncrypt, Status: model.CertStatusFailed, NotAfter: timep(time.Now().Add(60 * 24 * time.Hour))},
		{Meta: model.Meta{ID: "certpend"}, Name: "vault.home.lan", Domains: []string{"vault.home.lan"}, Provider: model.CertLetsEncrypt, Status: model.CertStatusPending},
		{Meta: model.Meta{ID: "certcustom"}, Name: "corp", Domains: []string{"corp.example.com"}, Provider: model.CertCustom, Status: model.CertStatusValid, NotAfter: timep(time.Now().Add(24 * time.Hour))},
	}
	snap.AccessLists = []model.AccessList{
		{Meta: model.Meta{ID: "lanonly"}, Name: "lan-only", Rules: []model.IPRule{{Action: "allow", CIDR: "192.168.0.0/16"}, {Action: "allow", CIDR: "10.0.0.0/8"}}},
		{Meta: model.Meta{ID: "family"}, Name: "family", Rules: []model.IPRule{{Action: "allow", CIDR: "192.168.1.0/24"}},
			BasicAuth:  model.BasicAuth{Enabled: true, Realm: "Family only", Users: []model.BasicAuthUser{{Username: "mira", PasswordHash: "$2a$10$abcdefghijklmnopqrstuv"}, {Username: "jonas", PasswordHash: "$2a$10$zyxwvutsrqponmlkjihgfe"}, {Username: "nohash"}}},
			SatisfyAny: true},
		{Meta: model.Meta{ID: "exempt"}, Name: "exempt", Rules: []model.IPRule{
			{Action: "allow", CIDR: "10.1.2.3"}, {Action: "deny", CIDR: "10.0.0.0/8"}, {Action: "allow", CIDR: "bogus"},
			{Action: "deny", CIDR: "all"}, {Action: "allow", CIDR: "ALL"}, {Action: "deny", CIDR: "10.1.2.3/32"},
		}},
		{Meta: model.Meta{ID: "Team-A"}, Name: "team a", BasicAuth: model.BasicAuth{Enabled: true, Users: []model.BasicAuthUser{{Username: "x", PasswordHash: "$2y$10$hash"}}}},
	}
	snap.Hosts = []model.ProxyHost{
		{
			Meta: model.Meta{ID: "hostcloud"}, Domains: []string{"cloud.home.lan"}, Enabled: true,
			Upstream:   model.Upstream{Scheme: "https", Host: "10.0.0.30", Port: 443},
			Websockets: true, BlockExploits: true, CacheAssets: true, AccessListID: "family",
			CertificateID: "certwild", ForceHTTPS: true, HTTP2: true, HSTS: "inherit", UpstreamTLSVerify: false, CipherProfile: "modern",
			Locations: []model.Location{
				{ID: "l1", Path: "/office/", Kind: model.LocationProxy, Upstream: model.Upstream{Scheme: "http", Host: "10.0.0.36", Port: 9980}, Websockets: true, StripPrefix: true,
					Headers: []model.Header{{Name: "X-Forwarded-Prefix", Value: "/office"}, {Name: "Bad Header", Value: "x"}}},
				{ID: "l2", Path: "/.well-known/", Kind: model.LocationSame, NoAuth: true},
				{ID: "l3", Path: "/metrics", Kind: model.LocationDeny},
			},
			ForwardAuth: model.ForwardAuth{Enabled: true, Provider: "authelia", VerifyURL: "http://10.0.0.5:9091/api/verify?rd=https://auth.home.lan", SignInURL: "https://auth.home.lan/", PassRemoteUser: true, PassRemoteGroups: true, SkipWellKnown: true},
			RateLimit:   model.RateLimit{Enabled: true, RequestsPerSecond: 30, Burst: 60, ExemptAccessListID: "lanonly"},
			GeoBlock:    model.GeoBlock{Enabled: true, AllowCountries: []string{"de", "US"}},
			NoIndex:     true, MaxBodySize: "10g", ProxyReadTimeout: 300, ProxySendTimeout: 300,
		},
		{
			Meta: model.Meta{ID: "hostgrafana"}, Domains: []string{"grafana.home.lan"}, Enabled: true,
			Upstream:      model.Upstream{Scheme: "http", Host: "10.0.0.21", Port: 3000, Path: "/grafana/"},
			CertificateID: "certwild", HTTP2: true, HTTP3: boolp(false), HSTS: "off", AccessListID: "lanonly",
			GeoBlock: model.GeoBlock{Enabled: true}, // no countries: no note
		},
		{
			Meta: model.Meta{ID: "hostvault"}, Domains: []string{"vault.home.lan"}, Enabled: true,
			Upstream: model.Upstream{Scheme: "http", Host: "10.0.0.40", Port: 8200}, CertificateID: "certpend", ForceHTTPS: true,
		},
		{Meta: model.Meta{ID: "hostold"}, Domains: []string{"old-wiki.home.lan"}, Enabled: false, Upstream: model.Upstream{Scheme: "http", Host: "10.0.0.9", Port: 80}, CustomNginx: "ignored when disabled;"},
		{Meta: model.Meta{ID: "hostapp"}, Domains: []string{"home.lan"}, Enabled: true, Upstream: model.Upstream{Scheme: "http", Host: "127.0.0.1", Port: 10080, BackendID: "web"}},
		{
			Meta: model.Meta{ID: "hostlab"}, Domains: []string{"LAB.home.lan", " "}, Enabled: true,
			Upstream:     model.Upstream{Scheme: "HTTPS", Host: "[fd00::5]", Path: "base/"},
			AccessListID: "family", CertificateID: "certmissing", HSTS: "on", UpstreamTLSVerify: true,
			MaxBodySize: "0",
			Locations: []model.Location{
				{ID: "a", Path: "api", Kind: model.LocationProxy, Upstream: model.Upstream{Scheme: "http", Host: "10.0.0.2", Port: 8080}, StripPrefix: true, AccessListID: "gone"},
				{ID: "b", Path: "/api", Kind: model.LocationDeny},
				{ID: "c", Path: acmePath, Kind: model.LocationDeny},
				{ID: "d", Path: "/", Kind: model.LocationSame, StripPrefix: true, Websockets: true, AccessListID: "lanonly", Cache: true},
				{ID: "e", Path: "/private/", Kind: model.LocationSame, Headers: []model.Header{{Name: "X-Multi", Value: "a\nb&c"}, {Name: "X-Empty", Value: ""}}},
				{ID: "f", Path: "  ", Kind: model.LocationDeny},
			},
			ForwardAuth: model.ForwardAuth{Enabled: true, VerifyURL: "  http://auth:9091/verify  ", SkipWellKnown: true},
			RateLimit:   model.RateLimit{Enabled: true, RequestsPerSecond: 5, Burst: -3, ExemptAccessListID: "exempt"},
		},
	}
	snap.Redirects = []model.Redirect{
		{Meta: model.Meta{ID: "r1"}, Domains: []string{"www.example.com"}, To: "https://example.com", Code: 301, KeepPath: true, Enabled: true},
		{Meta: model.Meta{ID: "r2"}, Domains: []string{"wiki.home.lan"}, To: "https://docs.home.lan/wiki/", Code: 302, CertificateID: "certwild", ForceHTTPS: true, Enabled: true},
		{Meta: model.Meta{ID: "r3"}, Domains: []string{"home.lan"}, FromPath: "/old-blog", To: "https://blog.example.com", Code: 308, KeepPath: true, Enabled: true},
		{Meta: model.Meta{ID: "r4"}, Domains: []string{"home.lan", "extra.example.com"}, To: "https://whole.example.com", Enabled: true},
		{Meta: model.Meta{ID: "r5"}, Domains: []string{"home.lan"}, FromPath: "/docs/", To: "  https://docs.example.com/ ", Code: 999, Enabled: true},
		{Meta: model.Meta{ID: "r6"}, Domains: []string{"Wiki.home.lan"}, FromPath: "/a", To: "https://x/", Code: 307, KeepPath: true, Enabled: true},
		{Meta: model.Meta{ID: "r7"}, Domains: []string{"wiki.home.lan"}, FromPath: "", To: "https://ignored.example.com", Enabled: true},
		{Meta: model.Meta{ID: "r8"}, Domains: []string{"pending.example.com", "B.example.com"}, FromPath: "x/", To: "https://p.example.com/", CertificateID: "certpend", ForceHTTPS: true, Enabled: true},
		{Meta: model.Meta{ID: "r9"}, Domains: []string{"off.example.com"}, To: "https://off.example.com", Enabled: false},
		{Meta: model.Meta{ID: "r10"}, Domains: []string{"empty.example.com"}, To: "  ", Enabled: true},
	}
	snap.Backends = []model.Backend{{Meta: model.Meta{ID: "pg"}, Name: "postgres-ro", Mode: "tcp"}}
	snap.Frontends = []model.Frontend{{Meta: model.Meta{ID: "fe"}, Name: "pg-local", Mode: "tcp", Bind: "127.0.0.1:15432", DefaultBackendID: "pg", Enabled: true}}
	snap.Streams = []model.Stream{
		{Meta: model.Meta{ID: "s1"}, Name: "minecraft", Protocol: "tcp", ListenAddress: "0.0.0.0", ListenPorts: "25565", ForwardHost: "10.0.0.50", IdleTimeout: "10m", Enabled: true},
		{Meta: model.Meta{ID: "s2"}, Name: "valheim", Protocol: "udp", ListenAddress: "0.0.0.0", ListenPorts: "2456-2458", ForwardHost: "10.0.0.61", ProxyProtocol: true, Enabled: true},
		{Meta: model.Meta{ID: "s3"}, Name: "dns", Protocol: "both", ListenPorts: "5353-5354", ForwardHost: "pihole.lan", ForwardPorts: "53-54", IdleTimeout: "30", Enabled: true},
		{Meta: model.Meta{ID: "s4"}, Name: "shifted", Protocol: "tcp", ListenPorts: "7000-7001", ForwardHost: "10.0.0.7", ForwardPorts: "8000-8001", IdleTimeout: "bogus", Enabled: true},
		{Meta: model.Meta{ID: "s5"}, Name: "postgres", Protocol: "tcp", ListenAddress: "10.0.0.1", ListenPorts: "5433", BackendID: "pg", Enabled: true},
		{Meta: model.Meta{ID: "s6"}, Name: "off", Protocol: "tcp", ListenPorts: "9999", ForwardHost: "10.0.0.1", Enabled: false},
	}
	return snap
}

// decode parses edge.json strictly against the data plane schema.
func decode(t *testing.T, data string) edgecfg.Config {
	t.Helper()
	var cfg edgecfg.Config
	dec := json.NewDecoder(strings.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		t.Fatalf("edge.json does not match the schema: %v\n%s", err, data)
	}
	return cfg
}

func mustRender(t *testing.T, snap *model.Snapshot, env render.Env) (edgecfg.Config, agent.Files) {
	t.Helper()
	files, err := Render(snap, env)
	if err != nil {
		t.Fatal(err)
	}
	return decode(t, files[ConfigFile]), files
}

func renderRich(t *testing.T) edgecfg.Config {
	t.Helper()
	cfg, _ := mustRender(t, richSnapshot(), testEnv())
	return cfg
}

func findHost(t *testing.T, cfg edgecfg.Config, id string) edgecfg.Host {
	t.Helper()
	for _, h := range cfg.Hosts {
		if h.ID == id {
			return h
		}
	}
	t.Fatalf("host %s not rendered", id)
	return edgecfg.Host{}
}

func equal(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		g, _ := json.MarshalIndent(got, "", "  ")
		w, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("%s:\n got %s\nwant %s", what, g, w)
	}
}

func hasNote(cfg edgecfg.Config, want string) bool {
	return slices.Contains(cfg.Notes, want)
}

var certwildRef = &edgecfg.CertRef{ID: "certwild", CertFile: "/data/certs/certwild/fullchain.pem", KeyFile: "/data/certs/certwild/privkey.pem"}

// ---------------------------------------------------------------- file set, format, determinism

func TestRenderFileSetAndFormat(t *testing.T) {
	files, err := Render(richSnapshot(), testEnv())
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{}
	for k := range files {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	equal(t, "files", keys, []string{"edge.json", "htpasswd/family", "htpasswd/team_a"})

	data := files[ConfigFile]
	if !strings.HasPrefix(data, "{\n  \"schema\": 1,\n") || !strings.HasSuffix(data, "}\n") || strings.HasSuffix(data, "\n\n") {
		t.Errorf("edge.json must be 2-space indented JSON with one trailing newline:\n%s", data)
	}
	if strings.Contains(data, `\u0026`) || strings.Contains(data, `\u003c`) {
		t.Error("edge.json must not HTML-escape")
	}
	cfg := decode(t, data)
	if cfg.Schema != 1 || cfg.HTTPPort != 80 || cfg.HTTPSPort != 443 || !cfg.HTTP3 ||
		cfg.StatusAddr != "127.0.0.1:18081" || cfg.LogDir != "/var/log/relay" || cfg.ACMEWebroot != "/data/acme" ||
		cfg.TLSProfile != "intermediate" || cfg.AssetCacheMB != 0 {
		t.Errorf("globals = %+v", cfg)
	}
}

func TestRenderDeterministic(t *testing.T) {
	a, _ := Render(richSnapshot(), testEnv())
	for i := 0; i < 20; i++ {
		b, _ := Render(richSnapshot(), testEnv())
		if !reflect.DeepEqual(a, b) {
			t.Fatal("render output is not deterministic")
		}
	}
	// Order of entities that are looked up by id must not matter.
	s := richSnapshot()
	slices.Reverse(s.AccessLists)
	slices.Reverse(s.Certificates)
	slices.Reverse(s.Backends)
	b, _ := Render(s, testEnv())
	if a[ConfigFile] != b[ConfigFile] {
		t.Fatal("edge.json depends on the order of access lists / certificates / backends")
	}
}

func TestStatusAddrAndEnvDefaults(t *testing.T) {
	old := StatusAddr
	defer func() { StatusAddr = old }()
	StatusAddr = "127.0.0.1:19999"
	env := testEnv()
	env.LogDir, env.ACMEWebroot = "", ""
	cfg, _ := mustRender(t, richSnapshot(), env)
	if cfg.StatusAddr != "127.0.0.1:19999" || cfg.LogDir != "/var/log/relay" || cfg.ACMEWebroot != "/data/acme" {
		t.Errorf("status/log/acme = %q %q %q", cfg.StatusAddr, cfg.LogDir, cfg.ACMEWebroot)
	}
	env.LogDir, env.ACMEWebroot = "/logs", "/srv/acme"
	cfg, _ = mustRender(t, richSnapshot(), env)
	if cfg.LogDir != "/logs" || cfg.ACMEWebroot != "/srv/acme" {
		t.Errorf("log/acme = %q %q", cfg.LogDir, cfg.ACMEWebroot)
	}
}

func TestRenderEmptySnapshot(t *testing.T) {
	cfg, files := mustRender(t, &model.Snapshot{}, render.DefaultEnv("/data", "/run/relay", "/var/log/relay"))
	if len(files) != 1 || cfg.HTTPPort != 80 || cfg.HTTPSPort != 443 || cfg.HTTP3 || cfg.Default.Action != "close" ||
		cfg.Hosts == nil || cfg.Redirects == nil || cfg.Streams == nil || cfg.Blocklist == nil || cfg.AccessLists == nil || cfg.Notes != nil {
		t.Errorf("empty config = %+v", cfg)
	}
	for _, key := range []string{`"hosts": []`, `"redirects": []`, `"streams": []`, `"blocklist": []`, `"accessLists": {}`} {
		if !strings.Contains(files[ConfigFile], key) {
			t.Errorf("edge.json missing %s", key)
		}
	}
}

// ---------------------------------------------------------------- hosts

func TestHostOrderAndBlocklist(t *testing.T) {
	cfg := renderRich(t)
	ids := []string{}
	for _, h := range cfg.Hosts {
		ids = append(ids, h.ID)
	}
	// Sorted by the raw first domain ("LAB…" sorts before lowercase), disabled hosts omitted.
	equal(t, "host order", ids, []string{"hostlab", "hostcloud", "hostgrafana", "hostapp", "hostvault"})
	equal(t, "blocklist", cfg.Blocklist, []string{"198.51.100.0/24", "203.0.113.88"})
}

func TestTLSHost(t *testing.T) {
	cfg := renderRich(t)
	up := edgecfg.Upstream{Scheme: "https", Host: "10.0.0.30", Port: 443}
	want := edgecfg.Host{
		ID: "hostcloud", Domains: []string{"cloud.home.lan"},
		Cert: certwildRef, ForceHTTPS: true, HTTP2: true, HTTP3: true, CipherProfile: "modern",
		HSTS:          "max-age=15768000; includeSubDomains",
		BlockExploits: true, NoIndex: true, MaxBodyBytes: 10 << 30, ReadTimeoutSec: 300, SendTimeoutSec: 300,
		RateLimit: &edgecfg.RateLimit{RequestsPerSecond: 30, Burst: 60,
			Exempt: []edgecfg.ExemptRule{{CIDR: "192.168.0.0/16", Exempt: true}, {CIDR: "10.0.0.0/8", Exempt: true}}},
		ForwardAuth: &edgecfg.ForwardAuth{VerifyURL: "http://10.0.0.5:9091/api/verify?rd=https://auth.home.lan", SignInURL: "https://auth.home.lan/", PassRemoteUser: true, PassRemoteGroups: true},
		Locations: []edgecfg.Location{
			// user "/.well-known/" (NoAuth) replaces the synthetic SkipWellKnown location
			{Path: "/.well-known/", Kind: "proxy", Upstream: up, Cache: true},
			{Path: "/metrics", Kind: "deny"},
			{Path: "/office/", Kind: "proxy", Upstream: edgecfg.Upstream{Scheme: "http", Host: "10.0.0.36", Port: 9980}, StripPrefix: true, Websockets: true, Cache: true,
				Headers: []edgecfg.Header{{Name: "X-Forwarded-Prefix", Value: "/office"}}, AccessListID: "family", ForwardAuth: true},
			{Path: "/", Kind: "proxy", Upstream: up, Websockets: true, Cache: true, AccessListID: "family", ForwardAuth: true},
		},
	}
	equal(t, "cloud host", findHost(t, cfg, "hostcloud"), want)
}

func TestHTTPOnlyAndPathsHosts(t *testing.T) {
	cfg := renderRich(t)
	equal(t, "grafana host", findHost(t, cfg, "hostgrafana"), edgecfg.Host{
		ID: "hostgrafana", Domains: []string{"grafana.home.lan"},
		Cert: certwildRef, HTTP2: true, CipherProfile: "intermediate", MaxBodyBytes: 1 << 20,
		Locations: []edgecfg.Location{{Path: "/", Kind: "proxy", Upstream: edgecfg.Upstream{Scheme: "http", Host: "10.0.0.21", Port: 3000, Path: "/grafana"}, AccessListID: "lanonly"}},
	})

	// Pending certificate: HTTP only, ForceHTTPS dropped.
	equal(t, "vault host", findHost(t, cfg, "hostvault"), edgecfg.Host{
		ID: "hostvault", Domains: []string{"vault.home.lan"}, MaxBodyBytes: 1 << 20,
		Locations: []edgecfg.Location{{Path: "/", Kind: "proxy", Upstream: edgecfg.Upstream{Scheme: "http", Host: "10.0.0.40", Port: 8200}}},
	})
	if !hasNote(cfg, "host vault.home.lan: certificate vault.home.lan has not been issued yet (pending): serving HTTP only until it is valid") {
		t.Errorf("missing pending certificate note: %q", cfg.Notes)
	}

	labUp := edgecfg.Upstream{Scheme: "https", Host: "fd00::5", Port: 443, Path: "/base"}
	equal(t, "lab host", findHost(t, cfg, "hostlab"), edgecfg.Host{
		ID: "hostlab", Domains: []string{"lab.home.lan"}, MaxBodyBytes: 0, UpstreamTLSVerify: true,
		RateLimit: &edgecfg.RateLimit{RequestsPerSecond: 5, Burst: 0, ExemptDefault: true,
			Exempt: []edgecfg.ExemptRule{{CIDR: "10.1.2.3", Exempt: false}, {CIDR: "10.0.0.0/8", Exempt: false}}},
		ForwardAuth: &edgecfg.ForwardAuth{VerifyURL: "http://auth:9091/verify"},
		Locations: []edgecfg.Location{
			// synthetic: host upstream + list, never forward auth
			{Path: "/.well-known/", Kind: "proxy", Upstream: labUp, AccessListID: "family"},
			{Path: "/private/", Kind: "proxy", Upstream: labUp, AccessListID: "family", ForwardAuth: true,
				Headers: []edgecfg.Header{{Name: "X-Multi", Value: "a b&c"}, {Name: "X-Empty", Value: ""}}},
			// "api" normalised, first definition wins over the "/api" deny, missing list denies all
			{Path: "/api", Kind: "proxy", Upstream: edgecfg.Upstream{Scheme: "http", Host: "10.0.0.2", Port: 8080}, StripPrefix: true, DenyAll: true, ForwardAuth: true},
			// user "/" keeps its own websockets flag and never strips
			{Path: "/", Kind: "proxy", Upstream: labUp, Websockets: true, Cache: true, AccessListID: "lanonly", ForwardAuth: true},
		},
	})
	if !hasNote(cfg, "host LAB.home.lan: certificate certmissing does not exist: serving HTTP only until it is valid") {
		t.Errorf("missing certificate note: %q", cfg.Notes)
	}

	equal(t, "home.lan path redirects", findHost(t, cfg, "hostapp").PathRedirects, []edgecfg.PathRedirect{
		{From: "/old-blog", To: "https://blog.example.com", Code: 308, KeepPath: true},
		{From: "/docs", To: "https://docs.example.com/", Code: 301},
	})
}

func TestHostVariants(t *testing.T) {
	s := richSnapshot()
	s.Hosts = []model.ProxyHost{{Meta: model.Meta{ID: "h"}, Domains: []string{"h.lan"}, Enabled: true, Upstream: model.Upstream{Host: "10.0.0.1"}, CertificateID: "certcustom"}}
	host := func(t *testing.T, mutate func(*model.Snapshot, *model.ProxyHost)) edgecfg.Host {
		t.Helper()
		c := richSnapshot()
		c.Hosts = append([]model.ProxyHost(nil), s.Hosts...)
		mutate(c, &c.Hosts[0])
		cfg, _ := mustRender(t, c, testEnv())
		return findHost(t, cfg, "h")
	}

	t.Run("defaults", func(t *testing.T) {
		h := host(t, func(*model.Snapshot, *model.ProxyHost) {})
		if h.Locations[0].Upstream != (edgecfg.Upstream{Scheme: "http", Host: "10.0.0.1", Port: 80}) {
			t.Errorf("upstream = %+v", h.Locations[0].Upstream)
		}
		if h.Cert == nil || !h.Cert.OCSPStapling || !h.HTTP3 || h.HSTS != "max-age=15768000; includeSubDomains" || h.RateLimit != nil || h.ForwardAuth != nil {
			t.Errorf("host = %+v", h)
		}
	})
	t.Run("ocsp off", func(t *testing.T) {
		if h := host(t, func(s *model.Snapshot, _ *model.ProxyHost) { s.TLS.OCSPStapling = false }); h.Cert.OCSPStapling {
			t.Error("OCSP stapling needs the TLS setting")
		}
	})
	t.Run("hsts", func(t *testing.T) {
		for _, c := range []struct {
			global   model.HSTSSettings
			mode     string
			expected string
		}{
			{model.HSTSSettings{Enabled: false}, "inherit", ""},
			{model.HSTSSettings{Enabled: false}, "on", "max-age=15768000"},
			{model.HSTSSettings{Enabled: true, MaxAgeSeconds: 60, IncludeSubdomains: true, Preload: true}, "", "max-age=60; includeSubDomains; preload"},
			{model.HSTSSettings{Enabled: true}, "off", ""},
		} {
			h := host(t, func(s *model.Snapshot, h *model.ProxyHost) { s.TLS.HSTS, h.HSTS = c.global, c.mode })
			if h.HSTS != c.expected {
				t.Errorf("hsts(%+v, %q) = %q, want %q", c.global, c.mode, h.HSTS, c.expected)
			}
		}
	})
	t.Run("cipher profiles", func(t *testing.T) {
		for _, c := range []struct{ global, host, want, wantGlobal string }{
			{"old", "", "old", "old"},
			{"old", "modern", "modern", "old"},
			{"bogus", "weird", "intermediate", "intermediate"},
			{"modern", "weird", "modern", "modern"},
		} {
			cs := richSnapshot()
			cs.Hosts = append([]model.ProxyHost(nil), s.Hosts...)
			cs.TLS.CipherProfile, cs.Hosts[0].CipherProfile = c.global, c.host
			cfg, _ := mustRender(t, cs, testEnv())
			if got := findHost(t, cfg, "h").CipherProfile; got != c.want || cfg.TLSProfile != c.wantGlobal {
				t.Errorf("profile(%q,%q) = %q/%q, want %q/%q", c.global, c.host, got, cfg.TLSProfile, c.want, c.wantGlobal)
			}
		}
	})
	t.Run("http3", func(t *testing.T) {
		if h := host(t, func(s *model.Snapshot, h *model.ProxyHost) { s.General.HTTP3 = false; h.HTTP3 = boolp(true) }); !h.HTTP3 {
			t.Error("host HTTP/3 override must win")
		}
		if h := host(t, func(_ *model.Snapshot, h *model.ProxyHost) { h.HTTP3 = boolp(false) }); h.HTTP3 {
			t.Error("host HTTP/3 off must win")
		}
		env := testEnv()
		delete(env.Modules, "http_v3")
		cfg, _ := mustRender(t, richSnapshot(), env)
		if cfg.HTTP3 || cfg.Default.HTTP3 || findHost(t, cfg, "hostcloud").HTTP3 {
			t.Error("HTTP/3 needs the http_v3 module")
		}
		// Only hosts with a usable certificate open the QUIC listener.
		c := richSnapshot()
		for i := range c.Hosts {
			if c.Hosts[i].CertificateID == "certwild" {
				c.Hosts[i].CertificateID = "certpend"
			}
		}
		cfg, _ = mustRender(t, c, testEnv())
		if cfg.HTTP3 || cfg.Default.HTTP3 {
			t.Error("HTTP/3 listener without any TLS host")
		}
	})
	t.Run("max body", func(t *testing.T) {
		for in, want := range map[string]int64{"": 1 << 20, " 0 ": 0, "512": 512, "8k": 8 << 10, "2M": 2 << 20, "1G": 1 << 30, "10x": 1 << 20, "1.5m": 1 << 20} {
			if h := host(t, func(_ *model.Snapshot, h *model.ProxyHost) { h.MaxBodySize = in }); h.MaxBodyBytes != want {
				t.Errorf("max body %q = %d, want %d", in, h.MaxBodyBytes, want)
			}
		}
	})
	t.Run("timeouts and rate limit", func(t *testing.T) {
		h := host(t, func(_ *model.Snapshot, h *model.ProxyHost) {
			h.ProxyReadTimeout, h.ProxySendTimeout = -1, 5
			h.RateLimit = model.RateLimit{Enabled: true, RequestsPerSecond: 0, Burst: 5}
		})
		if h.ReadTimeoutSec != 0 || h.SendTimeoutSec != 5 || h.RateLimit != nil {
			t.Errorf("host = %+v", h)
		}
		h = host(t, func(_ *model.Snapshot, h *model.ProxyHost) {
			h.RateLimit = model.RateLimit{Enabled: true, RequestsPerSecond: 2, Burst: 4, ExemptAccessListID: "gone"}
		})
		equal(t, "rate limit with missing exempt list", h.RateLimit, &edgecfg.RateLimit{RequestsPerSecond: 2, Burst: 4})
	})
	t.Run("forward auth off without verify url", func(t *testing.T) {
		h := host(t, func(_ *model.Snapshot, h *model.ProxyHost) {
			h.ForwardAuth = model.ForwardAuth{Enabled: true, VerifyURL: "  ", SkipWellKnown: true}
		})
		// the synthetic well-known location exists even though forward auth is inactive
		if h.ForwardAuth != nil || len(h.Locations) != 2 || h.Locations[0].Path != "/.well-known/" || h.Locations[0].ForwardAuth || h.Locations[1].ForwardAuth {
			t.Errorf("host = %+v", h)
		}
	})
	t.Run("upstream tls verify needs an https upstream", func(t *testing.T) {
		if h := host(t, func(_ *model.Snapshot, h *model.ProxyHost) { h.UpstreamTLSVerify = true }); h.UpstreamTLSVerify {
			t.Error("verify without https upstream")
		}
		h := host(t, func(_ *model.Snapshot, h *model.ProxyHost) {
			h.UpstreamTLSVerify = true
			h.Locations = []model.Location{{Path: "/x", Kind: model.LocationProxy, Upstream: model.Upstream{Scheme: "https", Host: "a", Port: 1}}}
		})
		if !h.UpstreamTLSVerify {
			t.Error("verify with a proxy location using https")
		}
	})
	t.Run("cache and websockets", func(t *testing.T) {
		h := host(t, func(_ *model.Snapshot, h *model.ProxyHost) {
			h.Websockets = true
			h.Locations = []model.Location{
				{ID: "default", Path: "/", Kind: model.LocationSame, Websockets: false},
				{ID: "x", Path: "/x/", Kind: model.LocationDeny, Cache: true, Websockets: true, AccessListID: "family"},
				{ID: "y", Path: "/y/", Kind: model.LocationSame, Cache: true},
			}
		})
		equal(t, "locations", h.Locations, []edgecfg.Location{
			{Path: "/x/", Kind: "deny"},
			{Path: "/y/", Kind: "proxy", Upstream: edgecfg.Upstream{Scheme: "http", Host: "10.0.0.1", Port: 80}, Cache: true},
			// a user "/" location with id "default" takes the host flag (nginx parity)
			{Path: "/", Kind: "proxy", Upstream: edgecfg.Upstream{Scheme: "http", Host: "10.0.0.1", Port: 80}, Websockets: true},
		})
	})
}

// ---------------------------------------------------------------- access lists

func TestAccessListsReferencedOnly(t *testing.T) {
	cfg := renderRich(t)
	equal(t, "access lists", cfg.AccessLists, map[string]edgecfg.AccessList{
		"family": {Name: "family", Rules: []edgecfg.IPRule{{Allow: true, CIDR: "192.168.1.0/24"}, {Allow: false, CIDR: "all"}},
			BasicAuth: &edgecfg.BasicAuth{Realm: "Family only", UsersFile: "htpasswd/family"}, SatisfyAny: true},
		"lanonly": {Name: "lan-only", Rules: []edgecfg.IPRule{{Allow: true, CIDR: "192.168.0.0/16"}, {Allow: true, CIDR: "10.0.0.0/8"}, {Allow: false, CIDR: "all"}}},
	})
}

func TestAccessListRules(t *testing.T) {
	rule := func(action, cidr string) model.IPRule { return model.IPRule{Action: action, CIDR: cidr} }
	basic := model.BasicAuth{Enabled: true}
	for _, c := range []struct {
		name string
		in   model.AccessList
		want edgecfg.AccessList
	}{
		{"stop after all, no implicit deny", model.AccessList{Meta: model.Meta{ID: "o"}, Rules: []model.IPRule{rule("deny", "10.0.0.1"), rule("allow", "10.0.0.0/8"), rule("allow", "nope"), rule("allow", "All"), rule("deny", "1.2.3.4")}, BasicAuth: basic, SatisfyAny: true},
			edgecfg.AccessList{Rules: []edgecfg.IPRule{{CIDR: "10.0.0.1"}, {Allow: true, CIDR: "10.0.0.0/8"}, {Allow: true, CIDR: "all"}}, BasicAuth: &edgecfg.BasicAuth{Realm: "Restricted", UsersFile: "htpasswd/o"}, SatisfyAny: true}},
		{"deny only: no implicit deny", model.AccessList{Meta: model.Meta{ID: "d"}, Rules: []model.IPRule{rule("deny", " 1.2.3.4 "), rule("whatever", "5.6.7.8")}},
			edgecfg.AccessList{Rules: []edgecfg.IPRule{{CIDR: "1.2.3.4"}, {CIDR: "5.6.7.8"}}}},
		{"only invalid rules", model.AccessList{Meta: model.Meta{ID: "i"}, Rules: []model.IPRule{rule("allow", "x")}},
			edgecfg.AccessList{Rules: []edgecfg.IPRule{}}},
		{"basic only: never satisfy any", model.AccessList{Meta: model.Meta{ID: "Team-A"}, BasicAuth: model.BasicAuth{Enabled: true, Realm: "R"}, SatisfyAny: true},
			edgecfg.AccessList{Rules: []edgecfg.IPRule{}, BasicAuth: &edgecfg.BasicAuth{Realm: "R", UsersFile: "htpasswd/team_a"}}},
		{"rules without basic: never satisfy any", model.AccessList{Meta: model.Meta{ID: "n"}, Rules: []model.IPRule{rule("allow", "::1")}, SatisfyAny: true},
			edgecfg.AccessList{Rules: []edgecfg.IPRule{{Allow: true, CIDR: "::1"}, {CIDR: "all"}}}},
	} {
		equal(t, c.name, accessList(&c.in), c.want)
	}
}

func TestLocationAccessResolution(t *testing.T) {
	s := richSnapshot()
	s.Hosts = []model.ProxyHost{{
		Meta: model.Meta{ID: "h"}, Domains: []string{"h.lan"}, Enabled: true, Upstream: model.Upstream{Host: "10.0.0.1", Port: 1},
		AccessListID: "gone",
		ForwardAuth:  model.ForwardAuth{Enabled: true, VerifyURL: "http://fa"},
		Locations: []model.Location{
			{Path: "/noauth/", Kind: model.LocationSame, NoAuth: true, AccessListID: "family"},
			{Path: "/own/", Kind: model.LocationSame, AccessListID: "lanonly"},
		},
	}}
	cfg, _ := mustRender(t, s, testEnv())
	up := edgecfg.Upstream{Scheme: "http", Host: "10.0.0.1", Port: 1}
	equal(t, "locations", findHost(t, cfg, "h").Locations, []edgecfg.Location{
		{Path: "/noauth/", Kind: "proxy", Upstream: up},
		{Path: "/own/", Kind: "proxy", Upstream: up, AccessListID: "lanonly", ForwardAuth: true},
		{Path: "/", Kind: "proxy", Upstream: up, DenyAll: true, ForwardAuth: true},
	})
	if _, ok := cfg.AccessLists["family"]; ok {
		t.Error("lists referenced only by NoAuth locations must not be rendered")
	}
}

func TestHtpasswd(t *testing.T) {
	_, files := mustRender(t, richSnapshot(), testEnv())
	if f := files["htpasswd/family"]; f != "# family\njonas:$2a$10$zyxwvutsrqponmlkjihgfe\nmira:$2a$10$abcdefghijklmnopqrstuv\n" {
		t.Errorf("htpasswd = %q", f)
	}
	// Same pattern internal/apply uses to redact hashes.
	redact := regexp.MustCompile(`(?m)^([^#:\n][^:\n]*):\S+$`)
	if got := redact.ReplaceAllString(files["htpasswd/family"], "$1:<bcrypt hash>"); strings.Contains(got, "$2a$") {
		t.Errorf("redaction pattern does not match: %q", got)
	}
	var b bytes.Buffer
	b.WriteString(htpasswd(&model.AccessList{Name: "a\nb", BasicAuth: model.BasicAuth{Users: []model.BasicAuthUser{{Username: "bad:name", PasswordHash: "h"}, {Username: " ok ", PasswordHash: " h "}}}}))
	if b.String() != "# a b\nok:h\n" {
		t.Errorf("htpasswd = %q", b.String())
	}
}

func TestRenderTunnel(t *testing.T) {
	plain := renderRich(t)
	if plain.Tunnel != nil {
		t.Errorf("tunnel ingress without published hosts: %+v", plain.Tunnel)
	}
	snap := richSnapshot()
	for i := range snap.Hosts {
		if snap.Hosts[i].ID == "hostcloud" {
			snap.Hosts[i].TunnelGatewayID = "gw1"
		}
	}
	for i := range snap.Streams {
		switch snap.Streams[i].ID {
		case "s1", "s2", "s3":
			snap.Streams[i].TunnelGatewayID = "gw1"
		}
	}
	cfg, _ := mustRender(t, snap, testEnv())
	equal(t, "ingress", cfg.Tunnel, &edgecfg.TunnelIngress{HTTPSocket: "/run/relay/tunnel/edge-http.sock", HTTPSSocket: "/run/relay/tunnel/edge-https.sock"})
	if !findHost(t, cfg, "hostcloud").Tunnel || findHost(t, cfg, "hostgrafana").Tunnel {
		t.Error("host tunnel flags")
	}
	socks := map[string][]edgecfg.TunnelSocket{}
	for _, s := range cfg.Streams {
		socks[s.ID] = s.TunnelSockets
	}
	equal(t, "minecraft", socks["s1"], []edgecfg.TunnelSocket{{Port: 25565, Socket: "/run/relay/tunnel/edge-stream-25565.sock"}})
	if len(socks["s2"]) != 0 {
		t.Errorf("udp stream got tunnel sockets: %+v", socks["s2"])
	}
	for id, s := range socks {
		for _, ts := range s {
			if ts.Port == 0 || ts.Socket == "" {
				t.Errorf("stream %s: bad socket %+v", id, ts)
			}
		}
	}
}
