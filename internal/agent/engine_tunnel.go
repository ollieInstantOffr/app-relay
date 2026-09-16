package agent

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// tunnelEngine runs the tunnel engine built into the relay binary (`relay
// tunnel run`). It only runs while something is published through a tunnel.
// Gateway addresses and keys live in a runtime file next to Relay's data
// (like certificates); /v1/reload re-reads it without a new release.
type tunnelEngine struct{ a *Agent }

// TunnelConfigFile is the only file of a tunnel engine release.
const TunnelConfigFile = "tunnel.json"

// TunnelRuntimeSocketName is the default status socket in the run directory.
const TunnelRuntimeSocketName = "tunnel-runtime.sock"

var tunnelReloadTimeout = 15 * time.Second

func (t *tunnelEngine) mainFile() string           { return TunnelConfigFile }
func (t *tunnelEngine) stopSignal() syscall.Signal { return syscall.SIGTERM }
func (t *tunnelEngine) stableWait() time.Duration  { return 800 * time.Millisecond }
func (t *tunnelEngine) bootstrap() Files           { return nil }
func (t *tunnelEngine) alwaysOn() bool             { return false }
func (t *tunnelEngine) proxy() bool                { return false }

func (t *tunnelEngine) binary() string {
	exe, err := os.Executable()
	if err != nil {
		return "relay"
	}
	return exe
}

func (t *tunnelEngine) currentConf() string {
	return filepath.Join(t.a.rel.root, "current", TunnelConfigFile)
}

func (t *tunnelEngine) prepare() { os.MkdirAll(t.a.o.RunDir, 0o755) }

func (t *tunnelEngine) validate(dir string) (string, error) {
	t.prepare()
	return t.a.reaper.run(60*time.Second, t.binary(), "tunnel", "check", dir)
}

func (t *tunnelEngine) command() *exec.Cmd {
	return exec.Command(t.binary(), "tunnel", "run", "--config", t.currentConf())
}

func (t *tunnelEngine) checkFiles(files Files) error {
	if _, ok := files[TunnelConfigFile]; !ok {
		return errors.New("tunnel.json is missing from the file set")
	}
	return nil
}

// runtimeSocket is the status socket declared in the active tunnel.json.
func (t *tunnelEngine) runtimeSocket() string {
	cfg := t.a.rel.readFile(t.a.rel.current(), TunnelConfigFile)
	var f struct {
		RuntimeSocket string `json:"runtimeSocket"`
	}
	if json.Unmarshal([]byte(cfg), &f) == nil && strings.HasPrefix(f.RuntimeSocket, "/") {
		return f.RuntimeSocket
	}
	return filepath.Join(t.a.o.RunDir, TunnelRuntimeSocketName)
}

// reload sends SIGHUP (the engine re-reads tunnel.json and the gateway list)
// and waits for "config loaded hash=<hash>" or "reload failed: <err>".
func (t *tunnelEngine) reload() (string, error) {
	return hupReload(t.a, "Tunnel engine", tunnelReloadTimeout)
}

// detect reports the relay version: the tunnel engine ships with the relay binary.
func (t *tunnelEngine) detect() (string, []string, map[string]string) {
	version := strings.TrimSpace(t.a.o.Version)
	if version == "" {
		version = "relay"
	}
	return version, []string{}, nil
}
