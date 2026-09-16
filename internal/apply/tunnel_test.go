package apply

import (
	"strings"
	"testing"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/tunnel"
)

func TestApplyTunnelEngine(t *testing.T) {
	e := newTestEnv(t)
	ctx := e.ctx
	host := e.createHost(t, "grafana.home.lan")
	if v, err := e.svc.Apply(ctx, core.ApplyOptions{}); err != nil || v.Status != "live" {
		t.Fatalf("first apply: %+v %v", v, err)
	}
	if e.tunnel.applies != 0 {
		t.Fatalf("tunnel engine touched without anything published: %d applies", e.tunnel.applies)
	}

	// Publishing a host starts the tunnel engine with a release routing it.
	host.TunnelGatewayID = "gw1"
	if err := e.app.Store.Hosts().Update(ctx, host); err != nil {
		t.Fatal(err)
	}
	v, err := e.svc.Apply(ctx, core.ApplyOptions{})
	if err != nil || v.Status != "live" {
		t.Fatalf("publish: %+v %v", v, err)
	}
	e.tunnel.mu.Lock()
	req := e.tunnel.lastApply
	running := e.tunnel.running
	e.tunnel.mu.Unlock()
	if req.Stop || !running || !strings.Contains(req.Files[tunnel.ConfigFile], `"grafana.home.lan"`) {
		t.Fatalf("tunnel apply %+v running %v", req, running)
	}
	live, _ := e.app.Store.LiveVersion(ctx, true)
	if !live.TunnelRunning || live.TunnelHash == "" || !strings.Contains(live.TunnelFiles, "grafana.home.lan") {
		t.Fatalf("live version tunnel fields: running %v hash %q", live.TunnelRunning, live.TunnelHash)
	}
	if !strings.Contains(e.nginx.lastApply.Files["conf.d/hosts/grafana.home.lan.conf"], "unix:") {
		t.Error("nginx has no tunnel listener for the published host")
	}

	// A tunnel release the engine rejects fails before anything changes.
	e.tunnel.mu.Lock()
	e.tunnel.validateOK, e.tunnel.validateOut = false, "tunnel.json: bad"
	e.tunnel.mu.Unlock()
	host.Domains = []string{"grafana2.home.lan"}
	e.app.Store.Hosts().Update(ctx, host)
	if v, _ := e.svc.Apply(ctx, core.ApplyOptions{}); v == nil || v.Status != "failed" {
		t.Fatalf("rejected tunnel release: %+v", v)
	}
	e.tunnel.mu.Lock()
	e.tunnel.validateOK = true
	e.tunnel.mu.Unlock()

	// A tunnel engine that fails to take the release rolls everything back.
	e.tunnel.mu.Lock()
	e.tunnel.applyFail = "reload"
	e.tunnel.mu.Unlock()
	nginxRollbacks := e.nginx.rollbacks
	if v, _ := e.svc.Apply(ctx, core.ApplyOptions{}); v == nil || v.Status != "rolled_back" {
		t.Fatalf("failed tunnel reload: %+v", v)
	}
	if e.nginx.rollbacks != nginxRollbacks+1 {
		t.Error("proxy engine not rolled back after the tunnel engine failed")
	}
	e.tunnel.mu.Lock()
	e.tunnel.applyFail = ""
	e.tunnel.mu.Unlock()

	// Unpublishing stops the tunnel engine.
	host.TunnelGatewayID = ""
	host.Domains = []string{"grafana.home.lan"}
	e.app.Store.Hosts().Update(ctx, host)
	if v, err := e.svc.Apply(ctx, core.ApplyOptions{}); err != nil || v.Status != "live" {
		t.Fatalf("unpublish: %+v %v", v, err)
	}
	e.tunnel.mu.Lock()
	defer e.tunnel.mu.Unlock()
	if !e.tunnel.lastApply.Stop || e.tunnel.running {
		t.Fatalf("tunnel engine still running after unpublishing: %+v", e.tunnel.lastApply)
	}
}
