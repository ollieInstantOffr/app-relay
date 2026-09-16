package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func runtimeCommand(t *testing.T, path, cmd string) string {
	t.Helper()
	c, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		t.Fatalf("dial runtime socket: %v", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(c, cmd+"\n")
	b, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRuntimeSocket(t *testing.T) {
	env := newPipeEnv(t, nil)
	disabled := env.gateway("a")
	disabled.Enabled = false
	writeGateways(t, env.gwFile, env.gateway("gw1"), disabled)
	// A stale socket file of a previous process is replaced.
	stale, err := net.Listen("unix", env.cfg.RuntimeSocket)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	if err := env.e.Start(env.cfg, "r1"); err != nil {
		t.Fatal(err)
	}
	fg := env.accept()
	fg.routes()
	waitState(t, env.e, "gw1", StateConnected)

	out := runtimeCommand(t, env.cfg.RuntimeSocket, "status")
	var st Status
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("status output %q: %v", out, err)
	}
	if st.Hash != "r1" || st.StartedAt.IsZero() || len(st.Gateways) != 2 ||
		st.Gateways[0].ID != "a" || st.Gateways[0].State != StateDisabled ||
		st.Gateways[1].ID != "gw1" || st.Gateways[1].State != StateConnected || st.Gateways[1].Generation != 1 {
		t.Fatalf("status = %s", out)
	}
	if !strings.Contains(out, `"rttMs"`) || !strings.Contains(out, `"ackedGeneration"`) {
		t.Fatalf("status JSON fields: %s", out)
	}
	if out := runtimeCommand(t, env.cfg.RuntimeSocket, "show stat"); !strings.HasPrefix(out, "error: ") || strings.Count(out, "\n") != 1 {
		t.Fatalf("unknown command = %q", out)
	}

	// The socket follows the configured path across reloads.
	cfg := env.cloneConfig()
	cfg.RuntimeSocket = filepath.Join(env.dir, "sub", "rt2.sock")
	if err := env.e.Reload(cfg, "r2"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(env.cfg.RuntimeSocket); !os.IsNotExist(err) {
		t.Fatalf("old runtime socket still present: %v", err)
	}
	if out := runtimeCommand(t, cfg.RuntimeSocket, "status"); !strings.Contains(out, `"hash":"r2"`) {
		t.Fatalf("status after reload = %s", out)
	}
	// Same path: the listener is kept.
	if err := env.e.Reload(cfg, "r3"); err != nil {
		t.Fatal(err)
	}
	if out := runtimeCommand(t, cfg.RuntimeSocket, "status"); !strings.Contains(out, `"hash":"r3"`) {
		t.Fatalf("status after second reload = %s", out)
	}

	env.e.Shutdown(t.Context())
	if _, err := os.Stat(cfg.RuntimeSocket); !os.IsNotExist(err) {
		t.Fatalf("runtime socket not removed at shutdown: %v", err)
	}
}

// writeRelease writes tunnel.json into root/<hash>.
func writeRelease(t *testing.T, root, hash string, cfg any) string {
	t.Helper()
	dir := filepath.Join(root, hash)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(dir, ConfigFile), cfg)
	return dir
}

func TestCheckCommand(t *testing.T) {
	dir := shortDir(t)
	gwFile := filepath.Join(dir, "gateways.json")
	valid := &Config{
		Schema: 1, GatewaysFile: gwFile, RuntimeSocket: filepath.Join(dir, "rt.sock"),
		Targets: Targets{HTTP: "/run/relay/tunnel/http.sock", HTTPS: "/run/relay/tunnel/https.sock"},
		Routes:  []Route{{GatewayID: "gw1", Names: []string{"a.test"}, TCP: []TCPRoute{{Port: 22, Socket: "/run/relay/tunnel/tcp-22.sock"}}}},
	}
	run := func(args ...string) (string, error) {
		var stdout, stderr bytes.Buffer
		err := RunCLI(context.Background(), args, &stdout, &stderr)
		return stdout.String() + stderr.String(), err
	}

	// Valid, gateways file missing (fine), by directory and by file.
	rel := writeRelease(t, dir, "good", valid)
	for _, p := range []string{rel, filepath.Join(rel, ConfigFile)} {
		if out, err := run("check", p); err != nil || out != "[notice] configuration is valid\n" {
			t.Fatalf("check %s = %q, %v", p, out, err)
		}
	}

	// With a valid gateways file.
	writeGateways(t, gwFile, Gateway{ID: "gw1", Address: "gw.example.net:443", Transport: "quic", Enabled: true, Pin: mustIdentity(t, "gw").Fingerprint()})
	if out, err := run("check", rel); err != nil || !strings.Contains(out, "[notice] configuration is valid") {
		t.Fatalf("check with gateways = %q, %v", out, err)
	}

	// An invalid gateways file.
	writeGateways(t, gwFile, Gateway{ID: "gw1", Transport: "udp"}, Gateway{ID: "gw1"})
	out, err := run("check", rel)
	if err == nil || !strings.Contains(out, "[emerg] ") || !strings.Contains(out, `unknown transport "udp"`) || !strings.Contains(out, "duplicate id") {
		t.Fatalf("check with bad gateways = %q, %v", out, err)
	}
	os.Remove(gwFile)

	// An invalid release: one [emerg] line per error.
	bad := *valid
	bad.Schema = 3
	bad.GatewaysFile = "relative.json"
	out, err = run("check", writeRelease(t, dir, "bad", &bad))
	if err == nil || strings.Count(out, "[emerg] ") < 2 || strings.Contains(out, "[notice]") {
		t.Fatalf("check invalid = %q, %v", out, err)
	}

	// Unparseable and missing.
	os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{"), 0o644)
	if out, err := run("check", filepath.Join(dir, "broken.json")); err == nil || !strings.HasPrefix(out, "[emerg] ") {
		t.Fatalf("check broken = %q, %v", out, err)
	}
	if out, err := run("check", filepath.Join(dir, "missing")); err == nil || !strings.HasPrefix(out, "[emerg] ") {
		t.Fatalf("check missing = %q, %v", out, err)
	}

	if _, err := run(); err == nil {
		t.Fatal("no command accepted")
	}
	if _, err := run("bogus"); err == nil {
		t.Fatal("unknown command accepted")
	}
	if _, err := run("run"); err == nil {
		t.Fatal("run without --config accepted")
	}
	if out, err := run("help"); err != nil || !strings.Contains(out, "relay tunnel run") {
		t.Fatalf("help = %q, %v", out, err)
	}
}

func TestRunCLILifecycle(t *testing.T) {
	dir := shortDir(t)
	cfg := &Config{Schema: 1, GatewaysFile: filepath.Join(dir, "gateways.json"), RuntimeSocket: filepath.Join(dir, "rt.sock"), Routes: []Route{}}
	writeRelease(t, dir, "r1", cfg)
	writeRelease(t, dir, "r2", cfg)
	bad := *cfg
	bad.Schema = 9
	writeRelease(t, dir, "r3", &bad)
	current := filepath.Join(dir, "current")
	if err := os.Symlink("r1", current); err != nil {
		t.Fatal(err)
	}
	swap := func(hash string) {
		tmp := current + ".tmp"
		os.Remove(tmp)
		if err := os.Symlink(hash, tmp); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, current); err != nil {
			t.Fatal(err)
		}
	}

	log := &syncBuffer{}
	sig := make(chan os.Signal, 1)
	done := make(chan error, 1)
	go func() {
		done <- serveCLI(context.Background(), filepath.Join(current, ConfigFile), sig, Options{Stderr: log})
	}()
	waitLine := func(substr string) {
		t.Helper()
		waitFor(t, "log line "+substr, func() bool { return hasLine(log.String(), substr) })
	}
	waitLine("[notice] started hash=r1")
	if out := runtimeCommand(t, cfg.RuntimeSocket, "status"); !strings.Contains(out, `"hash":"r1"`) {
		t.Fatalf("status = %s", out)
	}

	swap("r2")
	sig <- syscall.SIGHUP
	waitLine("config loaded hash=r2")

	swap("r3")
	sig <- syscall.SIGHUP
	waitLine("reload failed: ")
	if !hasLine(log.String(), "[emerg] reload failed: ") {
		t.Fatalf("reload failure level:\n%s", log)
	}
	if out := runtimeCommand(t, cfg.RuntimeSocket, "status"); !strings.Contains(out, `"hash":"r2"`) {
		t.Fatalf("status after failed reload = %s", out)
	}

	sig <- syscall.SIGTERM
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not stop")
	}
	if !hasLine(log.String(), "exited") {
		t.Fatalf("log:\n%s", log)
	}
	if _, err := os.Stat(cfg.RuntimeSocket); !os.IsNotExist(err) {
		t.Fatal("runtime socket left behind")
	}

	// A start failure is logged and returned.
	swap("r3")
	log2 := &syncBuffer{}
	if err := serveCLI(context.Background(), current, sig, Options{Stderr: log2}); err == nil || !hasLine(log2.String(), "[emerg]") {
		t.Fatalf("start with a bad release: %v\n%s", err, log2)
	}
}
