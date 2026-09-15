package balancer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/balancer/spec"
)

func writeConfig(t *testing.T, dir string, cfg *spec.Config) {
	t.Helper()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func cloneConfig(t *testing.T, cfg *spec.Config) *spec.Config {
	t.Helper()
	data, _ := json.Marshal(cfg)
	var out spec.Config
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

func TestCheckCLI(t *testing.T) {
	base := func(t *testing.T) *spec.Config {
		cfg := baseConfig(t)
		cfg.Backends = []spec.Backend{httpBackend("web", spec.Server{Name: "a", Address: "app.internal", Port: 80, Weight: 1, State: spec.StateReady})}
		cfg.Frontends = []spec.Frontend{httpFrontend(t, "fe", "web")}
		return cfg
	}
	check := func(t *testing.T, path string) (string, error) {
		var out bytes.Buffer
		err := RunCLI(context.Background(), []string{"check", path}, io.Discard, &out)
		return out.String(), err
	}
	t.Run("valid dir and file", func(t *testing.T) {
		dir := t.TempDir()
		writeConfig(t, dir, base(t))
		for _, p := range []string{dir, filepath.Join(dir, ConfigFile)} {
			if out, err := check(t, p); err != nil || out != "[notice] configuration is valid\n" {
				t.Fatalf("%s: %v %q", p, err, out)
			}
		}
	})
	t.Run("empty config", func(t *testing.T) {
		dir := t.TempDir()
		writeConfig(t, dir, &spec.Config{Schema: 1, RuntimeSocket: "/run/relay/balancer-runtime.sock", Frontends: []spec.Frontend{}, Backends: []spec.Backend{}})
		if out, err := check(t, dir); err != nil {
			t.Fatalf("%v %q", err, out)
		}
	})
	t.Run("invalid", func(t *testing.T) {
		dir := t.TempDir()
		cfg := base(t)
		cfg.Schema = 3
		cfg.Backends[0].Algorithm = "fastest"
		writeConfig(t, dir, cfg)
		out, err := check(t, dir)
		if err == nil {
			t.Fatal("accepted")
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "[emerg] unsupported schema 3") || !strings.HasPrefix(lines[1], "[emerg] backend \"web\": unknown algorithm") {
			t.Fatalf("output: %q", out)
		}
	})
	t.Run("ca file", func(t *testing.T) {
		dir := t.TempDir()
		cfg := base(t)
		cfg.Backends[0].TLS, cfg.Backends[0].TLSVerify = true, true
		cfg.CAFile = filepath.Join(dir, "missing.pem")
		writeConfig(t, dir, cfg)
		if _, err := check(t, dir); err != nil {
			t.Fatalf("missing CA file falls back to system roots: %v", err)
		}
		os.WriteFile(filepath.Join(dir, "garbage.pem"), []byte("not a certificate"), 0o644)
		cfg.CAFile = filepath.Join(dir, "garbage.pem")
		writeConfig(t, dir, cfg)
		if out, err := check(t, dir); err == nil || !strings.Contains(out, "[emerg] caFile") {
			t.Fatalf("garbage CA: %v %q", err, out)
		}
	})
	t.Run("broken json and missing file", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, ConfigFile), []byte("{"), 0o644)
		if out, err := check(t, dir); err == nil || !strings.HasPrefix(out, "[emerg] ") {
			t.Fatalf("%v %q", err, out)
		}
		if _, err := check(t, filepath.Join(dir, "nope")); err == nil {
			t.Fatal("missing path accepted")
		}
		if err := RunCLI(context.Background(), nil, io.Discard, io.Discard); err == nil {
			t.Fatal("no command accepted")
		}
	})
}

var (
	processLineRe = regexp.MustCompile(`^\[(NOTICE|WARNING|ALERT)\] +\(\d+\) : \S`)
	trafficLineRe = regexp.MustCompile(`^\S+:\d+ \[\d{2}/[A-Z][a-z]{2}/\d{4}:\d{2}:\d{2}:\d{2}\.\d{3}\] \S`)
)

func TestRunCLISignalsAndMarkers(t *testing.T) {
	root := t.TempDir()
	bind := freeBind(t)
	sock := shortSocket(t)
	inflight := make(chan struct{}, 1)
	mkUp := func(name string) *upstream {
		return newUpstream(t, name, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/slow" {
				inflight <- struct{}{}
				time.Sleep(400 * time.Millisecond)
			}
			io.WriteString(w, name)
		})
	}
	release := func(name string, u *upstream, schema int) {
		cfg := baseConfig(t)
		cfg.Schema, cfg.RuntimeSocket = schema, sock
		cfg.Backends = []spec.Backend{httpBackend("web", u.server())}
		fe := httpFrontend(t, "fe", "web")
		fe.Bind = bind
		cfg.Frontends = []spec.Frontend{fe}
		writeConfig(t, filepath.Join(root, "releases", name), cfg)
	}
	v1, v2 := mkUp("v1"), mkUp("v2")
	release("r1", v1, 1)
	release("r2", v2, 1)
	release("r3", v1, 9)
	current := filepath.Join(root, "current")
	point := func(name string) {
		os.Remove(current)
		if err := os.Symlink(filepath.Join("releases", name), current); err != nil {
			t.Fatal(err)
		}
	}
	point("r1")
	stderr := &syncBuffer{}
	errc := make(chan error, 1)
	go func() {
		errc <- RunCLI(context.Background(), []string{"run", "--config", filepath.Join(current, ConfigFile)}, io.Discard, stderr)
	}()
	wait := func(marker string) {
		t.Helper()
		if !waitFor(t, 10*time.Second, func() bool { return strings.Contains(stderr.String(), marker) }) {
			t.Fatalf("marker %q not seen in:\n%s", marker, stderr.String())
		}
	}
	wait("started hash=r1\n")
	if r := get(t, "http://"+bind+"/"); r.body != "v1" {
		t.Fatalf("r1: %q", r.body)
	}
	point("r2")
	syscall.Kill(os.Getpid(), syscall.SIGHUP)
	wait("config loaded hash=r2\n")
	if r := get(t, "http://"+bind+"/"); r.body != "v2" {
		t.Fatalf("r2: %q", r.body)
	}
	point("r3")
	syscall.Kill(os.Getpid(), syscall.SIGHUP)
	wait("reload failed: unsupported schema 9")
	if r := get(t, "http://"+bind+"/"); r.body != "v2" {
		t.Fatalf("failed reload changed the config: %q", r.body)
	}

	slow := make(chan result, 1)
	go func() { slow <- get(t, "http://"+bind+"/slow") }()
	<-inflight
	syscall.Kill(os.Getpid(), syscall.SIGTERM)
	wait("shutting down")
	if !waitFor(t, 3*time.Second, func() bool {
		c, err := net.DialTimeout("tcp", bind, 200*time.Millisecond)
		if err == nil {
			c.Close()
		}
		return err != nil
	}) {
		t.Fatal("still accepting after SIGTERM")
	}
	if r := <-slow; r.StatusCode != 200 || r.body != "v2" {
		t.Fatalf("in-flight request during shutdown: %d %q", r.StatusCode, r.body)
	}
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(25 * time.Second):
		t.Fatal("run did not stop")
	}
	wait("exited\n")
	for _, l := range strings.Split(strings.TrimSpace(stderr.String()), "\n") {
		if !processLineRe.MatchString(l) && !trafficLineRe.MatchString(l) {
			t.Errorf("malformed line %q", l)
		}
	}
	if !strings.Contains(stderr.String(), "[ALERT]    (") || !strings.Contains(stderr.String(), "[NOTICE]   (") {
		t.Fatalf("levels:\n%s", stderr.String())
	}
}

func TestReloadKeepsConnectionsAndListeners(t *testing.T) {
	started, unblock := make(chan struct{}, 1), make(chan struct{})
	a := newUpstream(t, "a", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			started <- struct{}{}
			<-unblock
		}
		io.WriteString(w, "a")
	})
	b := newUpstream(t, "b", nil)
	echo := tcpServer(t, echoTCP("echo"))
	cfg := baseConfig(t)
	cfg.Backends = []spec.Backend{
		httpBackend("web", a.server()),
		tcpBackend("t", spec.Server{Name: "t", Address: "127.0.0.1", Port: echo, Weight: 1, State: spec.StateReady}),
	}
	cfg.Frontends = []spec.Frontend{httpFrontend(t, "fe", "web"), tcpFrontend(t, "tfe", "t")}
	e := startEnv(t, cfg)
	addrsBefore := e.srv.Addrs()

	// A keep-alive client connection and a TCP session opened before the reload.
	kc, err := net.Dial("tcp", cfg.Frontends[0].Bind)
	if err != nil {
		t.Fatal(err)
	}
	defer kc.Close()
	kbr := bufio.NewReader(kc)
	ask := func() string {
		io.WriteString(kc, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
		kc.SetReadDeadline(time.Now().Add(3 * time.Second))
		res, err := http.ReadResponse(kbr, nil)
		if err != nil {
			t.Fatalf("keep-alive request: %v", err)
		}
		body, _ := io.ReadAll(res.Body)
		return string(body)
	}
	if got := ask(); !strings.HasPrefix(got, "a") {
		t.Fatalf("before: %q", got)
	}
	tc, err := net.Dial("tcp", cfg.Frontends[1].Bind)
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	tbr := bufio.NewReader(tc)
	io.WriteString(tc, "one\n")
	if l, _ := tbr.ReadString('\n'); l != "echo:one\n" {
		t.Fatalf("tcp before: %q", l)
	}
	slow := make(chan result, 1)
	go func() { slow <- get(t, e.feURL(0, "/slow")) }()
	<-started

	// Hammer new connections while reloading: none may be refused.
	var refused, served atomic.Int64
	stop := make(chan struct{})
	done := make(chan struct{})
	hammerURL := e.feURL(0, "/")
	go func() {
		defer close(done)
		client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 5 * time.Second}
		for {
			select {
			case <-stop:
				return
			default:
			}
			res, err := client.Get(hammerURL)
			if err != nil {
				refused.Add(1)
				continue
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			served.Add(1)
		}
	}()
	next := cloneConfig(t, cfg)
	next.Backends[0].Servers = []spec.Server{b.server()}
	extra := httpFrontend(t, "fe2", "web")
	next.Frontends = append(next.Frontends, extra)
	e.cfg = next
	for i := range 10 {
		if err := e.reload("r" + string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	<-done
	if refused.Load() != 0 || served.Load() == 0 {
		t.Fatalf("connections during reloads: %d refused, %d served", refused.Load(), served.Load())
	}
	if got := ask(); !strings.HasPrefix(got, "b") {
		t.Fatalf("kept connection must use the new config: %q", got)
	}
	io.WriteString(tc, "two\n")
	if l, _ := tbr.ReadString('\n'); l != "echo:two\n" {
		t.Fatalf("tcp session across reload: %q", l)
	}
	close(unblock)
	if r := <-slow; r.StatusCode != 200 || r.body != "a" {
		t.Fatalf("in-flight request across reload: %d %q", r.StatusCode, r.body)
	}
	if r := get(t, "http://"+extra.Bind+"/"); !strings.HasPrefix(r.body, "b") {
		t.Fatalf("new frontend: %q", r.body)
	}
	after := e.srv.Addrs()
	for k, v := range addrsBefore {
		if after[k].String() != v.String() {
			t.Fatalf("listener %s rebound", k)
		}
	}
	if e.srv.Hash() != "rj" {
		t.Fatalf("hash %q", e.srv.Hash())
	}

	// Removing a frontend closes its listener.
	e.cfg = cloneConfig(t, next)
	e.cfg.Frontends = e.cfg.Frontends[:2]
	if err := e.reload("removed"); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 3*time.Second, func() bool {
		c, err := net.DialTimeout("tcp", extra.Bind, 200*time.Millisecond)
		if err == nil {
			c.Close()
		}
		return err != nil
	}) {
		t.Fatal("removed frontend still accepting")
	}
}

func TestFailedReloadKeepsOldConfig(t *testing.T) {
	a := newUpstream(t, "a", nil)
	cfg := baseConfig(t)
	cfg.Backends = []spec.Backend{httpBackend("web", a.server())}
	cfg.Frontends = []spec.Frontend{httpFrontend(t, "fe", "web")}
	e := startEnv(t, cfg)

	bad := cloneConfig(t, cfg)
	bad.Backends[0].Algorithm = "nope"
	if err := e.srv.Reload(bad, "bad"); err == nil || !strings.Contains(err.Error(), "unknown algorithm") {
		t.Fatalf("invalid reload: %v", err)
	}
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	portInUse := cloneConfig(t, cfg)
	fresh := httpFrontend(t, "fresh", "web")
	taken := httpFrontend(t, "taken", "web")
	taken.Bind = busy.Addr().String()
	portInUse.Frontends = append(portInUse.Frontends, fresh, taken)
	portInUse.Backends[0].Servers[0].Weight = 3
	if err := e.srv.Reload(portInUse, "busy"); err == nil || !strings.Contains(err.Error(), "Starting frontend taken: cannot bind socket") {
		t.Fatalf("port in use: %v", err)
	}
	if r := get(t, e.feURL(0, "/")); r.StatusCode != 200 {
		t.Fatalf("old config must keep serving: %d", r.StatusCode)
	}
	if e.srv.Hash() != "h1" || e.stat("web", "a")["weight"] != "100" {
		t.Fatalf("state changed by a failed reload: %s %s", e.srv.Hash(), e.stat("web", "a")["weight"])
	}
	ln, err := net.Listen("tcp", fresh.Bind)
	if err != nil {
		t.Fatalf("listener opened by the failed reload is still bound: %v", err)
	}
	ln.Close()
}

func TestShutdownDrains(t *testing.T) {
	started := make(chan struct{}, 1)
	a := newUpstream(t, "a", func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		time.Sleep(300 * time.Millisecond)
		io.WriteString(w, "done")
	})
	echo := tcpServer(t, echoTCP("e"))
	cfg := baseConfig(t)
	cfg.Backends = []spec.Backend{httpBackend("web", a.server()), tcpBackend("t", spec.Server{Name: "t", Address: "127.0.0.1", Port: echo, Weight: 1, State: spec.StateReady})}
	cfg.Frontends = []spec.Frontend{httpFrontend(t, "fe", "web"), tcpFrontend(t, "tfe", "t")}
	srv := NewServer(Options{})
	if err := srv.Start(cfg, "h"); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	tc, err := net.Dial("tcp", cfg.Frontends[1].Bind)
	if err != nil {
		t.Fatal(err)
	}
	tbr := bufio.NewReader(tc)
	slow := make(chan result, 1)
	go func() { slow <- get(t, "http://"+cfg.Frontends[0].Bind+"/") }()
	<-started
	stopped := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		close(stopped)
	}()
	if !waitFor(t, 3*time.Second, func() bool {
		c, err := net.DialTimeout("tcp", cfg.Frontends[0].Bind, 100*time.Millisecond)
		if err == nil {
			c.Close()
		}
		return err != nil
	}) {
		t.Fatal("still accepting")
	}
	if r := <-slow; r.body != "done" {
		t.Fatalf("in-flight request: %q", r.body)
	}
	io.WriteString(tc, "still\n")
	tc.SetReadDeadline(time.Now().Add(2 * time.Second))
	if l, _ := tbr.ReadString('\n'); l != "e:still\n" {
		t.Fatalf("tcp session during drain: %q", l)
	}
	select {
	case <-stopped:
		t.Fatal("shutdown did not wait for the tcp session")
	default:
	}
	tc.Close()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not finish after the session ended")
	}
}

func TestEmptyConfigRunsIdle(t *testing.T) {
	cfg := &spec.Config{Schema: 1, RuntimeSocket: shortSocket(t)}
	e := startEnv(t, cfg)
	out := e.cmd("show stat")
	if out != "# "+statHeader+",\n\n" {
		t.Fatalf("show stat: %q", out)
	}
	if !strings.Contains(e.cmd("show info"), "Pid: ") {
		t.Fatal("show info")
	}
}
