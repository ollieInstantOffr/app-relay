package apply

import (
	"context"
	"net/http"
	"slices"

	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/render"
	"github.com/instantoffr/relay/internal/store"
)

func init() {
	httpx.SettingsHooks[model.SettingsErrorPages] = &httpx.SettingsHook{
		BeforeSave: func(_ *http.Request, _, next any) error {
			s, ok := next.(*model.ErrorPagesSettings)
			if !ok {
				return nil
			}
			s.Normalize()
			return s.Validate()
		},
	}
}

// previewErrorPage renders one error or maintenance page exactly as served.
func (h *handlers) previewErrorPage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Settings    model.ErrorPagesSettings `json:"settings"`
		Page        string                   `json:"page"`
		Maintenance *model.Maintenance       `json:"maintenance"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		writeErr(w, r, err)
		return
	}
	if !slices.Contains(model.ErrorPageKeys, body.Page) {
		writeErr(w, r, httpx.Errorf(http.StatusBadRequest, "bad_request", "page must be one of 403, 404, 429, 500, 502, 503, 504 or maintenance"))
		return
	}
	set := body.Settings
	set.Normalize()
	var m *model.Maintenance
	if body.Page == "maintenance" {
		m = body.Maintenance
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"html": render.ErrorPageHTML(set, body.Page, m)})
}

// LiveHost returns a host as in the live configuration, so Relay login
// checks what is actually served. Before anything was applied it returns the
// saved host.
func (s *Service) LiveHost(ctx context.Context, id string) (*model.ProxyHost, error) {
	_, snap, err := s.liveVersion(ctx)
	if err != nil {
		return nil, err
	}
	if snap == nil {
		return s.app.Store.Hosts().Get(ctx, id)
	}
	for i := range snap.Hosts {
		if snap.Hosts[i].ID == id {
			h := snap.Hosts[i]
			return &h, nil
		}
	}
	return nil, store.ErrNotFound
}
