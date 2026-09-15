// Package hostsapi is the hosts slice API: host actions, redirects, default host.
package hostsapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// Routes registers the hosts slice API (runs behind auth).
func Routes(app *core.App, r chi.Router) {
	registerHooks(app)
	h := &handlers{app: app}
	r.Get("/hosts/summary", h.summary)
	r.Post("/hosts/check-domain", h.checkDomain)
	r.Post("/hosts/bulk", h.bulk)
	r.Get("/hosts/{id}/usage", h.usage)
	r.Post("/hosts/{id}/duplicate", h.duplicate)
	r.Post("/hosts/{id}/toggle", h.toggle)
}

type handlers struct{ app *core.App }

// ---------------------------------------------------------------- summary

type summaryResp struct {
	All      int `json:"all"`
	Healthy  int `json:"healthy"`
	Errors   int `json:"errors"`
	NoSSL    int `json:"noSsl"`
	Disabled int `json:"disabled"`
}

func (h *handlers) summary(w http.ResponseWriter, r *http.Request) {
	hosts, err := h.app.Store.Hosts().List(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	var health map[string]core.HealthStatus
	if h.app.Health != nil {
		health = h.app.Health.All()
	}
	var out summaryResp
	for i := range hosts {
		host := &hosts[i]
		out.All++
		if host.CertificateID == "" {
			out.NoSSL++
		}
		if !host.Enabled {
			out.Disabled++
			continue
		}
		switch health[core.HostTarget(host.ID)].Status {
		case core.HealthHealthy:
			out.Healthy++
		case core.HealthDown, core.HealthDegraded:
			out.Errors++
		}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------- check-domain

type checkDomainReq struct {
	Domain    string `json:"domain"`
	ExcludeID string `json:"excludeId"`
	Kind      string `json:"kind"` // host (default) | redirect
	FromPath  string `json:"fromPath"`
}

type checkDomainResp struct {
	Domain   string       `json:"domain"`
	Valid    bool         `json:"valid"`
	Error    string       `json:"error,omitempty"`
	Conflict *domainOwner `json:"conflict,omitempty"`
	Message  string       `json:"message,omitempty"`
}

func (h *handlers) checkDomain(w http.ResponseWriter, r *http.Request) {
	var req checkDomainReq
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	kind := req.Kind
	if kind != "redirect" {
		kind = "host"
	}
	d := model.HostNormalizeDomain(req.Domain)
	resp := checkDomainResp{Domain: d}
	if msg := model.HostDomainError(d); msg != "" {
		resp.Error, resp.Message = msg, msg
		httpx.WriteJSON(w, http.StatusOK, resp)
		return
	}
	resp.Valid = true
	hosts, err := h.app.Store.Hosts().List(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	redirects, err := h.app.Store.Redirects().List(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if o := findDomainOwner(hosts, redirects, d, kind, req.ExcludeID, strings.TrimSpace(req.FromPath)); o != nil {
		resp.Conflict, resp.Message = o, o.message()
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------- usage

type usageResp struct {
	CertificateID         string   `json:"certificateId"`
	CertificateName       string   `json:"certificateName"`
	CertificateSharedWith []string `json:"certificateSharedWith"`
	Locations             int      `json:"locations"`
	// What unknown-domain traffic gets once the host is gone.
	DefaultHostAction string `json:"defaultHostAction"`
	DefaultHostTarget string `json:"defaultHostTarget,omitempty"`
	IsDefaultHost     bool   `json:"isDefaultHost"`
}

func (h *handlers) usage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	host, err := h.app.Store.Hosts().Get(ctx, chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	resp := usageResp{Locations: len(host.Locations), CertificateSharedWith: []string{}, CertificateID: host.CertificateID}
	if host.CertificateID != "" {
		if cert, err := h.app.Store.Certificates().Get(ctx, host.CertificateID); err == nil {
			resp.CertificateName = cert.Name
		}
		users, err := loadCertUsers(ctx, h.app.Store, host.CertificateID, host.ID)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		resp.CertificateSharedWith = users
	}
	dh, err := store.LoadSettings[model.DefaultHostSettings](ctx, h.app.Store, model.SettingsDefaultHost)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	resp.DefaultHostAction = dh.Action
	switch dh.Action {
	case model.DefaultHostRedirect:
		resp.DefaultHostTarget = dh.RedirectTo
	case model.DefaultHostServe:
		resp.IsDefaultHost = dh.HostID == host.ID
		if target, err := h.app.Store.Hosts().Get(ctx, dh.HostID); err == nil {
			resp.DefaultHostTarget = first(target.Domains)
		}
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------- duplicate

func (h *handlers) duplicate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	src, err := h.app.Store.Hosts().Get(ctx, chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	rf, err := loadRefs(ctx, h.app.Store)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	// Deep copy through JSON so nested slices aren't shared.
	var cp model.ProxyHost
	raw, _ := json.Marshal(src)
	if err := json.Unmarshal(raw, &cp); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	cp.Meta = model.Meta{}
	cp.System = false
	cp.Enabled = false
	cp.Source = model.SourceManual
	cp.SourceRef = ""
	cp.Domains = duplicateDomains(src.Domains, rf.hosts, rf.redirects)
	for i := range cp.Locations {
		cp.Locations[i].ID = ""
	}
	if err := prepareHost(rf, nil, &cp); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if err := h.app.Store.Hosts().Create(ctx, &cp); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	name := first(cp.Domains)
	h.app.Audit(ctx, core.AuditEntry{Action: "host.duplicate", Target: name, Detail: "copy of " + first(src.Domains), Result: "saved"})
	h.app.Changed(ctx, model.KindHost, cp.ID, name, core.ActionCreated)
	httpx.WriteJSON(w, http.StatusCreated, cp)
}

// ---------------------------------------------------------------- toggle

func (h *handlers) toggle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if body.Enabled == nil {
		httpx.Fail(w, r, httpx.Errorf(http.StatusBadRequest, "bad_request", "enabled is required"))
		return
	}
	host, err := h.app.Store.Hosts().Get(ctx, chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	updated, changed, err := h.setEnabled(r, host, *body.Enabled)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if changed {
		h.recordUpdate(r, updated, map[bool]string{true: "host.enable", false: "host.disable"}[*body.Enabled], "")
	}
	httpx.WriteJSON(w, http.StatusOK, updated)
}

// setEnabled validates and stores the enabled flag; changed is false when the
// host already had that state.
func (h *handlers) setEnabled(r *http.Request, host *model.ProxyHost, enabled bool) (*model.ProxyHost, bool, error) {
	if host.Enabled == enabled {
		return host, false, nil
	}
	if host.System && !enabled {
		return nil, false, httpx.Errorf(http.StatusConflict, "system_host", "Relay's admin UI host can't be disabled")
	}
	next := *host
	next.Enabled = enabled
	if enabled {
		rf, err := loadRefs(r.Context(), h.app.Store)
		if err != nil {
			return nil, false, err
		}
		if err := prepareHost(rf, host, &next); err != nil {
			return nil, false, err
		}
	}
	if err := h.app.Store.Hosts().Update(r.Context(), &next); err != nil {
		return nil, false, err
	}
	return &next, true, nil
}

func (h *handlers) recordUpdate(r *http.Request, host *model.ProxyHost, action, detail string) {
	name := first(host.Domains)
	h.app.Audit(r.Context(), core.AuditEntry{Action: action, Target: name, Detail: detail, Result: "saved"})
	h.app.Changed(r.Context(), model.KindHost, host.ID, name, core.ActionUpdated)
}

// ---------------------------------------------------------------- bulk

const (
	bulkEnable           = "enable"
	bulkDisable          = "disable"
	bulkDelete           = "delete"
	bulkAttachAccessList = "attach_access_list"
	bulkSetCertificate   = "set_certificate"
)

type bulkReq struct {
	IDs    []string `json:"ids"`
	Action string   `json:"action"`
	Value  string   `json:"value"`
}

type bulkItem struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

type bulkResp struct {
	Results []bulkItem `json:"results"`
	OK      int        `json:"ok"`
	Failed  int        `json:"failed"`
}

func (h *handlers) bulk(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req bulkReq
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	switch req.Action {
	case bulkEnable, bulkDisable, bulkDelete, bulkAttachAccessList, bulkSetCertificate:
	default:
		httpx.Fail(w, r, httpx.Errorf(http.StatusBadRequest, "bad_request", "unknown bulk action "+req.Action))
		return
	}
	if len(req.IDs) == 0 || len(req.IDs) > 1000 {
		httpx.Fail(w, r, httpx.Errorf(http.StatusBadRequest, "bad_request", "ids must list 1–1000 hosts"))
		return
	}
	req.Value = strings.TrimSpace(req.Value)

	var valueName string
	switch req.Action {
	case bulkAttachAccessList:
		valueName = "none"
		if req.Value != "" {
			list, err := h.app.Store.AccessLists().Get(ctx, req.Value)
			if err != nil {
				httpx.Fail(w, r, httpx.Errorf(http.StatusUnprocessableEntity, "invalid", "access list not found"))
				return
			}
			valueName = list.Name
		}
	case bulkSetCertificate:
		valueName = "HTTP only"
		if req.Value != "" {
			cert, err := h.app.Store.Certificates().Get(ctx, req.Value)
			if err != nil {
				httpx.Fail(w, r, httpx.Errorf(http.StatusUnprocessableEntity, "invalid", "certificate not found"))
				return
			}
			valueName = cert.Name
		}
	}
	gen, err := store.LoadSettings[model.GeneralSettings](ctx, h.app.Store, model.SettingsGeneral)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	resp := bulkResp{Results: []bulkItem{}}
	seen := map[string]bool{}
	for _, id := range req.IDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		item := bulkItem{ID: id}
		if err := h.bulkOne(r, req, id, valueName, gen, &item); err != nil {
			item.Error = errText(err)
			resp.Failed++
		} else {
			item.OK = true
			resp.OK++
		}
		resp.Results = append(resp.Results, item)
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func (h *handlers) bulkOne(r *http.Request, req bulkReq, id, valueName string, gen model.GeneralSettings, item *bulkItem) error {
	ctx := r.Context()
	host, err := h.app.Store.Hosts().Get(ctx, id)
	if err != nil {
		return err
	}
	item.Name = first(host.Domains)

	switch req.Action {
	case bulkDelete:
		if err := beforeDeleteHost(ctx, h.app, host); err != nil {
			return err
		}
		if err := h.app.Store.Hosts().Delete(ctx, id); err != nil {
			return err
		}
		h.app.Audit(ctx, core.AuditEntry{Action: "host.delete", Target: item.Name, Detail: "bulk", Result: "saved"})
		h.app.Changed(ctx, model.KindHost, id, item.Name, core.ActionDeleted)
		return nil
	case bulkEnable, bulkDisable:
		enabled := req.Action == bulkEnable
		updated, changed, err := h.setEnabled(r, host, enabled)
		if err != nil {
			return err
		}
		if changed {
			h.recordUpdate(r, updated, "host."+req.Action, "bulk")
		}
		return nil
	}

	next := *host
	var detail string
	switch req.Action {
	case bulkAttachAccessList:
		if host.AccessListID == req.Value {
			return nil
		}
		next.AccessListID = req.Value
		detail = "bulk: access list → " + valueName
	case bulkSetCertificate:
		if host.CertificateID == req.Value {
			return nil
		}
		next.CertificateID = req.Value
		if host.CertificateID == "" && req.Value != "" {
			next.ForceHTTPS = gen.Defaults.ForceHTTPS
		}
		detail = "bulk: certificate → " + valueName
	}
	rf, err := loadRefs(ctx, h.app.Store)
	if err != nil {
		return err
	}
	if err := prepareHost(rf, host, &next); err != nil {
		return err
	}
	if err := h.app.Store.Hosts().Update(ctx, &next); err != nil {
		return err
	}
	h.recordUpdate(r, &next, "host.update", detail)
	return nil
}
