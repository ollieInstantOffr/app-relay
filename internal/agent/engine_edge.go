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

// edgeEngine runs Relay Edge, the reverse proxy built into the relay binary
// (`relay edge run`). See docs/EDGE.md §2.
type edgeEngine struct{ a *Agent }

// EdgeStatusPortEnv overrides the loopback status listener port (default
// 18081). The relay container reads the same variable when rendering edge.json.
const EdgeStatusPortEnv = "RELAY_EDGE_STATUS_PORT"

// EdgeStatusPort returns the configured Relay Edge status port.
func EdgeStatusPort() string {
	if p := strings.TrimSpace(os.Getenv(EdgeStatusPortEnv)); p != "" {
		return p
	}
	return "18081"
}

// edgeReloadTimeout bounds the wait for the reload result line.
var edgeReloadTimeout = 15 * time.Second

func (e *edgeEngine) mainFile() string           { return "edge.json" }
func (e *edgeEngine) stopSignal() syscall.Signal { return syscall.SIGTERM }
func (e *edgeEngine) stableWait() time.Duration  { return 800 * time.Millisecond }
func (e *edgeEngine) alwaysOn() bool             { return true }
func (e *edgeEngine) proxy() bool                { return true }

// binary is the relay executable running this agent.
func (e *edgeEngine) binary() string {
	exe, err := os.Executable()
	if err != nil {
		return "relay"
	}
	return exe
}

func (e *edgeEngine) currentConf() string {
	return filepath.Join(e.a.rel.root, "current", "edge.json")
}

func (e *edgeEngine) prepare() {
	if e.a.o.LogDir != "" {
		os.MkdirAll(e.a.o.LogDir, 0o755)
	}
}

func (e *edgeEngine) validate(dir string) (string, error) {
	e.prepare()
	return e.a.reaper.run(60*time.Second, e.binary(), "edge", "check", dir)
}

func (e *edgeEngine) command() *exec.Cmd {
	return exec.Command(e.binary(), "edge", "run", "--config", e.currentConf())
}

func (e *edgeEngine) checkFiles(files Files) error {
	if _, ok := files["edge.json"]; !ok {
		return errors.New("edge.json is missing from the file set")
	}
	return nil
}

// reload sends SIGHUP and waits for Relay Edge to report the result for the
// release current points at: "config loaded hash=<hash>" or "reload failed".
func (e *edgeEngine) reload() (string, error) {
	p := e.a.sup.current()
	if p == nil {
		return "", errors.New("Relay Edge is not running")
	}
	hash := e.a.rel.current()
	seq := e.a.logs.mark()
	if err := syscall.Kill(p.pid, syscall.SIGHUP); err != nil {
		return "", fmt.Errorf("signal Relay Edge: %w", err)
	}
	loaded := "config loaded hash=" + hash
	deadline := time.Now().Add(edgeReloadTimeout)
	for {
		lines := e.a.logs.after(seq)
		for _, l := range lines {
			if strings.Contains(l.Text, "reload failed") {
				return joinLines(errorLines(lines)), errors.New("Relay Edge rejected the new configuration")
			}
			if hasHashMarker(l.Text, loaded) {
				return joinLines(errorLines(lines)), nil
			}
		}
		select {
		case <-p.done:
			return joinLines(e.a.logs.after(seq)), errors.New("Relay Edge exited during reload")
		default:
		}
		if time.Now().After(deadline) {
			return joinLines(errorLines(e.a.logs.after(seq))), fmt.Errorf("Relay Edge did not load the new configuration within %s", edgeReloadTimeout)
		}
		time.Sleep(40 * time.Millisecond)
	}
}

// hasHashMarker reports whether text contains marker followed by the end of
// the line or whitespace (so hash=abc doesn't match hash=abcd).
func hasHashMarker(text, marker string) bool {
	for {
		i := strings.Index(text, marker)
		if i < 0 {
			return false
		}
		rest := text[i+len(marker):]
		if rest == "" || rest[0] == ' ' || rest[0] == '\t' {
			return true
		}
		text = rest
	}
}

// bootstrap serves ACME HTTP-01 challenges on the HTTP port and closes
// everything else. It has no hosts, so the data plane doesn't need the HTTPS
// listener; the self-signed placeholder is referenced only when its files
// already exist.
func (e *edgeEngine) bootstrap() Files {
	port := 80
	if v := strings.TrimSpace(os.Getenv("RELAY_BOOTSTRAP_HTTP_PORT")); v != "" {
		fmt.Sscanf(v, "%d", &port)
	}
	def := map[string]any{"action": "close"}
	httpsPort := 443
	certDir := filepath.Join(e.a.o.DataDir, "certs", "_default")
	full, key := filepath.Join(certDir, "fullchain.pem"), filepath.Join(certDir, "privkey.pem")
	if fileNonEmpty(full) && fileNonEmpty(key) {
		def["cert"] = map[string]any{"id": "_default", "certFile": full, "keyFile": key}
	}
	cfg := map[string]any{
		"schema":      1,
		"notes":       []string{"Relay bootstrap configuration — replaced by the first apply."},
		"httpPort":    port,
		"httpsPort":   httpsPort,
		"http3":       false,
		"statusAddr":  "127.0.0.1:" + EdgeStatusPort(),
		"logDir":      e.a.o.LogDir,
		"acmeWebroot": filepath.Join(e.a.o.DataDir, "acme"),
		"tlsProfile":  "intermediate",
		"blocklist":   []string{},
		"accessLists": map[string]any{},
		"default":     def,
		"hosts":       []any{},
		"redirects":   []any{},
		"streams":     []any{},
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return Files{"edge.json": string(b) + "\n"}
}

func fileNonEmpty(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Size() > 0
}

// detect reports the relay version: Relay Edge ships with the relay binary.
func (e *edgeEngine) detect() (string, []string, map[string]string) {
	version := strings.TrimSpace(e.a.o.Version)
	if version == "" {
		version = "relay"
	}
	mods := []string{"stream", "http_v2", "http_v3", "auth_request"}
	if ipv6Available() {
		mods = append(mods, "ipv6")
	}
	return version, mods, nil
}
