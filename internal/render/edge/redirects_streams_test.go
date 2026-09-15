package edge

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	edgecfg "github.com/instantoffr/relay/internal/edge"
	"github.com/instantoffr/relay/internal/model"
)

// ---------------------------------------------------------------- redirects

func TestRedirectGroups(t *testing.T) {
	cfg := renderRich(t)
	equal(t, "redirect groups", cfg.Redirects, []edgecfg.RedirectGroup{
		// r6 ("Wiki.home.lan") sorts first and opens the group; r2 is the first
		// whole redirect (r7 ignored) and brings the certificate + ForceHTTPS.
		{Domains: []string{"wiki.home.lan"}, Cert: certwildRef, ForceHTTPS: true,
			Whole: &edgecfg.Redirect{To: "https://docs.home.lan/wiki/", Code: 302},
			Paths: []edgecfg.PathRedirect{{From: "/a", To: "https://x", Code: 307, KeepPath: true}}},
		// unusable certificate: HTTP only, ForceHTTPS dropped, no whole redirect → 404
		{Domains: []string{"b.example.com", "pending.example.com"},
			Paths: []edgecfg.PathRedirect{{From: "/x", To: "https://p.example.com/", Code: 301}}},
		{Domains: []string{"www.example.com"}, Whole: &edgecfg.Redirect{To: "https://example.com", Code: 301, KeepPath: true}},
	})
	if !hasNote(cfg, "redirect pending.example.com, B.example.com: certificate vault.home.lan has not been issued yet (pending): serving HTTP only") {
		t.Errorf("missing redirect certificate note: %q", cfg.Notes)
	}
	for _, g := range cfg.Redirects {
		for _, d := range g.Domains {
			if d == "home.lan" || d == "extra.example.com" || d == "off.example.com" || d == "empty.example.com" {
				t.Errorf("unexpected redirect group for %s", d)
			}
		}
	}
	// The whole-domain redirect for a served domain is not injected.
	for _, pr := range findHost(t, cfg, "hostapp").PathRedirects {
		if strings.Contains(pr.To, "whole") {
			t.Error("whole-domain redirect injected into a host")
		}
	}
}

func TestRedirectCodes(t *testing.T) {
	for in, want := range map[int]int{0: 301, 301: 301, 302: 302, 307: 307, 308: 308, 303: 301} {
		if got := redirectCode(in); got != want {
			t.Errorf("redirectCode(%d) = %d", in, got)
		}
	}
	for in, want := range map[string]bool{"": true, " / ": true, "/a": false, "//": false} {
		if got := isWholeDomain(&model.Redirect{FromPath: in}); got != want {
			t.Errorf("isWholeDomain(%q) = %v", in, got)
		}
	}
	got := pathRedirect(&model.Redirect{FromPath: " docs// ", To: "https://a.example/b/", KeepPath: true, Code: 302})
	equal(t, "path redirect", got, edgecfg.PathRedirect{From: "/docs", To: "https://a.example/b", Code: 302, KeepPath: true})
}

// ---------------------------------------------------------------- default server

func TestDefaultServer(t *testing.T) {
	cfg := renderRich(t)
	placeholder := &edgecfg.CertRef{ID: "_default", CertFile: "/data/certs/_default/fullchain.pem", KeyFile: "/data/certs/_default/privkey.pem"}
	equal(t, "default 404", cfg.Default, edgecfg.DefaultServer{Action: "404", Cert: placeholder, HTTP3: true})

	grafana := &edgecfg.Host{
		ID: "hostgrafana", Domains: []string{"grafana.home.lan"}, MaxBodyBytes: 1 << 20,
		Locations: []edgecfg.Location{{Path: "/", Kind: "proxy", Upstream: edgecfg.Upstream{Scheme: "http", Host: "10.0.0.21", Port: 3000, Path: "/grafana"}, AccessListID: "lanonly"}},
	}
	for _, c := range []struct {
		name string
		dh   model.DefaultHostSettings
		want edgecfg.DefaultServer
		note string
	}{
		{"empty", model.DefaultHostSettings{}, edgecfg.DefaultServer{Action: "close", Cert: placeholder, HTTP3: true}, ""},
		{"unknown action", model.DefaultHostSettings{Action: "teapot"}, edgecfg.DefaultServer{Action: "close", Cert: placeholder, HTTP3: true}, ""},
		{"close with cert", model.DefaultHostSettings{Action: "close", CertificateID: "certwild"}, edgecfg.DefaultServer{Action: "close", Cert: certwildRef, HTTP3: true}, ""},
		{"custom cert staples", model.DefaultHostSettings{Action: "404", CertificateID: "certcustom"},
			edgecfg.DefaultServer{Action: "404", Cert: &edgecfg.CertRef{ID: "certcustom", CertFile: "/data/certs/certcustom/fullchain.pem", KeyFile: "/data/certs/certcustom/privkey.pem", OCSPStapling: true}, HTTP3: true}, ""},
		{"pending cert", model.DefaultHostSettings{Action: "404", CertificateID: "certpend"}, edgecfg.DefaultServer{Action: "404", Cert: placeholder, HTTP3: true},
			"default server: certificate vault.home.lan has not been issued yet (pending): using the self-signed placeholder"},
		{"redirect", model.DefaultHostSettings{Action: "redirect", RedirectTo: " https://home.lan "}, edgecfg.DefaultServer{Action: "redirect", RedirectTo: "https://home.lan", Cert: placeholder, HTTP3: true}, ""},
		{"redirect without target", model.DefaultHostSettings{Action: "redirect", RedirectTo: " "}, edgecfg.DefaultServer{Action: "close", Cert: placeholder, HTTP3: true},
			"default server: redirect target missing: closing the connection instead"},
		{"host", model.DefaultHostSettings{Action: "host", HostID: "hostgrafana", CertificateID: "certwild"}, edgecfg.DefaultServer{Action: "host", Host: grafana, Cert: certwildRef, HTTP3: true}, ""},
		{"missing host", model.DefaultHostSettings{Action: "host", HostID: "nope"}, edgecfg.DefaultServer{Action: "close", Cert: placeholder, HTTP3: true},
			"default server: default host nope is missing or disabled: closing the connection instead"},
		{"disabled host", model.DefaultHostSettings{Action: "host", HostID: "hostold"}, edgecfg.DefaultServer{Action: "close", Cert: placeholder, HTTP3: true},
			"default server: default host hostold is missing or disabled: closing the connection instead"},
	} {
		s := richSnapshot()
		s.DefaultHost = c.dh
		got, _ := mustRender(t, s, testEnv())
		equal(t, "default/"+c.name, got.Default, c.want)
		if c.note != "" && !hasNote(got, c.note) {
			t.Errorf("default/%s: missing note %q in %q", c.name, c.note, got.Notes)
		}
	}

	// The default-served host's access lists are rendered even when the host is
	// only reachable through the default server.
	s := richSnapshot()
	s.Hosts[1].Domains = nil // grafana: enabled but no domains → not in hosts
	s.Hosts[0].AccessListID, s.Hosts[5].AccessListID = "", ""
	s.Hosts[5].Locations = nil
	s.DefaultHost = model.DefaultHostSettings{Action: "host", HostID: "hostgrafana"}
	got, _ := mustRender(t, s, testEnv())
	if got.Default.Action != "host" || got.Default.Host.ID != "hostgrafana" {
		t.Fatalf("default = %+v", got.Default)
	}
	if _, ok := got.AccessLists["lanonly"]; !ok {
		t.Error("access list of the default-served host missing")
	}
}

func TestCustomPorts(t *testing.T) {
	s := richSnapshot()
	s.General.HTTPPort, s.General.HTTPSPort = 8080, 8443
	cfg, _ := mustRender(t, s, testEnv())
	if cfg.HTTPPort != 8080 || cfg.HTTPSPort != 8443 {
		t.Errorf("ports = %d/%d", cfg.HTTPPort, cfg.HTTPSPort)
	}
	s.General.HTTPPort, s.General.HTTPSPort = 0, -1
	cfg, _ = mustRender(t, s, testEnv())
	if cfg.HTTPPort != 80 || cfg.HTTPSPort != 443 {
		t.Errorf("default ports = %d/%d", cfg.HTTPPort, cfg.HTTPSPort)
	}
}

// ---------------------------------------------------------------- unsupported features

// Custom nginx snippets are kept in the snapshot but skipped by Relay Edge,
// with a note, so switching engines never needs them removed.
func TestCustomSnippetSkippedWithNote(t *testing.T) {
	s := richSnapshot()
	s.Hosts[0].CustomNginx = "proxy_hide_header X-Powered-By;"
	s.Hosts[1].CustomNginx = "  \n"              // blank: no note
	s.Hosts[2].CustomNginx = "add_header X-A b;" // vault
	s.Hosts = append(s.Hosts, model.ProxyHost{Meta: model.Meta{ID: "hidden"}, Enabled: true, CustomNginx: "x;", Upstream: model.Upstream{Host: "a"}})
	s.DefaultHost = model.DefaultHostSettings{Action: "host", HostID: "hidden"}
	files, err := Render(s, testEnv())
	if err != nil {
		t.Fatalf("snippets must not fail the render: %v", err)
	}
	var cfg edgecfg.Config
	decodeStrict(t, files[ConfigFile], &cfg)
	var notes []string
	for _, n := range cfg.Notes {
		if strings.Contains(n, "custom nginx snippet") {
			notes = append(notes, n)
		}
	}
	want := []string{
		"host cloud.home.lan: custom nginx snippet kept but not run by Relay Edge (it applies again with nginx)",
		"host vault.home.lan: custom nginx snippet kept but not run by Relay Edge (it applies again with nginx)",
		"host hidden: custom nginx snippet kept but not run by Relay Edge (it applies again with nginx)",
	}
	equal(t, "snippet notes", notes, want)

	// The default-served host is not reported twice.
	s = richSnapshot()
	s.Hosts[0].CustomNginx = "x;"
	s.DefaultHost = model.DefaultHostSettings{Action: "host", HostID: "hostcloud"}
	files, err = Render(s, testEnv())
	if err != nil || strings.Count(files[ConfigFile], "custom nginx snippet") != 1 {
		t.Errorf("default-served host: err=%v notes=%d", err, strings.Count(files[ConfigFile], "custom nginx snippet"))
	}
}

func TestGeoBlockNote(t *testing.T) {
	cfg := renderRich(t)
	n := 0
	for _, note := range cfg.Notes {
		if strings.Contains(note, "geo-blocking") {
			n++
		}
	}
	if n != 1 || !hasNote(cfg, "host cloud.home.lan: geo-blocking by country is not supported by Relay Edge and is skipped") {
		t.Errorf("notes = %q", cfg.Notes)
	}
}

// ---------------------------------------------------------------- streams

func TestStreams(t *testing.T) {
	cfg := renderRich(t)
	equal(t, "streams", cfg.Streams, []edgecfg.Stream{
		{ID: "s3", Name: "dns", TCP: true, UDP: true, ListenLo: 5353, ListenHi: 5354, ForwardHost: "pihole.lan", ForwardLo: 53, ForwardHi: 54, IdleTimeoutMs: 30000, ConnectTimeoutMs: 10000},
		{ID: "s1", Name: "minecraft", TCP: true, ListenAddr: "0.0.0.0", ListenLo: 25565, ListenHi: 25565, ForwardHost: "10.0.0.50", ForwardLo: 25565, ForwardHi: 25565, IdleTimeoutMs: 600000, ConnectTimeoutMs: 10000},
		{ID: "s5", Name: "postgres", TCP: true, ListenAddr: "10.0.0.1", ListenLo: 5433, ListenHi: 5433, ForwardHost: "127.0.0.1", ForwardLo: 15432, ForwardHi: 15432, IdleTimeoutMs: 600000, ConnectTimeoutMs: 10000},
		{ID: "s4", Name: "shifted", TCP: true, ListenLo: 7000, ListenHi: 7001, ForwardHost: "10.0.0.7", ForwardLo: 8000, ForwardHi: 8001, IdleTimeoutMs: 600000, ConnectTimeoutMs: 10000},
		{ID: "s2", Name: "valheim", UDP: true, ListenAddr: "0.0.0.0", ListenLo: 2456, ListenHi: 2458, ForwardHost: "10.0.0.61", ForwardLo: 2456, ForwardHi: 2458, ProxyProtocol: true, IdleTimeoutMs: 600000, ConnectTimeoutMs: 10000},
	})
}

func TestStreamBackendFrontend(t *testing.T) {
	now := time.Now()
	s := richSnapshot()
	s.Frontends = []model.Frontend{
		{Meta: model.Meta{ID: "late", CreatedAt: now.Add(time.Hour)}, Mode: "tcp", Bind: "127.0.0.1:20000", DefaultBackendID: "pg", Enabled: true},
		{Meta: model.Meta{ID: "off", CreatedAt: now.Add(-3 * time.Hour)}, Mode: "tcp", Bind: "127.0.0.1:1", DefaultBackendID: "pg", Enabled: false},
		{Meta: model.Meta{ID: "http", CreatedAt: now.Add(-2 * time.Hour)}, Mode: "http", Bind: "127.0.0.1:2", DefaultBackendID: "pg", Enabled: true},
		{Meta: model.Meta{ID: "public", CreatedAt: now.Add(-time.Hour)}, Mode: "tcp", Bind: "0.0.0.0:3", DefaultBackendID: "pg", Enabled: true},
		{Meta: model.Meta{ID: "early", CreatedAt: now}, Mode: "tcp", Bind: " 127.0.0.1:15433 ", DefaultBackendID: "pg", Enabled: true},
	}
	cfg, _ := mustRender(t, s, testEnv())
	for _, st := range cfg.Streams {
		if st.ID == "s5" && (st.ForwardHost != "127.0.0.1" || st.ForwardLo != 15433 || st.ForwardHi != 15433) {
			t.Errorf("postgres stream = %+v", st)
		}
	}
}

func TestStreamErrors(t *testing.T) {
	for _, c := range []struct {
		name   string
		mutate func(*model.Snapshot)
		want   string
	}{
		{"no frontend", func(s *model.Snapshot) { s.Frontends = nil }, "stream postgres: backend postgres-ro has no enabled TCP frontend bound to 127.0.0.1"},
		{"unknown backend", func(s *model.Snapshot) { s.Frontends, s.Backends = nil, nil }, "stream postgres: backend pg has no enabled TCP frontend bound to 127.0.0.1"},
		{"range size", func(s *model.Snapshot) { s.Streams[3].ForwardPorts = "8000-8005" }, "stream shifted: listen range 7000-7001 and forward range 8000-8005 differ in size"},
		{"invalid listen", func(s *model.Snapshot) { s.Streams[0].ListenPorts = "abc" }, `stream minecraft: invalid port "abc"`},
		{"listen out of range", func(s *model.Snapshot) { s.Streams[0].ListenPorts = "70000" }, `stream minecraft: invalid port range "70000"`},
		{"reversed range", func(s *model.Snapshot) { s.Streams[0].ListenPorts = "10-5" }, `stream minecraft: invalid port range "10-5"`},
		{"invalid forward", func(s *model.Snapshot) { s.Streams[3].ForwardPorts = "x-y" }, `stream shifted: forward invalid port "x-y"`},
		{"empty forward host", func(s *model.Snapshot) { s.Streams[0].ForwardHost = " " }, "stream minecraft: forward host is empty"},
		{"hostname range", func(s *model.Snapshot) { s.Streams[2].ListenPorts, s.Streams[2].ForwardPorts = "10000-11001", "" }, "stream dns: port ranges to a hostname are limited to 1000 ports"},
		{"shifted ip range", func(s *model.Snapshot) {
			s.Streams[3].ListenPorts, s.Streams[3].ForwardPorts = "10000-15001", "20000-25001"
		}, "stream shifted: port range too large"},
	} {
		s := richSnapshot()
		c.mutate(s)
		files, err := Render(s, testEnv())
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error = %v, want %q", c.name, err, c.want)
			continue
		}
		// The failing stream is skipped; the others are still rendered.
		cfg := decode(t, files[ConfigFile])
		if len(cfg.Streams) != 4 {
			t.Errorf("%s: %d streams rendered, want 4", c.name, len(cfg.Streams))
		}
	}

	// Large ranges are fine where nginx needs no per-port servers or maps.
	s := richSnapshot()
	s.Streams = []model.Stream{
		{Meta: model.Meta{ID: "a"}, Name: "same", Protocol: "tcp", ListenPorts: "10000-20000", ForwardHost: "10.0.0.1", Enabled: true},
		{Meta: model.Meta{ID: "b"}, Name: "single", Protocol: "tcp", ListenPorts: "10000-20000", ForwardHost: "host.lan", ForwardPorts: "80", Enabled: true},
	}
	if cfg, _ := mustRender(t, s, testEnv()); len(cfg.Streams) != 2 || cfg.Streams[1].ForwardLo != 80 || cfg.Streams[1].ForwardHi != 80 {
		t.Errorf("streams = %+v", cfg.Streams)
	}
}

func TestIdleTimeout(t *testing.T) {
	for in, want := range map[string]int{
		"": 600000, "10m": 600000, "30": 30000, "45s": 45000, "500ms": 500, "2h": 7200000, "1d": 86400000,
		"5x": 600000, "-1": 600000, " 7s ": 7000, "0": 1, "99999999999d": math.MaxInt32,
	} {
		if got := idleTimeoutMs(in); got != want {
			t.Errorf("idleTimeoutMs(%q) = %d, want %d", in, got, want)
		}
	}
}

// ---------------------------------------------------------------- previews

func decodeStrict(t *testing.T, data string, v any) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("preview does not match the schema: %v\n%s", err, data)
	}
}

func TestRenderHostPreview(t *testing.T) {
	s := richSnapshot()
	draft := model.ProxyHost{Domains: []string{"new.home.lan"}, Upstream: model.Upstream{Scheme: "http", Host: "10.0.0.99", Port: 8080}, Websockets: true, AccessListID: "family"}
	out, err := RenderHost(s, &draft, testEnv())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "{\n  \"id\": \"\",\n") || !strings.HasSuffix(out, "}\n") {
		t.Errorf("preview format:\n%s", out)
	}
	var h edgecfg.Host
	decodeStrict(t, out, &h)
	equal(t, "draft preview", h, edgecfg.Host{
		Domains: []string{"new.home.lan"}, MaxBodyBytes: 1 << 20,
		Locations: []edgecfg.Location{{Path: "/", Kind: "proxy", Upstream: edgecfg.Upstream{Scheme: "http", Host: "10.0.0.99", Port: 8080}, Websockets: true, AccessListID: "family"}},
	})

	// An existing (even disabled) host previews with its edits and injected redirects.
	edited := s.Hosts[4] // home.lan
	edited.Enabled = false
	edited.Upstream.Port = 10081
	out, err = RenderHost(s, &edited, testEnv())
	if err != nil {
		t.Fatal(err)
	}
	decodeStrict(t, out, &h)
	if h.ID != "hostapp" || h.Locations[0].Upstream.Port != 10081 || len(h.PathRedirects) != 2 {
		t.Errorf("edited preview = %+v", h)
	}
	if s.Hosts[4].Upstream.Port != 10080 {
		t.Error("RenderHost must not mutate the snapshot")
	}

	custom := draft
	custom.CustomNginx = "x;"
	if _, err := RenderHost(s, &custom, testEnv()); err != nil {
		t.Errorf("custom snippet must not fail the preview: %v", err)
	}
}

func TestRenderStreamPreview(t *testing.T) {
	s := richSnapshot()
	out, err := RenderStream(s, &model.Stream{Meta: model.Meta{ID: "x"}, Name: "x", Protocol: "tcp", ListenPorts: "1234", ForwardHost: "10.0.0.1"}, testEnv())
	if err != nil {
		t.Fatal(err)
	}
	var st edgecfg.Stream
	decodeStrict(t, out, &st)
	equal(t, "stream preview", st, edgecfg.Stream{ID: "x", Name: "x", TCP: true, ListenLo: 1234, ListenHi: 1234, ForwardHost: "10.0.0.1", ForwardLo: 1234, ForwardHi: 1234, IdleTimeoutMs: 600000, ConnectTimeoutMs: 10000})

	out, err = RenderStream(s, &model.Stream{Name: "bad", ListenPorts: "nope", ForwardHost: "a"}, testEnv())
	if err == nil || out != "" || !strings.Contains(err.Error(), `stream bad: invalid port "nope"`) {
		t.Errorf("stream preview = %q, %v", out, err)
	}
}
