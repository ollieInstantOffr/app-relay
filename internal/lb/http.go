package lb

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/agent"
	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/lb/lbengine"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/render"
	"github.com/instantoffr/relay/internal/store"
)

// Routes registers the load balancer slice API (runs behind auth).
func Routes(app *core.App, r chi.Router) {
	registerHooks(app)
	svc, _ := app.LB.(*Service)
	if svc == nil {
		svc = New(app) // app.LB replaced by another implementation: still serve read paths
	}
	h := &handlers{app: app, svc: svc}

	r.Get("/lb/stats", h.stats)
	r.Get("/lb/series", h.series)
	r.Post("/backends/{id}/servers/{serverId}/state", h.serverState)
	r.Post("/backends/{id}/servers/{serverId}/weight", h.serverWeight)
	r.Post("/backends/{id}/servers/{serverId}/check", h.serverCheck)
	r.Post("/backends/from-host/{hostId}", h.fromHost)
	// The active load balancer engine (HAProxy or Relay Balancer); the
	// /haproxy paths are kept for older clients.
	r.Get("/lb/config", h.config)
	r.Post("/lb/validate", h.validate)
	r.Post("/preview/lb/backend", h.previewBackend)
	r.Post("/preview/lb/frontend", h.previewFrontend)
	r.Get("/haproxy/config", h.config)
	r.Post("/haproxy/validate", h.validate)
	r.Post("/preview/haproxy/backend", h.previewBackend)
	r.Post("/preview/haproxy/frontend", h.previewFrontend)
	r.Post("/lb/expose", h.expose)
	r.Post("/lb/expose/preview", h.exposePreview)
}

type handlers struct {
	app *core.App
	svc *Service
}

func (h *handlers) env(ctx context.Context) render.Env {
	cfg := h.app.Config
	env := render.DefaultEnv(cfg.DataDir, cfg.RunDir, cfg.LogDir)
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	pc, _ := h.app.Proxy(ctx)
	if st, err := pc.Status(cctx); err == nil {
		for _, m := range st.Modules {
			env.Modules[m] = true
		}
	}
	return env
}

func (h *handlers) stats(w http.ResponseWriter, r *http.Request) {
	st, err := h.svc.Stats(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, st)
}

func (h *handlers) series(w http.ResponseWriter, r *http.Request) {
	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = "1h"
	}
	name := ""
	id := r.URL.Query().Get("backend")
	if id != "" {
		b, err := h.app.Store.Backends().Get(r.Context(), id)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		name = b.Name
	}
	s, err := h.svc.Series(r.Context(), name, rng)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	s.BackendID = id
	httpx.WriteJSON(w, http.StatusOK, s)
}

func (h *handlers) serverState(w http.ResponseWriter, r *http.Request) {
	var body struct {
		State        string `json:"state"`
		GraceSeconds int    `json:"graceSeconds"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if body.GraceSeconds < 0 || body.GraceSeconds > 86400 {
		httpx.Fail(w, r, &model.ValidationError{Fields: map[string]string{"graceSeconds": "Grace timeout must be 0–86400 seconds"}})
		return
	}
	res, err := h.svc.SetServerStateGrace(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "serverId"), body.State, body.GraceSeconds)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (h *handlers) serverWeight(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Weight int `json:"weight"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	res, err := h.svc.SetServerWeight(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "serverId"), body.Weight)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (h *handlers) serverCheck(w http.ResponseWriter, r *http.Request) {
	b, err := h.app.Store.Backends().Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	i := serverIndex(b, chi.URLParam(r, "serverId"))
	if i < 0 {
		httpx.Fail(w, r, store.ErrNotFound)
		return
	}
	res := probeServer(r.Context(), b, b.Servers[i])
	h.app.Audit(r.Context(), core.AuditEntry{Action: "server.check", Target: b.Name + "/" + serverName(b, i), Detail: res.Detail, Result: map[bool]string{true: "ok", false: "failed"}[res.OK]})
	httpx.WriteJSON(w, http.StatusOK, res)
}

// ---------------------------------------------------------------- config & validation

func (h *handlers) config(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	snap, err := h.app.Store.Snapshot(ctx)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	engine := h.app.LBEngine(ctx)
	lr, err := lbengine.For(engine)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	files, renderErr := lr.Render(snap, h.env(ctx))
	rendered := files[lr.MainFile]
	// config / live / rendered hold the active engine's main file
	// (haproxy.cfg or balancer.json).
	resp := map[string]any{"rendered": rendered, "pending": false, "engine": engine, "file": lr.MainFile}
	if renderErr != nil {
		resp["renderError"] = renderErr.Error()
	}
	live, version, err := h.app.Store.LiveHAProxyConfig(ctx)
	switch {
	case err == nil:
		resp["config"] = live
		resp["live"] = live
		resp["liveVersion"] = version
		resp["pending"] = live != rendered
	case errors.Is(err, store.ErrNotFound) || strings.Contains(err.Error(), "no such table"):
		resp["config"] = rendered
	default:
		httpx.Fail(w, r, err)
		return
	}
	resp["lines"] = strings.Count(resp["config"].(string), "\n")
	httpx.WriteJSON(w, http.StatusOK, resp)
}

type validation struct {
	Valid      bool   `json:"valid"`
	Output     string `json:"output"`
	Checked    string `json:"checked"` // haproxy | balancer | local
	Engine     string `json:"engine"`  // active load balancer engine
	DurationMs int64  `json:"durationMs"`
	Lines      int    `json:"lines"`
}

// validateConfig runs `haproxy -c` / `relay balancer check` through the
// engine's agent when reachable.
func (h *handlers) validateConfig(ctx context.Context, engine string, files agent.Files) validation {
	label := core.ProxyEngineLabel(engine)
	v := validation{Engine: engine, Lines: strings.Count(files[lbengine.MainFile(engine)], "\n")}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c := h.app.Client(engine)
	if c == nil {
		v.Valid, v.Checked, v.Output = true, "local", label+" agent not configured — checked by Relay only"
		return v
	}
	resp, err := c.Validate(cctx, files)
	if err != nil {
		v.Valid = true
		v.Checked = "local"
		var ua agent.ErrUnavailable
		if errors.As(err, &ua) {
			v.Output = label + " agent not reachable — checked by Relay only"
		} else {
			v.Output = label + " validation unavailable — checked by Relay only (" + err.Error() + ")"
		}
		return v
	}
	v.Valid, v.Output, v.Checked, v.DurationMs = resp.OK, strings.TrimSpace(resp.Output), engine, resp.DurationMs
	return v
}

func (h *handlers) validate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	snap, err := h.app.Store.Snapshot(ctx)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	engine := h.app.LBEngine(ctx)
	lr, err := lbengine.For(engine)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	files, err := lr.Render(snap, h.env(ctx))
	if err != nil {
		httpx.WriteJSON(w, http.StatusOK, validation{Valid: false, Output: err.Error(), Checked: "local", Engine: engine})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, h.validateConfig(ctx, engine, files))
}

type preview struct {
	Config  string            `json:"config"`
	Valid   bool              `json:"valid"`
	Output  string            `json:"output"`
	Checked string            `json:"checked"` // haproxy | balancer | local
	Engine  string            `json:"engine"`  // load balancer engine that rendered it
	Fields  map[string]string `json:"fields,omitempty"`
}

func fieldsOf(err error) map[string]string {
	var ve *model.ValidationError
	if errors.As(err, &ve) {
		return ve.Fields
	}
	return nil
}

func mergeErrs(errs ...error) error {
	e := model.Errs{}
	for _, err := range errs {
		if err == nil {
			continue
		}
		f := fieldsOf(err)
		if f == nil {
			return err
		}
		for k, v := range f {
			e.Add(k, "%s", v)
		}
	}
	return e.Err()
}

func (h *handlers) previewBackend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		Backend model.Backend `json:"backend"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	snap, err := h.app.Store.Snapshot(ctx)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	b := body.Backend
	var prev *model.Backend
	idx := -1
	for i := range snap.Backends {
		if b.ID != "" && snap.Backends[i].ID == b.ID {
			prev, idx = &snap.Backends[i], i
		}
	}
	if b.Name == "" {
		b.Name = "new-backend"
	}
	normalizeBackend(snap.HAProxy, prev, &b)
	e := model.Errs{}
	for _, o := range snap.Backends {
		if o.ID != b.ID && strings.EqualFold(o.Name, b.Name) {
			e.Add("name", "A backend named %s already exists", o.Name)
		}
	}
	verr := mergeErrs(b.Validate(), e.Err())
	engine := h.app.LBEngine(ctx)
	lr, err := lbengine.For(engine)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := preview{Config: lr.Backend(snap, &b), Fields: fieldsOf(verr), Engine: engine}
	if verr != nil {
		out.Output, out.Checked = verr.Error(), "local"
		httpx.WriteJSON(w, http.StatusOK, out)
		return
	}
	if idx >= 0 {
		snap.Backends[idx] = b
	} else {
		if b.ID == "" {
			b.ID = "draft"
		}
		snap.Backends = append(snap.Backends, b)
	}
	h.finishPreview(ctx, w, snap, &out)
}

func (h *handlers) previewFrontend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		Frontend model.Frontend `json:"frontend"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	snap, err := h.app.Store.Snapshot(ctx)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	f := body.Frontend
	var prev *model.Frontend
	idx := -1
	for i := range snap.Frontends {
		if f.ID != "" && snap.Frontends[i].ID == f.ID {
			prev, idx = &snap.Frontends[i], i
		}
	}
	if f.Name == "" {
		f.Name = "new-frontend"
	}
	normalizeFrontend(prev, &f)
	verr := mergeErrs(f.Validate(), checkFrontend(h.app, snap, &f))
	engine := h.app.LBEngine(ctx)
	lr, err := lbengine.For(engine)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := preview{Config: lr.Frontend(snap, &f), Fields: fieldsOf(verr), Engine: engine}
	if verr != nil {
		out.Output, out.Checked = verr.Error(), "local"
		httpx.WriteJSON(w, http.StatusOK, out)
		return
	}
	if idx >= 0 {
		snap.Frontends[idx] = f
	} else {
		if f.ID == "" {
			f.ID = "draft"
		}
		snap.Frontends = append(snap.Frontends, f)
	}
	h.finishPreview(ctx, w, snap, &out)
}

func (h *handlers) finishPreview(ctx context.Context, w http.ResponseWriter, snap *model.Snapshot, out *preview) {
	lr, err := lbengine.For(out.Engine)
	var files agent.Files
	if err == nil {
		files, err = lr.Render(snap, h.env(ctx))
	}
	if err != nil {
		out.Valid, out.Output, out.Checked = false, err.Error(), "local"
		httpx.WriteJSON(w, http.StatusOK, out)
		return
	}
	v := h.validateConfig(ctx, out.Engine, files)
	out.Valid, out.Output, out.Checked = v.Valid, v.Output, v.Checked
	httpx.WriteJSON(w, http.StatusOK, out)
}
