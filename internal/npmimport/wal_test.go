package npmimport

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/store"
)

// TestPreviewUploadWALCopy uploads only database.sqlite of a WAL-mode NPM
// database (as copied from a running NPM) and requires the preview to finish.
func TestPreviewUploadWALCopy(t *testing.T) {
	npmDir := fixture(t)
	src := filepath.Join(npmDir, "database.sqlite")
	db, err := sql.Open("sqlite", src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO proxy_host (id, domain_names, forward_host, forward_port, forward_scheme, enabled, locations) VALUES (9, '["wal.example.com"]', '10.0.0.9', 80, 'http', 1, '[]')`)
	// Copy only the main file while the WAL is still open (not checkpointed).
	only := filepath.Join(t.TempDir(), "database.sqlite")
	b, _ := os.ReadFile(src)
	os.WriteFile(only, b, 0o644)
	db.Close()

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	app := core.New(core.Config{DataDir: dir, RunDir: dir, LogDir: dir}, st, events.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	s := New(app)
	f, _ := os.Open(only)
	defer f.Close()
	done := make(chan error, 1)
	go func() {
		pv, err := s.PreviewUpload(context.Background(), f, "database.sqlite")
		if err == nil {
			t.Logf("hosts=%d", pv.Counts["hosts"].Total)
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Logf("preview returned: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("preview of a WAL-mode upload hangs")
	}
	// The store must still answer.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := st.Hosts().List(ctx); err != nil {
		t.Fatalf("store after preview: %v", err)
	}
}
