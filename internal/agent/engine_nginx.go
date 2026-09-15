package agent

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// engine abstracts the nginx / haproxy specifics.
type engine interface {
	mainFile() string
	binary() string
	validate(dir string) (string, error)
	command() *exec.Cmd
	stopSignal() syscall.Signal
	stableWait() time.Duration
	reload() (string, error)
	bootstrap() Files // nil: the engine does not run until configured
	detect() (version string, modules []string)
	checkFiles(files Files) error
	prepare() // create runtime directories before start/validate
}

type nginxEngine struct{ a *Agent }

func (n *nginxEngine) mainFile() string           { return "nginx.conf" }
func (n *nginxEngine) binary() string             { return "nginx" }
func (n *nginxEngine) stopSignal() syscall.Signal { return syscall.SIGQUIT }
func (n *nginxEngine) stableWait() time.Duration  { return 1200 * time.Millisecond }
func (n *nginxEngine) currentConf() string {
	return filepath.Join(n.a.rel.root, "current", "nginx.conf")
}

func (n *nginxEngine) prepare() {
	for _, d := range []string{"/run/nginx", "/var/cache/nginx", "/var/lib/nginx/tmp", n.a.o.LogDir} {
		os.MkdirAll(d, 0o755)
	}
}

func (n *nginxEngine) validate(dir string) (string, error) {
	n.prepare()
	return n.a.reaper.run(60*time.Second, "nginx", "-t", "-c", filepath.Join(dir, "nginx.conf"))
}

func (n *nginxEngine) command() *exec.Cmd {
	return exec.Command("nginx", "-g", "daemon off;", "-c", n.currentConf())
}

func (n *nginxEngine) checkFiles(files Files) error {
	if _, ok := files["nginx.conf"]; !ok {
		return errors.New("nginx.conf is missing from the file set")
	}
	return nil
}

// reload sends SIGHUP to the master and waits until it has started new
// worker processes (proof the new configuration was accepted).
func (n *nginxEngine) reload() (string, error) {
	p := n.a.sup.current()
	if p == nil {
		return "", errors.New("nginx is not running")
	}
	before, procOK := childPIDs(p.pid)
	seq := n.a.logs.mark()
	if err := syscall.Kill(p.pid, syscall.SIGHUP); err != nil {
		return "", fmt.Errorf("signal nginx: %w", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(60 * time.Millisecond)
		select {
		case <-p.done:
			return joinLines(n.a.logs.after(seq)), errors.New("nginx exited during reload")
		default:
		}
		lines := n.a.logs.after(seq)
		for _, l := range lines {
			if strings.Contains(l.Text, "[emerg]") {
				return joinLines(errorLines(lines)), errors.New("nginx rejected the new configuration")
			}
		}
		if !procOK {
			// No /proc (non-Linux dev machine): assume success once the
			// master survived a second without reporting errors.
			if time.Since(deadline.Add(-15*time.Second)) > time.Second {
				return joinLines(errorLines(lines)), nil
			}
			continue
		}
		kids, _ := childPIDs(p.pid)
		for k := range kids {
			if !before[k] {
				// New workers are up; give them a moment to log bind errors.
				time.Sleep(150 * time.Millisecond)
				lines = n.a.logs.after(seq)
				for _, l := range lines {
					if strings.Contains(l.Text, "[emerg]") {
						return joinLines(errorLines(lines)), errors.New("nginx rejected the new configuration")
					}
				}
				return joinLines(errorLines(lines)), nil
			}
		}
	}
	return joinLines(errorLines(n.a.logs.after(seq))), errors.New("nginx did not load the new configuration within 15 s")
}

// bootstrap serves ACME HTTP-01 challenges and closes everything else, so
// certificates can be issued before the first apply.
func (n *nginxEngine) bootstrap() Files {
	port := os.Getenv("RELAY_BOOTSTRAP_HTTP_PORT")
	if port == "" {
		port = "80"
	}
	v6 := ""
	if ipv6Available() {
		v6 = "\n        listen [::]:" + port + " default_server;"
	}
	acme := filepath.Join(n.a.o.DataDir, "acme")
	return Files{"nginx.conf": `# Relay bootstrap configuration — replaced by the first apply.
include /etc/nginx/modules/*.conf;
worker_processes 1;
pid /run/nginx/nginx.pid;
error_log stderr warn;

events {
    worker_connections 1024;
}

http {
    include /etc/nginx/mime.types;
    default_type application/octet-stream;
    access_log off;
    server_tokens off;

    server {
        listen ` + port + ` default_server;` + v6 + `
        server_name _;

        location ^~ /.well-known/acme-challenge/ {
            root ` + acme + `;
            default_type text/plain;
            try_files $uri =404;
        }

        location / {
            return 444;
        }
    }
}
`}
}

var nginxVersionRe = regexp.MustCompile(`nginx/(\S+)`)

func (n *nginxEngine) detect() (string, []string) {
	out, err := n.a.reaper.run(10*time.Second, "nginx", "-V")
	if err != nil && out == "" {
		return "", nil
	}
	version := ""
	if m := nginxVersionRe.FindStringSubmatch(out); m != nil {
		version = m[1]
	}
	modDir := "/usr/lib/nginx/modules"
	if m := regexp.MustCompile(`--modules-path=(\S+)`).FindStringSubmatch(out); m != nil {
		modDir = m[1]
	}
	dynamic := func(so string) bool {
		_, err := os.Stat(filepath.Join(modDir, so))
		return err == nil
	}
	mods := []string{}
	if strings.Contains(out, "--with-http_v3_module") {
		mods = append(mods, "http_v3")
	}
	if strings.Contains(out, "--with-http_v2_module") {
		mods = append(mods, "http_v2")
	}
	if strings.Contains(out, "--with-http_auth_request_module") {
		mods = append(mods, "auth_request")
	}
	if strings.Contains(out, "--with-http_stub_status_module") {
		mods = append(mods, "stub_status")
	}
	if strings.Contains(out, "--with-stream=dynamic") {
		if dynamic("ngx_stream_module.so") {
			mods = append(mods, "stream")
		}
	} else if strings.Contains(out, "--with-stream") {
		mods = append(mods, "stream")
	}
	if strings.Contains(out, "geoip2") && dynamic("ngx_http_geoip2_module.so") {
		mods = append(mods, "geoip2")
	}
	if ipv6Available() {
		mods = append(mods, "ipv6")
	}
	return version, mods
}

func ipv6Available() bool {
	b, err := os.ReadFile("/proc/net/if_inet6")
	return err == nil && len(strings.TrimSpace(string(b))) > 0
}
