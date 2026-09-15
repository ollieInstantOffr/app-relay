package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/instantoffr/relay/internal/model"
)

// TestAdminHostBodyMigration re-runs 120_admin_host_body.sql: only the admin
// UI host without a size set gets an unlimited body size.
func TestAdminHostBodyMigration(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	up := model.Upstream{Scheme: "http", Host: "127.0.0.1", Port: 8181}
	sys := &model.ProxyHost{Domains: []string{"relay.example.com"}, Upstream: up, System: true}
	tuned := &model.ProxyHost{Domains: []string{"relay2.example.com"}, Upstream: up, System: true, MaxBodySize: "10g"}
	plain := &model.ProxyHost{Domains: []string{"app.example.com"}, Upstream: up}
	for _, h := range []*model.ProxyHost{sys, tuned, plain} {
		if err := st.Hosts().Create(ctx, h); err != nil {
			t.Fatal(err)
		}
	}
	sql, err := migrationsFS.ReadFile("migrations/120_admin_host_body.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, string(sql)); err != nil {
		t.Fatal(err)
	}
	for h, want := range map[*model.ProxyHost]string{sys: "0", tuned: "10g", plain: ""} {
		got, err := st.Hosts().Get(ctx, h.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.MaxBodySize != want || got.System != h.System {
			t.Errorf("%s: maxBodySize = %q, want %q", h.Domains[0], got.MaxBodySize, want)
		}
	}
}
