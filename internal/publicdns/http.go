package publicdns

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
)

// Routes registers the public DNS API (runs behind auth).
func Routes(app *core.App, r chi.Router) {
	s, _ := app.PublicDNS.(*Service)
	if s == nil {
		return
	}
	r.Get("/dns/provider-types", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, model.PublicDNSProviderTypes)
	})
	r.Get("/dns/status", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, s.Status(r.Context()))
	})
	r.Get("/dns/zones/{zone}/records", func(w http.ResponseWriter, r *http.Request) {
		z, list, err := s.Records(r.Context(), chi.URLParam(r, "zone"))
		if err != nil {
			fail(w, r, err)
			return
		}
		if list == nil {
			list = []Record{}
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"zone": z.Name, "providerType": z.ProviderType, "records": list})
	})
	r.Post("/dns/zones/{zone}/records", func(w http.ResponseWriter, r *http.Request) {
		var in RecordInput
		if err := httpx.Decode(r, &in); err != nil {
			fail(w, r, err)
			return
		}
		rec, err := s.Create(r.Context(), chi.URLParam(r, "zone"), in)
		if err != nil {
			fail(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusCreated, rec)
	})
	r.Put("/dns/zones/{zone}/records/{id}", func(w http.ResponseWriter, r *http.Request) {
		var in RecordInput
		if err := httpx.Decode(r, &in); err != nil {
			fail(w, r, err)
			return
		}
		rec, err := s.Update(r.Context(), chi.URLParam(r, "zone"), chi.URLParam(r, "id"), in)
		if err != nil {
			fail(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, rec)
	})
	r.Delete("/dns/zones/{zone}/records/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Delete(r.Context(), chi.URLParam(r, "zone"), chi.URLParam(r, "id")); err != nil {
			fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	r.Post("/dns/check", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Domains []string `json:"domains"`
		}
		if err := httpx.Decode(r, &body); err != nil {
			fail(w, r, err)
			return
		}
		results, err := s.Check(r.Context(), body.Domains)
		if err != nil {
			fail(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"results": results})
	})
	r.Post("/dns/sync", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			HostIDs []string `json:"hostIds"`
		}
		if r.ContentLength > 0 {
			if err := httpx.Decode(r, &body); err != nil {
				fail(w, r, err)
				return
			}
		}
		results, err := s.Sync(r.Context(), body.HostIDs)
		if err != nil {
			fail(w, r, err)
			return
		}
		if results == nil {
			results = []DomainCheck{}
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"results": results})
	})
}

func fail(w http.ResponseWriter, r *http.Request, err error) {
	var pe *ProviderError
	if errors.As(err, &pe) {
		httpx.WriteError(w, http.StatusBadGateway, "provider_error", pe.Message)
		return
	}
	httpx.Fail(w, r, err)
}
