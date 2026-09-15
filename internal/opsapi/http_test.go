package opsapi

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/backup"
	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/docker"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/notify"
	"github.com/instantoffr/relay/internal/npmimport"
	"github.com/instantoffr/relay/internal/store"
)

func setup(t *testing.T) (*core.App, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	app := core.New(core.Config{Version: "test", DataDir: dir, RunDir: dir, LogDir: dir}, st, events.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	app.Docker = docker.New(app)
	app.Notify = notify.New(app)
	b := backup.New(app)
	b.ScryptLogN = 10
	app.Backup = b
	app.Importer = npmimport.New(app)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(core.WithActor(req.Context(), core.Actor{Type: core.ActorUser, ID: "u1", Name: "jonas", Role: core.RoleAdmin})))
		})
	})
	Routes(app, r)
	return app, r
}

func do(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestRoutes(t *testing.T) {
	app, h := setup(t)
	ctx := t.Context()

	if w := do(t, h, "GET", "/docker/status", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"endpoint":"unix:///var/run/docker.sock"`) {
		t.Fatalf("docker status: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "GET", "/docker/containers", nil); w.Code != 200 || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("containers: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/docker/hosts", map[string]any{"items": []any{}}); w.Code != 503 {
		t.Fatalf("bulk create while disconnected: %d %s", w.Code, w.Body)
	}

	// Backups: passphrase required, then create, list, restore by id, download, delete.
	if w := do(t, h, "POST", "/backups", nil); w.Code != 400 || !strings.Contains(w.Body.String(), "passphrase_required") {
		t.Fatalf("backup without passphrase: %d %s", w.Code, w.Body)
	}
	set := store.DefaultBackup()
	set.Passphrase = "long enough pass"
	app.Store.PutSettings(ctx, model.SettingsBackup, set)
	w := do(t, h, "POST", "/backups", nil)
	if w.Code != 201 {
		t.Fatalf("create backup: %d %s", w.Code, w.Body)
	}
	var row store.BackupRow
	json.Unmarshal(w.Body.Bytes(), &row)
	w = do(t, h, "GET", "/backups", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), row.ID) || !strings.Contains(w.Body.String(), `"destination"`) {
		t.Fatalf("list backups: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "GET", "/backups/"+row.ID+"/download", nil); w.Code != 200 || w.Header().Get("Content-Disposition") == "" || w.Body.Len() != int(row.Size) {
		t.Fatalf("download: %d %v", w.Code, w.Header())
	}
	if w := do(t, h, "POST", "/backups/"+row.ID+"/restore", map[string]string{"passphrase": "wrong wrong"}); w.Code != 400 || !strings.Contains(w.Body.String(), "wrong_passphrase") {
		t.Fatalf("restore wrong passphrase: %d %s", w.Code, w.Body)
	}
	req := httptest.NewRequest("POST", "/backups/"+row.ID+"/restore", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"reviewPending":true`) {
		t.Fatalf("restore empty body: %d %s", rec.Code, rec.Body)
	}

	// Multipart restore upload.
	dl := do(t, h, "GET", "/backups/"+row.ID+"/download", nil)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", row.File)
	fw.Write(dl.Body.Bytes())
	mw.WriteField("passphrase", "long enough pass")
	mw.Close()
	req = httptest.NewRequest("POST", "/backups/restore", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("upload restore: %d %s", rec.Code, rec.Body)
	}
	if w := do(t, h, "DELETE", "/backups/"+row.ID, nil); w.Code != 204 {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}

	// Settings hooks.
	bh := httpx.SettingsHooks[model.SettingsBackup]
	stored := store.DefaultBackup()
	stored.Passphrase = "secret-pass"
	shown := bh.Decorate(nil, &stored).(model.BackupSettings)
	if shown.Passphrase != "" || !shown.PassphraseSet {
		t.Fatalf("backup decorate: %+v", shown)
	}
	next := store.DefaultBackup()
	if err := bh.BeforeSave(nil, &stored, &next); err != nil || next.Passphrase != "secret-pass" {
		t.Fatalf("backup keep passphrase: %v %+v", err, next)
	}
	short := store.DefaultBackup()
	short.Passphrase = "short"
	if err := bh.BeforeSave(nil, &stored, &short); err == nil {
		t.Fatal("short passphrase accepted")
	}
	dh := httpx.SettingsHooks[model.SettingsDocker]
	bad := store.DefaultDocker()
	bad.Endpoint, bad.DomainPattern = "ftp://x", "static.lan"
	if err := dh.BeforeSave(nil, &bad, &bad); err == nil || !strings.Contains(err.Error(), "endpoint") || !strings.Contains(err.Error(), "domainPattern") {
		t.Fatalf("docker settings validation: %v", err)
	}

	// Notifications.
	if w := do(t, h, "POST", "/notifications/test", map[string]any{"channel": map[string]any{"type": "ntfy", "config": map[string]string{"url": "nope"}}}); w.Code != 422 {
		t.Fatalf("test invalid channel: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/notifications/test", map[string]any{"channel": map[string]any{"type": "webhook", "name": "hook", "config": map[string]string{"url": "http://127.0.0.1:1/unreachable"}}}); w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":false`) {
		t.Fatalf("test unreachable channel: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "GET", "/notifications/log?limit=5", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"failed"`) {
		t.Fatalf("notification log: %d %s", w.Code, w.Body)
	}

	// Import.
	if w := do(t, h, "POST", "/import/npm/preview", map[string]string{"path": "/definitely/not/here"}); w.Code != 422 {
		t.Fatalf("import bad path: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/import/npm/commit", map[string]string{"token": "nope"}); w.Code != 410 {
		t.Fatalf("import bad token: %d %s", w.Code, w.Body)
	}
}
