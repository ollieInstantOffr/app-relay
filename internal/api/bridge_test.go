package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/store"
)

// In-process calls from MCP tools carry their actor in the context; normal
// requests must still authenticate.
func TestInternalCallsUseContextActor(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	app := core.New(core.Config{Version: "test", RunDir: t.TempDir()}, st, events.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := New(app, fstest.MapFS{}).Handler()

	do := func(ctx context.Context, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "127.0.0.1:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	writer := core.Actor{Type: core.ActorMCP, ID: "tok1", TokenID: "tok1", Name: "desktop", Scope: core.ScopeWrite, Role: core.RoleAdmin}
	reader := writer
	reader.Scope = core.ScopeRead

	if rec := do(context.Background(), http.MethodGet, "/api/redirects", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET = %d", rec.Code)
	}
	internal := func(a core.Actor) context.Context {
		return core.WithInternalCall(core.WithActor(context.Background(), a))
	}
	if rec := do(internal(writer), http.MethodGet, "/api/redirects", ""); rec.Code != http.StatusOK {
		t.Fatalf("internal GET = %d %s", rec.Code, rec.Body)
	}
	if rec := do(internal(reader), http.MethodPost, "/api/redirects", `{"domains":["a.example.com"],"to":"https://b.example.com","code":301}`); rec.Code != http.StatusForbidden {
		t.Fatalf("internal POST with a read-only token = %d %s", rec.Code, rec.Body)
	}
	if rec := do(core.WithInternalCall(context.Background()), http.MethodGet, "/api/redirects", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("internal marker without an actor = %d", rec.Code)
	}
}
