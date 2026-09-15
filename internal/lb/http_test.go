package lb

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// testApp wires the lb routes against a real SQLite store. The engine agents
// are not running (sockets don't exist), exercising the degraded paths.
func testApp(t *testing.T) (*core.App, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := core.Config{DataDir: dir, RunDir: dir, LogDir: dir, Listen: ":8181"}
	app := core.New(cfg, st, events.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	app.LB = New(app)
	r := chi.NewRouter()
	Routes(app, r)
	return app, r
}

func call(t *testing.T, h http.Handler, method, path string, body any, out any) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	req = req.WithContext(core.WithActor(req.Context(), core.Actor{Type: core.ActorUser, Name: "tester", Role: core.RoleAdmin}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if out != nil && rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: decode %q: %v", method, path, rec.Body.String(), err)
		}
	}
	return rec.Code
}

func seedBackend(t *testing.T, app *core.App) *model.Backend {
	t.Helper()
	b := &model.Backend{Name: "api", Servers: []model.Server{{Address: "127.0.0.1", Port: 1, Check: true}, {Address: "127.0.0.1", Port: 2, Check: true}}}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	if err := httpx.BackendHooks.BeforeSave(req, nil, b); err != nil {
		t.Fatal(err)
	}
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := app.Store.Backends().Create(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestHTTPFlow(t *testing.T) {
	app, h := testApp(t)
	ctx := context.Background()
	b := seedBackend(t, app)

	// Stats without haproxy: not running, empty lists.
	var stats core.LBStats
	if code := call(t, h, "GET", "/lb/stats", nil, &stats); code != 200 || stats.Running || stats.Backends == nil {
		t.Fatalf("stats: %d %+v", code, stats)
	}
	var series Series
	if code := call(t, h, "GET", "/lb/series?range=1h&backend="+b.ID, nil, &series); code != 200 || len(series.Points) != 120 {
		t.Fatalf("series: %d points=%d", code, len(series.Points))
	}
	if code := call(t, h, "GET", "/lb/series?range=2h", nil, nil); code != 400 {
		t.Fatalf("bad range: %d", code)
	}

	// Backend preview renders locally when the agent is unreachable.
	var pv preview
	draft := *b
	draft.HealthCheck.Path = "/healthz"
	if code := call(t, h, "POST", "/preview/haproxy/backend", map[string]any{"backend": draft}, &pv); code != 200 {
		t.Fatalf("preview: %d", code)
	}
	if !strings.Contains(pv.Config, "backend api") || !strings.Contains(pv.Config, "uri /healthz") || pv.Checked != "local" || !pv.Valid {
		t.Fatalf("preview: %+v", pv)
	}
	draft.Servers[0].Weight = 999
	call(t, h, "POST", "/preview/haproxy/backend", map[string]any{"backend": draft}, &pv)
	if pv.Valid || pv.Fields["servers.0.weight"] == "" {
		t.Fatalf("invalid preview: %+v", pv)
	}

	// Drain at runtime: haproxy is down, so it's persisted with a note.
	var sr StateResult
	if code := call(t, h, "POST", "/backends/"+b.ID+"/servers/"+b.Servers[1].ID+"/state", map[string]any{"state": "drain", "graceSeconds": 60}, &sr); code != 200 || sr.Runtime || sr.Note == "" {
		t.Fatalf("state: %d %+v", code, sr)
	}
	got, _ := app.Store.Backends().Get(ctx, b.ID)
	if got.Servers[1].State != "drain" {
		t.Fatalf("state not persisted: %+v", got.Servers[1])
	}
	// A stale editor save keeps the admin state.
	stale := *got
	stale.Servers = append([]model.Server(nil), got.Servers...)
	stale.Servers[1].State = "ready"
	if err := httpx.BackendHooks.BeforeSave(httptest.NewRequest("PUT", "/", nil), got, &stale); err != nil || stale.Servers[1].State != "drain" {
		t.Fatalf("stale save reset admin state: %v %+v", err, stale.Servers[1])
	}
	if code := call(t, h, "POST", "/backends/"+b.ID+"/servers/"+b.Servers[1].ID+"/weight", map[string]any{"weight": 300}, nil); code != 422 {
		t.Fatalf("weight validation: %d", code)
	}
	if code := call(t, h, "POST", "/backends/"+b.ID+"/servers/nope/state", map[string]any{"state": "maint"}, nil); code != 404 {
		t.Fatalf("missing server: %d", code)
	}

	// Health probe against a closed port.
	var pr ProbeResult
	if code := call(t, h, "POST", "/backends/"+b.ID+"/servers/"+b.Servers[0].ID+"/check", nil, &pr); code != 200 || pr.OK || pr.Status != "DOWN" {
		t.Fatalf("probe: %d %+v", code, pr)
	}

	// Convert a host's upstream into a pool.
	host := &model.ProxyHost{
		Domains: []string{"grafana.home.lan"}, Enabled: true, Upstream: model.Upstream{Scheme: "http", Host: "10.0.0.5", Port: 3000},
		HSTS: "inherit", Source: model.SourceManual, Locations: []model.Location{},
	}
	if err := app.Store.Hosts().Create(ctx, host); err != nil {
		t.Fatal(err)
	}
	var conv struct {
		Backend  model.Backend
		Frontend model.Frontend
		Host     model.ProxyHost
	}
	if code := call(t, h, "POST", "/backends/from-host/"+host.ID, nil, &conv); code != 201 {
		var raw map[string]any
		call(t, h, "POST", "/backends/from-host/"+host.ID, nil, &raw)
		t.Fatalf("from-host: %d %v", code, raw)
	}
	if conv.Backend.Name != "grafana" || conv.Frontend.Bind != "127.0.0.1:10080" || conv.Host.Upstream.Port != 10080 || conv.Host.Upstream.BackendID != conv.Backend.ID {
		t.Fatalf("from-host result: %+v", conv)
	}
	if code := call(t, h, "POST", "/backends/from-host/"+host.ID, nil, nil); code != 409 {
		t.Fatalf("second convert: %d", code)
	}
	// The converted backend can't be deleted while referenced.
	del := httptest.NewRequest("DELETE", "/", nil)
	if err := httpx.BackendHooks.BeforeDelete(del, &conv.Backend); err == nil {
		t.Fatal("delete guard missing")
	}
	if err := httpx.FrontendHooks.BeforeDelete(del, &conv.Frontend); err == nil {
		t.Fatal("frontend delete guard missing")
	}

	// Expose: preview then create.
	exp := map[string]any{
		"backendId": b.ID, "domain": "API.example.com", "certificate": map[string]any{"mode": "none"},
		"forceHttps": true, "websockets": true, "access": map[string]any{"mode": "public"}, "blockExploits": true,
	}
	var ep exposePreview
	if code := call(t, h, "POST", "/lb/expose/preview", exp, &ep); code != 200 {
		t.Fatalf("expose preview: %d", code)
	}
	if ep.Port != 10081 || !strings.Contains(ep.HAProxy, "frontend fe-api") || !strings.Contains(ep.HAProxy, "bind 127.0.0.1:10081") || !strings.Contains(ep.HAProxy, "default_backend api") {
		t.Fatalf("expose preview: %+v", ep)
	}
	var er struct {
		Host     model.ProxyHost
		Frontend model.Frontend
	}
	if code := call(t, h, "POST", "/lb/expose", exp, &er); code != 201 {
		t.Fatalf("expose: %d", code)
	}
	if er.Host.Domains[0] != "api.example.com" || er.Host.ForceHTTPS || er.Host.Upstream.BackendID != b.ID || er.Frontend.HostID != er.Host.ID || er.Frontend.Source != "expose" {
		t.Fatalf("expose result: %+v", er)
	}
	// Same domain again → field error.
	var fail struct {
		Error struct {
			Code   string
			Fields map[string]string
		}
	}
	if code := call(t, h, "POST", "/lb/expose", exp, &fail); code != 422 || fail.Error.Fields["domain"] == "" {
		t.Fatalf("duplicate domain: %d %+v", code, fail)
	}

	// Frontend port clash with nginx.
	fe := &model.Frontend{Name: "web", Mode: "http", Bind: "0.0.0.0:443", Enabled: true, DefaultBackendID: b.ID}
	err := httpx.FrontendHooks.BeforeSave(httptest.NewRequest("POST", "/", nil), nil, fe)
	if ve, ok := err.(*model.ValidationError); !ok || !strings.Contains(ve.Fields["bind"], "nginx") {
		t.Fatalf("port clash: %v", err)
	}

	// Full config renders everything created above.
	var cfg map[string]any
	if code := call(t, h, "GET", "/haproxy/config", nil, &cfg); code != 200 {
		t.Fatalf("config: %d", code)
	}
	c, _ := cfg["config"].(string)
	for _, want := range []string{"frontend fe-grafana", "frontend fe-api", "backend grafana", "server api-2 127.0.0.1:2 check weight 100\n"} {
		if !strings.Contains(c, want) {
			t.Errorf("config missing %q:\n%s", want, c)
		}
	}
	var v validation
	if code := call(t, h, "POST", "/haproxy/validate", nil, &v); code != 200 || v.Checked != "local" || !v.Valid {
		t.Fatalf("validate: %d %+v", code, v)
	}

	// Settings hook: stats bind clashing with a frontend.
	s := store.DefaultHAProxy()
	s.StatsBind = "127.0.0.1:10080"
	if err := httpx.SettingsHooks[model.SettingsHAProxy].BeforeSave(httptest.NewRequest("PUT", "/", nil), nil, &s); err == nil {
		t.Fatal("stats bind clash not detected")
	}
}
