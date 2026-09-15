package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// balancerEngine runs Relay Balancer, the load balancer built into the relay
// binary (`relay balancer run`), selectable instead of HAProxy. Like HAProxy
// it only runs while backends exist, and it answers the same runtime API
// subset on its runtime socket.
type balancerEngine struct{ a *Agent }

// BalancerConfigFile is the only file of a Relay Balancer release.
const BalancerConfigFile = "balancer.json"

// BalancerRuntimeSocketName is the default Relay Balancer runtime API socket
// in the run directory (render/balancer.RuntimeSocketName).
const BalancerRuntimeSocketName = "balancer-runtime.sock"

// BalancerRuntimeSocket returns the default runtime socket inside runDir.
func BalancerRuntimeSocket(runDir string) string {
	return filepath.Join(runDir, BalancerRuntimeSocketName)
}

// balancerReloadTimeout bounds the wait for the reload result line.
var balancerReloadTimeout = 15 * time.Second

func (b *balancerEngine) mainFile() string           { return BalancerConfigFile }
func (b *balancerEngine) stopSignal() syscall.Signal { return syscall.SIGTERM } // drains
func (b *balancerEngine) stableWait() time.Duration  { return 800 * time.Millisecond }
func (b *balancerEngine) bootstrap() Files           { return nil }
func (b *balancerEngine) alwaysOn() bool             { return false }
func (b *balancerEngine) proxy() bool                { return false }

// binary is the relay executable running this agent.
func (b *balancerEngine) binary() string {
	exe, err := os.Executable()
	if err != nil {
		return "relay"
	}
	return exe
}

func (b *balancerEngine) currentConf() string {
	return filepath.Join(b.a.rel.root, "current", BalancerConfigFile)
}

func (b *balancerEngine) prepare() { os.MkdirAll(b.a.o.RunDir, 0o755) }

func (b *balancerEngine) validate(dir string) (string, error) {
	b.prepare()
	return b.a.reaper.run(60*time.Second, b.binary(), "balancer", "check", dir)
}

func (b *balancerEngine) command() *exec.Cmd {
	return exec.Command(b.binary(), "balancer", "run", "--config", b.currentConf())
}

// balancerSocketField is the part of balancer.json the agent reads.
type balancerSocketField struct {
	RuntimeSocket string `json:"runtimeSocket"`
}

func (b *balancerEngine) checkFiles(files Files) error {
	cfg, ok := files[BalancerConfigFile]
	if !ok {
		return errors.New("balancer.json is missing from the file set")
	}
	var f balancerSocketField
	if json.Unmarshal([]byte(cfg), &f) == nil && f.RuntimeSocket != "" {
		agentSock := filepath.Clean(SocketPath(b.a.o.RunDir, EngineBalancer))
		if filepath.Clean(f.RuntimeSocket) == agentSock {
			return fmt.Errorf("balancer.json: runtimeSocket %s collides with the Relay agent socket; use %s", f.RuntimeSocket, BalancerRuntimeSocket(b.a.o.RunDir))
		}
	}
	return nil
}

// runtimeSocket returns the runtime API socket declared in the active
// balancer.json, or the default path.
func (b *balancerEngine) runtimeSocket() string {
	cfg := b.a.rel.readFile(b.a.rel.current(), BalancerConfigFile)
	var f balancerSocketField
	if json.Unmarshal([]byte(cfg), &f) == nil && strings.HasPrefix(f.RuntimeSocket, "/") {
		return f.RuntimeSocket
	}
	return BalancerRuntimeSocket(b.a.o.RunDir)
}

// reload sends SIGHUP and waits for "config loaded hash=<hash>" or
// "reload failed: <err>".
func (b *balancerEngine) reload() (string, error) {
	return hupReload(b.a, "Relay Balancer", balancerReloadTimeout)
}

// detect reports the relay version: Relay Balancer ships with the relay binary.
func (b *balancerEngine) detect() (string, []string, map[string]string) {
	version := strings.TrimSpace(b.a.o.Version)
	if version == "" {
		version = "relay"
	}
	return version, []string{}, nil
}
