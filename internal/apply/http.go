package apply

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/agent"
	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/render/nginx"
	"github.com/instantoffr/relay/internal/store"
)

// Routes registers the engine slice API (runs behind auth). See docs/SLICES.md.
func Routes(app *core.App, r chi.Router) {
	h := &handlers{app: app}
	r.Get("/pending", h.pending)
	r.Get("/pending/diff", h.pendingDiff)
	r.Post("/pending/discard", h.discard)
	r.Post("/apply", h.apply)
	r.Get("/versions", h.versions)
	r.Get("/versions/{id}", h.version)
	r.Get("/versions/{id}/diff", h.versionDiff)
	r.Get("/versions/{id}/download", h.download)
	r.Post("/versions/{id}/rollback", h.rollback)
	r.Get("/engines", h.engines)
	r.Get("/engines/updates", h.engineUpdates)
	r.Post("/engines/updates/check", httpx.RequireAdmin(h.checkEngineUpdates))
	r.Get("/engines/upgrade-status", h.upgradeStatus)
	r.Post("/engines/relay/update", httpx.RequireAdmin(h.updateRelay))
	r.Get("/engines/relay/update-status", h.relayUpdateStatus)
	r.Post("/engines/{engine}/upgrade", httpx.RequireAdmin(h.upgradeEngine))
	r.Post("/engines/{engine}/keep-image", httpx.RequireAdmin(h.keepEngineImage))
	r.Post("/engines/{engine}/{action}", h.engineAction)
	r.Get("/engines/{engine}/logs", h.engineLogs)
	r.Get("/engines/{engine}/listeners", h.engineListeners)
	r.Post("/preview/proxy/host", h.previewHost)
	r.Post("/preview/proxy/stream", h.previewStream)
	// Aliases kept for older clients; they preview the active proxy engine too.
	r.Post("/preview/nginx/host", h.previewHost)
	r.Post("/preview/nginx/stream", h.previewStream)
}

type handlers struct{ app *core.App }

func (h *handlers) svc() (*Service, error) {
	s, ok := h.app.Engine.(*Service)
	if !ok || s == nil {
		return nil, core.ErrNotImplemented
	}
	return s, nil
}

func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	var ae *ApplyError
	if errors.As(err, &ae) {
		body := map[string]any{"error": map[string]any{"code": ae.Code, "message": ae.Message}}
		if ae.Version != nil {
			body["version"] = ae.Version
		}
		if ae.Output != "" {
			body["output"] = ae.Output
		}
		httpx.WriteJSON(w, ae.Status, body)
		return
	}
	httpx.Fail(w, r, err)
}

// ---------------------------------------------------------------- pending & apply

func (h *handlers) pending(w http.ResponseWriter, r *http.Request) {
	s, err := h.svc()
	if err != nil {
		writeErr(w, r, err)
		return
	}
	p, summary, err := s.pendingWithSummary(r.Context())
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"count": p.Count, "items": p.Items, "liveVersion": p.LiveVersion, "summary": summary})
}

type diffResponse struct {
	Version    int64      `json:"version"`
	Against    int64      `json:"against"`
	Files      []FileDiff `json:"files"`
	Paths      []string   `json:"paths"`
	ValidateMs int64      `json:"validateMs"`
	ReloadMs   int64      `json:"reloadMs"`
	Error      string     `json:"error,omitempty"`
}

func (h *handlers) pendingDiff(w http.ResponseWriter, r *http.Request) {
	s, err := h.svc()
	if err != nil {
		writeErr(w, r, err)
		return
	}
	ctx := r.Context()
	snap, err := h.app.Store.Snapshot(ctx)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	resp := diffResponse{Files: []FileDiff{}, Paths: []string{}}
	oldFiles := map[string]string{}
	if live, err := h.app.Store.LiveVersion(ctx, true); err == nil {
		resp.Against = live.ID
		oldFiles = versionFileMap(live)
	} else if !errors.Is(err, store.ErrNotFound) {
		writeErr(w, r, err)
		return
	}
	rd, err := s.renderAll(ctx, snap)
	if err != nil {
		resp.Error = err.Error()
		httpx.WriteJSON(w, http.StatusOK, resp)
		return
	}
	newFiles := proxyFileMap(rd.proxyEngine, rd.proxy)
	newFiles["haproxy.cfg"] = rd.haproxy["haproxy.cfg"]
	resp.Files, resp.Paths = diffMaps(redactFiles(oldFiles), redactFiles(newFiles), r.URL.Query().Get("file"))
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func (h *handlers) apply(w http.ResponseWriter, r *http.Request) {
	var opts core.ApplyOptions
	if r.ContentLength != 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&opts); err != nil && !errors.Is(err, io.EOF) {
			writeErr(w, r, httpx.Errorf(http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error()))
			return
		}
	}
	if h.app.Engine == nil {
		writeErr(w, r, core.ErrNotImplemented)
		return
	}
	v, err := h.app.Engine.Apply(r.Context(), opts)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	h.writeVersion(w, r, v)
}

func (h *handlers) writeVersion(w http.ResponseWriter, r *http.Request, v *core.Version) {
	row, err := h.app.Store.GetVersion(r.Context(), v.ID, false)
	if err != nil {
		httpx.WriteJSON(w, http.StatusOK, v)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toVersionJSON(row))
}

func (h *handlers) discard(w http.ResponseWriter, r *http.Request) {
	s, err := h.svc()
	if err != nil {
		writeErr(w, r, err)
		return
	}
	p, err := s.Discard(r.Context())
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

// ---------------------------------------------------------------- versions

type versionJSON struct {
	core.Version
	FailedEngine   string `json:"failedEngine,omitempty"`
	FailedStage    string `json:"failedStage,omitempty"`
	Output         string `json:"output,omitempty"`
	ProxyEngine    string `json:"proxyEngine"` // nginx | edge
	ProxyHash      string `json:"proxyHash"`
	NginxHash      string `json:"nginxHash"` // proxy engine hash (kept for compatibility)
	HAProxyHash    string `json:"haproxyHash"`
	HAProxyRunning bool   `json:"haproxyRunning"`
}

func toVersionJSON(row *store.VersionRow) versionJSON {
	return versionJSON{Version: toVersion(row), FailedEngine: row.FailedEngine, FailedStage: row.FailedStage, Output: row.Output,
		ProxyEngine: rowEngine(row), ProxyHash: row.NginxHash, NginxHash: row.NginxHash, HAProxyHash: row.HAProxyHash, HAProxyRunning: row.HAProxyRunning}
}

func (h *handlers) versions(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	rows, err := h.app.Store.ListVersions(r.Context(), limit, before)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	out := make([]versionJSON, 0, len(rows))
	for i := range rows {
		out = append(out, toVersionJSON(&rows[i]))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func versionID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(chi.URLParam(r, "id"), "v"), 10, 64)
	if err != nil || id <= 0 {
		return 0, httpx.Errorf(http.StatusBadRequest, "bad_request", "invalid version id")
	}
	return id, nil
}

func (h *handlers) version(w http.ResponseWriter, r *http.Request) {
	id, err := versionID(r)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	row, err := h.app.Store.GetVersion(r.Context(), id, true)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	files := map[string]string{}
	json.Unmarshal([]byte(row.NginxFiles), &files)
	httpx.WriteJSON(w, http.StatusOK, struct {
		versionJSON
		// NginxFiles holds the proxy engine's files (see proxyEngine).
		NginxFiles map[string]string `json:"nginxFiles"`
		HAProxyCfg string            `json:"haproxyCfg"`
	}{toVersionJSON(row), redactFiles(files), row.HAProxyCfg})
}

// versionFileMap returns a version's files for diffs: proxy engine files
// (Relay Edge ones under edge/) plus haproxy.cfg.
func versionFileMap(row *store.VersionRow) map[string]string {
	raw := map[string]string{}
	json.Unmarshal([]byte(row.NginxFiles), &raw)
	files := proxyFileMap(rowEngine(row), raw)
	if row.HAProxyCfg != "" {
		files["haproxy.cfg"] = row.HAProxyCfg
	}
	return files
}

func diffMaps(old, new map[string]string, only string) ([]FileDiff, []string) {
	all := diffFileSets(old, new)
	paths := make([]string, 0, len(all))
	for _, f := range all {
		paths = append(paths, f.Path)
	}
	if only == "" {
		return all, paths
	}
	out := []FileDiff{}
	for _, f := range all {
		if f.Path == only {
			out = append(out, f)
		}
	}
	return out, paths
}

func (h *handlers) versionDiff(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := versionID(r)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	row, err := h.app.Store.GetVersion(ctx, id, true)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	against := int64(0)
	if v := r.URL.Query().Get("against"); v != "" {
		if against, err = strconv.ParseInt(strings.TrimPrefix(v, "v"), 10, 64); err != nil {
			writeErr(w, r, httpx.Errorf(http.StatusBadRequest, "bad_request", "invalid against version"))
			return
		}
	} else if against, err = h.app.Store.PreviousVersionID(ctx, id); err != nil {
		writeErr(w, r, err)
		return
	}
	oldFiles := map[string]string{}
	if against > 0 {
		prev, err := h.app.Store.GetVersion(ctx, against, true)
		if err != nil {
			writeErr(w, r, err)
			return
		}
		oldFiles = versionFileMap(prev)
	}
	resp := diffResponse{Version: id, Against: against, ValidateMs: row.ValidateMs, ReloadMs: row.ReloadMs}
	resp.Files, resp.Paths = diffMaps(redactFiles(oldFiles), redactFiles(versionFileMap(row)), r.URL.Query().Get("file"))
	httpx.WriteJSON(w, http.StatusOK, resp)
}

var htpasswdLine = regexp.MustCompile(`(?m)^([^#:\n][^:\n]*):\S+$`)

// redactFiles hides password hashes in htpasswd files.
func redactFiles(files map[string]string) map[string]string {
	out := make(map[string]string, len(files))
	for p, c := range files {
		if strings.HasPrefix(p, "htpasswd/") || strings.HasPrefix(p, "edge/htpasswd/") {
			c = htpasswdLine.ReplaceAllString(c, "$1:<bcrypt hash>")
		}
		out[p] = c
	}
	return out
}

func (h *handlers) download(w http.ResponseWriter, r *http.Request) {
	id, err := versionID(r)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	row, err := h.app.Store.GetVersion(r.Context(), id, true)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	files := map[string]string{}
	json.Unmarshal([]byte(row.NginxFiles), &files)
	if !httpx.Actor(r).IsAdmin() {
		files = redactFiles(files)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	root := fmt.Sprintf("relay-v%d", id)
	add := func(name, content string) {
		tw.WriteHeader(&tar.Header{Name: root + "/" + name, Mode: 0o644, Size: int64(len(content)), ModTime: row.CreatedAt, Typeflag: tar.TypeReg})
		io.WriteString(tw, content)
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	engine := rowEngine(row)
	for _, p := range paths {
		add(engine+"/"+p, files[p])
	}
	if row.HAProxyCfg != "" {
		add("haproxy/haproxy.cfg", row.HAProxyCfg)
	}
	meta, _ := json.MarshalIndent(toVersionJSON(row), "", "  ")
	add("version.json", string(meta)+"\n")
	tw.Close()
	gz.Close()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.tar.gz"`, root))
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(http.StatusOK)
	w.Write(buf.Bytes())
}

func (h *handlers) rollback(w http.ResponseWriter, r *http.Request) {
	s, err := h.svc()
	if err != nil {
		writeErr(w, r, err)
		return
	}
	id, err := versionID(r)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	v, err := s.RollbackTo(r.Context(), id)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	h.writeVersion(w, r, v)
}

// ---------------------------------------------------------------- engines

func (h *handlers) engines(w http.ResponseWriter, r *http.Request) {
	if h.app.Engine == nil {
		writeErr(w, r, core.ErrNotImplemented)
		return
	}
	st, err := h.app.Engine.Status(r.Context())
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, st)
}

func (h *handlers) client(r *http.Request) (*agent.Client, string, error) {
	switch e := chi.URLParam(r, "engine"); e {
	case agent.EngineNginx:
		return h.app.Nginx, e, nil
	case agent.EngineHAProxy:
		return h.app.HAProxy, e, nil
	case agent.EngineEdge:
		return h.app.Edge, e, nil
	}
	return nil, "", httpx.Errorf(http.StatusNotFound, "not_found", "unknown engine (nginx | haproxy | edge)")
}

func (h *handlers) engineAction(w http.ResponseWriter, r *http.Request) {
	c, engine, err := h.client(r)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	action := chi.URLParam(r, "action")
	if action != "start" && action != "stop" && action != "reload" {
		writeErr(w, r, httpx.Errorf(http.StatusNotFound, "not_found", "unknown action (start | stop | reload)"))
		return
	}
	// Starting may first start the engine's container and wait for its agent.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 150*time.Second)
	defer cancel()
	var resp *agent.ActionResponse
	if svc, ok := h.app.Engine.(*Service); ok {
		resp, err = svc.EngineAction(ctx, engine, action)
	} else {
		resp, err = runAction(ctx, c, action)
	}
	if err != nil {
		writeErr(w, r, err)
		return
	}
	result, detail := "ok", ""
	if !resp.OK {
		result, detail = "failed", firstErrorLine(resp.Output)
	}
	h.app.Audit(r.Context(), core.AuditEntry{Action: "engine." + action, Target: engine, Detail: detail, Result: result})
	sctx, scancel := context.WithTimeout(ctx, 3*time.Second)
	if st, err := c.Status(sctx); err == nil {
		h.app.Bus.Publish(events.EngineChanged, map[string]any{"engine": engine, "running": st.Running, "reachable": true})
	}
	scancel()
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func runAction(ctx context.Context, c *agent.Client, action string) (*agent.ActionResponse, error) {
	switch action {
	case "start":
		return c.Start(ctx)
	case "stop":
		return c.Stop(ctx)
	}
	return c.Reload(ctx)
}

func (h *handlers) engineLogs(w http.ResponseWriter, r *http.Request) {
	c, _, err := h.client(r)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	var since time.Time
	if v, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64); err == nil && v > 0 {
		since = time.UnixMilli(v)
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	resp, err := c.Logs(r.Context(), since, limit)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if resp.Lines == nil {
		resp.Lines = []agent.LogLine{}
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func (h *handlers) engineListeners(w http.ResponseWriter, r *http.Request) {
	c, _, err := h.client(r)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	resp, err := c.Listeners(r.Context())
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if resp.Listeners == nil {
		resp.Listeners = []agent.Listener{}
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------- previews

type configPreview struct {
	Config string `json:"config"`
	Valid  bool   `json:"valid"`
	Output string `json:"output"`
	Engine string `json:"engine"` // proxy engine that rendered it: nginx | edge
}

func (h *handlers) previewHost(w http.ResponseWriter, r *http.Request) {
	s, err := h.svc()
	if err != nil {
		writeErr(w, r, err)
		return
	}
	var body struct {
		Host *model.ProxyHost `json:"host"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		writeErr(w, r, err)
		return
	}
	if body.Host == nil {
		writeErr(w, r, httpx.Errorf(http.StatusBadRequest, "bad_request", "host required"))
		return
	}
	ctx := r.Context()
	snap, err := h.app.Store.Snapshot(ctx)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	engine := h.app.ProxyEngine(ctx)
	pr, err := rendererFor(engine)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	env := s.envFor(ctx, engine)
	ensureDefaultCert(env)
	rs := s.renderSnapshot(snap, env)
	host := *body.Host
	cfg, rerr := pr.host(rs, &host, env)
	resp := configPreview{Config: cfg, Valid: rerr == nil, Engine: engine}
	if rerr != nil {
		resp.Output = rerr.Error()
		httpx.WriteJSON(w, http.StatusOK, resp)
		return
	}
	host.Enabled = true
	files, err := pr.render(nginx.WithHost(rs, &host), env)
	if err != nil {
		resp.Valid, resp.Output = false, err.Error()
		httpx.WriteJSON(w, http.StatusOK, resp)
		return
	}
	h.validatePreview(ctx, engine, files, &resp)
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func (h *handlers) previewStream(w http.ResponseWriter, r *http.Request) {
	s, err := h.svc()
	if err != nil {
		writeErr(w, r, err)
		return
	}
	var body struct {
		Stream *model.Stream `json:"stream"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		writeErr(w, r, err)
		return
	}
	if body.Stream == nil {
		writeErr(w, r, httpx.Errorf(http.StatusBadRequest, "bad_request", "stream required"))
		return
	}
	ctx := r.Context()
	snap, err := h.app.Store.Snapshot(ctx)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	engine := h.app.ProxyEngine(ctx)
	pr, err := rendererFor(engine)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	env := s.envFor(ctx, engine)
	ensureDefaultCert(env)
	rs := s.renderSnapshot(snap, env)
	st := *body.Stream
	cfg, rerr := pr.stream(rs, &st, env)
	resp := configPreview{Config: cfg, Valid: rerr == nil, Engine: engine}
	if rerr != nil {
		resp.Output = rerr.Error()
		httpx.WriteJSON(w, http.StatusOK, resp)
		return
	}
	st.Enabled = true
	files, err := pr.render(nginx.WithStream(rs, &st), env)
	if err != nil {
		resp.Valid, resp.Output = false, err.Error()
		httpx.WriteJSON(w, http.StatusOK, resp)
		return
	}
	h.validatePreview(ctx, engine, files, &resp)
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func (h *handlers) validatePreview(ctx context.Context, engine string, files agent.Files, resp *configPreview) {
	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	vr, err := h.app.Client(engine).Validate(vctx, files)
	var ua agent.ErrUnavailable
	switch {
	case errors.As(err, &ua):
		resp.Output = "Rendered, but not checked with " + checkName(engine) + ": the " + engineLabel(engine) + " engine is unreachable."
	case err != nil:
		resp.Output = "Rendered, but not checked with " + checkName(engine) + ": " + err.Error()
	default:
		resp.Valid = vr.OK
		resp.Output = vr.Output
	}
}

// ---------------------------------------------------------------- engine image updates

func (h *handlers) enginesSvc(w http.ResponseWriter, r *http.Request) core.Engines {
	if h.app.Engines == nil {
		writeErr(w, r, core.ErrNotImplemented)
		return nil
	}
	return h.app.Engines
}

func (h *handlers) engineUpdates(w http.ResponseWriter, r *http.Request) {
	e := h.enginesSvc(w, r)
	if e == nil {
		return
	}
	u, err := e.Updates(r.Context())
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, u)
}

func (h *handlers) checkEngineUpdates(w http.ResponseWriter, r *http.Request) {
	e := h.enginesSvc(w, r)
	if e == nil {
		return
	}
	u, err := e.CheckUpdates(r.Context())
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, u)
}

func (h *handlers) upgradeStatus(w http.ResponseWriter, r *http.Request) {
	e := h.enginesSvc(w, r)
	if e == nil {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"job": e.UpgradeStatus()})
}

func (h *handlers) upgradeEngine(w http.ResponseWriter, r *http.Request) {
	e := h.enginesSvc(w, r)
	if e == nil {
		return
	}
	var body struct {
		Version string `json:"version"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		writeErr(w, r, err)
		return
	}
	job, err := e.Upgrade(r.Context(), chi.URLParam(r, "engine"), body.Version)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, job)
}

func (h *handlers) updateRelay(w http.ResponseWriter, r *http.Request) {
	e := h.enginesSvc(w, r)
	if e == nil {
		return
	}
	var body struct {
		RestartEngines bool `json:"restartEngines"`
	}
	if r.ContentLength != 0 {
		if err := httpx.Decode(r, &body); err != nil {
			writeErr(w, r, err)
			return
		}
	}
	job, err := e.UpdateRelay(r.Context(), body.RestartEngines)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, job)
}

func (h *handlers) relayUpdateStatus(w http.ResponseWriter, r *http.Request) {
	e := h.enginesSvc(w, r)
	if e == nil {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"job": e.RelayUpdateStatus()})
}

func (h *handlers) keepEngineImage(w http.ResponseWriter, r *http.Request) {
	e := h.enginesSvc(w, r)
	if e == nil {
		return
	}
	if err := e.KeepRunningImage(r.Context(), chi.URLParam(r, "engine")); err != nil {
		writeErr(w, r, err)
		return
	}
	u, err := e.Updates(r.Context())
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, u)
}
