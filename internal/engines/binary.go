package engines

import (
	"crypto/sha256"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

// DefaultAgentBinDir is where the relay container publishes its binary for
// the engine containers (shared volume relay-bin mounted at /opt/relay).
const DefaultAgentBinDir = "/opt/relay/bin"

// InstallAgentBinary copies the running relay binary to the shared agent
// volume so official nginx/haproxy images can run `relay agent`. It is a
// no-op unless $RELAY_AGENT_BIN_DIR is set or /opt/relay exists.
func InstallAgentBinary(log *slog.Logger) {
	dir := os.Getenv("RELAY_AGENT_BIN_DIR")
	if dir == "" {
		if st, err := os.Stat(filepath.Dir(DefaultAgentBinDir)); err != nil || !st.IsDir() {
			return
		}
		dir = DefaultAgentBinDir
	}
	exe, err := os.Executable()
	if err != nil {
		log.Warn("agent binary: locate executable", "err", err)
		return
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		log.Warn("agent binary: resolve executable", "err", err)
		return
	}
	dst := filepath.Join(dir, "relay")
	if same(exe, dst) {
		return
	}
	if err := installFile(exe, dst); err != nil {
		log.Error("agent binary: install failed — engine containers can't start the agent", "dst", dst, "err", err)
		return
	}
	log.Info("agent binary installed for engine containers", "path", dst)
}

func fileSum(p string) ([]byte, bool) {
	f, err := os.Open(p)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, false
	}
	return h.Sum(nil), true
}

func same(a, b string) bool {
	sa, ok1 := fileSum(a)
	sb, ok2 := fileSum(b)
	return ok1 && ok2 && string(sa) == string(sb)
}

// installFile writes src to dst atomically with mode 0755.
func installFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".relay-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
