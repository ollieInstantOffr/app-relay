package auth

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/store"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestService(t *testing.T) (*Service, *fakeClock) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	app := core.New(core.Config{Version: "test", RunDir: t.TempDir()}, st, events.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	s := New(app)
	clock := &fakeClock{t: time.Now().UTC()}
	s.now = clock.now
	app.Auth = s
	return s, clock
}

func createTestUser(t *testing.T, s *Service, username, role, password string) *store.User {
	t.Helper()
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	u := &store.User{Username: username, Role: role, PasswordHash: hash, CreatedAt: s.now()}
	if err := s.app.Store.CreateUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

// testRouter mirrors internal/api: resolve the actor, apply the guard, then
// the auth routes under /api.
func testRouter(s *Service) http.Handler {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if a, err := s.Authenticate(r); err == nil {
				a.IP = core.ClientIP(r)
				r = r.WithContext(core.WithActor(r.Context(), a))
			}
			next.ServeHTTP(w, r)
		})
	})
	r.Use(func(next http.Handler) http.Handler { return networkGuard(s.app, next) })
	r.Route("/api", func(r chi.Router) {
		PublicRoutes(s.app, r)
		r.Group(func(r chi.Router) {
			r.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if core.ActorFrom(r.Context()).IsZero() {
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					next.ServeHTTP(w, r)
				})
			})
			Routes(s.app, r)
			r.Get("/hosts", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
		})
	})
	r.Get("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	return r
}
