package backup

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

func newTestService(t *testing.T) (*Service, *core.App) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := core.Config{Version: "test", DataDir: dir, RunDir: dir, LogDir: dir}
	app := core.New(cfg, st, events.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	s := New(app)
	s.ScryptLogN = 10
	return s, app
}

func TestParseAndNextRun(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 9, 14, 2, 30, 0, 0, loc)
	if got := NextRun("03:00", now); !got.Equal(time.Date(2026, 9, 14, 3, 0, 0, 0, loc)) {
		t.Fatalf("next run = %v", got)
	}
	now = time.Date(2026, 9, 14, 3, 0, 0, 0, loc)
	if got := NextRun("03:00", now); !got.Equal(time.Date(2026, 9, 15, 3, 0, 0, 0, loc)) {
		t.Fatalf("next run at due time = %v", got)
	}
	if h, m := parseHHMM("bogus"); h != 3 || m != 0 {
		t.Fatalf("default = %d:%d", h, m)
	}
}

func TestBackupRoundTrip(t *testing.T) {
	s, app := newTestService(t)
	ctx := context.Background()
	st := app.Store

	host := &model.ProxyHost{Domains: []string{"grafana.home.lan"}, Enabled: true, Upstream: model.Upstream{Scheme: "http", Host: "10.0.0.2", Port: 3000}, Source: model.SourceManual}
	if err := st.Hosts().Create(ctx, host); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO users (id, username, role, password_hash, created_at) VALUES ('u1', 'jonas', 'admin', 'x', ?)`, store.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO sessions (id, user_id, created_at, expires_at, last_seen_at) VALUES ('s-old', 'u1', ?, ?, ?)`, store.Now(), store.Now(), store.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.PutKV(ctx, "blob", []byte{0, 1, 2, 255}); err != nil {
		t.Fatal(err)
	}
	set := store.DefaultBackup()
	set.Passphrase = "correct horse battery"
	if err := st.PutSettings(ctx, model.SettingsBackup, set); err != nil {
		t.Fatal(err)
	}
	certDir := filepath.Join(app.Config.DataDir, "certs", "c1")
	os.MkdirAll(certDir, 0o755)
	os.WriteFile(filepath.Join(certDir, "fullchain.pem"), []byte("CHAIN"), 0o644)
	os.WriteFile(filepath.Join(certDir, "privkey.pem"), []byte("KEY"), 0o600)

	if err := st.InsertVersion(ctx, &store.VersionRow{ID: 1, CreatedAt: time.Now(), Actor: "jonas", Status: "live", Snapshot: "{}", NginxFiles: "{}"}); err != nil {
		t.Fatal(err)
	}

	row, err := s.Create(ctx, TriggerManual, "")
	if err != nil {
		t.Fatal(err)
	}
	// Applied after the backup: the engines now run v2.
	if err := st.InsertVersion(ctx, &store.VersionRow{ID: 2, CreatedAt: time.Now(), Actor: "jonas", Status: "draft", Snapshot: "{}", NginxFiles: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := st.PromoteVersion(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if row.Status != "ok" || row.Size == 0 || row.Contents["hosts"] != 1 || row.Contents["users"] != 1 {
		t.Fatalf("unexpected row %+v", row)
	}
	raw, _ := os.ReadFile(filepath.Join(s.Dir(), row.File))
	if bytes.Contains(raw, []byte("grafana.home.lan")) {
		t.Fatal("archive is not encrypted")
	}

	// Mutate state after the backup.
	if err := st.Hosts().Delete(ctx, host.ID); err != nil {
		t.Fatal(err)
	}
	other := &model.ProxyHost{Domains: []string{"new.home.lan"}, Upstream: model.Upstream{Scheme: "http", Host: "10.0.0.3", Port: 80}}
	st.Hosts().Create(ctx, other)
	st.PutKV(ctx, "blob", []byte("changed"))
	os.WriteFile(filepath.Join(certDir, "fullchain.pem"), []byte("OTHER"), 0o644)
	// The admin performing the restore has a session created after the backup.
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO sessions (id, user_id, created_at, expires_at, last_seen_at) VALUES ('s-now', 'u1', ?, ?, ?)`, store.Now(), store.Now(), store.Now()); err != nil {
		t.Fatal(err)
	}

	// Wrong passphrase is rejected without changing anything.
	if _, err := s.RestoreByID(ctx, row.ID, "nope nope nope"); err == nil {
		t.Fatal("expected wrong passphrase error")
	}
	if hosts, _ := st.Hosts().List(ctx); len(hosts) != 1 || hosts[0].ID != other.ID {
		t.Fatal("failed restore changed data")
	}

	actx := core.WithActor(ctx, core.Actor{Type: core.ActorUser, ID: "u1", Name: "jonas", Role: core.RoleAdmin, SessionID: "s-now"})
	res, err := s.RestoreByID(actx, row.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.SessionKept || res.BeforeRestore == "" {
		t.Fatalf("result %+v", res)
	}
	hosts, _ := st.Hosts().List(ctx)
	if len(hosts) != 1 || hosts[0].ID != host.ID || hosts[0].Domains[0] != "grafana.home.lan" {
		t.Fatalf("hosts after restore: %+v", hosts)
	}
	if b, _ := st.GetKV(ctx, "blob"); !bytes.Equal(b, []byte{0, 1, 2, 255}) {
		t.Fatalf("blob = %v", b)
	}
	if b, _ := os.ReadFile(filepath.Join(certDir, "fullchain.pem")); string(b) != "CHAIN" {
		t.Fatalf("cert = %s", b)
	}
	// The local version history is kept: v2 is what the engines run, so the
	// restored configuration must show up as pending against it.
	if live, err := st.LiveVersion(ctx, false); err != nil || live.ID != 2 {
		t.Fatalf("live version after restore = %+v, %v (want 2)", live, err)
	}
	var n int
	st.DB.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id IN ('s-old', 's-now')`).Scan(&n)
	if n != 2 {
		t.Fatalf("sessions = %d", n)
	}
	rows, _ := st.ListBackups(ctx)
	if len(rows) != 2 {
		t.Fatalf("backups = %d (want manual + before-restore)", len(rows))
	}

	// Upload path: restore the before-restore archive from a reader.
	p, _, err := s.Path(ctx, res.BeforeRestore)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(p)
	defer f.Close()
	if _, err := s.Restore(actx, f, "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	hosts, _ = st.Hosts().List(ctx)
	if len(hosts) != 1 || hosts[0].ID != other.ID {
		t.Fatalf("hosts after second restore: %+v", hosts)
	}
}

func TestExcludesPrivateKeys(t *testing.T) {
	s, app := newTestService(t)
	ctx := context.Background()
	certDir := filepath.Join(app.Config.DataDir, "certs", "c1")
	os.MkdirAll(certDir, 0o755)
	os.WriteFile(filepath.Join(certDir, "privkey.pem"), []byte("KEY"), 0o600)
	os.WriteFile(filepath.Join(certDir, "fullchain.pem"), []byte("CHAIN"), 0o644)
	var buf bytes.Buffer
	if _, err := writeArchive(ctx, &buf, app.Store, s.certDir(), "passphrase!", "t", false, 10); err != nil {
		t.Fatal(err)
	}
	a, err := readArchive(&buf, "passphrase!")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Certs["c1/privkey.pem"]; ok {
		t.Fatal("private key included")
	}
	if string(a.Certs["c1/fullchain.pem"]) != "CHAIN" {
		t.Fatal("chain missing")
	}
	if _, ok := a.Tables["access_log"]; ok {
		t.Fatal("traffic table included")
	}
	if _, ok := a.Tables["hosts"]; !ok {
		t.Fatal("hosts table missing")
	}
}

func TestRetention(t *testing.T) {
	s, app := newTestService(t)
	ctx := context.Background()
	set := store.DefaultBackup()
	set.Passphrase = "passphrase!"
	set.Keep = 2
	app.Store.PutSettings(ctx, model.SettingsBackup, set)
	for i := 0; i < 4; i++ {
		if _, err := s.Create(ctx, TriggerScheduled, ""); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := s.Create(ctx, TriggerManual, ""); err != nil {
		t.Fatal(err)
	}
	rows, _ := app.Store.ListBackups(ctx)
	byTrigger := map[string]int{}
	for _, r := range rows {
		byTrigger[r.Trigger]++
		if _, err := os.Stat(filepath.Join(s.Dir(), r.File)); err != nil {
			t.Fatalf("file missing for %s", r.ID)
		}
	}
	if byTrigger[TriggerScheduled] != 2 || byTrigger[TriggerManual] != 1 {
		t.Fatalf("retention: %v", byTrigger)
	}
	entries, _ := os.ReadDir(s.Dir())
	if len(entries) != 3 {
		t.Fatalf("files on disk = %d", len(entries))
	}
}
