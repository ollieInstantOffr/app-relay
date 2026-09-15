package logs

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/model"
)

func TestClientSource(t *testing.T) {
	for ip, want := range map[string]string{
		"192.168.1.24": "lan", "10.0.0.5": "lan", "127.0.0.1": "lan", "fe80::1": "lan",
		"100.101.102.103": "vpn", "fd7a:115c:a1e0::1": "vpn", "::ffff:100.64.0.1": "vpn",
		"203.0.113.7": "internet", "2a01:4f8::1": "internet", "": "internet",
	} {
		if got := ClientSource(ip); got != want {
			t.Errorf("ClientSource(%q) = %s, want %s", ip, got, want)
		}
	}
}

func TestTopologyEndpoints(t *testing.T) {
	app := testApp(t)
	ctx := context.Background()
	host := &model.ProxyHost{Domains: []string{"app.home.lan"}, Enabled: true, Upstream: model.Upstream{Scheme: "http", Host: "10.0.0.1", Port: 80}}
	if err := app.Store.Hosts().Create(ctx, host); err != nil {
		t.Fatal(err)
	}
	g := newIngester(app, newNameCache())
	now := time.Now().UTC()
	g.add(chunk{src: srcAccess, lines: [][]byte{
		accessLine(now.Add(-2*time.Minute), host.ID, "app.home.lan", "GET", "/", 200, 0.020, "192.168.1.24", "a"),
		accessLine(now.Add(-2*time.Minute), host.ID, "app.home.lan", "GET", "/", 200, 0.040, "192.168.1.25", "a"),
		accessLine(now.Add(-1*time.Minute), host.ID, "app.home.lan", "GET", "/", 502, 0.010, "100.90.1.2", "a"),
		accessLine(now.Add(-1*time.Minute), host.ID, "app.home.lan", "GET", "/", 429, 0.001, "203.0.113.9", "a"),
		accessLine(now.Add(-1*time.Minute), host.ID, "app.home.lan", "GET", "/admin", 403, 0.001, "203.0.113.9", "a"),
		accessLine(now.Add(-2*time.Hour), host.ID, "app.home.lan", "GET", "/old", 200, 0.01, "203.0.113.10", "a"),
	}})
	g.flush(ctx)

	r := chi.NewRouter()
	Routes(app, r)
	srv := httptest.NewServer(r)
	defer srv.Close()
	c := apiClient{t, srv}

	var topo Topology
	if code := c.do("GET", "/metrics/topology?range=15m", nil, &topo); code != 200 {
		t.Fatalf("topology: %d", code)
	}
	if topo.Totals.Requests != 5 || topo.Hosts[host.ID].Requests != 5 || topo.Hosts[host.ID].S5xx != 1 {
		t.Fatalf("totals = %+v host = %+v", topo.Totals, topo.Hosts[host.ID])
	}
	if topo.Totals.RPS <= 0 || topo.Totals.P95Ms == nil {
		t.Fatalf("rates missing: %+v", topo.Totals)
	}
	cl := topo.Clients
	if cl.Unique != 4 || cl.LAN != 2 || cl.VPN != 1 || cl.Internet != 1 || cl.Blocked != 1 || cl.Requests != 5 {
		t.Fatalf("clients = %+v", cl)
	}

	if hc := topo.HostClients[host.ID]; hc.Unique != 4 || hc.LAN != 2 || hc.Blocked != 1 {
		t.Fatalf("host clients = %+v", hc)
	}

	var flow HostFlow
	if code := c.do("GET", "/metrics/topology/hosts/"+host.ID+"?range=15m", nil, &flow); code != 200 {
		t.Fatalf("host flow: %d", code)
	}
	if flow.Traffic.Requests != 5 || flow.RateLimitedPct != 20 || flow.ErrorPct != 20 {
		t.Fatalf("flow = %+v", flow)
	}
	if len(flow.Upstreams) != 1 || flow.Upstreams[0].Addr != "10.0.0.1:80" || flow.Upstreams[0].SharePct != 100 || flow.Timing.UpstreamMs == nil {
		t.Fatalf("upstreams = %+v timing = %+v", flow.Upstreams, flow.Timing)
	}
	if code := c.do("GET", "/metrics/topology?range=2d", nil, nil); code != 400 {
		t.Fatalf("bad range: %d", code)
	}
	if code := c.do("GET", "/metrics/topology/hosts/nope", nil, nil); code != 404 {
		t.Fatalf("missing host: %d", code)
	}
}
