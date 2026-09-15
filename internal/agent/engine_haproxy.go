package agent

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

type haproxyEngine struct{ a *Agent }

func (h *haproxyEngine) mainFile() string           { return "haproxy.cfg" }
func (h *haproxyEngine) binary() string             { return "haproxy" }
func (h *haproxyEngine) stopSignal() syscall.Signal { return syscall.SIGUSR1 } // soft stop
func (h *haproxyEngine) stableWait() time.Duration  { return 1500 * time.Millisecond }
func (h *haproxyEngine) bootstrap() Files           { return nil }
func (h *haproxyEngine) masterSocket() string       { return HAProxyMasterSocket(h.a.o.RunDir) }

func (h *haproxyEngine) prepare() { os.MkdirAll(h.a.o.RunDir, 0o755) }

func (h *haproxyEngine) validate(dir string) (string, error) {
	h.prepare()
	return h.a.reaper.run(60*time.Second, "haproxy", "-c", "-f", filepath.Join(dir, "haproxy.cfg"))
}

func (h *haproxyEngine) command() *exec.Cmd {
	os.Remove(h.masterSocket())
	return exec.Command("haproxy", "-W", "-db", "-S", h.masterSocket(), "-f", filepath.Join(h.a.rel.root, "current", "haproxy.cfg"))
}

var statsSocketRe = regexp.MustCompile(`(?m)^\s*stats\s+socket\s+(\S+)`)

func (h *haproxyEngine) checkFiles(files Files) error {
	cfg, ok := files["haproxy.cfg"]
	if !ok {
		return errors.New("haproxy.cfg is missing from the file set")
	}
	agentSock := filepath.Clean(SocketPath(h.a.o.RunDir, EngineHAProxy))
	for _, m := range statsSocketRe.FindAllStringSubmatch(cfg, -1) {
		if filepath.Clean(m[1]) == agentSock || filepath.Clean(m[1]) == filepath.Clean(h.masterSocket()) {
			return fmt.Errorf("haproxy.cfg: stats socket %s collides with a Relay control socket; use %s", m[1], HAProxyRuntimeSocket(h.a.o.RunDir))
		}
	}
	return nil
}

// runtimeSocket returns the admin stats socket declared in the active config.
func (h *haproxyEngine) runtimeSocket() string {
	cfg := h.a.rel.readFile(h.a.rel.current(), "haproxy.cfg")
	for _, m := range statsSocketRe.FindAllStringSubmatch(cfg, -1) {
		if strings.HasPrefix(m[1], "/") {
			return m[1]
		}
	}
	return HAProxyRuntimeSocket(h.a.o.RunDir)
}

// reload asks the master process to re-execute with the current config.
func (h *haproxyEngine) reload() (string, error) {
	p := h.a.sup.current()
	if p == nil {
		return "", errors.New("haproxy is not running")
	}
	out, err := unixCommand(h.masterSocket(), "reload", 60*time.Second)
	if err == nil && strings.Contains(out, "Success=") {
		body := out
		if i := strings.Index(out, "--"); i >= 0 {
			body = strings.TrimSpace(out[i+2:])
		}
		if strings.Contains(out, "Success=1") {
			return body, nil
		}
		return body, errors.New("haproxy rejected the new configuration")
	}
	// Older masters don't report status: fall back to SIGUSR2 and watch.
	seq := h.a.logs.mark()
	if err := syscall.Kill(p.pid, syscall.SIGUSR2); err != nil {
		return "", fmt.Errorf("signal haproxy: %w", err)
	}
	time.Sleep(1500 * time.Millisecond)
	lines := h.a.logs.after(seq)
	select {
	case <-p.done:
		return joinLines(lines), errors.New("haproxy exited during reload")
	default:
	}
	for _, l := range lines {
		if strings.Contains(l.Text, "[ALERT]") {
			return joinLines(errorLines(lines)), errors.New("haproxy rejected the new configuration")
		}
	}
	return joinLines(errorLines(lines)), nil
}

var haproxyVersionRe = regexp.MustCompile(`(?i)HA-?Proxy version (\S+)`)

func (h *haproxyEngine) detect() (string, []string) {
	out, _ := h.a.reaper.run(10*time.Second, "haproxy", "-v")
	version := ""
	if m := haproxyVersionRe.FindStringSubmatch(out); m != nil {
		version = m[1]
		if i := strings.IndexByte(version, '-'); i > 0 {
			version = version[:i]
		}
	}
	return version, []string{}
}

// unixCommand writes one command line to a unix socket and reads the reply.
func unixCommand(path, command string, timeout time.Duration) (string, error) {
	conn, err := net.DialTimeout("unix", path, 3*time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))
	if _, err := io.WriteString(conn, strings.TrimSpace(command)+"\n"); err != nil {
		return "", err
	}
	b, err := io.ReadAll(io.LimitReader(conn, 8<<20))
	if err != nil && len(b) == 0 {
		return "", err
	}
	return string(b), nil
}
