package api

import (
	"net/http"

	"github.com/instantoffr/relay/internal/core"
)

// withActor resolves the session cookie / bearer token (if any) into ctx.
func (s *Server) withActor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := core.ClientIP(r)
		if s.app.Auth != nil {
			if a, err := s.app.Auth.Authenticate(r); err == nil && !a.IsZero() {
				a.IP = ip
				r = r.WithContext(core.WithActor(r.Context(), a))
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if actorOf(r).IsZero() {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "sign in required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// readOnlyForViewers rejects writes from viewers and read-only tokens.
func (s *Server) readOnlyForViewers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if !actorOf(r).CanWrite() {
				writeError(w, http.StatusForbidden, "read_only", "your role can't change anything")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
