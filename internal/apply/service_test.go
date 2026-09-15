package apply

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/agent"
	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// fakeAgent implements the agent protocol in memory.
type fakeAgent struct {
	mu          sync.Mutex
	engine      string
	hash        string
	history     []string
	running     bool
	validateOK  bool
	validateOut string
	applies     int
	rollbacks   int
	starts      int
	stops       int    // Apply requests with Stop
	applyFail   string // stage of a failed (non-stop) apply ("" = succeed)
	lastApply   agent.ApplyRequest
	calls       *[]string // shared call log ("edge:apply", "nginx:stop" …)
	onApply     func()
	onRollback  func()
	down        bool // container stopped: connections are dropped
}

func (f *fakeAgent) log(call string) {
	if f.calls != nil {
		*f.calls = append(*f.calls, f.engine+":"+call)
	}
}

func (f *fakeAgent) serve(t *testing.T, sock string) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewEncoder(w).Encode(agent.Status{Engine: f.engine, Running: f.running, ConfigHash: f.hash, Configured: f.hash != "", Modules: []string{"stream", "auth_request"}})
	})
	mux.HandleFunc("POST /v1/validate", func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewEncoder(w).Encode(agent.ValidateResponse{OK: f.validateOK, Output: f.validateOut, DurationMs: 12})
	})
	mux.HandleFunc("POST /v1/apply", func(w http.ResponseWriter, r *http.Request) {
		var req agent.ApplyRequest
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.applies++
		f.lastApply = req
		prev := f.hash
		if req.Stop {
			f.stops++
			f.log("stop")
		} else {
			f.log("apply")
		}
		if !req.Stop && f.applyFail != "" {
			f.mu.Unlock()
			json.NewEncoder(w).Encode(agent.ApplyResponse{OK: false, Stage: f.applyFail, Output: "[emerg] listen tcp :80: bind: address already in use", PreviousHash: prev, Running: f.running})
			return
		}
		if req.Stop && len(req.Files) == 0 && req.Hash == "" {
			f.running = false
			f.mu.Unlock()
			json.NewEncoder(w).Encode(agent.ApplyResponse{OK: true, Stage: "stop", PreviousHash: prev})
			return
		}
		if f.hash != "" {
			f.history = append(f.history, f.hash)
		}
		f.hash = req.Hash
		f.running = !req.Stop
		hook := f.onApply
		f.mu.Unlock()
		if hook != nil {
			hook()
		}
		json.NewEncoder(w).Encode(agent.ApplyResponse{OK: true, Stage: "reload", ReloadMs: 7, PreviousHash: prev, Running: !req.Stop})
	})
	mux.HandleFunc("POST /v1/start", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.starts++
		f.log("start")
		f.running = true
		f.mu.Unlock()
		json.NewEncoder(w).Encode(agent.ActionResponse{OK: true})
	})
	mux.HandleFunc("POST /v1/stop", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.running = false
		f.log("stopaction")
		f.mu.Unlock()
		json.NewEncoder(w).Encode(agent.ActionResponse{OK: true})
	})
	mux.HandleFunc("POST /v1/rollback", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.rollbacks++
		if n := len(f.history); n > 0 {
			f.hash = f.history[n-1]
			f.history = f.history[:n-1]
		}
		hook := f.onRollback
		f.mu.Unlock()
		if hook != nil {
			hook()
		}
		json.NewEncoder(w).Encode(agent.ApplyResponse{OK: true, Stage: "reload", Running: true})
	})
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		down := f.down
		f.mu.Unlock()
		if hj, ok := w.(http.Hijacker); down && ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
			}
			return
		}
		mux.ServeHTTP(w, r)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
}

type testEnv struct {
	svc      *Service
	app      *core.App
	nginx    *fakeAgent
	haproxy  *fakeAgent
	edge     *fakeAgent
	calls    []string
	upstream *atomic.Int32
	ctx      context.Context
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "rlapply")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	runDir := filepath.Join(dir, "run")
	os.MkdirAll(runDir, 0o755)
	st, err := store.Open(filepath.Join(dir, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := core.Config{DataDir: filepath.Join(dir, "data"), RunDir: runDir, LogDir: filepath.Join(dir, "logs")}
	app := core.New(cfg, st, events.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	e := &testEnv{app: app, ctx: core.WithActor(context.Background(), core.Actor{Type: core.ActorUser, Name: "admin", Role: core.RoleAdmin})}
	e.nginx = &fakeAgent{engine: "nginx", validateOK: true, running: true, hash: agent.BootstrapHash}
	e.haproxy = &fakeAgent{engine: "haproxy", validateOK: true}
	e.edge = &fakeAgent{engine: "edge", validateOK: true, hash: agent.BootstrapHash, calls: &e.calls}
	e.nginx.calls = &e.calls
	e.nginx.serve(t, agent.SocketPath(runDir, "nginx"))
	e.haproxy.serve(t, agent.SocketPath(runDir, "haproxy"))
	e.edge.serve(t, agent.SocketPath(runDir, "edge"))

	// The "nginx" the health check talks to.
	e.upstream = &atomic.Int32{}
	e.upstream.Store(200)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(e.upstream.Load()))
	}))
	t.Cleanup(up.Close)
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(up.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	gen := store.DefaultGeneral()
	gen.HTTPPort = port
	if err := st.PutSettings(e.ctx, model.SettingsGeneral, gen); err != nil {
		t.Fatal(err)
	}

	e.svc = New(app)
	e.svc.healthWindow = 3 * time.Second
	app.Engine = e.svc
	return e
}

func (e *testEnv) createHost(t *testing.T, domain string) *model.ProxyHost {
	h := &model.ProxyHost{Domains: []string{domain}, Enabled: true, Upstream: model.Upstream{Scheme: "http", Host: "10.0.0.21", Port: 3000}, HSTS: "inherit"}
	if err := e.app.Store.Hosts().Create(e.ctx, h); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestApplyLifecycle(t *testing.T) {
	e := newTestEnv(t)
	ctx := e.ctx
	host := e.createHost(t, "grafana.home.lan")

	p, err := e.svc.Pending(ctx)
	// The test's custom HTTP port is a pending settings change too.
	if err != nil || p.Count != 2 || p.Items[0].Action != core.ActionCreated || p.Items[1].Name != "General settings" || p.LiveVersion != 0 {
		t.Fatalf("pending = %+v, %v", p, err)
	}

	// v1: first apply.
	v1, err := e.svc.Apply(ctx, core.ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if v1.ID != 1 || v1.Status != "live" || v1.Summary != "New host grafana.home.lan · General settings changed" || v1.Actor != "admin" {
		t.Fatalf("v1 = %+v", v1)
	}
	if p, _ := e.svc.Pending(ctx); p.Count != 0 || p.LiveVersion != 1 {
		t.Fatalf("pending after apply = %+v", p)
	}
	if e.haproxy.applies != 1 || e.haproxy.running {
		t.Fatalf("haproxy should have been stopped (no backends): applies=%d running=%v", e.haproxy.applies, e.haproxy.running)
	}
	// Nothing to apply now.
	var ae *ApplyError
	if _, err := e.svc.Apply(ctx, core.ApplyOptions{}); !errors.As(err, &ae) || ae.Code != "no_changes" {
		t.Fatalf("expected no_changes, got %v", err)
	}

	// v2: edit breaks the host after reload → automatic rollback.
	host.Websockets = true
	if err := e.app.Store.Hosts().Update(ctx, host); err != nil {
		t.Fatal(err)
	}
	e.nginx.mu.Lock()
	e.nginx.onApply = func() { e.upstream.Store(502) }
	e.nginx.onRollback = func() { e.upstream.Store(200) }
	e.nginx.mu.Unlock()
	v2, err := e.svc.Apply(ctx, core.ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if v2.Status != "rolled_back" || v2.RolledBackTo == nil || *v2.RolledBackTo != 1 {
		t.Fatalf("v2 = %+v", v2)
	}
	want := "grafana.home.lan returned 502 for 3 s after reload. v1 is live again; your edit is saved as a draft."
	if v2.Error != want {
		t.Fatalf("v2 error\n got %q\nwant %q", v2.Error, want)
	}
	if e.nginx.rollbacks != 1 {
		t.Fatalf("nginx rollbacks = %d", e.nginx.rollbacks)
	}
	if p, _ := e.svc.Pending(ctx); p.Count != 1 || p.LiveVersion != 1 {
		t.Fatalf("draft must remain pending: %+v", p)
	}
	audit, _ := e.app.Store.ListAudit(ctx, store.AuditQuery{Search: "config.rollback"})
	if len(audit) != 1 || audit[0].Result != "auto" || audit[0].ActorType != core.ActorSystem || audit[0].Target != "v2 → v1" {
		t.Fatalf("audit = %+v", audit)
	}

	// v3: validation failure → failed version, 422, engines untouched.
	e.nginx.mu.Lock()
	e.nginx.onApply, e.nginx.onRollback = nil, nil
	e.nginx.validateOK = false
	e.nginx.validateOut = "nginx: [emerg] unknown directive \"proxy_hide_headr\" in conf.d/hosts/grafana.home.lan.conf:40\nnginx: configuration file test failed"
	applies := e.nginx.applies
	e.nginx.mu.Unlock()
	v3, err := e.svc.Apply(ctx, core.ApplyOptions{})
	if !errors.As(err, &ae) || ae.Status != http.StatusUnprocessableEntity || v3 == nil || v3.Status != "failed" {
		t.Fatalf("v3 = %+v err %v", v3, err)
	}
	if !strings.Contains(v3.Error, `unknown directive "proxy_hide_headr"`) || e.nginx.applies != applies {
		t.Fatalf("v3 error %q applies %d", v3.Error, e.nginx.applies)
	}

	// v4: fixed → live, v1 superseded.
	e.nginx.mu.Lock()
	e.nginx.validateOK = true
	e.nginx.mu.Unlock()
	v4, err := e.svc.Apply(ctx, core.ApplyOptions{Summary: "Enable websockets"})
	if err != nil || v4.Status != "live" || v4.ID != 4 || v4.Summary != "Enable websockets" {
		t.Fatalf("v4 = %+v, %v", v4, err)
	}
	rows, _ := e.app.Store.ListVersions(ctx, 10, 0)
	statuses := []string{}
	for _, r := range rows {
		statuses = append(statuses, strconv.FormatInt(r.ID, 10)+":"+r.Status)
	}
	if strings.Join(statuses, ",") != "4:live,3:failed,2:rolled_back,1:superseded" {
		t.Fatalf("versions = %v", statuses)
	}

	// Diff between v4 and v1 shows the websocket headers.
	r4, _ := e.app.Store.GetVersion(ctx, 4, true)
	r1, _ := e.app.Store.GetVersion(ctx, 1, true)
	fds := diffFileSets(versionFileMap(r1), versionFileMap(r4))
	if len(fds) != 1 || fds[0].Path != "conf.d/hosts/grafana.home.lan.conf" || fds[0].Added != 2 {
		t.Fatalf("diff = %+v", fds)
	}

	// Discard: edit, then restore the live snapshot.
	host.Upstream.Port = 9999
	e.app.Store.Hosts().Update(ctx, host)
	if p, _ := e.svc.Pending(ctx); p.Count != 1 {
		t.Fatalf("pending before discard = %+v", p)
	}
	if p, err := e.svc.Discard(ctx); err != nil || p.Count != 0 {
		t.Fatalf("discard = %+v, %v", p, err)
	}
	if h, _ := e.app.Store.Hosts().Get(ctx, host.ID); h.Upstream.Port != 3000 || !h.Websockets {
		t.Fatalf("host after discard = %+v", h)
	}

	// Roll back to v1 (websockets off) → new version v5.
	v5, err := e.svc.RollbackTo(ctx, 1)
	if err != nil || v5.Status != "live" || v5.Summary != "Rollback to v1" {
		t.Fatalf("v5 = %+v, %v", v5, err)
	}
	if h, _ := e.app.Store.Hosts().Get(ctx, host.ID); h.Websockets {
		t.Fatal("rollback did not restore the snapshot")
	}
	if _, err := e.svc.RollbackTo(ctx, 3); !errors.As(err, &ae) || ae.Code != "not_restorable" {
		t.Fatalf("rollback to failed version: %v", err)
	}
}

func TestReconcilePushesLiveVersion(t *testing.T) {
	e := newTestEnv(t)
	ctx := e.ctx
	e.createHost(t, "grafana.home.lan")
	if _, err := e.svc.Apply(ctx, core.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	live, _ := e.app.Store.LiveVersion(ctx, false)
	// Simulate an engine container restart (bootstrap config).
	e.nginx.mu.Lock()
	e.nginx.hash = agent.BootstrapHash
	applies := e.nginx.applies
	e.nginx.mu.Unlock()
	e.svc.reconcile(ctx)
	e.nginx.mu.Lock()
	defer e.nginx.mu.Unlock()
	if e.nginx.hash != live.NginxHash || e.nginx.applies != applies+1 {
		t.Fatalf("reconcile did not restore: hash %s want %s", e.nginx.hash, live.NginxHash)
	}
	vs, _ := e.app.Store.CountVersions(ctx)
	if vs != 1 {
		t.Fatalf("reconcile must not create versions (got %d)", vs)
	}
}

func TestStatusUnreachable(t *testing.T) {
	dir, _ := os.MkdirTemp("/tmp", "rlst")
	defer os.RemoveAll(dir)
	st, err := store.Open(filepath.Join(dir, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	app := core.New(core.Config{RunDir: dir, DataDir: dir, LogDir: dir}, st, events.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	s := New(app)
	es, err := s.Status(context.Background())
	if err != nil || es.Nginx.Reachable || es.Nginx.Error == "" || es.HAProxy.Engine != "haproxy" {
		t.Fatalf("status = %+v, %v", es, err)
	}
}
