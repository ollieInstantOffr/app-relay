package agent

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// prepareTunnelSockets creates the tunnel ingress socket directory and removes
// stale sockets of engine (left behind by a crash: nothing accepts on them).
// Sockets a running process still listens on are kept, so it is safe to call
// before validate as well as before start.
func prepareTunnelSockets(runDir, engine string) {
	if runDir == "" {
		return
	}
	dir := filepath.Join(runDir, "tunnel")
	os.MkdirAll(dir, 0o750)
	matches, _ := filepath.Glob(filepath.Join(dir, engine+"-*.sock"))
	for _, p := range matches {
		c, err := net.DialTimeout("unix", p, 200*time.Millisecond)
		if err == nil {
			c.Close()
			continue
		}
		if errors.Is(err, syscall.ECONNREFUSED) {
			os.Remove(p)
		}
	}
}
