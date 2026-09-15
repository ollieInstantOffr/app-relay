package apply

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/instantoffr/relay/internal/agent"
	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/lb/lbengine"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/render"
	"github.com/instantoffr/relay/internal/store"
)

// fakeBalancerRenderer replaces the Relay Balancer renderer with a
// deterministic one.
func fakeBalancerRenderer(t *testing.T) {
	t.Helper()
	old := lbengine.Renderers[agent.EngineBalancer]
	r := old
	r.Render = func(s *model.Snapshot, _ render.Env) (agent.Files, error) {
		names := []string{}
		for _, b := range s.Backends {
			names = append(names, b.Name)
		}
		return agent.Files{"balancer.json": fmt.Sprintf("{\"schema\":1,\"backends\":%q}\n", strings.Join(names, ","))}, nil
	}
	r.HasBackends = func(s *model.Snapshot) bool { return len(s.Backends) > 0 }
	r.Backend = func(_ *model.Snapshot, b *model.Backend) string { return `{"name":"` + b.Name + `"}` }
	r.Frontend = func(_ *model.Snapshot, f *model.Frontend) string { return `{"name":"` + f.Name + `"}` }
	lbengine.Renderers[agent.EngineBalancer] = r
	t.Cleanup(func() { lbengine.Renderers[agent.EngineBalancer] = old })
}

func (e *testEnv) selectLB(t *testing.T, engine string) {
	t.Helper()
	gen, err := store.LoadSettings[model.GeneralSettings](e.ctx, e.app.Store, model.SettingsGeneral)
	if err != nil {
		t.Fatal(err)
	}
	gen.LBEngine = engine
	if err := e.app.Store.PutSettings(e.ctx, model.SettingsGeneral, gen); err != nil {
		t.Fatal(err)
	}
}

// createPool adds a backend and a host routed through it.
func (e *testEnv) createPool(t *testing.T, domain string) *model.Backend {
	t.Helper()
	b := &model.Backend{Name: "api", Mode: "http", Algorithm: "roundrobin", Source: "manual",
		Servers: []model.Server{{ID: "s1", Name: "api-1", Address: "10.0.0.81", Port: 9000, Weight: 100, Role: "active", State: "ready"}}}
	if err := e.app.Store.Backends().Create(e.ctx, b); err != nil {
		t.Fatal(err)
	}
	h := &model.ProxyHost{Domains: []string{domain}, Enabled: true, HSTS: "inherit",
		Upstream: model.Upstream{Scheme: "http", Host: "127.0.0.1", Port: 10080, BackendID: b.ID}}
	if err := e.app.Store.Hosts().Create(e.ctx, h); err != nil {
		t.Fatal(err)
	}
	return b
}

// logLB makes the load balancer fakes record their calls too.
func (e *testEnv) logLB() {
	e.haproxy.mu.Lock()
	e.haproxy.calls = &e.calls
	e.haproxy.mu.Unlock()
}

func TestLBSwitchToBalancer(t *testing.T) {
	e := newTestEnv(t)
	fakeBalancerRenderer(t)
	ctx := e.ctx
	e.createPool(t, "api.home.lan")
	if _, err := e.svc.Apply(ctx, core.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	live1, _ := e.app.Store.LiveVersion(ctx, true)
	if live1.LBEngine != "haproxy" || !live1.HAProxyRunning || !strings.Contains(live1.HAProxyCfg, "backend api") || !e.haproxy.running {
		t.Fatalf("v1: lb=%q running=%v cfg=%q", live1.LBEngine, live1.HAProxyRunning, live1.HAProxyCfg)
	}

	// The switch is a pending General settings change with its own label.
	e.selectLB(t, "balancer")
	p, summary, err := e.svc.pendingWithSummary(ctx)
	if err != nil || p.Count != 1 || p.Items[0].ID != model.SettingsGeneral || summary != "Load balancer engine: HAProxy → Relay Balancer" {
		t.Fatalf("pending = %+v %q %v", p, summary, err)
	}
	rec := httptest.NewRecorder()
	(&handlers{app: e.app}).pendingDiff(rec, httptest.NewRequest(http.MethodGet, "/pending/diff", nil))
	var diff diffResponse
	json.Unmarshal(rec.Body.Bytes(), &diff)
	status := map[string]string{}
	for _, f := range diff.Files {
		status[f.Path] = f.Status
	}
	if status["balancer/balancer.json"] != "added" || status["haproxy.cfg"] != "removed" || len(status) != 2 {
		t.Fatalf("diff = %v", status)
	}

	e.logLB()
	e.resetCalls()
	v2, err := e.svc.Apply(ctx, core.ApplyOptions{})
	if err != nil || v2.Status != "live" {
		t.Fatalf("v2 = %+v %v", v2, err)
	}
	// The old engine stops before the new one starts; then the proxy engine.
	if got := strings.Join(e.calls, ","); got != "haproxy:stop,balancer:apply,nginx:apply" {
		t.Fatalf("calls = %s", got)
	}
	row2, _ := e.app.Store.GetVersion(ctx, v2.ID, true)
	if row2.LBEngine != "balancer" || toVersionJSON(row2).LBEngine != "balancer" || row2.HAProxyCfg != "{\"schema\":1,\"backends\":\"api\"}\n" || !row2.HAProxyRunning {
		t.Fatalf("v2 row = lb %q cfg %q", row2.LBEngine, row2.HAProxyCfg)
	}
	if _, ok := versionFileMap(row2)["balancer/balancer.json"]; !ok {
		t.Fatalf("version files = %v", versionFileMap(row2))
	}
	e.haproxy.mu.Lock()
	if !e.haproxy.lastApply.Stop || e.haproxy.lastApply.Hash != live1.HAProxyHash || e.haproxy.lastApply.Files["haproxy.cfg"] != live1.HAProxyCfg || e.haproxy.running {
		t.Fatalf("haproxy stop request = %+v running=%v", e.haproxy.lastApply, e.haproxy.running)
	}
	e.haproxy.mu.Unlock()
	e.balancer.mu.Lock()
	if !e.balancer.running || e.balancer.hash != row2.HAProxyHash || e.balancer.lastApply.Files["balancer.json"] == "" {
		t.Fatalf("balancer running=%v hash=%s", e.balancer.running, e.balancer.hash)
	}
	e.balancer.mu.Unlock()
	if e.app.LBEngine(ctx) != "balancer" || e.app.ProxyEngine(ctx) != "nginx" {
		t.Fatal("load balancer engine not recorded")
	}
	if p, _ := e.svc.Pending(ctx); p.Count != 0 {
		t.Fatalf("pending after switch = %+v", p)
	}
	if es, _ := e.svc.Status(ctx); es.LB != "balancer" || es.Proxy != "nginx" || !es.Balancer.Reachable || es.Balancer.Engine != "balancer" {
		t.Fatalf("status = %+v", es)
	}
	if files, _, _, _, _ := e.svc.LiveRelease(ctx, "haproxy"); files != nil {
		t.Fatal("haproxy has no live release while Relay Balancer is selected")
	}
	if files, hash, running, _, _ := e.svc.LiveRelease(ctx, "balancer"); files["balancer.json"] == "" || hash != row2.HAProxyHash || !running {
		t.Fatalf("balancer live release = %v %s %v", files, hash, running)
	}
	acts, _ := e.app.Store.ListActivity(ctx, 20)
	found := false
	for _, a := range acts {
		if a.Title == "Switched load balancer from HAProxy to Relay Balancer" {
			found = true
		}
	}
	if !found {
		t.Fatalf("activity = %+v", acts)
	}

	// Reconcile stops HAProxy if it runs again …
	e.haproxy.mu.Lock()
	e.haproxy.running = true
	stops := e.haproxy.stops
	e.haproxy.mu.Unlock()
	balApplies := e.balancer.applies
	e.svc.reconcile(ctx)
	e.haproxy.mu.Lock()
	if e.haproxy.stops != stops+1 || e.haproxy.running || e.haproxy.lastApply.Hash != "" {
		t.Fatalf("reconcile: stops=%d running=%v req=%+v", e.haproxy.stops, e.haproxy.running, e.haproxy.lastApply)
	}
	e.haproxy.mu.Unlock()
	if e.balancer.applies != balApplies {
		t.Fatal("reconcile must not touch the balancer when it runs the live version")
	}
	// … and restores the live release on Relay Balancer.
	e.balancer.mu.Lock()
	e.balancer.hash = ""
	e.balancer.running = false
	e.balancer.mu.Unlock()
	e.svc.reconcile(ctx)
	e.balancer.mu.Lock()
	defer e.balancer.mu.Unlock()
	if e.balancer.hash != row2.HAProxyHash || !e.balancer.running {
		t.Fatal("reconcile did not restore the balancer")
	}
}

// HAProxy → Relay Balancer → HAProxy changes no stored configuration.
func TestLBSwitchRoundTrip(t *testing.T) {
	e := newTestEnv(t)
	fakeBalancerRenderer(t)
	ctx := e.ctx
	e.createPool(t, "api.home.lan")
	if _, err := e.svc.Apply(ctx, core.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	v1, _ := e.app.Store.LiveVersion(ctx, true)
	before, _ := e.app.Store.Backends().List(ctx)

	e.selectLB(t, "balancer")
	if v, err := e.svc.Apply(ctx, core.ApplyOptions{}); err != nil || v.Status != "live" {
		t.Fatalf("switch to balancer: %+v %v", v, err)
	}
	e.selectLB(t, "haproxy")
	v3, err := e.svc.Apply(ctx, core.ApplyOptions{})
	if err != nil || v3.Status != "live" {
		t.Fatalf("switch back: %+v %v", v3, err)
	}
	row3, _ := e.app.Store.GetVersion(ctx, v3.ID, true)
	if row3.LBEngine != "haproxy" || row3.HAProxyHash != v1.HAProxyHash || row3.HAProxyCfg != v1.HAProxyCfg || row3.NginxHash != v1.NginxHash {
		t.Fatalf("haproxy release changed after the round trip: lb=%q hash %s vs %s", row3.LBEngine, row3.HAProxyHash, v1.HAProxyHash)
	}
	e.haproxy.mu.Lock()
	e.balancer.mu.Lock()
	if !e.haproxy.running || e.haproxy.hash != v1.HAProxyHash || e.balancer.running {
		t.Fatalf("haproxy running=%v hash=%s balancer running=%v", e.haproxy.running, e.haproxy.hash, e.balancer.running)
	}
	e.balancer.mu.Unlock()
	e.haproxy.mu.Unlock()
	after, _ := e.app.Store.Backends().List(ctx)
	if fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatal("backends changed during the round trip")
	}
	if p, _ := e.svc.Pending(ctx); p.Count != 0 || e.app.LBEngine(ctx) != "haproxy" {
		t.Fatalf("pending after round trip = %+v", p)
	}
}

func TestLBSwitchRollsBack(t *testing.T) {
	e := newTestEnv(t)
	fakeBalancerRenderer(t)
	ctx := e.ctx
	e.createPool(t, "api.home.lan")
	if _, err := e.svc.Apply(ctx, core.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	live1, _ := e.app.Store.LiveVersion(ctx, true)
	e.selectLB(t, "balancer")
	e.logLB()

	// Hosts routed through a backend fail once Relay Balancer serves the frontends.
	e.balancer.mu.Lock()
	e.balancer.onApply = func() {
		e.balancer.mu.Lock()
		stop := e.balancer.lastApply.Stop
		e.balancer.mu.Unlock()
		if stop {
			e.upstream.Store(200)
		} else {
			e.upstream.Store(502)
		}
	}
	e.balancer.mu.Unlock()
	e.resetCalls()
	v2, err := e.svc.Apply(ctx, core.ApplyOptions{})
	if err != nil || v2.Status != "rolled_back" || v2.RolledBackTo == nil || *v2.RolledBackTo != 1 {
		t.Fatalf("v2 = %+v %v", v2, err)
	}
	if !strings.HasPrefix(v2.Error, "api.home.lan returned 502") {
		t.Fatalf("v2 error = %q", v2.Error)
	}
	if got := strings.Join(e.calls, ","); got != "haproxy:stop,balancer:apply,nginx:apply,balancer:stop,haproxy:apply" {
		t.Fatalf("calls = %s", got)
	}
	row2, _ := e.app.Store.GetVersion(ctx, v2.ID, false)
	if row2.FailedEngine != "balancer" || row2.FailedStage != "health" || row2.LBEngine != "balancer" || toVersionJSON(row2).FailedEngine != "balancer" {
		t.Fatalf("row2 = %+v", row2)
	}
	e.haproxy.mu.Lock()
	if !e.haproxy.running || e.haproxy.hash != live1.HAProxyHash || e.haproxy.lastApply.Stop || e.balancer.running {
		t.Fatalf("after rollback haproxy running=%v hash=%s balancer=%v", e.haproxy.running, e.haproxy.hash, e.balancer.running)
	}
	e.haproxy.mu.Unlock()
	if e.app.LBEngine(ctx) != "haproxy" {
		t.Fatal("load balancer engine must stay haproxy")
	}
	if p, _ := e.svc.Pending(ctx); p.Count != 1 {
		t.Fatalf("switch must stay pending: %+v", p)
	}

	// Relay Balancer fails to start.
	e.balancer.mu.Lock()
	e.balancer.onApply = nil
	e.balancer.applyFail = "start"
	e.balancer.mu.Unlock()
	e.resetCalls()
	v3, err := e.svc.Apply(ctx, core.ApplyOptions{})
	if err != nil || v3.Status != "rolled_back" || !strings.HasPrefix(v3.Error, "Relay Balancer failed to start after apply v3. v1 is live again") {
		t.Fatalf("v3 = %+v %v", v3, err)
	}
	if got := strings.Join(e.calls, ","); got != "haproxy:stop,balancer:apply,balancer:stop,haproxy:apply" {
		t.Fatalf("calls = %s", got)
	}

	// Relay Balancer unreachable during validation: 503, nothing touched.
	e.balancer.mu.Lock()
	e.balancer.applyFail = ""
	e.balancer.mu.Unlock()
	e.app.Balancer = agent.NewClient("balancer", e.app.Config.RunDir+"/missing.sock")
	e.resetCalls()
	var ae *ApplyError
	if _, err := e.svc.Apply(ctx, core.ApplyOptions{}); !errors.As(err, &ae) || ae.Status != http.StatusServiceUnavailable ||
		!strings.Contains(ae.Message, "Relay Balancer engine is unreachable") || !strings.Contains(ae.Message, "relay-balancer") {
		t.Fatalf("unreachable balancer: %v", err)
	}
	if len(e.calls) != 0 {
		t.Fatalf("calls = %v", e.calls)
	}
}

func TestBalancerContainersAndActions(t *testing.T) {
	e := newTestEnv(t)
	fakeBalancerRenderer(t)
	fc := withContainers(e)
	ctx := e.ctx
	e.createPool(t, "api.home.lan")
	if _, err := e.svc.Apply(ctx, core.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	e.svc.reconcile(ctx)
	if _, stops := fc.calls(); stops != "edge,balancer" {
		t.Fatalf("stops %q", stops)
	}
	st, _ := e.svc.Status(ctx)
	if !st.Balancer.Standby || st.HAProxy.Standby || st.LB != "haproxy" {
		t.Fatalf("status haproxy %+v balancer %+v", st.HAProxy, st.Balancer)
	}
	// Starting the load balancer engine that isn't selected is refused.
	if _, err := e.svc.EngineAction(ctx, "balancer", "start"); err == nil || !strings.Contains(err.Error(), "Switch in Settings → Load balancer engine") {
		t.Fatalf("start balancer: %v", err)
	}

	// Switching starts the balancer container; HAProxy's stops afterwards.
	e.selectLB(t, "balancer")
	if v, err := e.svc.Apply(ctx, core.ApplyOptions{}); err != nil || v.Status != "live" {
		t.Fatalf("switch: %+v %v", v, err)
	}
	if starts, _ := fc.calls(); starts != "balancer" {
		t.Fatalf("starts %q", starts)
	}
	e.svc.reconcile(ctx)
	if _, stops := fc.calls(); stops != "edge,balancer,haproxy" {
		t.Fatalf("stops %q", stops)
	}
	st, _ = e.svc.Status(ctx)
	if !st.HAProxy.Standby || st.Balancer.Standby || !st.Balancer.Running {
		t.Fatalf("after switch: haproxy %+v balancer %+v", st.HAProxy, st.Balancer)
	}
	if _, err := e.svc.EngineAction(ctx, "haproxy", "start"); err == nil || !strings.Contains(err.Error(), "HAProxy isn't the selected load balancer engine") {
		t.Fatalf("start haproxy: %v", err)
	}

	// Stopping Relay Balancer marks it stopped and stops its container.
	if resp, err := e.svc.EngineAction(ctx, "balancer", "stop"); err != nil || !resp.OK {
		t.Fatalf("stop: %+v %v", resp, err)
	}
	if _, stops := fc.calls(); stops != "edge,balancer,haproxy,balancer" || !e.svc.stoppedEngines(ctx)["balancer"] {
		t.Fatalf("stops %q", stops)
	}
	if files, _, running, _, _ := e.svc.LiveRelease(ctx, "balancer"); files == nil || running {
		t.Fatal("an admin-stopped balancer has a stopped live release")
	}
	// Start brings the container back and clears the mark.
	if resp, err := e.svc.EngineAction(ctx, "balancer", "start"); err != nil || !resp.OK || e.svc.stoppedEngines(ctx)["balancer"] {
		t.Fatalf("start: %+v %v", resp, err)
	}
}
