// Command relay is the Relay reverse proxy + load balancer manager.
//
//	relay [serve]                         run the API, UI and MCP server
//	relay agent --engine nginx | haproxy | edge | balancer | tunnel  supervise an engine (engine containers)
//	relay edge run --config FILE          run Relay Edge, the built-in reverse proxy
//	relay edge check DIR                  validate a Relay Edge config release
//	relay balancer run --config FILE      run Relay Balancer, the built-in load balancer
//	relay balancer check DIR              validate a Relay Balancer config release
//	relay gateway run|reset|info          run a tunnel gateway on a public server
//	relay users reset-password <name>     reset a user's password
//	relay mcp-stdio --url URL --token T   MCP over stdio for local clients
//	relay version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/instantoffr/relay/internal/acme"
	"github.com/instantoffr/relay/internal/adminlisten"
	"github.com/instantoffr/relay/internal/agent"
	"github.com/instantoffr/relay/internal/api"
	"github.com/instantoffr/relay/internal/apply"
	"github.com/instantoffr/relay/internal/auth"
	"github.com/instantoffr/relay/internal/backup"
	"github.com/instantoffr/relay/internal/balancer"
	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/docker"
	"github.com/instantoffr/relay/internal/edge"
	"github.com/instantoffr/relay/internal/engines"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/gateway"
	"github.com/instantoffr/relay/internal/geoip"
	"github.com/instantoffr/relay/internal/health"
	"github.com/instantoffr/relay/internal/lb"
	"github.com/instantoffr/relay/internal/logs"
	"github.com/instantoffr/relay/internal/mcp"
	"github.com/instantoffr/relay/internal/notify"
	"github.com/instantoffr/relay/internal/npmimport"
	"github.com/instantoffr/relay/internal/publicdns"
	"github.com/instantoffr/relay/internal/store"
	"github.com/instantoffr/relay/internal/tunnels"
	"github.com/instantoffr/relay/internal/webui"
)

// version is MAJOR.MINOR.PATCH from scripts/version.sh (-X main.version=…); "dev" for untagged builds.
var version = "dev"

// commit is the git commit of the build (-X main.commit=…, set by the in-app updater).
var commit = ""

func main() {
	level := slog.LevelInfo
	if os.Getenv("RELAY_DEBUG") == "1" {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}

	var err error
	switch cmd {
	case "serve":
		err = serve(ctx, log)
	case "agent":
		fs := flag.NewFlagSet("agent", flag.ExitOnError)
		engine := fs.String("engine", "", "nginx | haproxy | edge | balancer | tunnel")
		runDir := fs.String("run-dir", envOr("RELAY_RUN_DIR", "/run/relay"), "socket directory")
		logDir := fs.String("log-dir", envOr("RELAY_LOG_DIR", "/var/log/relay"), "log directory")
		dataDir := fs.String("data-dir", envOr("RELAY_DATA_DIR", "/data"), "data directory")
		fs.Parse(args)
		err = agent.Run(ctx, agent.Options{Engine: *engine, RunDir: *runDir, LogDir: *logDir, DataDir: *dataDir, Log: log.With("engine", *engine), Version: version})
	case "edge":
		err = edge.RunCLI(ctx, args, os.Stdout, os.Stderr)
	case "balancer":
		err = balancer.RunCLI(ctx, args, os.Stdout, os.Stderr)
	case "gateway":
		gateway.Version = version
		err = gateway.RunCLI(ctx, args, os.Stdout, os.Stderr)
	case "users":
		if len(args) != 2 || args[0] != "reset-password" {
			err = errors.New("usage: relay users reset-password <username>")
			break
		}
		cfg := core.ConfigFromEnv(version)
		var st *store.Store
		if st, err = store.Open(filepath.Join(cfg.DataDir, "relay.db")); err == nil {
			err = auth.ResetPasswordCLI(ctx, st, args[1], os.Stdout)
			st.Close()
		}
	case "mcp-stdio":
		fs := flag.NewFlagSet("mcp-stdio", flag.ExitOnError)
		url := fs.String("url", envOr("RELAY_URL", "http://127.0.0.1:8181"), "Relay base URL")
		token := fs.String("token", os.Getenv("RELAY_TOKEN"), "MCP API token")
		fs.Parse(args)
		err = mcp.RunStdio(ctx, mcp.StdioOptions{URL: *url, Token: *token})
	case "version", "--version", "-v":
		if commit != "" {
			fmt.Println("relay", version, commit)
		} else {
			fmt.Println("relay", version)
		}
	case "healthcheck":
		err = healthcheck(ctx)
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, "relay:", err)
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// healthcheck probes /healthz on the admin address the running server
// recorded (the port can change at runtime), falling back to RELAY_LISTEN.
func healthcheck(ctx context.Context) error {
	addr := adminlisten.ReadAddr(envOr("RELAY_RUN_DIR", "/run/relay"), envOr("RELAY_LISTEN", ":8181"))
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, "http://"+addr+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz on %s: %s", addr, resp.Status)
	}
	return nil
}

func serve(ctx context.Context, log *slog.Logger) error {
	cfg := core.ConfigFromEnv(version)
	cfg.Commit = commit
	engines.InstallAgentBinary(log) // publish the agent binary for the engine containers
	for _, d := range []string{cfg.DataDir, filepath.Join(cfg.DataDir, "certs"), filepath.Join(cfg.DataDir, "acme"), filepath.Join(cfg.DataDir, "backups"), cfg.RunDir, cfg.LogDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "relay.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	app := core.New(cfg, st, events.New(), log)
	app.Auth = auth.New(app)
	app.Engine = apply.New(app)
	app.LB = lb.New(app)
	app.Certs = acme.New(app)
	app.Logs = logs.New(app)
	app.Health = health.New(app)
	app.Docker = docker.New(app)
	app.Notify = notify.New(app)
	app.Backup = backup.New(app)
	app.Importer = npmimport.New(app)
	app.MCP = mcp.New(app)
	eng := engines.New(app)
	app.Engines = eng
	app.Containers = eng
	app.PublicDNS = publicdns.New(app)
	app.GeoIP = geoip.New(app)
	app.Tunnels = tunnels.New(app)

	for _, svc := range []core.Service{app.Auth, app.Notify, app.Certs, app.Engine, app.LB, app.Logs, app.Health, app.Docker, app.Backup, app.MCP, app.Engines, app.PublicDNS, app.GeoIP, app.Tunnels} {
		if err := svc.Start(ctx); err != nil {
			return fmt.Errorf("start %T: %w", svc, err)
		}
	}

	// The admin UI port comes from Settings → General and can change at
	// runtime; RELAY_LISTEN supplies the bind host and the initial port.
	handler := api.New(app, webui.FS()).Handler()
	app.API = handler
	admin := adminlisten.New(app, handler)
	app.AdminListener = admin
	if err := admin.Start(ctx); err != nil {
		return err
	}
	log.Info("relay listening", "ports", admin.Ports(), "version", version)
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	admin.Shutdown(shutdown)
	return nil
}
