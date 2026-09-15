package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newBalancerTestAgent(t *testing.T) *Agent {
	t.Helper()
	t.Setenv(fakeEdgeEnv, "1")
	// Short base directory: unix socket paths are limited to ~104 bytes.
	dir, err := os.MkdirTemp("/tmp", "rlbal")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	o := Options{Engine: EngineBalancer, RunDir: filepath.Join(dir, "run"), LogDir: filepath.Join(dir, "logs"), DataDir: filepath.Join(dir, "data"),
		ConfigRoot: filepath.Join(dir, "balancer"), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Version: "2.0.0"}
	for _, d := range []string{o.RunDir, o.LogDir, o.ConfigRoot} {
		os.MkdirAll(d, 0o755)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := newAgent(ctx, o, engineFactories[EngineBalancer])
	a.version, a.modules, a.dynMods = a.eng.detect()
	go a.reaper.loop(ctx)
	if err := a.rel.init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		a.sup.op.Lock()
		a.sup.stop(5 * time.Second)
		a.sup.op.Unlock()
		cancel()
	})
	return a
}

func balancerFiles(note, socket string) Files {
	b, _ := json.Marshal(map[string]any{"schema": 1, "notes": []string{note}, "runtimeSocket": socket, "frontends": []any{}, "backends": []any{}})
	return Files{"balancer.json": string(b)}
}

func TestBalancerBootAndApply(t *testing.T) {
	old := balancerReloadTimeout
	balancerReloadTimeout = 2 * time.Second
	defer func() { balancerReloadTimeout = old }()
	a := newBalancerTestAgent(t)

	// No bootstrap: a fresh root stays unconfigured and stopped.
	a.boot()
	if a.rel.current() != "" || a.sup.running() {
		t.Fatalf("fresh boot: current=%q running=%v", a.rel.current(), a.sup.running())
	}
	st := a.status()
	if st.Engine != EngineBalancer || st.Version != "2.0.0" || st.Configured || len(st.Modules) != 0 {
		t.Fatalf("status = %+v", st)
	}

	// Validation runs `relay balancer check <dir>`.
	if v := a.validate(balancerFiles("invalid", "")); v.OK || !strings.Contains(v.Output, "balancer.json: invalid configuration") {
		t.Fatalf("validate invalid = %+v", v)
	}
	if v := a.validate(Files{"haproxy.cfg": "global"}); v.OK || !strings.Contains(v.Output, "balancer.json is missing") {
		t.Fatalf("validate missing = %+v", v)
	}
	if v := a.validate(balancerFiles("v1", SocketPath(a.o.RunDir, EngineBalancer))); v.OK || !strings.Contains(v.Output, "collides") {
		t.Fatalf("validate socket collision = %+v", v)
	}

	// The first apply starts `relay balancer run --config <root>/current/balancer.json`.
	v1 := balancerFiles("v1", "")
	r1 := a.apply(ApplyRequest{Files: v1})
	if !r1.OK || !r1.Running || a.rel.current() != HashFiles(v1) {
		t.Fatalf("v1 = %+v", r1)
	}
	// SIGHUP reload waits for "config loaded hash=<v2>".
	v2 := balancerFiles("v2", "")
	if r := a.apply(ApplyRequest{Files: v2}); !r.OK || r.Stage != "reload" || a.rel.current() != HashFiles(v2) {
		t.Fatalf("v2 = %+v", r)
	}
	// "reload failed: …" reverts to v2.
	r3 := a.apply(ApplyRequest{Files: balancerFiles("bad", "")})
	if r3.OK || !strings.Contains(r3.Output, "Relay Balancer rejected the new configuration") || !strings.Contains(r3.Output, "reload failed") || a.rel.current() != HashFiles(v2) || !a.sup.running() {
		t.Fatalf("bad = %+v (current %s)", r3, a.rel.current())
	}
	// The process exiting during a reload is a failure.
	r4 := a.apply(ApplyRequest{Files: balancerFiles("exit", "")})
	if r4.OK || !strings.Contains(r4.Output, "Relay Balancer exited during reload") {
		t.Fatalf("exit = %+v", r4)
	}
	// Stop (no backends / not selected) sets the marker, like HAProxy.
	if a.sup.running() {
		if r := a.stop(); !r.OK {
			t.Fatalf("stop = %+v", r)
		}
	} else {
		a.rel.setMarker(stoppedMarker, true)
	}
	if !a.rel.marker(stoppedMarker) || a.sup.running() {
		t.Fatal("stop must persist for a non-alwaysOn engine")
	}
	a.boot()
	if a.sup.running() {
		t.Fatal("a stopped balancer must stay stopped on boot")
	}
	// A failed start of a stopped balancer leaves it stopped.
	if rc := a.apply(ApplyRequest{Files: balancerFiles("crash", "")}); rc.OK || rc.Stage != "start" || a.sup.running() || !a.rel.marker(stoppedMarker) {
		t.Fatalf("crash = %+v running=%v", rc, a.sup.running())
	}
}

// fakeRuntime answers one command per connection like HAProxy's stats socket.
func fakeRuntime(t *testing.T, path string, answers map[string]string) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				line, _ := bufio.NewReader(c).ReadString('\n')
				io.WriteString(c, answers[strings.TrimSpace(line)])
			}(c)
		}
	}()
}

func TestBalancerRuntimeEndpoint(t *testing.T) {
	a := newBalancerTestAgent(t)
	if got := a.eng.(*balancerEngine).runtimeSocket(); got != BalancerRuntimeSocket(a.o.RunDir) {
		t.Fatalf("default runtime socket = %s", got)
	}
	custom := filepath.Join(a.o.RunDir, "custom-rt.sock")
	if r := a.apply(ApplyRequest{Files: balancerFiles("v1", custom)}); !r.OK {
		t.Fatalf("apply = %+v", r)
	}
	if got := a.eng.(*balancerEngine).runtimeSocket(); got != custom {
		t.Fatalf("runtime socket from balancer.json = %s", got)
	}
	fakeRuntime(t, custom, map[string]string{"show info": "Name: Relay Balancer\nPid: 42\n"})

	post := func(cmd string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(RuntimeRequest{Command: cmd})
		rec := httptest.NewRecorder()
		a.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, PathRuntime, bytes.NewReader(body)))
		return rec
	}
	rec := post("show info")
	var out RuntimeResponse
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil || !strings.Contains(out.Output, "Pid: 42") {
		t.Fatalf("runtime = %d %s", rec.Code, rec.Body.String())
	}
	if rec := post("show stat\nshow info"); rec.Code != http.StatusBadRequest {
		t.Fatalf("multi-line command = %d", rec.Code)
	}
	os.Remove(custom)
	if rec := post("show info"); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "balancer runtime API") {
		t.Fatalf("socket gone = %d %s", rec.Code, rec.Body.String())
	}

	// Proxy engines don't serve the runtime API.
	e, _ := newEdgeTestAgent(t)
	erec := httptest.NewRecorder()
	e.routes().ServeHTTP(erec, httptest.NewRequest(http.MethodPost, PathRuntime, strings.NewReader(`{"command":"show info"}`)))
	if erec.Code != http.StatusNotFound {
		t.Fatalf("edge runtime = %d", erec.Code)
	}
}
