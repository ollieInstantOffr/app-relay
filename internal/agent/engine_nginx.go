package agent

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// engine abstracts the nginx / haproxy / edge / balancer specifics.
type engine interface {
	mainFile() string
	binary() string
	validate(dir string) (string, error)
	command() *exec.Cmd
	stopSignal() syscall.Signal
	stableWait() time.Duration
	reload() (string, error)
	bootstrap() Files // nil: the engine does not run until configured
	detect() (version string, modules []string, dynamic map[string]string)
	checkFiles(files Files) error
	prepare() // create runtime directories before start/validate
	// alwaysOn engines should run whenever they are configured: a failed
	// start restores and restarts the previous release, and /v1/stop does
	// not persist (the container restart brings them back). HAProxy only
	// and Relay Balancer only run while backends exist.
	alwaysOn() bool
	// proxy engines serve the HTTP/HTTPS ports and streams; only the one
	// selected in /run/relay/proxy-engine starts on a fresh config root.
	proxy() bool
}

// engineFactories is the engine registry (agent --engine <name>).
var engineFactories = map[string]func(a *Agent) engine{
	EngineNginx:    func(a *Agent) engine { return &nginxEngine{a: a} },
	EngineHAProxy:  func(a *Agent) engine { return &haproxyEngine{a: a} },
	EngineEdge:     func(a *Agent) engine { return &edgeEngine{a: a} },
	EngineBalancer: func(a *Agent) engine { return &balancerEngine{a: a} },
}

// EngineNames lists the registered engines, sorted.
func EngineNames() []string {
	names := make([]string, 0, len(engineFactories))
	for n := range engineFactories {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

type nginxEngine struct{ a *Agent }

func (n *nginxEngine) mainFile() string           { return "nginx.conf" }
func (n *nginxEngine) binary() string             { return "nginx" }
func (n *nginxEngine) stopSignal() syscall.Signal { return syscall.SIGQUIT }
func (n *nginxEngine) stableWait() time.Duration  { return 1200 * time.Millisecond }
func (n *nginxEngine) alwaysOn() bool             { return true }
func (n *nginxEngine) proxy() bool                { return true }
func (n *nginxEngine) currentConf() string {
	return filepath.Join(n.a.rel.root, "current", "nginx.conf")
}

func (n *nginxEngine) prepare() {
	for _, d := range []string{"/run/nginx", "/var/cache/nginx", "/var/lib/nginx/tmp", n.a.o.LogDir} {
		os.MkdirAll(d, 0o755)
	}
	prepareTunnelSockets(n.a.o.RunDir, EngineNginx)
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

// StatusPortEnv overrides the loopback stub_status port (default 18080).
// The relay container reads the same variable when rendering nginx.conf.
const StatusPortEnv = "RELAY_NGINX_STATUS_PORT"

// NginxStatusPort returns the configured stub_status port.
func NginxStatusPort() string {
	if p := strings.TrimSpace(os.Getenv(StatusPortEnv)); p != "" {
		return p
	}
	return "18080"
}

// bootstrap serves ACME HTTP-01 challenges and closes everything else, so
// certificates can be issued before the first apply. It also serves
// stub_status on loopback so container health checks work before an apply.
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

    server {
        listen 127.0.0.1:` + NginxStatusPort() + `;
        server_name _;
        location = /stub_status {
            stub_status;
        }
        location / {
            return 404;
        }
    }
}
`}
}

var (
	nginxVersionRe    = regexp.MustCompile(`nginx/(\S+)`)
	nginxModulesDirRe = regexp.MustCompile(`--modules-path=(\S+)`)
)

// detect parses `nginx -V`. Official nginx images compile stream, http_v2,
// http_v3, auth_request and stub_status in; distro packages (Alpine's nginx)
// ship stream and geoip2 as dynamic modules, which the renderer loads with
// load_module using the paths returned here.
func (n *nginxEngine) detect() (string, []string, map[string]string) {
	out, err := n.a.reaper.run(10*time.Second, "nginx", "-V")
	if err != nil && out == "" {
		return "", nil, nil
	}
	modDirs := []string{"/usr/lib/nginx/modules", "/etc/nginx/modules"}
	if m := nginxModulesDirRe.FindStringSubmatch(out); m != nil {
		modDirs = append([]string{m[1]}, modDirs...)
	}
	return parseNginxV(out, func(so string) string {
		for _, d := range modDirs {
			p := filepath.Join(d, so)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
		return ""
	}, ipv6Available())
}

// parseNginxV is detect without the side effects (tested).
func parseNginxV(out string, findSO func(string) string, ipv6 bool) (string, []string, map[string]string) {
	version := ""
	if m := nginxVersionRe.FindStringSubmatch(out); m != nil {
		version = m[1]
	}
	has := func(flag string) bool {
		for _, f := range strings.Fields(out) {
			if f == flag {
				return true
			}
		}
		return false
	}
	mods := []string{}
	dyn := map[string]string{}
	for _, m := range []struct{ name, flag string }{
		{"http_v3", "--with-http_v3_module"},
		{"http_v2", "--with-http_v2_module"},
		{"auth_request", "--with-http_auth_request_module"},
		{"stub_status", "--with-http_stub_status_module"},
		{"http_realip", "--with-http_realip_module"},
		{"stream_realip", "--with-stream_realip_module"},
	} {
		if has(m.flag) {
			mods = append(mods, m.name)
		}
	}
	switch {
	case has("--with-stream"):
		mods = append(mods, "stream")
	case has("--with-stream=dynamic"):
		if p := findSO("ngx_stream_module.so"); p != "" {
			mods = append(mods, "stream")
			dyn["stream"] = p
		}
	}
	// geoip2 is a third-party module: never in the official image, a
	// dynamic module in distro packages.
	if p := findSO("ngx_http_geoip2_module.so"); p != "" {
		mods = append(mods, "geoip2")
		dyn["geoip2"] = p
	}
	if ipv6 {
		mods = append(mods, "ipv6")
	}
	if len(dyn) == 0 {
		dyn = nil
	}
	return version, mods, dyn
}

func ipv6Available() bool {
	b, err := os.ReadFile("/proc/net/if_inet6")
	return err == nil && len(strings.TrimSpace(string(b))) > 0
}
