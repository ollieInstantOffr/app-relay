package hostsapi

import (
	"context"
	"testing"

	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// Snippets are saved with either engine so switching back to nginx applies them.
func TestCustomNginxKeptWithRelayEdge(t *testing.T) {
	app, _ := newTestApp(t)
	ctx := context.Background()
	host := func(domain, snippet string) *model.ProxyHost {
		return &model.ProxyHost{Domains: []string{domain}, Enabled: true, HSTS: "inherit", CustomNginx: snippet,
			Upstream: model.Upstream{Scheme: "http", Host: "10.0.0.21", Port: 3000}}
	}
	if err := saveHost(app, nil, host("a.home.lan", "add_header X-Test 1;")); err != nil {
		t.Fatalf("nginx allows snippets: %v", err)
	}

	gen := store.DefaultGeneral()
	gen.ProxyEngine = "edge"
	if err := app.Store.PutSettings(ctx, model.SettingsGeneral, gen); err != nil {
		t.Fatal(err)
	}
	if err := saveHost(app, nil, host("b.home.lan", "add_header X-Test 1;")); err != nil {
		t.Fatalf("edge keeps snippets: %v", err)
	}
	if err := saveHost(app, nil, host("c.home.lan", "  \n")); err != nil {
		t.Fatalf("blank snippet is fine: %v", err)
	}
}
