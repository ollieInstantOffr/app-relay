package agent

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareTunnelSockets(t *testing.T) {
	run, err := os.MkdirTemp("/tmp", "rt") // unix socket paths must stay short
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(run)
	prepareTunnelSockets(run, "nginx")
	dir := filepath.Join(run, "tunnel")
	live, err := net.Listen("unix", filepath.Join(dir, "nginx-https.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	stale, err := net.Listen("unix", filepath.Join(dir, "nginx-http.sock"))
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	other, err := net.Listen("unix", filepath.Join(dir, "edge-http.sock"))
	if err != nil {
		t.Fatal(err)
	}
	other.(*net.UnixListener).SetUnlinkOnClose(false)
	other.Close()

	prepareTunnelSockets(run, "nginx")
	if _, err := os.Stat(filepath.Join(dir, "nginx-https.sock")); err != nil {
		t.Error("live socket removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "nginx-http.sock")); !os.IsNotExist(err) {
		t.Error("stale socket kept")
	}
	if _, err := os.Stat(filepath.Join(dir, "edge-http.sock")); err != nil {
		t.Error("another engine's socket removed")
	}
}
