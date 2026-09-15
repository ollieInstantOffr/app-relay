package apply

import (
	"strings"
	"testing"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

func TestErrorPagesArePendingChanges(t *testing.T) {
	e := newTestEnv(t)
	ctx := e.ctx
	e.createHost(t, "grafana.home.lan")
	if _, err := e.svc.Apply(ctx, core.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if p, _ := e.svc.Pending(ctx); p.Count != 0 {
		t.Fatalf("default error pages must not be a pending change (count %d)", p.Count)
	}

	set, err := store.LoadSettings[model.ErrorPagesSettings](ctx, e.app.Store, model.SettingsErrorPages)
	if err != nil {
		t.Fatal(err)
	}
	set.Enabled = true
	if err := e.app.Store.PutSettings(ctx, model.SettingsErrorPages, set); err != nil {
		t.Fatal(err)
	}
	if p, _ := e.svc.Pending(ctx); p.Count != 1 {
		t.Fatalf("enabling error pages: pending count %d", p.Count)
	}
	if _, err := e.svc.Apply(ctx, core.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	e.nginx.mu.Lock()
	page := e.nginx.lastApply.Files["errors/502.html"]
	e.nginx.mu.Unlock()
	if !strings.Contains(page, "Service unavailable") {
		t.Fatalf("applied files lack errors/502.html: %q", page)
	}
}
