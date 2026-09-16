package apply

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// fakeContainers stops a fake agent's "container" by making it unreachable.
type fakeContainers struct {
	mu     sync.Mutex
	agents map[string]*fakeAgent
	starts []string
	stops  []string
}

func (f *fakeContainers) ContainerState(_ context.Context, engine string) string {
	a := f.agents[engine]
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.down {
		return "stopped"
	}
	return "running"
}

func (f *fakeContainers) StartContainer(_ context.Context, engine string) error {
	f.mu.Lock()
	f.starts = append(f.starts, engine)
	f.mu.Unlock()
	a := f.agents[engine]
	a.mu.Lock()
	a.down = false
	a.mu.Unlock()
	return nil
}

func (f *fakeContainers) StopContainer(_ context.Context, engine, _ string) error {
	f.mu.Lock()
	f.stops = append(f.stops, engine)
	f.mu.Unlock()
	a := f.agents[engine]
	a.mu.Lock()
	a.down, a.running = true, false
	a.mu.Unlock()
	return nil
}

func (f *fakeContainers) calls() (starts, stops string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.starts, ","), strings.Join(f.stops, ",")
}

func withContainers(e *testEnv) *fakeContainers {
	fc := &fakeContainers{agents: map[string]*fakeAgent{"nginx": e.nginx, "edge": e.edge, "haproxy": e.haproxy, "balancer": e.balancer, "tunnel": e.tunnel}}
	e.app.Containers = fc
	return fc
}

func TestIdleEngineContainersStopAndStart(t *testing.T) {
	e := newTestEnv(t)
	fc := withContainers(e)
	ctx := e.ctx
	e.createHost(t, "grafana.home.lan")
	if _, err := e.svc.Apply(ctx, core.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}

	// Relay Edge and Relay Balancer aren't selected and HAProxy has nothing to run.
	e.svc.reconcile(ctx)
	if starts, stops := fc.calls(); stops != "edge,haproxy,balancer,tunnel" || starts != "" {
		t.Fatalf("starts %q stops %q", starts, stops)
	}
	st, _ := e.svc.Status(ctx)
	if !st.Edge.Standby || st.Edge.Container != "stopped" || !st.HAProxy.Standby || !st.Balancer.Standby || st.Nginx.Standby || !st.Nginx.Reachable {
		t.Fatalf("status = nginx %+v edge %+v haproxy %+v", st.Nginx, st.Edge, st.HAProxy)
	}
	e.svc.reconcile(ctx)
	if _, stops := fc.calls(); stops != "edge,haproxy,balancer,tunnel" {
		t.Fatalf("second reconcile stopped again: %q", stops)
	}

	// Starting a proxy engine that isn't selected is refused.
	if _, err := e.svc.EngineAction(ctx, "edge", "start"); err == nil || !strings.Contains(err.Error(), "isn't the selected proxy engine") {
		t.Fatalf("start edge: %v", err)
	}

	// Switching to Relay Edge starts its container; nginx's stops afterwards.
	gen, err := store.LoadSettings[model.GeneralSettings](ctx, e.app.Store, model.SettingsGeneral)
	if err != nil {
		t.Fatal(err)
	}
	gen.ProxyEngine = "edge"
	if err := e.app.Store.PutSettings(ctx, model.SettingsGeneral, gen); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Apply(ctx, core.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if starts, _ := fc.calls(); starts != "edge" {
		t.Fatalf("starts %q", starts)
	}
	e.svc.reconcile(ctx)
	if _, stops := fc.calls(); stops != "edge,haproxy,balancer,tunnel,nginx" {
		t.Fatalf("stops %q", stops)
	}
	st, _ = e.svc.Status(ctx)
	if !st.Nginx.Standby || st.Edge.Standby || !st.Edge.Running {
		t.Fatalf("after switch: nginx %+v edge %+v", st.Nginx, st.Edge)
	}
}

func TestHAProxyStartStopManagesContainer(t *testing.T) {
	e := newTestEnv(t)
	fc := withContainers(e)
	ctx := e.ctx
	e.createHost(t, "grafana.home.lan")
	if _, err := e.svc.Apply(ctx, core.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	e.svc.reconcile(ctx)

	resp, err := e.svc.EngineAction(ctx, "haproxy", "start")
	if err != nil || !resp.OK {
		t.Fatalf("start: %+v %v", resp, err)
	}
	if starts, _ := fc.calls(); starts != "haproxy" {
		t.Fatalf("starts %q", starts)
	}
	// Running on request: reconcile leaves it alone.
	e.svc.reconcile(ctx)
	if _, stops := fc.calls(); stops != "edge,haproxy,balancer,tunnel" {
		t.Fatalf("stops %q", stops)
	}

	if resp, err := e.svc.EngineAction(ctx, "haproxy", "stop"); err != nil || !resp.OK {
		t.Fatalf("stop: %+v %v", resp, err)
	}
	if _, stops := fc.calls(); stops != "edge,haproxy,balancer,tunnel,haproxy" {
		t.Fatalf("stops %q", stops)
	}
	st, _ := e.svc.Status(ctx)
	if !st.HAProxy.Standby || !e.svc.stoppedEngines(ctx)["haproxy"] {
		t.Fatalf("haproxy %+v", st.HAProxy)
	}
	if _, err := e.svc.EngineAction(ctx, "haproxy", "reload"); err == nil || !strings.Contains(err.Error(), "container is stopped") {
		t.Fatalf("reload stopped haproxy: %v", err)
	}
}
