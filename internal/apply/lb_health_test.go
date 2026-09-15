package apply

import (
	"strings"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/model"
)

const (
	statAPIUp   = "# pxname,svname,status\nfe-api,FRONTEND,OPEN\napi,api-1,UP\napi,BACKEND,UP\n"
	statAPIDown = "# pxname,svname,status\nfe-api,FRONTEND,OPEN\napi,api-1,DOWN\napi,BACKEND,DOWN\n"
)

// After a load balancer switch, a backend that had a usable server on the old
// engine but has none on the new one fails the health check even though the
// frontend accepts connections (e.g. a stream whose servers the new engine
// can't reach), and the switch is rolled back.
func TestLBSwitchRollsBackWhenBackendLosesServers(t *testing.T) {
	e := newTestEnv(t)
	fakeBalancerRenderer(t)
	if e.svc.healthWindow < 3*time.Second {
		e.svc.healthWindow = 3 * time.Second
	}
	ctx := e.ctx
	e.createPool(t, "api.home.lan")
	if _, err := e.svc.Apply(ctx, core.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	e.haproxy.mu.Lock()
	e.haproxy.runtime = statAPIUp
	e.haproxy.mu.Unlock()
	e.balancer.mu.Lock()
	e.balancer.runtime = statAPIDown
	e.balancer.mu.Unlock()
	e.selectLB(t, "balancer")

	v2, err := e.svc.Apply(ctx, core.ApplyOptions{})
	if err != nil || v2.Status != "rolled_back" {
		t.Fatalf("v2 = %+v %v", v2, err)
	}
	if !strings.HasPrefix(v2.Error, "backend api has no usable server on Relay Balancer") {
		t.Fatalf("v2 error = %q", v2.Error)
	}
	row, _ := e.app.Store.GetVersion(ctx, v2.ID, false)
	if row.FailedEngine != "balancer" || row.FailedStage != "health" {
		t.Fatalf("row = %+v", row)
	}
	if e.app.LBEngine(ctx) != "haproxy" {
		t.Fatal("load balancer engine must stay haproxy")
	}

	// Same servers healthy on Relay Balancer: the switch goes through.
	e.balancer.mu.Lock()
	e.balancer.runtime = statAPIUp
	e.balancer.mu.Unlock()
	v3, err := e.svc.Apply(ctx, core.ApplyOptions{})
	if err != nil || v3.Status != "live" {
		t.Fatalf("v3 = %+v %v", v3, err)
	}
	if e.app.LBEngine(ctx) != "balancer" {
		t.Fatal("load balancer engine should be balancer")
	}
}

func TestCheckSettleTicks(t *testing.T) {
	snap := &model.Snapshot{}
	if got := checkSettleTicks(snap); got != 4 {
		t.Fatalf("default = %d", got)
	}
	snap.HAProxy.CheckInterval = "5s"
	snap.Backends = []model.Backend{{HealthCheck: model.HealthCheck{Interval: "7500ms"}}}
	if got := checkSettleTicks(snap); got != 10 {
		t.Fatalf("longest interval = %d", got)
	}
	for in, want := range map[string]int64{"250": 250, "2s": 2000, "1m": 60000, "500us": 0, "3ms": 3} {
		if got, ok := durationMs(in); !ok || got != want {
			t.Errorf("durationMs(%q) = %d %v", in, got, ok)
		}
	}
	if _, ok := durationMs("soon"); ok {
		t.Error("invalid duration accepted")
	}
}
