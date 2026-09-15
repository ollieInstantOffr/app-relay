package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// fakeAPI stands in for the REST API handler and records every request.
type fakeAPI struct {
	mu     sync.Mutex
	routes map[string]func(w http.ResponseWriter, r *http.Request, body []byte)
	calls  []fakeReq
}

type fakeReq struct {
	Method, Path string
	Body         []byte
	Internal     bool
	Actor        core.Actor
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	f.mu.Lock()
	f.calls = append(f.calls, fakeReq{Method: r.Method, Path: r.URL.RequestURI(), Body: body, Internal: core.IsInternalCall(r.Context()), Actor: core.ActorFrom(r.Context())})
	h := f.routes[r.Method+" "+r.URL.Path]
	f.mu.Unlock()
	if h == nil {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"not found"}}`))
		return
	}
	h(w, r, body)
}

func (f *fakeAPI) last(method string) fakeReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].Method == method {
			return f.calls[i]
		}
	}
	return fakeReq{}
}

func jsonReply(v any) func(w http.ResponseWriter, r *http.Request, body []byte) {
	return func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
}

func echoBody(w http.ResponseWriter, r *http.Request, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func withFakeAPI(f *fixture) *fakeAPI {
	api := &fakeAPI{routes: map[string]func(http.ResponseWriter, *http.Request, []byte){}}
	f.app.API = api
	return api
}

func (f *fixture) plan(t *testing.T, c *call, tool string, args any) (*plan, error) {
	t.Helper()
	wt, ok := f.svc.writes[tool]
	if !ok {
		t.Fatalf("write tool %s is not registered", tool)
	}
	raw, _ := json.Marshal(args)
	return wt.plan(core.WithActor(context.Background(), c.actor), c, raw)
}

func TestBridgeCallsAPIAsInternalActor(t *testing.T) {
	f := newFixture(t)
	api := withFakeAPI(f)
	api.routes["POST /api/things"] = jsonReply(map[string]any{"id": "t1"})
	api.routes["PUT /api/things/bad"] = func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":{"code":"validation","message":"invalid thing","fields":{"to":"must be a URL","code":"must be 301"}}}`))
	}
	actor := f.mcpActor()
	actor.IP = "10.1.2.3"
	ctx := core.WithActor(context.Background(), actor)

	var out map[string]any
	if err := f.svc.apiCall(ctx, http.MethodPost, "/things", map[string]any{"name": "x"}, &out); err != nil {
		t.Fatal(err)
	}
	if out["id"] != "t1" {
		t.Fatalf("decoded %v", out)
	}
	got := api.last(http.MethodPost)
	if !got.Internal || got.Actor.TokenID != "tok1" || got.Actor.Type != core.ActorMCP || string(got.Body) != `{"name":"x"}` {
		t.Fatalf("request = %+v", got)
	}

	err := f.svc.apiCall(ctx, http.MethodPut, "/things/bad", map[string]any{}, nil)
	if err == nil || err.Error() != "invalid thing (code: must be 301; to: must be a URL)" {
		t.Fatalf("error = %v", err)
	}
	if err := f.svc.apiCall(context.Background(), http.MethodGet, "/things", nil, nil); err == nil {
		t.Fatal("a call without an actor must be refused")
	}
}

func TestMergePatch(t *testing.T) {
	cur := map[string]any{"name": "a", "keep": 1, "nested": map[string]any{"x": 1, "y": 2}, "list": []any{1, 2}}
	got, err := mergePatch(cur, json.RawMessage(`{"name":"b","keep":null,"nested":{"y":3,"z":4},"list":[3]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	if string(b) != `{"list":[3],"name":"b","nested":{"x":1,"y":3,"z":4}}` {
		t.Fatalf("merged = %s", b)
	}
	if _, err := mergePatch(cur, json.RawMessage(`[1]`)); err == nil {
		t.Fatal("a non-object patch must fail")
	}
}

func TestUpdateRedirectMergesAndPuts(t *testing.T) {
	f := newFixture(t)
	api := withFakeAPI(f)
	api.routes["GET /api/redirects"] = jsonReply([]map[string]any{
		{"id": "r1", "domains": []string{"old.example.com"}, "to": "https://new.example.com", "code": 301, "keepPath": true, "enabled": true},
	})
	api.routes["PUT /api/redirects/r1"] = echoBody
	c := &call{actor: f.mcpActor(), scope: scope{}}

	pl, err := f.plan(t, c, "update_redirect", map[string]any{"id": "OLD.example.com", "changes": map[string]any{"code": 308}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pl.Preview, `+   "code": 308`) || !strings.Contains(pl.Preview, `-   "code": 301`) {
		t.Fatalf("preview = %s", pl.Preview)
	}
	out, err := pl.Exec(core.WithActor(context.Background(), c.actor))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(api.last(http.MethodPut).Body, &body)
	if body["code"] != float64(308) || body["keepPath"] != true || body["to"] != "https://new.example.com" || body["id"] != "r1" {
		t.Fatalf("PUT body = %v", body)
	}
	if !strings.Contains(out.Text, "Updated redirect old.example.com") {
		t.Fatalf("text = %s", out.Text)
	}

	// A token limited to other domains can't touch it.
	limited := &call{actor: f.mcpActor(), scope: newScope([]string{"*.home.lan"})}
	if _, err := f.plan(t, limited, "update_redirect", map[string]any{"id": "r1", "changes": map[string]any{"code": 302}}); err == nil || !strings.Contains(err.Error(), "outside this token's scope") {
		t.Fatalf("limited err = %v", err)
	}
}

func TestAccessListPasswordsRedacted(t *testing.T) {
	f := newFixture(t)
	withFakeAPI(f)
	c := &call{actor: f.mcpActor(), scope: scope{}}
	pl, err := f.plan(t, c, "create_access_list", map[string]any{"config": map[string]any{
		"name": "staff", "basicAuth": map[string]any{"enabled": true, "users": []any{map[string]any{"username": "ann", "password": "hunter2hunter2"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(pl.Preview, "hunter2") || !strings.Contains(pl.Preview, "ann") {
		t.Fatalf("preview leaks or drops users: %s", pl.Preview)
	}
}

func TestInstanceWideToolsNeedUnrestrictedToken(t *testing.T) {
	f := newFixture(t)
	withFakeAPI(f)
	limited := &call{actor: f.mcpActor(), scope: newScope([]string{"*.home.lan"})}
	cases := map[string]any{
		"update_settings":    map[string]any{"key": "tls", "changes": map[string]any{"hsts": "on"}},
		"discard_changes":    map[string]any{},
		"set_proxy_engine":   map[string]any{"engine": "edge"},
		"set_lb_engine":      map[string]any{"engine": "balancer"},
		"engine_action":      map[string]any{"engine": "nginx", "action": "reload"},
		"upgrade_relay":      map[string]any{},
		"create_access_list": map[string]any{"config": map[string]any{"name": "x"}},
		"block_ip":           map[string]any{"cidr": "203.0.113.7"},
		"rollback_version":   map[string]any{"version": 3},
		"create_backup":      map[string]any{},
		"update_frontend":    map[string]any{"id": "fe", "changes": map[string]any{"bind": ":81"}},
	}
	for tool, args := range cases {
		if _, err := f.plan(t, limited, tool, args); err == nil || !strings.Contains(err.Error(), "limited to") {
			t.Errorf("%s with a limited token: err = %v", tool, err)
		}
	}
}

func TestSettingsSafeguards(t *testing.T) {
	f := newFixture(t)
	api := withFakeAPI(f)
	api.routes["GET /api/settings/general"] = jsonReply(map[string]any{"adminPort": 81, "adminDomain": "", "proxyEngine": "nginx", "http3": false})
	api.routes["PUT /api/settings/general"] = echoBody
	c := &call{actor: f.mcpActor(), scope: scope{}}

	for _, key := range []string{model.SettingsSecurity, model.SettingsMCP, "nope"} {
		if _, err := f.plan(t, c, "update_settings", map[string]any{"key": key, "changes": map[string]any{"x": 1}}); err == nil || !strings.Contains(err.Error(), "not available over MCP") {
			t.Errorf("update_settings %s: err = %v", key, err)
		}
	}
	if _, err := f.plan(t, c, "update_settings", map[string]any{"key": "general", "changes": map[string]any{"adminPort": 9000}}); err == nil || !strings.Contains(err.Error(), "adminPort") {
		t.Fatalf("admin port change: err = %v", err)
	}

	pl, err := f.plan(t, c, "set_proxy_engine", map[string]any{"engine": "edge"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := pl.Exec(core.WithActor(context.Background(), c.actor))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(api.last(http.MethodPut).Body, &body)
	if body["proxyEngine"] != "edge" || body["adminPort"] != float64(81) {
		t.Fatalf("PUT body = %v", body)
	}
	if !strings.Contains(out.Text, "Relay Edge") {
		t.Fatalf("text = %s", out.Text)
	}
	if _, err := f.plan(t, c, "set_proxy_engine", map[string]any{"engine": "nginx"}); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("no-op switch: err = %v", err)
	}

	// Load balancer engine (lbEngine missing from older settings = haproxy).
	pl, err = f.plan(t, c, "set_lb_engine", map[string]any{"engine": "balancer"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pl.Summary, "HAProxy") || !strings.Contains(pl.Summary, "Relay Balancer") {
		t.Fatalf("summary = %s", pl.Summary)
	}
	out, err = pl.Exec(core.WithActor(context.Background(), c.actor))
	if err != nil {
		t.Fatal(err)
	}
	body = nil
	_ = json.Unmarshal(api.last(http.MethodPut).Body, &body)
	if body["lbEngine"] != "balancer" || body["proxyEngine"] != "nginx" || body["adminPort"] != float64(81) {
		t.Fatalf("PUT body = %v", body)
	}
	if !strings.Contains(out.Text, "Relay Balancer") || !strings.Contains(out.Text, "apply_changes") {
		t.Fatalf("text = %s", out.Text)
	}
	if _, err := f.plan(t, c, "set_lb_engine", map[string]any{"engine": "haproxy"}); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("no-op lb switch: err = %v", err)
	}
	if _, err := f.plan(t, c, "set_lb_engine", map[string]any{"engine": "envoy"}); err == nil || !strings.Contains(err.Error(), "haproxy or balancer") {
		t.Fatalf("unknown lb engine: err = %v", err)
	}
	if _, err := f.plan(t, c, "engine_action", map[string]any{"engine": "balancer", "action": "reload"}); err != nil {
		t.Fatalf("engine_action balancer: %v", err)
	}
}

func TestCatalogMatchesDefaults(t *testing.T) {
	f := newFixture(t)
	seen := map[string]bool{}
	for _, info := range f.svc.Catalog() {
		seen[info.Name] = true
		def, ok := store.MCPTools[info.Name]
		if !ok {
			t.Errorf("%s has no default permission in store.MCPTools", info.Name)
			continue
		}
		if (info.Kind == KindWrite) != IsWriteTool(info.Name) {
			t.Errorf("%s: kind %s but IsWriteTool = %v", info.Name, info.Kind, IsWriteTool(info.Name))
		}
		if info.Kind == KindWrite && def == model.ToolRead {
			t.Errorf("write tool %s defaults to read", info.Name)
		}
		if info.Kind == KindRead && def != model.ToolRead && def != model.ToolDisabled {
			t.Errorf("read tool %s defaults to %s", info.Name, def)
		}
	}
	for name := range store.MCPTools {
		if !seen[name] {
			t.Errorf("store.MCPTools lists %s but no such tool is registered", name)
		}
	}
}
