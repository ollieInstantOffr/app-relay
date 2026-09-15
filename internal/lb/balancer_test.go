package lb

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/agent"
	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/lb/lbengine"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/render"
	"github.com/instantoffr/relay/internal/store"
)

// fakeLBAgent is a Relay Balancer agent: status, validate and the runtime API.
type fakeLBAgent struct {
	mu        sync.Mutex
	runtimeUp bool     // false: /v1/runtime answers 503 (process not running)
	setReply  string   // output of "set server …"
	commands  []string // runtime commands received
	validated []string // main file of validated releases
}

func (f *fakeLBAgent) serve(t *testing.T, sock string) {
	t.Helper()
	stat, _ := os.ReadFile("testdata/showstat.csv")
	info, _ := os.ReadFile("testdata/showinfo.txt")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(agent.Status{Engine: "balancer", Running: true, Modules: []string{}})
	})
	mux.HandleFunc("POST /v1/validate", func(w http.ResponseWriter, r *http.Request) {
		var req agent.ValidateRequest
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.validated = append(f.validated, req.Files["balancer.json"])
		f.mu.Unlock()
		json.NewEncoder(w).Encode(agent.ValidateResponse{OK: true, Output: "[notice] configuration is valid", DurationMs: 3})
	})
	mux.HandleFunc("POST /v1/runtime", func(w http.ResponseWriter, r *http.Request) {
		var req agent.RuntimeRequest
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.commands = append(f.commands, req.Command)
		if !f.runtimeUp {
			http.Error(w, "balancer runtime API: dial unix: no such file", http.StatusServiceUnavailable)
			return
		}
		out := f.setReply
		switch req.Command {
		case "show stat":
			out = string(stat)
		case "show info":
			out = string(info)
		}
		json.NewEncoder(w).Encode(agent.RuntimeResponse{Output: out})
	})
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
}

// balancerApp wires the lb routes with Relay Balancer as the selected load
// balancer engine and a fake balancer agent.
func balancerApp(t *testing.T) (*core.App, http.Handler, *fakeLBAgent) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "rllb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	st, err := store.Open(filepath.Join(dir, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	gen := store.DefaultGeneral()
	gen.LBEngine = "balancer"
	if err := st.PutSettings(context.Background(), model.SettingsGeneral, gen); err != nil {
		t.Fatal(err)
	}
	app := core.New(core.Config{DataDir: dir, RunDir: dir, LogDir: dir, Listen: ":8181"}, st, events.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	fa := &fakeLBAgent{runtimeUp: true}
	fa.serve(t, agent.SocketPath(dir, "balancer"))
	app.LB = New(app)
	r := chi.NewRouter()
	Routes(app, r)

	old := lbengine.Renderers[agent.EngineBalancer]
	fake := old
	fake.Render = func(s *model.Snapshot, _ render.Env) (agent.Files, error) {
		names := []string{}
		for _, b := range s.Backends {
			names = append(names, b.Name)
		}
		for _, f := range s.Frontends {
			names = append(names, "fe:"+f.Name)
		}
		return agent.Files{"balancer.json": `{"schema":1,"sections":"` + strings.Join(names, ",") + `"}` + "\n"}, nil
	}
	fake.Backend = func(_ *model.Snapshot, b *model.Backend) string { return `{"backend":"` + b.Name + `"}` }
	fake.Frontend = func(_ *model.Snapshot, f *model.Frontend) string { return `{"frontend":"` + f.Name + `"}` }
	lbengine.Renderers[agent.EngineBalancer] = fake
	t.Cleanup(func() { lbengine.Renderers[agent.EngineBalancer] = old })
	return app, r, fa
}

func TestBalancerRuntimeStatsAndStates(t *testing.T) {
	app, h, fa := balancerApp(t)
	b := seedBackend(t, app)

	// Stats come from Relay Balancer's runtime API (HAProxy CSV format).
	var stats core.LBStats
	if code := call(t, h, "GET", "/lb/stats", nil, &stats); code != 200 || !stats.Running || len(stats.Backends) != 2 || len(stats.Frontends) != 1 {
		t.Fatalf("stats: %d %+v", code, stats)
	}

	// set server … through the balancer; "" = applied at runtime.
	var sr StateResult
	if code := call(t, h, "POST", "/backends/"+b.ID+"/servers/"+b.Servers[0].ID+"/state", map[string]any{"state": "drain"}, &sr); code != 200 || !sr.Runtime || sr.Note != "" {
		t.Fatalf("state: %d %+v", code, sr)
	}
	fa.mu.Lock()
	last := fa.commands[len(fa.commands)-1]
	fa.mu.Unlock()
	if last != "set server api/"+b.Servers[0].Name+" state drain" {
		t.Fatalf("runtime command = %q", last)
	}
	if code := call(t, h, "POST", "/backends/"+b.ID+"/servers/"+b.Servers[0].ID+"/weight", map[string]any{"weight": 50}, &sr); code != 200 || !sr.Runtime {
		t.Fatalf("weight: %d %+v", code, sr)
	}

	// Notes name the active engine.
	fa.mu.Lock()
	fa.runtimeUp = false
	fa.mu.Unlock()
	call(t, h, "POST", "/backends/"+b.ID+"/servers/"+b.Servers[1].ID+"/state", map[string]any{"state": "maint"}, &sr)
	if sr.Runtime || sr.Note != "Relay Balancer is not running; takes effect on the next apply" {
		t.Fatalf("not running note: %+v", sr)
	}
	fa.mu.Lock()
	fa.runtimeUp, fa.setReply = true, "Unknown command"
	fa.mu.Unlock()
	var fail struct{ Error struct{ Message string } }
	if code := call(t, h, "POST", "/backends/"+b.ID+"/servers/"+b.Servers[1].ID+"/state", map[string]any{"state": "ready"}, &fail); code != 502 || fail.Error.Message != "Relay Balancer: Unknown command" {
		t.Fatalf("runtime error: %d %+v", code, fail)
	}
	app.Balancer = agent.NewClient("balancer", filepath.Join(app.Config.RunDir, "missing.sock"))
	call(t, h, "POST", "/backends/"+b.ID+"/servers/"+b.Servers[1].ID+"/state", map[string]any{"state": "maint"}, &sr)
	if sr.Runtime || sr.Note != "Relay Balancer is not reachable; takes effect on the next apply" {
		t.Fatalf("unreachable note: %+v", sr)
	}
}

func TestBalancerConfigPreviewAndValidate(t *testing.T) {
	app, h, fa := balancerApp(t)
	b := seedBackend(t, app)

	for _, path := range []string{"/lb/config", "/haproxy/config"} {
		var cfg map[string]any
		if code := call(t, h, "GET", path, nil, &cfg); code != 200 || cfg["engine"] != "balancer" || cfg["file"] != "balancer.json" ||
			!strings.Contains(cfg["config"].(string), `"sections":"api"`) {
			t.Fatalf("%s: %d %v", path, code, cfg)
		}
	}
	for _, path := range []string{"/lb/validate", "/haproxy/validate"} {
		var v validation
		if code := call(t, h, "POST", path, nil, &v); code != 200 || v.Checked != "balancer" || !v.Valid || v.Engine != "balancer" {
			t.Fatalf("%s: %d %+v", path, code, v)
		}
	}
	for _, path := range []string{"/preview/lb/backend", "/preview/haproxy/backend"} {
		var pv preview
		draft := *b
		draft.Name = "api2"
		draft.ID = ""
		if code := call(t, h, "POST", path, map[string]any{"backend": draft}, &pv); code != 200 || pv.Engine != "balancer" || pv.Config != `{"backend":"api2"}` || pv.Checked != "balancer" || !pv.Valid {
			t.Fatalf("%s: %d %+v", path, code, pv)
		}
	}
	fa.mu.Lock()
	lastValidated := fa.validated[len(fa.validated)-1]
	fa.mu.Unlock()
	if !strings.Contains(lastValidated, "api,api2") {
		t.Fatalf("validated release = %q", lastValidated)
	}

	// Expose preview renders the frontend with the active engine.
	exp := map[string]any{"backendId": b.ID, "domain": "api.example.com", "certificate": map[string]any{"mode": "none"}, "access": map[string]any{"mode": "public"}}
	var ep exposePreview
	if code := call(t, h, "POST", "/lb/expose/preview", exp, &ep); code != 200 {
		t.Fatalf("expose preview: %d", code)
	}
	if ep.LBEngine != "balancer" || !strings.HasPrefix(ep.HAProxy, `{"frontend":"fe-api"}`) || !strings.Contains(ep.HAProxy, "// backend api unchanged") || ep.HAProxyValid == nil || !*ep.HAProxyValid {
		t.Fatalf("expose preview: %+v", ep)
	}
}
