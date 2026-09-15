// Package apply is the engine slice's apply pipeline: pending changes,
// render → validate → swap → reload → health check with automatic rollback,
// config versions, and the reconcile loop that keeps the engine containers on
// the live version.
package apply

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/instantoffr/relay/internal/agent"
	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/render"
	"github.com/instantoffr/relay/internal/render/edge"
	"github.com/instantoffr/relay/internal/render/haproxy"
	"github.com/instantoffr/relay/internal/render/nginx"
	"github.com/instantoffr/relay/internal/store"
)

type Service struct {
	app *core.App
	log *slog.Logger

	applyMu sync.Mutex

	mu           sync.Mutex
	modules      map[string]map[string]bool   // per proxy engine (nginx, edge)
	modulePaths  map[string]map[string]string // per proxy engine
	lastPending  int
	liveID       int64
	liveSnap     *model.Snapshot
	observed     map[string]engineObs
	healthWindow time.Duration
	switchTo     string // proxy engine an apply is switching to
}

type engineObs struct {
	reachable, running bool
}

func New(app *core.App) *Service {
	if port := strings.TrimSpace(os.Getenv(agent.StatusPortEnv)); port != "" {
		nginx.StubStatusAddr = "127.0.0.1:" + port
	} else if addr := strings.TrimSpace(os.Getenv("RELAY_STUB_STATUS_ADDR")); addr != "" {
		nginx.StubStatusAddr = addr
	}
	if port := strings.TrimSpace(os.Getenv(agent.EdgeStatusPortEnv)); port != "" {
		edge.StatusAddr = "127.0.0.1:" + port
	}
	log := app.Log
	if log == nil {
		log = slog.Default()
	}
	return &Service{app: app, log: log.With("svc", "engine"), lastPending: -1, observed: map[string]engineObs{}, healthWindow: 10 * time.Second,
		modules: map[string]map[string]bool{}, modulePaths: map[string]map[string]string{}}
}

// modulesKV / modulePathsKV store the modules last reported by a proxy
// engine agent ("engine.nginx.modules", "engine.edge.modules").
func modulesKV(engine string) string     { return "engine." + engine + ".modules" }
func modulePathsKV(engine string) string { return "engine." + engine + ".modulePaths" }

// defaultModules are assumed before the proxy engine agent was ever reached
// (they match the official nginx image, where all of them are compiled in,
// and Relay Edge).
var defaultModules = map[string]bool{"stream": true, "http_v3": true, "http_v2": true, "auth_request": true, "stub_status": true}

func (s *Service) Start(ctx context.Context) error {
	for _, engine := range []string{agent.EngineNginx, agent.EngineEdge} {
		if b, err := s.app.Store.GetKV(ctx, modulesKV(engine)); err == nil {
			var mods []string
			if json.Unmarshal(b, &mods) == nil && len(mods) > 0 {
				var paths map[string]string
				if pb, err := s.app.Store.GetKV(ctx, modulePathsKV(engine)); err == nil {
					json.Unmarshal(pb, &paths)
				}
				s.setModules(ctx, engine, mods, paths, false)
			}
		}
	}
	s.app.SyncProxyEngineFile(ctx)
	go s.watchConfig(ctx)
	go s.reconcileLoop(ctx)
	return nil
}

func (s *Service) setModules(ctx context.Context, engine string, mods []string, paths map[string]string, persist bool) {
	m := map[string]bool{}
	for _, x := range mods {
		m[x] = true
	}
	s.mu.Lock()
	cur, curPaths := s.modules[engine], s.modulePaths[engine]
	changed := len(m) != len(cur)
	for k := range m {
		if !cur[k] {
			changed = true
		}
	}
	if len(paths) != len(curPaths) {
		changed = true
	}
	for k, v := range paths {
		if curPaths[k] != v {
			changed = true
		}
	}
	s.modules[engine] = m
	s.modulePaths[engine] = paths
	s.mu.Unlock()
	if changed && persist {
		pb, _ := json.Marshal(paths)
		s.app.Store.PutKV(context.WithoutCancel(ctx), modulePathsKV(engine), pb)
		sorted := append([]string(nil), mods...)
		sort.Strings(sorted)
		b, _ := json.Marshal(sorted)
		s.app.Store.PutKV(context.WithoutCancel(ctx), modulesKV(engine), b)
	}
}

// env returns the render environment of the active proxy engine as seen
// from the engine containers.
func (s *Service) env(ctx context.Context) render.Env {
	return s.envFor(ctx, s.app.ProxyEngine(ctx))
}

// envFor returns the render environment for a proxy engine (its modules).
func (s *Service) envFor(ctx context.Context, engine string) render.Env {
	cfg := s.app.Config
	env := render.DefaultEnv(cfg.DataDir, cfg.RunDir, cfg.LogDir)
	if gen, err := store.LoadSettings[model.GeneralSettings](ctx, s.app.Store, model.SettingsGeneral); err == nil && gen.AdminPort > 0 {
		env.AdminUpstream = fmt.Sprintf("127.0.0.1:%d", gen.AdminPort)
	}
	geo := filepath.Join(cfg.DataDir, "geoip", "GeoLite2-Country.mmdb")
	if st, err := os.Stat(geo); err == nil && st.Size() > 0 {
		env.GeoIPCountry = geo
	}
	s.mu.Lock()
	mods := s.modules[engine]
	paths := s.modulePaths[engine]
	s.mu.Unlock()
	if len(mods) == 0 {
		mods = defaultModules
	}
	env.Modules = map[string]bool{}
	for k, v := range mods {
		env.Modules[k] = v
	}
	env.ModulePaths = map[string]string{}
	for k, v := range paths {
		env.ModulePaths[k] = v
	}
	return env
}

// ---------------------------------------------------------------- snapshots

func (s *Service) liveVersion(ctx context.Context) (*store.VersionRow, *model.Snapshot, error) {
	row, err := s.app.Store.LiveVersion(ctx, false)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	if s.liveID == row.ID && s.liveSnap != nil {
		snap := s.liveSnap
		s.mu.Unlock()
		return row, snap, nil
	}
	s.mu.Unlock()
	full, err := s.app.Store.GetVersion(ctx, row.ID, true)
	if err != nil {
		return nil, nil, err
	}
	var snap model.Snapshot
	if err := json.Unmarshal([]byte(full.Snapshot), &snap); err != nil {
		return nil, nil, fmt.Errorf("live version v%d snapshot: %w", row.ID, err)
	}
	s.mu.Lock()
	s.liveID, s.liveSnap = row.ID, &snap
	s.mu.Unlock()
	return full, &snap, nil
}

// renderSnapshot adjusts a snapshot for rendering: certificates whose PEM
// files are missing are treated as unusable so one broken certificate can't
// break the whole nginx config.
func (s *Service) renderSnapshot(snap *model.Snapshot, env render.Env) *model.Snapshot {
	out := *snap
	out.Certificates = make([]model.Certificate, len(snap.Certificates))
	copy(out.Certificates, snap.Certificates)
	for i := range out.Certificates {
		c := &out.Certificates[i]
		if c.NotAfter == nil || c.NotAfter.IsZero() {
			continue
		}
		full, key := env.CertPaths(c.ID)
		if !fileExists(full) || !fileExists(key) {
			c.NotAfter = nil
			c.Status = "PEM files missing"
		}
	}
	return &out
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Size() > 0
}

type rendered struct {
	proxyEngine string      // nginx | edge
	proxy       agent.Files // the proxy engine's files
	haproxy     agent.Files
	proxyHash   string
	haproxyHash string
	haproxyRun  bool
}

func (s *Service) renderAll(ctx context.Context, snap *model.Snapshot) (*rendered, error) {
	engine := snapshotEngine(snap)
	env := s.envFor(ctx, engine)
	if err := ensureDefaultCert(env); err != nil {
		s.log.Warn("default certificate", "err", err)
	}
	rs := s.renderSnapshot(snap, env)
	pr, err := rendererFor(engine)
	if err != nil {
		return nil, err
	}
	files, err := pr.render(rs, env)
	if err != nil {
		return nil, err
	}
	run := haproxy.HasBackends(rs)
	cfg, err := haproxy.Render(rs, env)
	if err != nil {
		return nil, fmt.Errorf("haproxy render: %w", err)
	}
	if run && strings.TrimSpace(cfg) == "" {
		return nil, errors.New("haproxy render: the HAProxy renderer produced no configuration")
	}
	if strings.TrimSpace(cfg) == "" {
		cfg = "# HAProxy is stopped: no load-balancer backends are configured.\n"
	}
	hfiles := agent.Files{"haproxy.cfg": cfg}
	return &rendered{proxyEngine: engine, proxy: files, haproxy: hfiles, proxyHash: agent.HashFiles(files), haproxyHash: agent.HashFiles(hfiles), haproxyRun: run}, nil
}

// ---------------------------------------------------------------- pending

func (s *Service) Pending(ctx context.Context) (*core.Pending, error) {
	p, _, err := s.pendingWithSummary(ctx)
	return p, err
}

func (s *Service) pendingWithSummary(ctx context.Context) (*core.Pending, string, error) {
	snap, err := s.app.Store.Snapshot(ctx)
	if err != nil {
		return nil, "", err
	}
	row, live, err := s.liveVersion(ctx)
	if err != nil {
		return nil, "", err
	}
	items := computePending(snap, live)
	p := &core.Pending{Count: len(items), Items: items}
	if row != nil {
		p.LiveVersion = row.ID
	}
	return p, summarize(items, snap, live), nil
}

func (s *Service) watchConfig(ctx context.Context) {
	ch, cancel := s.app.Bus.Subscribe(128)
	defer cancel()
	var timer *time.Timer
	fire := make(chan struct{}, 1)
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.Topic != events.ConfigChanged && ev.Topic != events.ApplyFinished && ev.Topic != events.CertChanged {
				continue
			}
			if timer != nil {
				timer.Stop()
			}
			timer = time.AfterFunc(300*time.Millisecond, func() {
				select {
				case fire <- struct{}{}:
				default:
				}
			})
		case <-fire:
			s.publishPending(ctx, false)
		}
	}
}

func (s *Service) publishPending(ctx context.Context, force bool) {
	p, _, err := s.pendingWithSummary(ctx)
	if err != nil {
		s.log.Warn("pending", "err", err)
		return
	}
	s.mu.Lock()
	changed := p.Count != s.lastPending
	s.lastPending = p.Count
	s.mu.Unlock()
	if changed || force {
		s.app.Bus.Publish(events.PendingChanged, map[string]any{"count": p.Count})
	}
}

// ---------------------------------------------------------------- status

func (s *Service) Status(ctx context.Context) (*core.EnginesStatus, error) {
	var out core.EnginesStatus
	var wg sync.WaitGroup
	get := func(engine string, c *agent.Client, dst *core.EngineState) {
		defer wg.Done()
		if c == nil {
			*dst = core.EngineState{Status: agent.Status{Engine: engine, Modules: []string{}}, Error: "agent client not configured"}
			return
		}
		cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		st, err := c.Status(cctx)
		if err != nil {
			*dst = core.EngineState{Status: agent.Status{Engine: c.Engine, Modules: []string{}}, Reachable: false, Error: agentError(err)}
			return
		}
		if st.Modules == nil {
			st.Modules = []string{}
		}
		*dst = core.EngineState{Status: *st, Reachable: true}
	}
	wg.Add(3)
	go get(agent.EngineNginx, s.app.Nginx, &out.Nginx)
	go get(agent.EngineHAProxy, s.app.HAProxy, &out.HAProxy)
	go get(agent.EngineEdge, s.app.Edge, &out.Edge)
	wg.Wait()
	if out.Nginx.Reachable && len(out.Nginx.Modules) > 0 {
		s.setModules(ctx, agent.EngineNginx, out.Nginx.Modules, out.Nginx.DynamicModules, true)
	}
	if out.Edge.Reachable && len(out.Edge.Modules) > 0 {
		s.setModules(ctx, agent.EngineEdge, out.Edge.Modules, out.Edge.DynamicModules, true)
	}
	out.Proxy = s.app.ProxyEngine(ctx)
	return &out, nil
}

func agentError(err error) string {
	var ua agent.ErrUnavailable
	if errors.As(err, &ua) {
		var op *net.OpError
		if errors.As(err, &op) {
			return "agent socket unreachable (" + op.Err.Error() + ")"
		}
		return "agent socket unreachable"
	}
	return err.Error()
}

// ---------------------------------------------------------------- apply

// ApplyError carries the failed version and engine output.
type ApplyError struct {
	Status  int
	Code    string
	Message string
	Version *core.Version
	Output  string
}

func (e *ApplyError) Error() string { return e.Message }

func (s *Service) Apply(ctx context.Context, opts core.ApplyOptions) (*core.Version, error) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	return s.applyLocked(ctx, opts, false)
}

type progressFn func(stage, message string, pct int)

func (s *Service) applyLocked(ctx context.Context, opts core.ApplyOptions, force bool) (*core.Version, error) {
	ctx = context.WithoutCancel(ctx)
	actor := core.ActorFrom(ctx)
	if actor.IsZero() {
		actor = core.SystemActor
		ctx = core.WithActor(ctx, actor)
	}
	snap, err := s.app.Store.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	liveRow, liveSnap, err := s.liveVersion(ctx)
	if err != nil {
		return nil, err
	}
	items := computePending(snap, liveSnap)
	if len(items) == 0 && liveRow != nil && !force {
		return nil, &ApplyError{Status: http.StatusConflict, Code: "no_changes", Message: fmt.Sprintf("Nothing to apply: v%d already matches the configuration.", liveRow.ID)}
	}
	id, err := s.app.Store.NextVersionID(ctx)
	if err != nil {
		return nil, err
	}
	summary := strings.TrimSpace(opts.Summary)
	if summary == "" {
		summary = summarize(items, snap, liveSnap)
	}
	if summary == "" {
		summary = "Re-applied configuration"
	}
	progress := func(stage, message string, pct int) {
		s.app.Bus.Publish(events.ApplyProgress, map[string]any{"version": id, "stage": stage, "message": message, "progress": pct})
	}
	var liveID int64
	if liveRow != nil {
		liveID = liveRow.ID
	}
	snapJSON, _ := json.Marshal(snap)
	changesJSON, _ := json.Marshal(items)
	row := &store.VersionRow{ID: id, CreatedAt: time.Now().UTC(), Actor: actor.Label(), Summary: summary, Status: "draft", Snapshot: string(snapJSON), Changes: string(changesJSON)}

	// The proxy engine of the new version; switching engines stops the old
	// one and starts the new one instead of reloading.
	engine := snapshotEngine(snap)
	prevEngine := agent.EngineNginx
	if liveRow != nil {
		prevEngine = rowEngine(liveRow)
	}
	switching := prevEngine != engine
	row.ProxyEngine = engine
	pc := s.app.Client(engine)

	// Render.
	progress("render", fmt.Sprintf("Applying v%d… · rendering config", id), 5)
	r, err := s.renderAll(ctx, snap)
	if err != nil {
		row.NginxFiles, row.HAProxyCfg = "{}", ""
		return s.failBeforeSwap(ctx, row, engine, "render", err.Error(), "Config could not be rendered")
	}
	nf, _ := json.Marshal(r.proxy)
	row.NginxFiles, row.HAProxyCfg = string(nf), r.haproxy["haproxy.cfg"]
	row.NginxHash, row.HAProxyHash, row.HAProxyRunning = r.proxyHash, r.haproxyHash, r.haproxyRun

	// Validate.
	progress("validate", fmt.Sprintf("Applying v%d… · validating · %s", id, checkName(engine)), 20)
	vctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	nv, err := pc.Validate(vctx, r.proxy)
	cancel()
	if err != nil {
		return nil, s.unavailable(engine, err)
	}
	row.ValidateMs = nv.DurationMs
	if !nv.OK {
		return s.failBeforeSwap(ctx, row, engine, "validate", nv.Output, checkName(engine)+" failed")
	}
	warnings := countWarnings(nv.Output)
	output := nv.Output
	hst, herr := s.app.HAProxy.Status(ctx)
	haproxyReachable := herr == nil
	if r.haproxyRun {
		progress("validate", fmt.Sprintf("Applying v%d… · validating · haproxy -c", id), 30)
		vctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		hv, err := s.app.HAProxy.Validate(vctx, r.haproxy)
		cancel()
		if err != nil {
			return nil, s.unavailable("haproxy", err)
		}
		row.ValidateMs += hv.DurationMs
		if !hv.OK {
			return s.failBeforeSwap(ctx, row, "haproxy", "validate", hv.Output, "haproxy -c failed")
		}
		warnings += countWarnings(hv.Output)
		output = joinOutput(output, hv.Output)
	}
	row.Output = output

	// Learn which changed hosts (every routable host when switching engines)
	// are healthy before the swap.
	targets := s.probeTargets(snap, liveSnap, items)
	if switching {
		targets = switchTargets(snap)
	}
	var liveFiles agent.Files
	liveHash := ""
	if switching && liveRow != nil {
		if liveFiles, liveHash, err = s.liveProxyFiles(ctx, liveRow.ID); err != nil {
			return nil, err
		}
	}
	healthyBefore := map[string]bool{}
	if liveRow != nil && len(targets) > 0 {
		progress("validate", fmt.Sprintf("Applying v%d… · validated · probing %d host(s)", id, len(targets)), 35)
		for t, ok := range s.probeAll(ctx, liveSnap, targets) {
			healthyBefore[t] = ok.ok
		}
	}

	if err := s.app.Store.InsertVersion(ctx, row); err != nil {
		return nil, err
	}

	// Swap + reload: HAProxy first so new localhost frontends exist before
	// nginx routes to them.
	sw := &proxySwap{engine: engine, files: r.proxy, hash: r.proxyHash}
	if haproxyReachable && (hst.ConfigHash != r.haproxyHash || hst.Running != r.haproxyRun) {
		verb := "reloading haproxy"
		if !r.haproxyRun {
			verb = "stopping haproxy"
		} else if !hst.Running {
			verb = "starting haproxy"
		}
		progress("reload", fmt.Sprintf("Applying v%d… · validated · %s", id, verb), 45)
		resp, err := s.app.HAProxy.Apply(ctx, agent.ApplyRequest{Files: r.haproxy, Hash: r.haproxyHash, Stop: !r.haproxyRun})
		if err != nil {
			return s.failAfterSwap(ctx, row, liveID, "haproxy", "swap", err.Error(), &proxySwap{})
		}
		row.ReloadMs += resp.ReloadMs
		if !resp.OK {
			return s.failAfterSwap(ctx, row, liveID, "haproxy", resp.Stage, resp.Output, &proxySwap{})
		}
		sw.haproxy = true
	} else if !haproxyReachable && r.haproxyRun {
		return s.failAfterSwap(ctx, row, liveID, "haproxy", "swap", agentError(herr), &proxySwap{})
	}

	if switching {
		s.setSwitching(engine)
		defer s.setSwitching("")
		sw.from = prevEngine
		progress("reload", fmt.Sprintf("Applying v%d… · validated · stopping %s", id, engineLabel(prevEngine)), 50)
		stop := agent.ApplyRequest{Stop: true}
		if liveFiles != nil {
			stop.Files, stop.Hash = liveFiles, liveHash
		}
		sw.oldStop = true
		sresp, err := s.app.Client(prevEngine).Apply(ctx, stop)
		var ua agent.ErrUnavailable
		switch {
		case errors.As(err, &ua):
			// An unreachable agent can't be holding the ports (nor restarted).
			s.log.Warn("previous proxy engine unreachable, not stopped", "engine", prevEngine, "err", err)
			sw.oldStop = false
		case err != nil:
			return s.failAfterSwap(ctx, row, liveID, prevEngine, "stop", err.Error(), sw)
		case !sresp.OK:
			return s.failAfterSwap(ctx, row, liveID, prevEngine, "stop", sresp.Output, sw)
		}
		progress("reload", fmt.Sprintf("Applying v%d… · validated · starting %s", id, engineLabel(engine)), 55)
		sw.started = true
	} else {
		progress("reload", fmt.Sprintf("Applying v%d… · validated · reloading %s", id, engineLabel(engine)), 55)
	}
	resp, err := pc.Apply(ctx, agent.ApplyRequest{Files: r.proxy, Hash: r.proxyHash})
	if err != nil {
		return s.failAfterSwap(ctx, row, liveID, engine, "swap", err.Error(), sw)
	}
	row.ReloadMs += resp.ReloadMs
	if !resp.OK {
		return s.failAfterSwap(ctx, row, liveID, engine, resp.Stage, resp.Output, sw)
	}
	sw.swapped = true

	// Health check.
	if failure := s.healthCheck(ctx, id, engine, snap, targets, healthyBefore, progress); failure != "" {
		return s.failAfterSwap(ctx, row, liveID, engine, "health", failure, sw)
	}

	// Success.
	if err := s.app.Store.FinishVersion(ctx, row); err != nil {
		return nil, err
	}
	if err := s.app.Store.PromoteVersion(ctx, id); err != nil {
		return nil, err
	}
	row.Status = "live"
	s.mu.Lock()
	s.liveID, s.liveSnap = id, snap
	if switching {
		// Don't report the old engine stopping or the new one starting as outages.
		delete(s.observed, prevEngine)
		delete(s.observed, engine)
	}
	s.mu.Unlock()
	s.app.SetProxyEngine(engine)
	vid := id
	s.app.Audit(ctx, core.AuditEntry{Action: "config.apply", Target: fmt.Sprintf("v%d", id), Detail: summary, Result: "applied", Version: &vid})
	reloadDetail := fmt.Sprintf("nginx -s reload · %d ms", resp.ReloadMs)
	switch {
	case switching:
		reloadDetail = fmt.Sprintf("Switched from %s to %s · %d ms", engineLabel(prevEngine), engineLabel(engine), resp.ReloadMs)
	case engine == agent.EngineEdge:
		reloadDetail = fmt.Sprintf("Relay Edge reload · %d ms", resp.ReloadMs)
	}
	s.app.Activity(ctx, "reload", "info", fmt.Sprintf("Config reloaded · %d warning%s", warnings, plural(warnings)), fmt.Sprintf("v%d", id), reloadDetail)
	if switching {
		s.app.Bus.Publish(events.EngineChanged, map[string]any{"engine": prevEngine, "running": false, "reachable": true})
		s.app.Bus.Publish(events.EngineChanged, map[string]any{"engine": engine, "running": resp.Running, "reachable": true})
	}
	progress("done", fmt.Sprintf("v%d is live", id), 100)
	v := toVersion(row)
	s.app.Bus.Publish(events.ApplyFinished, map[string]any{"version": id, "status": "live", "error": ""})
	s.publishPending(ctx, true)
	return &v, nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func countWarnings(out string) int {
	n := 0
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "[warn]") || strings.Contains(l, "[WARNING]") {
			n++
		}
	}
	return n
}

func joinOutput(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n")
}

func (s *Service) unavailable(engine string, err error) error {
	name := engine
	if engine == agent.EngineEdge {
		name = engineLabel(engine)
	}
	return &ApplyError{Status: http.StatusServiceUnavailable, Code: "engine_unavailable",
		Message: fmt.Sprintf("The %s engine is unreachable (%s). Is the relay-%s container running?", name, agentError(err), engine)}
}

// failBeforeSwap records a failed version (nothing was changed on the engines).
func (s *Service) failBeforeSwap(ctx context.Context, row *store.VersionRow, engine, stage, output, title string) (*core.Version, error) {
	output = strings.TrimSpace(output)
	row.Status, row.FailedEngine, row.FailedStage, row.Output = "failed", engine, stage, output
	row.Error = title + ": " + firstErrorLine(output)
	if err := s.app.Store.InsertVersion(ctx, row); err != nil {
		return nil, err
	}
	vid := row.ID
	s.app.Audit(ctx, core.AuditEntry{Action: "config.apply", Target: fmt.Sprintf("v%d", row.ID), Detail: row.Error, Result: "failed", Version: &vid})
	s.app.Bus.Publish(events.ApplyFinished, map[string]any{"version": row.ID, "status": "failed", "error": row.Error})
	v := toVersion(row)
	return &v, &ApplyError{Status: http.StatusUnprocessableEntity, Code: "validation_failed", Message: row.Error, Version: &v, Output: output}
}

// failAfterSwap rolls the engines back to the live version and records the
// version as rolled back. The database keeps the edit as a draft.
func (s *Service) failAfterSwap(ctx context.Context, row *store.VersionRow, liveID int64, engine, stage, output string, sw *proxySwap) (*core.Version, error) {
	output = strings.TrimSpace(output)
	rbctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	var rbErrs []string
	switch {
	case sw.from == "" && sw.swapped:
		if resp, err := s.app.Client(sw.engine).Rollback(rbctx); err != nil {
			rbErrs = append(rbErrs, sw.engine+": "+err.Error())
		} else if !resp.OK {
			rbErrs = append(rbErrs, sw.engine+": "+resp.Output)
		}
	case sw.from != "":
		// Engine switch: stop the new engine again, then restart the old one.
		if sw.started {
			if resp, err := s.app.Client(sw.engine).Apply(rbctx, agent.ApplyRequest{Files: sw.files, Hash: sw.hash, Stop: true}); err != nil {
				rbErrs = append(rbErrs, sw.engine+": "+err.Error())
			} else if !resp.OK {
				rbErrs = append(rbErrs, sw.engine+": "+resp.Output)
			}
		}
		if sw.oldStop {
			if resp, err := s.app.Client(sw.from).Start(rbctx); err != nil {
				rbErrs = append(rbErrs, sw.from+": "+err.Error())
			} else if !resp.OK {
				rbErrs = append(rbErrs, sw.from+": "+resp.Output)
			}
		}
	}
	if sw.haproxy {
		if resp, err := s.app.HAProxy.Rollback(rbctx); err != nil {
			rbErrs = append(rbErrs, "haproxy: "+err.Error())
		} else if !resp.OK {
			rbErrs = append(rbErrs, "haproxy: "+resp.Output)
		}
	}
	row.FailedEngine, row.FailedStage, row.Output = engine, stage, output
	engineName := engineLabel(engine)
	restored := "The previous configuration is live again"
	if liveID > 0 {
		restored = fmt.Sprintf("v%d is live again", liveID)
		row.RolledBackTo = &liveID
		row.Status = "rolled_back"
	} else {
		row.Status = "failed"
	}
	switch stage {
	case "health":
		row.Error = fmt.Sprintf("%s. %s; your edit is saved as a draft.", output, restored)
	case "start":
		row.Error = fmt.Sprintf("%s failed to start after apply v%d. %s; your change is kept as a draft.", engineName, row.ID, restored)
	default:
		row.Error = fmt.Sprintf("%s failed to %s after apply v%d. %s; your change is kept as a draft.", engineName, orStage(stage), row.ID, restored)
	}
	if len(rbErrs) > 0 {
		row.Error += " Rollback problems: " + strings.Join(rbErrs, "; ")
	}
	if err := s.app.Store.FinishVersion(ctx, row); err != nil {
		s.log.Error("record failed version", "err", err)
	}
	vid := row.ID
	target := fmt.Sprintf("v%d", row.ID)
	if liveID > 0 {
		target = fmt.Sprintf("v%d → v%d", row.ID, liveID)
	}
	detail := fmt.Sprintf("%s %s failed", engineName, orStage(stage))
	if stage == "health" {
		detail = "health check failed (" + output + ")"
	}
	sys := core.SystemActor
	s.app.Audit(ctx, core.AuditEntry{Actor: &sys, Action: "config.rollback", Target: target, Detail: detail, Result: "auto", Version: &vid})
	s.app.Activity(ctx, "reload.rollback", "warn", "Change rolled back automatically", target, row.Error)
	if s.app.Notify != nil {
		s.app.Notify.Notify(ctx, core.Notification{Event: model.EventReloadFailed, Level: "error", Title: "Change rolled back automatically", Message: row.Error, URL: "/history?pending=1"})
	}
	s.app.Bus.Publish(events.ApplyFinished, map[string]any{"version": row.ID, "status": row.Status, "error": row.Error, "engine": engine, "stage": stage})
	s.publishPending(ctx, true)
	v := toVersion(row)
	return &v, nil
}

func orStage(stage string) string {
	switch stage {
	case "reload":
		return "reload"
	case "swap":
		return "swap its config"
	case "stop":
		return "stop"
	case "validate":
		return "validate"
	}
	return stage
}

func firstErrorLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for _, l := range lines {
		if strings.Contains(l, "[emerg]") || strings.Contains(l, "[ALERT]") || strings.Contains(l, "[error]") || strings.Contains(l, "render") {
			return strings.TrimSpace(l)
		}
	}
	if len(lines) > 0 {
		return strings.TrimSpace(lines[0])
	}
	return "unknown error"
}

func toVersion(r *store.VersionRow) core.Version {
	v := core.Version{
		ID: r.ID, CreatedAt: r.CreatedAt, Actor: r.Actor, Summary: r.Summary, Status: r.Status, Error: r.Error,
		ValidateMs: r.ValidateMs, ReloadMs: r.ReloadMs, RolledBackTo: r.RolledBackTo, Changes: []core.PendingItem{},
	}
	if r.Changes != "" {
		json.Unmarshal([]byte(r.Changes), &v.Changes)
	}
	return v
}

// ---------------------------------------------------------------- health check

type probeResult struct {
	ok     bool
	detail string // "502" | "connection refused" …
}

// probeTargets returns enabled hosts affected by the pending items that were
// already live (only those can regress).
func (s *Service) probeTargets(cur, live *model.Snapshot, items []core.PendingItem) []string {
	if live == nil {
		return nil
	}
	liveEnabled := map[string]bool{}
	for _, h := range live.Hosts {
		if h.Enabled {
			liveEnabled[h.ID] = true
		}
	}
	changedHosts := map[string]bool{}
	changedBackends := map[string]bool{}
	for _, it := range items {
		switch {
		case it.Kind == model.KindHost && it.Action == core.ActionUpdated:
			changedHosts[it.ID] = true
		case it.Kind == model.KindBackend || it.Kind == model.KindFrontend:
			changedBackends[it.ID] = true
		}
	}
	var out []string
	for _, h := range cur.Hosts {
		if !h.Enabled || !liveEnabled[h.ID] || len(h.Domains) == 0 {
			continue
		}
		if changedHosts[h.ID] || (h.Upstream.BackendID != "" && changedBackends[h.Upstream.BackendID]) {
			out = append(out, h.ID)
		}
	}
	sort.Strings(out)
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

func (s *Service) probeAll(ctx context.Context, snap *model.Snapshot, ids []string) map[string]probeResult {
	env := s.env(ctx)
	hosts := map[string]*model.ProxyHost{}
	for i := range snap.Hosts {
		hosts[snap.Hosts[i].ID] = &snap.Hosts[i]
	}
	out := map[string]probeResult{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, id := range ids {
		h := hosts[id]
		if h == nil {
			continue
		}
		wg.Add(1)
		go func(h *model.ProxyHost) {
			defer wg.Done()
			res := s.probe(ctx, snap, env, h)
			mu.Lock()
			out[h.ID] = res
			mu.Unlock()
		}(h)
	}
	wg.Wait()
	return out
}

// probe requests a host through the local nginx (host networking).
func (s *Service) probe(ctx context.Context, snap *model.Snapshot, env render.Env, h *model.ProxyHost) probeResult {
	domain := ""
	for _, d := range h.Domains {
		if !strings.HasPrefix(d, "*.") {
			domain = d
			break
		}
	}
	if domain == "" {
		domain = "relay-probe" + strings.TrimPrefix(h.Domains[0], "*")
	}
	httpPort, httpsPort := snap.General.HTTPPort, snap.General.HTTPSPort
	if httpPort <= 0 {
		httpPort = 80
	}
	if httpsPort <= 0 {
		httpsPort = 443
	}
	useTLS := false
	if h.CertificateID != "" {
		for _, c := range snap.Certificates {
			if c.ID == h.CertificateID && c.NotAfter != nil && !c.NotAfter.IsZero() {
				full, key := env.CertPaths(c.ID)
				useTLS = fileExists(full) && fileExists(key)
			}
		}
	}
	scheme, port := "http", httpPort
	if useTLS {
		scheme, port = "https", httpsPort
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
		TLSClientConfig:   &tls.Config{ServerName: domain, InsecureSkipVerify: true}, //nolint:gosec // probing our own nginx
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(pctx, http.MethodGet, scheme+"://"+domain+"/", nil)
	req.Header.Set("User-Agent", "Relay-HealthCheck/1")
	resp, err := client.Do(req)
	if err != nil {
		msg := err.Error()
		switch {
		case strings.Contains(msg, "connection refused"):
			msg = "refused connections"
		case strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline"):
			msg = "timed out"
		case strings.Contains(msg, "EOF") || strings.Contains(msg, "reset"):
			msg = "closed connections"
		default:
			msg = "failed (" + msg + ")"
		}
		return probeResult{ok: false, detail: msg}
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return probeResult{ok: false, detail: fmt.Sprintf("returned %d", resp.StatusCode)}
	}
	return probeResult{ok: true, detail: fmt.Sprintf("returned %d", resp.StatusCode)}
}

// healthCheck probes previously healthy hosts once per second. It returns a
// failure description when a host failed for the whole window or the proxy
// engine died.
func (s *Service) healthCheck(ctx context.Context, id int64, engine string, snap *model.Snapshot, targets []string, healthyBefore map[string]bool, progress progressFn) string {
	watch := []string{}
	for _, t := range targets {
		if healthyBefore[t] {
			watch = append(watch, t)
		}
	}
	window := s.healthWindow
	if len(watch) == 0 && window > 2*time.Second {
		window = 2 * time.Second
	}
	secs := int(window / time.Second)
	passed := map[string]bool{}
	last := map[string]probeResult{}
	t0 := time.Now()
	for i := 1; i <= secs; i++ {
		if d := time.Until(t0.Add(time.Duration(i) * time.Second)); d > 0 {
			time.Sleep(d)
		}
		progress("health", fmt.Sprintf("Applying v%d… · validated · reloaded · health check %d/%d s", id, i, int(s.healthWindow/time.Second)), 60+int(float64(i)/float64(secs)*38))
		sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		st, err := s.app.Client(engine).Status(sctx)
		cancel()
		if err == nil && !st.Running {
			reason := engineLabel(engine) + " exited after reload"
			if st.ExitError != "" {
				reason += " (" + st.ExitError + ")"
			}
			return reason
		}
		pending := []string{}
		for _, t := range watch {
			if !passed[t] {
				pending = append(pending, t)
			}
		}
		if len(pending) == 0 {
			if i >= 3 || len(watch) == 0 {
				if len(watch) == 0 && i < secs {
					continue
				}
				return ""
			}
			continue
		}
		for t, res := range s.probeAll(ctx, snap, pending) {
			last[t] = res
			if res.ok {
				passed[t] = true
			}
		}
	}
	for _, t := range watch {
		if !passed[t] {
			name := t
			for _, h := range snap.Hosts {
				if h.ID == t && len(h.Domains) > 0 {
					name = h.Domains[0]
				}
			}
			return fmt.Sprintf("%s %s for %d s after reload", name, last[t].detail, secs)
		}
	}
	return ""
}

// ---------------------------------------------------------------- rollback & discard

func (s *Service) RollbackTo(ctx context.Context, id int64) (*core.Version, error) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	row, err := s.app.Store.GetVersion(ctx, id, true)
	if err != nil {
		return nil, err
	}
	if row.Status != "live" && row.Status != "superseded" {
		return nil, &ApplyError{Status: http.StatusConflict, Code: "not_restorable", Message: fmt.Sprintf("v%d was never live (%s), so it can't be restored.", id, strings.ReplaceAll(row.Status, "_", " "))}
	}
	var snap model.Snapshot
	if err := json.Unmarshal([]byte(row.Snapshot), &snap); err != nil {
		return nil, fmt.Errorf("v%d snapshot: %w", id, err)
	}
	liveRow, _, _ := s.liveVersion(ctx)
	if err := s.app.Store.RestoreSnapshot(ctx, &snap); err != nil {
		return nil, err
	}
	from := ""
	if liveRow != nil {
		from = fmt.Sprintf("v%d → ", liveRow.ID)
	}
	s.app.Audit(ctx, core.AuditEntry{Action: "config.rollback", Target: fmt.Sprintf("%sv%d", from, id), Detail: "restored snapshot, re-validating", Result: "ok"})
	s.app.Changed(ctx, "snapshot", fmt.Sprintf("v%d", id), fmt.Sprintf("v%d", id), core.ActionUpdated)
	return s.applyLocked(ctx, core.ApplyOptions{Summary: fmt.Sprintf("Rollback to v%d", id)}, true)
}

func (s *Service) Discard(ctx context.Context) (*core.Pending, error) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	row, live, err := s.liveVersion(ctx)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, &ApplyError{Status: http.StatusConflict, Code: "nothing_live", Message: "Nothing has been applied yet, so there is no live configuration to restore."}
	}
	p, _, err := s.pendingWithSummary(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.app.Store.RestoreSnapshot(ctx, live); err != nil {
		return nil, err
	}
	s.app.Audit(ctx, core.AuditEntry{Action: "config.discard", Target: fmt.Sprintf("v%d", row.ID), Detail: fmt.Sprintf("%d pending change%s discarded", p.Count, plural(p.Count)), Result: "ok"})
	s.app.Changed(ctx, "snapshot", fmt.Sprintf("v%d", row.ID), fmt.Sprintf("v%d", row.ID), core.ActionUpdated)
	s.publishPending(ctx, true)
	return s.Pending(ctx)
}

// ---------------------------------------------------------------- reconcile

func (s *Service) reconcileLoop(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	s.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.reconcile(ctx)
		}
	}
}

func (s *Service) reconcile(ctx context.Context) {
	st, _ := s.Status(ctx)
	s.observe(ctx, "nginx", st.Nginx)
	s.observe(ctx, "haproxy", st.HAProxy)
	s.observe(ctx, "edge", st.Edge)

	if !s.applyMu.TryLock() {
		return
	}
	defer s.applyMu.Unlock()
	n, err := s.app.Store.CountVersions(ctx)
	if err != nil || n == 0 {
		return
	}
	live, err := s.app.Store.LiveVersion(ctx, true)
	if err != nil {
		return
	}
	engine := rowEngine(live)
	pst, other, ost := st.Nginx, agent.EngineEdge, st.Edge
	if engine == agent.EngineEdge {
		pst, other, ost = st.Edge, agent.EngineNginx, st.Nginx
	}
	if pst.Reachable && live.NginxHash != "" && pst.ConfigHash != live.NginxHash {
		var files agent.Files
		if json.Unmarshal([]byte(live.NginxFiles), &files) == nil && len(files) > 0 {
			s.log.Info(engine+" is not running the live version, restoring", "version", live.ID, "agent", pst.ConfigHash)
			resp, err := s.app.Client(engine).Apply(ctx, agent.ApplyRequest{Files: files, Hash: live.NginxHash})
			if err != nil || !resp.OK {
				msg := ""
				if err != nil {
					msg = err.Error()
				} else {
					msg = firstErrorLine(resp.Output)
				}
				s.log.Warn("restore live "+engine+" config", "err", msg)
			} else {
				s.app.Activity(ctx, "engine.restored", "info", fmt.Sprintf("Restored v%d on %s", live.ID, engineLabel(engine)), engine, "engine restarted with a different config")
				s.app.Bus.Publish(events.EngineChanged, map[string]any{"engine": engine, "running": resp.Running, "reachable": true})
			}
		}
	}
	// Only the selected proxy engine may hold the ports.
	if ost.Reachable && ost.Running {
		s.log.Info("stopping the proxy engine that isn't selected", "engine", other, "selected", engine)
		resp, err := s.app.Client(other).Apply(ctx, agent.ApplyRequest{Stop: true})
		switch {
		case err != nil:
			s.log.Warn("stop "+other, "err", err)
		case !resp.OK:
			s.log.Warn("stop "+other, "err", firstErrorLine(resp.Output))
		default:
			s.app.Bus.Publish(events.EngineChanged, map[string]any{"engine": other, "running": false, "reachable": true})
		}
	}
	if st.HAProxy.Reachable && live.HAProxyHash != "" && st.HAProxy.ConfigHash != live.HAProxyHash && (live.HAProxyRunning || st.HAProxy.ConfigHash != "") {
		files := agent.Files{"haproxy.cfg": live.HAProxyCfg}
		s.log.Info("haproxy is not running the live version, restoring", "version", live.ID)
		resp, err := s.app.HAProxy.Apply(ctx, agent.ApplyRequest{Files: files, Hash: live.HAProxyHash, Stop: !live.HAProxyRunning})
		if err == nil && resp.OK {
			s.app.Bus.Publish(events.EngineChanged, map[string]any{"engine": "haproxy", "running": resp.Running, "reachable": true})
		} else if err != nil {
			s.log.Warn("restore live haproxy config", "err", err)
		} else {
			s.log.Warn("restore live haproxy config", "err", firstErrorLine(resp.Output))
		}
	}
}

func (s *Service) observe(ctx context.Context, engine string, st core.EngineState) {
	now := engineObs{reachable: st.Reachable, running: st.Running}
	s.mu.Lock()
	prev, seen := s.observed[engine]
	s.observed[engine] = now
	s.mu.Unlock()
	if seen && prev == now {
		return
	}
	s.app.Bus.Publish(events.EngineChanged, map[string]any{"engine": engine, "running": st.Running, "reachable": st.Reachable})
	// Outage alerts only for the active proxy engine, and not while an apply
	// is switching engines.
	if !seen || !agent.IsProxyEngine(engine) || engine != s.app.ProxyEngine(ctx) || s.switching() != "" {
		return
	}
	label := engineLabel(engine)
	switch {
	case prev.running && st.Reachable && !st.Running:
		detail := st.ExitError
		if detail == "" {
			detail = "the " + label + " process exited"
		}
		s.app.Activity(ctx, "engine.down", "error", label+" is not running", engine, detail)
		if s.app.Notify != nil {
			s.app.Notify.Notify(ctx, core.Notification{Event: model.EventReloadFailed, Level: "error", Title: label + " is not running", Message: detail, URL: "/"})
		}
	case !prev.running && st.Running && prev.reachable:
		s.app.Activity(ctx, "engine.up", "ok", label+" is running again", engine, "")
	}
}
