package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The test binary doubles as a fake `relay edge` process: when fakeEdgeEnv is
// set, TestMain handles `edge run --config <file>` and `edge check <dir>`.
const fakeEdgeEnv = "RELAY_AGENT_TEST_FAKE_EDGE"

func TestMain(m *testing.M) {
	if os.Getenv(fakeEdgeEnv) == "1" {
		os.Exit(fakeEdge(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeEdge also plays `relay balancer run|check` (same protocol, balancer.json).
func fakeEdge(args []string) int {
	if len(args) >= 3 && (args[0] == "edge" || args[0] == "balancer") && args[1] == "check" {
		name := args[0] + ".json"
		b, err := os.ReadFile(filepath.Join(args[2], name))
		if err != nil || strings.Contains(string(b), "invalid") {
			fmt.Println("[emerg] " + name + ": invalid configuration")
			return 1
		}
		fmt.Println("[notice] configuration is valid")
		return 0
	}
	if len(args) < 4 || (args[0] != "edge" && args[0] != "balancer") || args[1] != "run" || args[2] != "--config" {
		fmt.Fprintln(os.Stderr, "fake edge: bad args", args)
		return 2
	}
	conf := args[3]
	hash := func() string {
		dir, err := filepath.EvalSymlinks(filepath.Dir(conf))
		if err != nil {
			return ""
		}
		return filepath.Base(dir)
	}
	read := func() string { b, _ := os.ReadFile(conf); return string(b) }
	if strings.Contains(read(), "crash") {
		fmt.Fprintln(os.Stderr, "[emerg] listen tcp :80: bind: address already in use")
		return 1
	}
	sig := make(chan os.Signal, 4)
	signal.Notify(sig, syscall.SIGHUP, syscall.SIGTERM)
	fmt.Fprintln(os.Stderr, "[notice] started hash="+hash())
	for s := range sig {
		if s == syscall.SIGTERM {
			return 0
		}
		c := read()
		switch {
		case strings.Contains(c, "silent"):
		case strings.Contains(c, "exit"):
			return 3
		case strings.Contains(c, "bad"):
			fmt.Fprintln(os.Stderr, "[emerg] reload failed: host example: bad upstream")
		default:
			fmt.Fprintln(os.Stderr, "[notice] config loaded hash="+hash())
		}
	}
	return 0
}

func newEdgeTestAgent(t *testing.T) (*Agent, context.CancelFunc) {
	t.Helper()
	t.Setenv(fakeEdgeEnv, "1")
	dir := t.TempDir()
	o := Options{Engine: EngineEdge, RunDir: filepath.Join(dir, "run"), LogDir: filepath.Join(dir, "logs"), DataDir: filepath.Join(dir, "data"),
		ConfigRoot: filepath.Join(dir, "edge"), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Version: "1.2.3"}
	for _, d := range []string{o.RunDir, o.LogDir, o.ConfigRoot} {
		os.MkdirAll(d, 0o755)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := newAgent(ctx, o, engineFactories[EngineEdge])
	a.version, a.modules, a.dynMods = a.eng.detect()
	go a.reaper.loop(ctx)
	if err := a.rel.init(); err != nil {
		t.Fatal(err)
	}
	stop := func() {
		a.sup.op.Lock()
		a.sup.stop(5 * time.Second)
		a.sup.op.Unlock()
		cancel()
	}
	t.Cleanup(stop)
	return a, stop
}

func edgeFiles(content string) Files {
	return Files{"edge.json": `{"schema":1,"note":"` + content + `"}`}
}

func TestEngineRegistryAndPolicy(t *testing.T) {
	if got := strings.Join(EngineNames(), ","); got != "balancer,edge,haproxy,nginx,tunnel" {
		t.Fatalf("engines = %s", got)
	}
	a := &Agent{}
	for name, want := range map[string][2]bool{EngineNginx: {true, true}, EngineEdge: {true, true}, EngineHAProxy: {false, false}, EngineBalancer: {false, false}, EngineTunnel: {false, false}} {
		e := engineFactories[name](a)
		if e.alwaysOn() != want[0] || e.proxy() != want[1] {
			t.Errorf("%s: alwaysOn=%v proxy=%v", name, e.alwaysOn(), e.proxy())
		}
	}
	if IsProxyEngine(EngineHAProxy) || !IsProxyEngine(EngineEdge) || NormalizeProxyEngine("") != EngineNginx || NormalizeProxyEngine("edge") != EngineEdge {
		t.Fatal("proxy engine helpers")
	}
	if IsLBEngine(EngineEdge) || !IsLBEngine(EngineHAProxy) || !IsLBEngine(EngineBalancer) || NormalizeLBEngine("") != EngineHAProxy ||
		NormalizeLBEngine("bogus") != EngineHAProxy || NormalizeLBEngine("balancer") != EngineBalancer {
		t.Fatal("load balancer engine helpers")
	}
	b := &balancerEngine{a: &Agent{}}
	if err := b.checkFiles(Files{"haproxy.cfg": ""}); err == nil {
		t.Fatal("balancer.json must be required")
	}
	if b.stopSignal() != syscall.SIGTERM || b.mainFile() != "balancer.json" || b.bootstrap() != nil {
		t.Fatal("balancer engine basics")
	}
	e := &edgeEngine{a: &Agent{}}
	if err := e.checkFiles(Files{"nginx.conf": ""}); err == nil {
		t.Fatal("edge.json must be required")
	}
	if e.stopSignal() != syscall.SIGTERM || e.mainFile() != "edge.json" {
		t.Fatal("edge engine basics")
	}
}

func TestProxyEngineFile(t *testing.T) {
	dir := t.TempDir()
	if ReadProxyEngine(dir) != EngineNginx {
		t.Fatal("missing file must mean nginx")
	}
	if err := WriteProxyEngine(dir, EngineEdge); err != nil || ReadProxyEngine(dir) != EngineEdge {
		t.Fatalf("write edge: %v", err)
	}
	os.WriteFile(ProxyEngineFile(dir), []byte("bogus\n"), 0o644)
	if ReadProxyEngine(dir) != EngineNginx {
		t.Fatal("unknown value must mean nginx")
	}
}

func TestEdgeBootstrapConfig(t *testing.T) {
	t.Setenv("RELAY_BOOTSTRAP_HTTP_PORT", "8080")
	t.Setenv(EdgeStatusPortEnv, "19091")
	a := &Agent{o: Options{DataDir: "/data", LogDir: "/var/log/relay"}}
	files := (&edgeEngine{a: a}).bootstrap()
	var cfg map[string]any
	if err := json.Unmarshal([]byte(files["edge.json"]), &cfg); err != nil {
		t.Fatal(err)
	}
	def, _ := cfg["default"].(map[string]any)
	if cfg["schema"] != 1.0 || cfg["httpPort"] != 8080.0 || cfg["statusAddr"] != "127.0.0.1:19091" || cfg["acmeWebroot"] != "/data/acme" ||
		cfg["logDir"] != "/var/log/relay" || def["action"] != "close" || def["cert"] != nil {
		t.Fatalf("bootstrap = %s", files["edge.json"])
	}
}

func TestEdgeBootGating(t *testing.T) {
	// Selected engine is nginx: edge writes its bootstrap but stays stopped.
	a, _ := newEdgeTestAgent(t)
	WriteProxyEngine(a.o.RunDir, EngineNginx)
	a.boot()
	if a.rel.current() != BootstrapHash || !a.rel.marker(stoppedMarker) || a.sup.running() {
		t.Fatalf("not selected: current=%q stopped=%v running=%v", a.rel.current(), a.rel.marker(stoppedMarker), a.sup.running())
	}
	// A later boot with an existing release keeps it stopped (marker governs),
	// even once edge is selected.
	WriteProxyEngine(a.o.RunDir, EngineEdge)
	a.boot()
	if a.sup.running() {
		t.Fatal("stopped marker must govern existing releases")
	}
	// Applying files starts it and clears the marker.
	resp := a.apply(ApplyRequest{Files: edgeFiles("v1")})
	if !resp.OK || !a.sup.running() || a.rel.marker(stoppedMarker) {
		t.Fatalf("apply after gating: %+v", resp)
	}

	// Selected engine is edge on a fresh root: it starts.
	b, _ := newEdgeTestAgent(t)
	WriteProxyEngine(b.o.RunDir, EngineEdge)
	b.boot()
	if b.rel.current() != BootstrapHash || b.rel.marker(stoppedMarker) || !b.sup.running() {
		t.Fatalf("selected: current=%q stopped=%v running=%v", b.rel.current(), b.rel.marker(stoppedMarker), b.sup.running())
	}
	st := b.status()
	if st.Version != "1.2.3" || st.Configured || st.Engine != EngineEdge {
		t.Fatalf("status = %+v", st)
	}
}

func TestEdgeApplyReloadDetection(t *testing.T) {
	old := edgeReloadTimeout
	edgeReloadTimeout = 2 * time.Second
	defer func() { edgeReloadTimeout = old }()
	a, _ := newEdgeTestAgent(t)

	// First apply starts the process.
	v1 := edgeFiles("v1")
	r1 := a.apply(ApplyRequest{Files: v1})
	if !r1.OK || !r1.Running || a.rel.current() != HashFiles(v1) {
		t.Fatalf("v1 = %+v", r1)
	}
	// Validation failure: nothing changes.
	rv := a.apply(ApplyRequest{Files: edgeFiles("invalid")})
	if rv.OK || rv.Stage != "validate" || !strings.Contains(rv.Output, "[emerg]") || a.rel.current() != HashFiles(v1) {
		t.Fatalf("invalid = %+v", rv)
	}
	// Successful reload waits for "config loaded hash=<v2>".
	v2 := edgeFiles("v2")
	r2 := a.apply(ApplyRequest{Files: v2})
	if !r2.OK || r2.Stage != "reload" || a.rel.current() != HashFiles(v2) {
		t.Fatalf("v2 = %+v", r2)
	}
	// Reload failure: reverted to v2 and the error text is returned.
	r3 := a.apply(ApplyRequest{Files: edgeFiles("bad")})
	if r3.OK || r3.Stage != "reload" || !strings.Contains(r3.Output, "reload failed: host example: bad upstream") || a.rel.current() != HashFiles(v2) {
		t.Fatalf("bad = %+v (current %s)", r3, a.rel.current())
	}
	if !a.sup.running() {
		t.Fatal("edge must keep running after a failed reload")
	}
	// No answer within the timeout.
	r4 := a.apply(ApplyRequest{Files: edgeFiles("silent")})
	if r4.OK || !strings.Contains(r4.Output, "did not load") {
		t.Fatalf("silent = %+v", r4)
	}
	// Stop frees the ports and sets the marker.
	rs := a.apply(ApplyRequest{Files: v2, Hash: HashFiles(v2), Stop: true})
	if !rs.OK || rs.Running || !a.rel.marker(stoppedMarker) || a.sup.running() {
		t.Fatalf("stop = %+v", rs)
	}
	// A failed start of a stopped engine leaves it stopped.
	rc := a.apply(ApplyRequest{Files: edgeFiles("crash")})
	if rc.OK || rc.Stage != "start" || a.sup.running() || !a.rel.marker(stoppedMarker) || a.rel.current() != HashFiles(v2) {
		t.Fatalf("crash = %+v running=%v stopped=%v", rc, a.sup.running(), a.rel.marker(stoppedMarker))
	}
	// /v1/start brings it back.
	if r := a.start(); !r.OK || !a.sup.running() {
		t.Fatalf("start = %+v", r)
	}
	// The process exiting during reload is a failure.
	r5 := a.apply(ApplyRequest{Files: edgeFiles("exit")})
	if r5.OK || !strings.Contains(r5.Output, "exited during reload") {
		t.Fatalf("exit = %+v", r5)
	}
}

func TestHasHashMarker(t *testing.T) {
	if !hasHashMarker("2026/09/15 10:00:00 [notice] config loaded hash=abcd1234", "config loaded hash=abcd1234") ||
		hasHashMarker("[notice] config loaded hash=abcd12345", "config loaded hash=abcd1234") ||
		!hasHashMarker("[notice] config loaded hash=abcd1234 hosts=3", "config loaded hash=abcd1234") {
		t.Fatal("hasHashMarker")
	}
}
