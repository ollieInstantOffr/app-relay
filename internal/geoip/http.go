package geoip

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
)

// Routes registers /api/geoip (runs behind auth).
func Routes(app *core.App, r chi.Router) {
	s, _ := app.GeoIP.(*Service)
	if s == nil {
		return
	}
	r.Get("/geoip", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, s.Status(r.Context()))
	})
	r.Post("/geoip/update", httpx.RequireAdmin(func(w http.ResponseWriter, r *http.Request) {
		if err := s.Update(r.Context()); err != nil {
			httpx.Fail(w, r, httpx.Errorf(http.StatusBadRequest, "download_failed", err.Error()))
			return
		}
		app.Audit(r.Context(), core.AuditEntry{Action: "geoip.update", Target: "country database", Result: "ok"})
		httpx.WriteJSON(w, http.StatusOK, s.Status(r.Context()))
	}))
}
