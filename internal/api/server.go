// Package api is Relay's REST API (/api/*), MCP mount (/mcp) and SPA server.
//
// Conventions for feature slices:
// Feature slices register their routes from their own packages (see
// docs/SLICES.md); the routesX methods below only delegate to them.
package api

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
)

type Server struct {
	app *core.App
	web fs.FS
}

func New(app *core.App, web fs.FS) *Server { return &Server{app: app, web: web} }

func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(s.withActor)
	if httpx.NetworkGuard != nil {
		r.Use(func(next http.Handler) http.Handler {
			guarded := httpx.NetworkGuard(s.app, next)
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// MCP tools were admitted by the MCP endpoint's own access list.
				if core.IsInternalCall(r.Context()) {
					next.ServeHTTP(w, r)
					return
				}
				guarded.ServeHTTP(w, r)
			})
		})
	}
	if s.app.Config.DevMode {
		r.Use(devCORS)
	}

	// Unauthenticated liveness probe (Docker HEALTHCHECK). /api/health is the observe slice's health map.
	r.Get("/healthz", s.handleHealth)

	r.Route("/api", func(r chi.Router) {
		r.Use(noStore)
		s.routesAuth(r) // public: sign-in, setup, session (handlers check auth themselves)

		r.Group(func(r chi.Router) {
			r.Use(s.requireAuth)
			r.Use(s.readOnlyForViewers)
			s.routesCRUD(r)
			s.routesSettings(r)
			s.routesEvents(r)
			s.routesEngine(r)
			s.routesLB(r)
			s.routesHosts(r)
			s.routesCerts(r)
			s.routesObserve(r)
			s.routesUsers(r)
			s.routesOps(r)
			s.routesMCPAdmin(r)
			s.routesDNS(r)
		})
		r.NotFound(func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
		})
		r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		})
	})

	s.routesPortal(r)

	if s.app.MCP != nil {
		h := s.app.MCP.Handler()
		r.Handle("/mcp", h)
		r.Handle("/mcp/*", h)
	}
	r.NotFound(s.serveSPA)
	return r
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": s.app.Config.Version, "time": time.Now()})
}

// serveSPA serves the embedded web UI, falling back to index.html.
func (s *Server) serveSPA(w http.ResponseWriter, r *http.Request) {
	if s.web == nil || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		http.NotFound(w, r)
		return
	}
	p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if p == "" {
		p = "index.html"
	}
	if st, err := fs.Stat(s.web, p); err != nil || st.IsDir() {
		p = "index.html"
	}
	if _, err := fs.Stat(s.web, "index.html"); err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("Relay UI is not built. Run `make ui` (npm run build in web/)."))
		return
	}
	if strings.HasPrefix(p, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeFileFS(w, r, s.web, p)
}

func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func devCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" {
			w.Header().Set("Access-Control-Allow-Origin", o)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
