package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Options struct {
	Engine  string // nginx | haproxy | edge
	RunDir  string // socket directory (also holds the proxy-engine selection file)
	LogDir  string
	DataDir string
	Log     *slog.Logger
	// ConfigRoot defaults to $RELAY_CONFIG_ROOT or /etc/relay/<engine>.
	ConfigRoot string
	// Version is the relay binary version (reported by the edge engine).
	Version string
}

// Agent supervises one engine and serves the protocol in protocol.go.
type Agent struct {
	o      Options
	log    *slog.Logger
	eng    engine
	rel    *releases
	sup    *supervisor
	logs   *ringLog
	reaper *reaper

	version string
	modules []string
	dynMods map[string]string

	mu           sync.Mutex
	lastReloadAt *time.Time
	lastReloadMs int64
	configLines  int
	linesHash    string
}

const stoppedMarker = "stopped"

// Run supervises the engine process and serves the agent protocol until ctx ends.
func Run(ctx context.Context, o Options) error {
	// Official nginx/HAProxy images declare STOPSIGNAL SIGQUIT / SIGUSR1,
	// which Docker sends to PID 1 (this agent) unless compose overrides it.
	// Treat them like SIGTERM: stop the engine gracefully, then exit.
	ctx, stopSignals := signal.NotifyContext(ctx, syscall.SIGQUIT, syscall.SIGUSR1)
	defer stopSignals()
	factory, ok := engineFactories[o.Engine]
	if !ok {
		return fmt.Errorf("agent: --engine must be one of %s", strings.Join(EngineNames(), " | "))
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.ConfigRoot == "" {
		o.ConfigRoot = os.Getenv("RELAY_CONFIG_ROOT")
	}
	if o.ConfigRoot == "" {
		o.ConfigRoot = filepath.Join("/etc/relay", o.Engine)
	}
	for _, d := range []string{o.RunDir, o.LogDir, o.ConfigRoot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}

	// The reaper and supervisor outlive ctx so shutdown can wait for the engine.
	bg, stopBG := context.WithCancel(context.Background())
	defer stopBG()

	a := newAgent(ctx, o, factory)
	go a.reaper.loop(bg)

	if err := a.rel.init(); err != nil {
		return err
	}
	if _, err := exec.LookPath(a.eng.binary()); err != nil {
		a.log.Error("engine binary not found", "binary", a.eng.binary(), "err", err)
	}
	a.version, a.modules, a.dynMods = a.eng.detect()
	a.log.Info("engine detected", "version", a.version, "modules", strings.Join(a.modules, ","))

	a.boot()

	sock := SocketPath(o.RunDir, o.Engine)
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		a.sup.op.Lock()
		a.sup.stop(10 * time.Second)
		a.sup.op.Unlock()
		return fmt.Errorf("listen %s: %w", sock, err)
	}
	os.Chmod(sock, 0o660)

	srv := &http.Server{Handler: a.routes(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	a.log.Info("agent listening", "socket", sock, "config", o.ConfigRoot)

	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	srv.Shutdown(shutdown)
	cancel()
	a.sup.op.Lock()
	a.sup.stop(20 * time.Second)
	a.sup.op.Unlock()
	os.Remove(sock)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func newAgent(ctx context.Context, o Options, factory func(*Agent) engine) *Agent {
	a := &Agent{o: o, log: o.Log, logs: newRingLog(2000), reaper: newReaper(), rel: &releases{root: o.ConfigRoot, keep: 10}}
	a.eng = factory(a)
	a.sup = &supervisor{ctx: ctx, log: o.Log, reaper: a.reaper, logs: a.logs, command: a.eng.command, stopSignal: a.eng.stopSignal(), beforeStart: a.eng.prepare}
	return a
}

// boot activates the bootstrap config (nginx, edge) when nothing was applied
// yet and starts the engine when appropriate. On a fresh config root a proxy
// engine that isn't the selected one (/run/relay/proxy-engine) keeps its
// bootstrap release stopped so it doesn't take the ports.
func (a *Agent) boot() {
	a.sup.op.Lock()
	defer a.sup.op.Unlock()
	cur := a.rel.current()
	if cur == "" {
		if files := a.eng.bootstrap(); files != nil {
			os.RemoveAll(a.rel.dir(BootstrapHash))
			if _, _, err := a.rel.write(BootstrapHash, files); err != nil {
				a.log.Error("write bootstrap config", "err", err)
				return
			}
			if err := a.rel.swap(BootstrapHash); err != nil {
				a.log.Error("activate bootstrap config", "err", err)
				return
			}
			cur = BootstrapHash
			if a.eng.proxy() {
				selected := ReadProxyEngine(a.o.RunDir)
				a.rel.setMarker(stoppedMarker, selected != a.o.Engine)
				if selected != a.o.Engine {
					a.log.Info("proxy engine not selected, bootstrap config kept stopped", "selected", selected)
				}
			}
		}
	}
	if cur == "" || a.rel.marker(stoppedMarker) {
		a.log.Info("engine not started", "configured", cur != "")
		return
	}
	if _, err := a.sup.start(); err != nil {
		a.log.Error("start engine", "err", err)
	}
}

func (a *Agent) status() Status {
	cur := a.rel.current()
	st := a.sup.state()
	a.mu.Lock()
	if a.linesHash != cur {
		a.configLines = a.rel.lines(cur)
		a.linesHash = cur
	}
	s := Status{
		Engine:         a.o.Engine,
		Running:        st.pid != 0,
		PID:            st.pid,
		Version:        a.version,
		Modules:        a.modules,
		DynamicModules: a.dynMods,
		StartedAt:      st.startedAt,
		ExitedAt:       st.exitedAt,
		ExitError:      st.exitErr,
		LastReloadAt:   a.lastReloadAt,
		LastReloadMs:   a.lastReloadMs,
		ConfigHash:     cur,
		ConfigLines:    a.configLines,
		Configured:     cur != "" && cur != BootstrapHash,
	}
	a.mu.Unlock()
	if s.Modules == nil {
		s.Modules = []string{}
	}
	return s
}

func (a *Agent) recordReload(d time.Duration) {
	a.mu.Lock()
	now := time.Now().UTC()
	a.lastReloadAt = &now
	a.lastReloadMs = d.Milliseconds()
	a.mu.Unlock()
}

// ---------------------------------------------------------------- operations

func (a *Agent) validate(files Files) ValidateResponse {
	if err := a.eng.checkFiles(files); err != nil {
		return ValidateResponse{OK: false, Output: err.Error()}
	}
	dir, cleanup, err := a.rel.stage(files)
	defer cleanup()
	if err != nil {
		return ValidateResponse{OK: false, Output: err.Error()}
	}
	t0 := time.Now()
	out, err := a.eng.validate(dir)
	resp := ValidateResponse{OK: err == nil, Output: cleanOutput(out, dir, a.rel.root), DurationMs: time.Since(t0).Milliseconds()}
	if err != nil && resp.Output == "" {
		resp.Output = err.Error()
	}
	return resp
}

// cleanOutput rewrites staging paths in engine output to release-relative ones.
func cleanOutput(out, dir, root string) string {
	out = strings.ReplaceAll(out, dir+"/", "")
	out = strings.ReplaceAll(out, root+"/releases/", "releases/")
	return strings.TrimSpace(out)
}

func (a *Agent) apply(req ApplyRequest) ApplyResponse {
	a.sup.op.Lock()
	defer a.sup.op.Unlock()

	prev := a.rel.current()
	resp := ApplyResponse{PreviousHash: prev, Stage: "validate"}
	fail := func(stage, output string) ApplyResponse {
		resp.OK, resp.Stage, resp.Running = false, stage, a.sup.running()
		resp.Output = strings.TrimSpace(output)
		a.log.Warn("apply failed", "stage", stage, "output", resp.Output)
		return resp
	}
	if req.Files == nil {
		req.Files = Files{}
	}
	if !req.Stop {
		if err := a.eng.checkFiles(req.Files); err != nil {
			return fail("validate", err.Error())
		}
	}
	hash := req.Hash
	if !validHash(hash) {
		hash = HashFiles(req.Files)
	}
	if !req.Stop && hash == prev && a.sup.running() {
		resp.OK, resp.Stage, resp.Running = true, "reload", true
		resp.Output = "configuration already active"
		return resp
	}

	dir, created, err := a.rel.write(hash, req.Files)
	if err != nil {
		return fail("swap", err.Error())
	}
	if !req.Stop {
		t0 := time.Now()
		out, err := a.eng.validate(dir)
		resp.ValidateMs = time.Since(t0).Milliseconds()
		resp.Output = cleanOutput(out, dir, a.rel.root)
		if err != nil {
			if created {
				a.rel.remove(hash)
			}
			if resp.Output == "" {
				resp.Output = err.Error()
			}
			return fail("validate", resp.Output)
		}
	}
	if err := a.rel.activate(hash); err != nil {
		return fail("swap", err.Error())
	}

	if req.Stop {
		a.rel.setMarker(stoppedMarker, true)
		a.sup.stop(20 * time.Second)
		a.rel.prune()
		resp.OK, resp.Stage, resp.Running = true, "stop", false
		return resp
	}
	wasStopped := a.rel.marker(stoppedMarker)
	a.rel.setMarker(stoppedMarker, false)

	wasWanted := a.sup.wanted()
	validateOut := resp.Output
	if a.sup.running() {
		t0 := time.Now()
		out, err := a.eng.reload()
		resp.ReloadMs = time.Since(t0).Milliseconds()
		if err != nil {
			a.log.Warn("reload failed, reverting", "err", err)
			a.rel.revert(hash, prev)
			if a.sup.running() && prev != "" {
				a.eng.reload()
			} else if prev != "" {
				a.startAndWait()
			}
			return fail("reload", joinNonEmpty(err.Error(), out))
		}
		a.recordReload(time.Since(t0))
		resp.Output = joinNonEmpty(validateOut, out)
	} else {
		t0 := time.Now()
		ok, out := a.startAndWait()
		resp.ReloadMs = time.Since(t0).Milliseconds()
		if !ok {
			a.rel.revert(hash, prev)
			if prev != "" && (wasWanted || (a.eng.alwaysOn() && !wasStopped)) {
				a.startAndWait()
			} else if prev == "" || !wasWanted {
				a.sup.stop(5 * time.Second)
				if !wasWanted && (!a.eng.alwaysOn() || wasStopped) {
					a.rel.setMarker(stoppedMarker, true)
				}
			}
			return fail("start", out)
		}
		a.recordReload(time.Since(t0))
	}
	a.rel.prune()
	resp.OK, resp.Running = true, a.sup.running()
	resp.Stage = "reload"
	return resp
}

// startAndWait starts the engine and checks it stays up.
func (a *Agent) startAndWait() (bool, string) {
	p, err := a.sup.start()
	if err != nil {
		a.sup.stop(time.Second)
		return false, err.Error()
	}
	if a.sup.stable(p, a.eng.stableWait()) {
		return true, ""
	}
	lines := a.logs.after(p.logSeq)
	out := joinLines(errorLines(lines))
	if out == "" {
		out = joinLines(lines)
	}
	if out == "" {
		out = describeExit(p.status, nil)
	}
	// Don't let the supervisor restart a config that just failed.
	a.sup.stop(time.Second)
	return false, out
}

func joinNonEmpty(parts ...string) string {
	var out []string
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, "\n")
}

func (a *Agent) rollback() ApplyResponse {
	a.sup.op.Lock()
	defer a.sup.op.Unlock()
	cur := a.rel.current()
	resp := ApplyResponse{PreviousHash: cur, Stage: "swap"}
	prev, err := a.rel.rollback()
	if err != nil {
		resp.Output = err.Error()
		resp.Running = a.sup.running()
		return resp
	}
	a.rel.setMarker(stoppedMarker, false)
	if a.sup.running() {
		t0 := time.Now()
		out, err := a.eng.reload()
		resp.ReloadMs = time.Since(t0).Milliseconds()
		resp.Stage, resp.Output = "reload", out
		if err != nil {
			// The master may have died on the bad config: restart it.
			if !a.sup.running() {
				if ok, o := a.startAndWait(); !ok {
					resp.Output = joinNonEmpty(err.Error(), o)
					resp.Running = false
					return resp
				}
			} else {
				resp.Output = joinNonEmpty(err.Error(), out)
				resp.Running = true
				return resp
			}
		}
		a.recordReload(time.Since(t0))
	} else {
		t0 := time.Now()
		ok, out := a.startAndWait()
		resp.Stage, resp.Output = "start", out
		resp.ReloadMs = time.Since(t0).Milliseconds()
		if !ok {
			resp.Running = false
			return resp
		}
		a.recordReload(time.Since(t0))
	}
	a.log.Info("rolled back", "from", cur, "to", prev)
	resp.OK, resp.Running = true, a.sup.running()
	return resp
}

func (a *Agent) start() ActionResponse {
	a.sup.op.Lock()
	defer a.sup.op.Unlock()
	if a.rel.current() == "" {
		return ActionResponse{OK: false, Output: "no configuration has been applied yet"}
	}
	if a.sup.running() {
		return ActionResponse{OK: true, Output: "already running"}
	}
	a.rel.setMarker(stoppedMarker, false)
	ok, out := a.startAndWait()
	return ActionResponse{OK: ok, Output: out}
}

func (a *Agent) stop() ActionResponse {
	a.sup.op.Lock()
	defer a.sup.op.Unlock()
	a.sup.stop(20 * time.Second)
	if !a.eng.alwaysOn() {
		a.rel.setMarker(stoppedMarker, true)
	}
	return ActionResponse{OK: true}
}

func (a *Agent) reload() ActionResponse {
	a.sup.op.Lock()
	defer a.sup.op.Unlock()
	if !a.sup.running() {
		return ActionResponse{OK: false, Output: a.o.Engine + " is not running"}
	}
	t0 := time.Now()
	out, err := a.eng.reload()
	if err != nil {
		return ActionResponse{OK: false, Output: joinNonEmpty(err.Error(), out)}
	}
	a.recordReload(time.Since(t0))
	return ActionResponse{OK: true, Output: out}
}

// ---------------------------------------------------------------- HTTP

func (a *Agent) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+PathStatus, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.status()) })
	mux.HandleFunc("POST "+PathValidate, func(w http.ResponseWriter, r *http.Request) {
		var req ValidateRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		writeJSON(w, 200, a.validate(req.Files))
	})
	mux.HandleFunc("POST "+PathApply, func(w http.ResponseWriter, r *http.Request) {
		var req ApplyRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		writeJSON(w, 200, a.apply(req))
	})
	mux.HandleFunc("POST "+PathRollback, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.rollback()) })
	mux.HandleFunc("POST "+PathStart, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.start()) })
	mux.HandleFunc("POST "+PathStop, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.stop()) })
	mux.HandleFunc("POST "+PathReload, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.reload()) })
	mux.HandleFunc("GET "+PathLogs, func(w http.ResponseWriter, r *http.Request) {
		var since time.Time
		if v, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64); err == nil && v > 0 {
			since = time.UnixMilli(v)
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 || limit > 2000 {
			limit = 500
		}
		writeJSON(w, 200, LogsResponse{Lines: a.logs.since(since, limit)})
	})
	mux.HandleFunc("GET "+PathListeners, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, ListenersResponse{Listeners: listListeners("/proc")})
	})
	mux.HandleFunc("POST "+PathRuntime, func(w http.ResponseWriter, r *http.Request) {
		h, ok := a.eng.(*haproxyEngine)
		if !ok {
			http.Error(w, "runtime API is only available on the haproxy agent", http.StatusNotFound)
			return
		}
		var req RuntimeRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		cmd := strings.TrimSpace(req.Command)
		if cmd == "" || strings.ContainsAny(cmd, "\n\r") {
			http.Error(w, "one runtime command required", http.StatusBadRequest)
			return
		}
		out, err := unixCommand(h.runtimeSocket(), cmd, 15*time.Second)
		if err != nil {
			http.Error(w, "haproxy runtime API: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, 200, RuntimeResponse{Output: out})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}
