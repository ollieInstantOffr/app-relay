// Package balancer is Relay Balancer, the load balancer built into the relay
// binary (an alternative to HAProxy). It executes the resolved configuration
// in balancer.json (package spec) with the behaviour of the haproxy.cfg Relay
// renders for the same snapshot, and answers the subset of HAProxy's runtime
// API and log formats Relay consumes. See docs/BALANCER.md.
package balancer

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/instantoffr/relay/internal/balancer/spec"
)

// ConfigFile is the configuration file name inside a release directory.
const ConfigFile = "balancer.json"

const cliUsage = `usage:
  relay balancer run --config <path/to/balancer.json>   serve (SIGHUP reloads, SIGTERM/SIGQUIT/SIGINT stop)
  relay balancer check <dir | path/to/balancer.json>    validate a configuration without binding
`

// RunCLI implements `relay balancer run --config <balancer.json>` and
// `relay balancer check <dir|file>`.
//
// run logs "started hash=<h>" once serving, "config loaded hash=<h>" after a
// successful SIGHUP reload, "reload failed: <err>" otherwise, and drains on
// SIGTERM/SIGQUIT/SIGINT. check prints "[emerg] <msg>" per error or
// "[notice] configuration is valid" and returns an error when invalid.
func RunCLI(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, cliUsage)
		return errors.New("balancer: missing command")
	}
	switch args[0] {
	case "run":
		return runCommand(ctx, args[1:], stderr)
	case "check":
		return checkCommand(args[1:], stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, cliUsage)
		return nil
	}
	fmt.Fprint(stderr, cliUsage)
	return fmt.Errorf("balancer: unknown command %q", args[0])
}

// Check loads and validates a configuration without binding anything: the
// same validation and compilation as run, without DNS resolution.
func Check(path string) error {
	_, _, err := loadRelease(path, true)
	return err
}

// release is a resolved configuration path. The hash is the name of the
// directory holding balancer.json (the release directory behind the agent's
// "current" symlink).
type release struct {
	path string
	hash string
}

func resolveRelease(path string) (release, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return release{}, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return release{}, err
	}
	if fi, err := os.Stat(resolved); err == nil && fi.IsDir() {
		resolved = filepath.Join(resolved, ConfigFile)
	}
	return release{path: resolved, hash: filepath.Base(filepath.Dir(resolved))}, nil
}

func loadRelease(path string, dry bool) (release, *spec.Config, error) {
	rel, err := resolveRelease(path)
	if err != nil {
		return rel, nil, err
	}
	cfg, err := spec.Load(rel.path)
	if err != nil {
		return rel, nil, err
	}
	if dry {
		if _, err := compile(cfg, compileEnv{dry: true}); err != nil {
			return rel, nil, err
		}
	}
	return rel, cfg, nil
}

// errorLines flattens joined errors into one message per line.
func errorLines(err error) []string {
	var lines []string
	for _, l := range strings.Split(err.Error(), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

func checkCommand(args []string, stderr io.Writer) error {
	if len(args) != 1 {
		fmt.Fprint(stderr, cliUsage)
		return errors.New("balancer check: expected one path")
	}
	if err := Check(args[0]); err != nil {
		for _, l := range errorLines(err) {
			fmt.Fprintf(stderr, "[emerg] %s\n", l)
		}
		return err
	}
	fmt.Fprintln(stderr, "[notice] configuration is valid")
	return nil
}

func runCommand(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("balancer run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to balancer.json (resolved through symlinks on every reload)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *configPath == "" {
		fmt.Fprint(stderr, cliUsage)
		return errors.New("balancer run: --config is required")
	}
	// Subscribe before starting so a SIGHUP sent right after "started" is
	// never lost.
	sig := make(chan os.Signal, 8)
	signal.Notify(sig, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGINT)
	defer signal.Stop(sig)

	srv := NewServer(Options{Stderr: stderr})
	defer srv.Close()
	rel, cfg, err := loadRelease(*configPath, false)
	if err == nil {
		err = srv.Start(cfg, rel.hash)
	}
	if err != nil {
		srv.log.sync(lvlAlert, "%s", strings.Join(errorLines(err), "; "))
		return err
	}
	srv.log.sync(lvlNotice, "started hash=%s", rel.hash)

	for {
		select {
		case <-ctx.Done():
			return stop(srv)
		case sg := <-sig:
			if sg != syscall.SIGHUP {
				return stop(srv)
			}
			rel, cfg, err := loadRelease(*configPath, false)
			if err == nil {
				err = srv.Reload(cfg, rel.hash)
			}
			if err != nil {
				srv.log.sync(lvlAlert, "reload failed: %s", strings.Join(errorLines(err), "; "))
				continue
			}
			srv.log.sync(lvlNotice, "config loaded hash=%s", rel.hash)
		}
	}
}

func stop(srv *Server) error {
	srv.log.sync(lvlNotice, "shutting down (draining up to %s)", drainTimeout)
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	srv.Shutdown(ctx)
	srv.log.sync(lvlNotice, "exited")
	return nil
}
