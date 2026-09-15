package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// Settings keys that change rendered config (trigger pending changes).
var configSettings = map[string]bool{
	model.SettingsGeneral:     true,
	model.SettingsTLS:         true,
	model.SettingsDefaultHost: true,
	model.SettingsHAProxy:     true,
	model.SettingsBlocklist:   true,
	model.SettingsErrorPages:  true,
}

// Settings keys editors may change (the rest are admin-only).
var editorSettings = map[string]bool{
	model.SettingsDefaultHost: true,
	model.SettingsBlocklist:   true,
}

func (s *Server) routesSettings(r chi.Router) {
	r.Get("/settings/{key}", func(w http.ResponseWriter, r *http.Request) {
		key := chi.URLParam(r, "key")
		v := store.SettingsDefaults(key)
		if v == nil {
			writeError(w, http.StatusNotFound, "not_found", "unknown settings key")
			return
		}
		if err := s.app.Store.GetSettings(r.Context(), key, v); err != nil {
			s.fail(w, r, err)
			return
		}
		if rd, ok := v.(model.Redactor); ok {
			rd.Redact()
		}
		if h := httpx.SettingsHooks[key]; h != nil && h.Decorate != nil {
			writeJSON(w, http.StatusOK, h.Decorate(r, v))
			return
		}
		writeJSON(w, http.StatusOK, v)
	})

	r.Put("/settings/{key}", func(w http.ResponseWriter, r *http.Request) {
		key := chi.URLParam(r, "key")
		prev := store.SettingsDefaults(key)
		next := store.SettingsDefaults(key)
		if prev == nil {
			writeError(w, http.StatusNotFound, "not_found", "unknown settings key")
			return
		}
		if !editorSettings[key] && !actorOf(r).IsAdmin() {
			writeError(w, http.StatusForbidden, "admin_only", "only admins can change these settings")
			return
		}
		if err := s.app.Store.GetSettings(r.Context(), key, prev); err != nil {
			s.fail(w, r, err)
			return
		}
		if err := decode(r, next); err != nil {
			s.fail(w, r, err)
			return
		}
		if sk, ok := next.(model.SecretKeeper); ok {
			if err := sk.KeepSecrets(prev); err != nil {
				s.fail(w, r, err)
				return
			}
		}
		h := httpx.SettingsHooks[key]
		if h != nil && h.BeforeSave != nil {
			if err := h.BeforeSave(r, prev, next); err != nil {
				s.fail(w, r, err)
				return
			}
		}
		if v, ok := next.(model.Validator); ok {
			if err := v.Validate(); err != nil {
				s.fail(w, r, err)
				return
			}
		}
		if err := s.app.Store.PutSettings(r.Context(), key, next); err != nil {
			s.fail(w, r, err)
			return
		}
		s.app.Audit(r.Context(), core.AuditEntry{Action: "settings.update", Target: key, Result: "saved"})
		if configSettings[key] {
			s.app.Changed(r.Context(), "settings", key, key, core.ActionUpdated)
		}
		if h != nil && h.AfterSave != nil {
			h.AfterSave(r, prev, next)
		}
		if rd, ok := next.(model.Redactor); ok {
			rd.Redact()
		}
		writeJSON(w, http.StatusOK, next)
	})
}
