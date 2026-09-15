package apply

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/agent"
	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/render"
	"github.com/instantoffr/relay/internal/store"
)

// fakeEdgeRenderer replaces the Relay Edge renderer with a deterministic one.
func fakeEdgeRenderer(t *testing.T) {
	t.Helper()
	old := proxyRenderers[agent.EngineEdge]
	proxyRenderers[agent.EngineEdge] = proxyRenderer{
		render: func(s *model.Snapshot, _ render.Env) (agent.Files, error) {
			var b strings.Builder
			for _, h := range s.Hosts {
				if h.Enabled {
					fmt.Fprintf(&b, "%s %s ws=%v\n", h.ID, strings.Join(h.Domains, ","), h.Websockets)
				}
			}
			return agent.Files{"edge.json": b.String(), "htpasswd/x": "jonas:$2a$10$abc\n"}, nil
		},
		host: func(_ *model.Snapshot, h *model.ProxyHost, _ render.Env) (string, error) {
			return "edge host " + strings.Join(h.Domains, ","), nil
		},
		stream: func(_ *model.Snapshot, st *model.Stream, _ render.Env) (string, error) {
			return "edge stream " + st.Name, nil
		},
	}
	t.Cleanup(func() { proxyRenderers[agent.EngineEdge] = old })
}

func (e *testEnv) selectEngine(t *testing.T, engine string) {
	t.Helper()
	gen, err := store.LoadSettings[model.GeneralSettings](e.ctx, e.app.Store, model.SettingsGeneral)
	if err != nil {
		t.Fatal(err)
	}
	gen.ProxyEngine = engine
	if err := e.app.Store.PutSettings(e.ctx, model.SettingsGeneral, gen); err != nil {
		t.Fatal(err)
	}
}

func (e *testEnv) resetCalls() {
	for _, f := range []*fakeAgent{e.nginx, e.edge} {
		f.mu.Lock()
	}
	e.calls = nil
	for _, f := range []*fakeAgent{e.nginx, e.edge} {
		f.mu.Unlock()
	}
}

func TestEngineSwitchToEdge(t *testing.T) {
	e := newTestEnv(t)
	fakeEdgeRenderer(t)
	ctx := e.ctx
	e.createHost(t, "grafana.home.lan")
	if _, err := e.svc.Apply(ctx, core.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	live1, _ := e.app.Store.LiveVersion(ctx, false)
	if live1.ProxyEngine != "nginx" || toVersionJSON(live1).ProxyEngine != "nginx" {
		t.Fatalf("v1 engine = %q", live1.ProxyEngine)
	}

	// The switch is a pending change with its own label.
	e.selectEngine(t, "edge")
	p, summary, err := e.svc.pendingWithSummary(ctx)
	if err != nil || p.Count != 1 || p.Items[0].ID != model.SettingsGeneral || summary != "Proxy engine: nginx → Relay Edge" {
		t.Fatalf("pending = %+v %q %v", p, summary, err)
	}
	// The pending diff crosses engines: nginx files removed, edge/ files added.
	rec := httptest.NewRecorder()
	(&handlers{app: e.app}).pendingDiff(rec, httptest.NewRequest(http.MethodGet, "/pending/diff", nil))
	var diff diffResponse
	json.Unmarshal(rec.Body.Bytes(), &diff)
	status := map[string]string{}
	for _, f := range diff.Files {
		status[f.Path] = f.Status
	}
	if status["edge/edge.json"] != "added" || status["nginx.conf"] != "removed" || !strings.Contains(strings.Join(diff.Paths, ","), "edge/htpasswd/x") {
		t.Fatalf("diff = %v paths %v", status, diff.Paths)
	}
	for _, f := range diff.Files {
		if f.Path == "edge/htpasswd/x" && strings.Contains(fmt.Sprint(f.Lines), "$2a$") {
			t.Fatal("edge htpasswd must be redacted")
		}
	}

	e.resetCalls()
	v2, err := e.svc.Apply(ctx, core.ApplyOptions{})
	if err != nil || v2.Status != "live" {
		t.Fatalf("v2 = %+v %v", v2, err)
	}
	if got := strings.Join(e.calls, ","); got != "nginx:stop,edge:apply" {
		t.Fatalf("calls = %s", got)
	}
	row2, _ := e.app.Store.GetVersion(ctx, v2.ID, true)
	if row2.ProxyEngine != "edge" || toVersionJSON(row2).ProxyEngine != "edge" || toVersionJSON(row2).ProxyHash != row2.NginxHash {
		t.Fatalf("v2 row engine = %q", row2.ProxyEngine)
	}
	if _, ok := versionFileMap(row2)["edge/edge.json"]; !ok {
		t.Fatalf("version files = %v", versionFileMap(row2))
	}
	e.nginx.mu.Lock()
	if !e.nginx.lastApply.Stop || e.nginx.lastApply.Hash != live1.NginxHash || len(e.nginx.lastApply.Files) == 0 || e.nginx.running {
		t.Fatalf("nginx stop request = %+v running=%v", e.nginx.lastApply, e.nginx.running)
	}
	e.nginx.mu.Unlock()
	e.edge.mu.Lock()
	if !e.edge.running || e.edge.hash != row2.NginxHash {
		t.Fatalf("edge running=%v hash=%s", e.edge.running, e.edge.hash)
	}
	e.edge.mu.Unlock()
	if agent.ReadProxyEngine(e.app.Config.RunDir) != "edge" || e.app.ProxyEngine(ctx) != "edge" {
		t.Fatal("proxy engine selection not recorded")
	}
	if p, _ := e.svc.Pending(ctx); p.Count != 0 {
		t.Fatalf("pending after switch = %+v", p)
	}
	if es, _ := e.svc.Status(ctx); es.Proxy != "edge" || !es.Edge.Reachable || es.Edge.Engine != "edge" {
		t.Fatalf("status = %+v", es)
	}
	if files, _, _, _, _ := e.svc.LiveRelease(ctx, "nginx"); files != nil {
		t.Fatal("nginx has no live release while edge is selected")
	}
	if files, hash, _, _, _ := e.svc.LiveRelease(ctx, "edge"); files == nil || hash != row2.NginxHash {
		t.Fatal("edge live release")
	}

	// Reconcile stops nginx if it runs again (e.g. container recreated).
	e.nginx.mu.Lock()
	e.nginx.running = true
	stops := e.nginx.stops
	e.nginx.mu.Unlock()
	edgeApplies := e.edge.applies
	e.svc.reconcile(ctx)
	e.nginx.mu.Lock()
	if e.nginx.stops != stops+1 || e.nginx.running || e.nginx.lastApply.Hash != "" || len(e.nginx.lastApply.Files) != 0 {
		t.Fatalf("reconcile: stops=%d running=%v req=%+v", e.nginx.stops, e.nginx.running, e.nginx.lastApply)
	}
	e.nginx.mu.Unlock()
	if e.edge.applies != edgeApplies {
		t.Fatal("reconcile must not touch edge when it runs the live version")
	}
	// ... and restores the live version on edge.
	e.edge.mu.Lock()
	e.edge.hash = agent.BootstrapHash
	e.edge.mu.Unlock()
	e.svc.reconcile(ctx)
	if e.edge.hash != row2.NginxHash {
		t.Fatal("reconcile did not restore edge")
	}
}

func TestEngineSwitchRollsBack(t *testing.T) {
	e := newTestEnv(t)
	fakeEdgeRenderer(t)
	ctx := e.ctx
	e.createHost(t, "grafana.home.lan")
	if _, err := e.svc.Apply(ctx, core.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	e.selectEngine(t, "edge")

	// Health check fails once edge serves the ports.
	e.edge.mu.Lock()
	e.edge.onApply = func() {
		e.edge.mu.Lock()
		stop := e.edge.lastApply.Stop
		e.edge.mu.Unlock()
		if stop {
			e.upstream.Store(200)
		} else {
			e.upstream.Store(502)
		}
	}
	e.edge.mu.Unlock()
	e.resetCalls()
	v2, err := e.svc.Apply(ctx, core.ApplyOptions{})
	if err != nil || v2.Status != "rolled_back" || v2.RolledBackTo == nil || *v2.RolledBackTo != 1 {
		t.Fatalf("v2 = %+v %v", v2, err)
	}
	if !strings.HasPrefix(v2.Error, "grafana.home.lan returned 502") {
		t.Fatalf("v2 error = %q", v2.Error)
	}
	if got := strings.Join(e.calls, ","); got != "nginx:stop,edge:apply,edge:stop,nginx:start" {
		t.Fatalf("calls = %s", got)
	}
	row2, _ := e.app.Store.GetVersion(ctx, v2.ID, false)
	if row2.FailedEngine != "edge" || row2.FailedStage != "health" || row2.ProxyEngine != "edge" {
		t.Fatalf("row2 = %+v", row2)
	}
	if !e.nginx.running || e.edge.running || agent.ReadProxyEngine(e.app.Config.RunDir) != "nginx" || e.app.ProxyEngine(ctx) != "nginx" {
		t.Fatalf("after rollback nginx=%v edge=%v", e.nginx.running, e.edge.running)
	}
	if p, _ := e.svc.Pending(ctx); p.Count != 1 {
		t.Fatalf("switch must stay pending: %+v", p)
	}

	// Edge fails to start.
	e.edge.mu.Lock()
	e.edge.onApply = nil
	e.edge.applyFail = "start"
	e.edge.mu.Unlock()
	e.resetCalls()
	v3, err := e.svc.Apply(ctx, core.ApplyOptions{})
	if err != nil || v3.Status != "rolled_back" || !strings.HasPrefix(v3.Error, "Relay Edge failed to start after apply v3. v1 is live again") {
		t.Fatalf("v3 = %+v %v", v3, err)
	}
	if got := strings.Join(e.calls, ","); got != "nginx:stop,edge:apply,edge:stop,nginx:start" {
		t.Fatalf("calls = %s", got)
	}

	// Edge unreachable during validation: 503, nothing touched.
	e.edge.mu.Lock()
	e.edge.applyFail = ""
	e.edge.mu.Unlock()
	e.app.Edge = agent.NewClient("edge", e.app.Config.RunDir+"/missing.sock")
	e.resetCalls()
	var ae *ApplyError
	if _, err := e.svc.Apply(ctx, core.ApplyOptions{}); !errors.As(err, &ae) || ae.Status != http.StatusServiceUnavailable || !strings.Contains(ae.Message, "Relay Edge engine is unreachable") {
		t.Fatalf("unreachable edge: %v", err)
	}
	if len(e.calls) != 0 {
		t.Fatalf("calls = %v", e.calls)
	}
}

func TestPreviewUsesActiveEngine(t *testing.T) {
	e := newTestEnv(t)
	fakeEdgeRenderer(t)
	r := chi.NewRouter()
	Routes(e.app, r)
	preview := func(path string) configPreview {
		body, _ := json.Marshal(map[string]any{"host": model.ProxyHost{Domains: []string{"new.home.lan"}, Enabled: true, HSTS: "inherit",
			Upstream: model.Upstream{Scheme: "http", Host: "10.0.0.5", Port: 80}}})
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)).WithContext(e.ctx))
		var out configPreview
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
		return out
	}
	if p := preview("/preview/proxy/host"); p.Engine != "nginx" || !strings.Contains(p.Config, "new.home.lan") || !p.Valid {
		t.Fatalf("nginx preview = %+v", p)
	}

	// First apply with Relay Edge selected: nginx (bootstrap) is stopped
	// without files and edge starts.
	e.selectEngine(t, "edge")
	e.resetCalls()
	if v, err := e.svc.Apply(e.ctx, core.ApplyOptions{}); err != nil || v.Status != "live" {
		t.Fatalf("apply = %+v %v", v, err)
	}
	if got := strings.Join(e.calls, ","); got != "nginx:stop,edge:apply" || e.nginx.lastApply.Hash != "" {
		t.Fatalf("calls = %s (nginx req %+v)", got, e.nginx.lastApply)
	}
	for _, path := range []string{"/preview/proxy/host", "/preview/nginx/host"} {
		if p := preview(path); p.Engine != "edge" || p.Config != "edge host new.home.lan" || !p.Valid {
			t.Fatalf("%s = %+v", path, p)
		}
	}
	body, _ := json.Marshal(map[string]any{"stream": model.Stream{Name: "mc", Protocol: "tcp", ListenPorts: "25565", ForwardHost: "10.0.0.9", ForwardPorts: "25565", Enabled: true}})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/preview/proxy/stream", bytes.NewReader(body)).WithContext(e.ctx))
	var sp configPreview
	json.Unmarshal(rec.Body.Bytes(), &sp)
	if sp.Engine != "edge" || sp.Config != "edge stream mc" {
		t.Fatalf("stream preview = %s", rec.Body.String())
	}
}
