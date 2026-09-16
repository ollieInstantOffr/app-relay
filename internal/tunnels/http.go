package tunnels

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/tunnel"
)

// GatewayView is a gateway with its live connection and what it publishes.
type GatewayView struct {
	model.Gateway
	Status    *tunnel.GatewayStatus `json:"status"` // nil while the tunnel engine isn't running
	Published References            `json:"published"`
}

// Overview is GET /api/tunnels.
type Overview struct {
	Engine   *tunnel.Status `json:"engine"` // nil while the tunnel engine isn't running
	Gateways []GatewayView  `json:"gateways"`
}

// Overview combines gateways, the engine's status and what each publishes.
func (s *Service) Overview(r *http.Request) (*Overview, error) {
	ctx := r.Context()
	list, err := s.app.Store.Gateways().List(ctx)
	if err != nil {
		return nil, err
	}
	st, err := s.EngineStatus(ctx)
	if err != nil {
		s.log.Debug("tunnel status", "err", err)
	}
	snap, err := s.app.Store.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	out := &Overview{Engine: st, Gateways: []GatewayView{}}
	for _, g := range list {
		g.Redact()
		v := GatewayView{Gateway: g, Published: References{Hosts: []string{}, Streams: []string{}}}
		if st != nil {
			for i := range st.Gateways {
				if st.Gateways[i].ID == g.ID {
					gs := st.Gateways[i]
					v.Status = &gs
				}
			}
		}
		for _, h := range snap.Hosts {
			if h.TunnelGatewayID == g.ID && len(h.Domains) > 0 {
				v.Published.Hosts = append(v.Published.Hosts, h.Domains[0])
			}
		}
		for _, st := range snap.Streams {
			if st.TunnelGatewayID == g.ID {
				v.Published.Streams = append(v.Published.Streams, st.Name)
			}
		}
		out.Gateways = append(out.Gateways, v)
	}
	return out, nil
}

// Routes mounts the tunnel endpoints and the gateway CRUD hooks. It must be
// registered after the hosts and streams slices so it can wrap their hooks.
func Routes(app *core.App, r chi.Router) {
	s, _ := app.Tunnels.(*Service)
	if s == nil {
		return
	}
	s.hooks.Do(s.registerHooks)

	r.Get("/tunnels", func(w http.ResponseWriter, r *http.Request) {
		o, err := s.Overview(r)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, o)
	})
	r.Post("/gateways/{id}/pairing", func(w http.ResponseWriter, r *http.Request) {
		p, err := s.StartPairing(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, p)
	})
	r.Post("/gateways/{id}/pair", func(w http.ResponseWriter, r *http.Request) {
		g, err := s.Pair(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		g.Redact()
		httpx.WriteJSON(w, http.StatusOK, g)
	})
}

// registerHooks wires gateway CRUD side effects and checks that hosts and
// streams only publish through existing gateways.
func (s *Service) registerHooks() {
	httpx.GatewayHooks.AfterSave = func(r *http.Request, prev, next *model.Gateway) {
		ctx := r.Context()
		if prev == nil {
			if ident, err := s.identity(next.ID); err != nil {
				s.log.Warn("create gateway identity", "gateway", next.ID, "err", err)
			} else {
				next.HomeFingerprint = ident.Fingerprint()
				if err := s.app.Store.Gateways().Update(ctx, next); err != nil {
					s.log.Warn("record gateway identity", "gateway", next.ID, "err", err)
				}
			}
		}
		s.gatewaysChanged(ctx)
	}
	httpx.GatewayHooks.BeforeDelete = func(r *http.Request, cur *model.Gateway) error {
		refs, err := s.references(r.Context(), cur.ID)
		if err != nil {
			return err
		}
		if !refs.empty() {
			names := append(append([]string{}, refs.Hosts...), refs.Streams...)
			if len(names) > 5 {
				names = append(names[:5], fmt.Sprintf("%d more", len(refs.Hosts)+len(refs.Streams)-5))
			}
			return httpx.Errorf(http.StatusConflict, "gateway_in_use",
				fmt.Sprintf("%s still publishes %s. Stop publishing them through this gateway (and apply) first.", cur.Name, strings.Join(names, ", ")))
		}
		return nil
	}
	httpx.GatewayHooks.AfterDelete = func(r *http.Request, cur *model.Gateway) {
		if err := os.RemoveAll(s.gatewayDir(cur.ID)); err != nil {
			s.log.Warn("remove gateway identity", "gateway", cur.ID, "err", err)
		}
		s.mu.Lock()
		delete(s.conn, cur.ID)
		s.mu.Unlock()
		s.gatewaysChanged(r.Context())
	}

	hostSave := httpx.HostHooks.BeforeSave
	httpx.HostHooks.BeforeSave = func(r *http.Request, prev, next *model.ProxyHost) error {
		if hostSave != nil {
			if err := hostSave(r, prev, next); err != nil {
				return err
			}
		}
		return s.checkGateway(r, "tunnelGatewayId", next.TunnelGatewayID, prev != nil && prev.TunnelGatewayID == next.TunnelGatewayID)
	}
	streamSave := httpx.StreamHooks.BeforeSave
	httpx.StreamHooks.BeforeSave = func(r *http.Request, prev, next *model.Stream) error {
		if streamSave != nil {
			if err := streamSave(r, prev, next); err != nil {
				return err
			}
		}
		if next.TunnelGatewayID != "" && next.Protocol == "udp" {
			return model.Errs{"tunnelGatewayId": "Tunnels carry TCP only; UDP streams can't be published through a gateway"}.Err()
		}
		return s.checkGateway(r, "tunnelGatewayId", next.TunnelGatewayID, prev != nil && prev.TunnelGatewayID == next.TunnelGatewayID)
	}
}

// checkGateway validates a tunnelGatewayId reference. An unchanged reference
// is accepted so other edits don't fail while a gateway is being re-created.
func (s *Service) checkGateway(r *http.Request, field, id string, unchanged bool) error {
	if id == "" || unchanged {
		return nil
	}
	if _, err := s.app.Store.Gateways().Get(r.Context(), id); err != nil {
		return model.Errs{field: "Unknown tunnel gateway"}.Err()
	}
	return nil
}
