package tunnel

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
	"time"
)

const cliUsage = `usage:
  relay tunnel run --config <path/to/tunnel.json>   serve (SIGHUP reloads, SIGTERM/SIGQUIT/SIGINT stop)
  relay tunnel check <dir | path/to/tunnel.json>    validate a configuration without dialing
`

// RunCLI implements `relay tunnel run --config <tunnel.json>` and
// `relay tunnel check <dir|tunnel.json>`.
//
// run logs "started hash=<h>" once running, "config loaded hash=<h>" after a
// successful SIGHUP reload (which also re-reads the gateway list), "reload
// failed: <err>" otherwise, and stops on SIGTERM/SIGQUIT/SIGINT. check prints
// "[emerg] <msg>" per error or "[notice] configuration is valid" and returns
// an error when invalid.
func RunCLI(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, cliUsage)
		return errors.New("tunnel: missing command")
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
	return fmt.Errorf("tunnel: unknown command %q", args[0])
}

// Check validates tunnel.json and, when it exists, the gateway list it
// names, without dialing or binding anything.
func Check(path string) error {
	_, cfg, err := loadRelease(path)
	if err != nil {
		return err
	}
	if _, err := os.Stat(cfg.GatewaysFile); err == nil {
		if _, err := loadGateways(cfg.GatewaysFile); err != nil {
			return err
		}
	}
	return nil
}

// release is a resolved configuration path. The hash is the name of the
// directory holding tunnel.json (the release directory behind the agent's
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

func loadRelease(path string) (release, *Config, error) {
	rel, err := resolveRelease(path)
	if err != nil {
		return rel, nil, err
	}
	cfg, err := LoadConfig(rel.path)
	if err != nil {
		return rel, nil, err
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
		return errors.New("tunnel check: expected one path")
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
	fs := flag.NewFlagSet("tunnel run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to tunnel.json (resolved through symlinks on every reload)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *configPath == "" {
		fmt.Fprint(stderr, cliUsage)
		return errors.New("tunnel run: --config is required")
	}
	// Subscribe before starting so a SIGHUP sent right after "started" is
	// never lost.
	sig := make(chan os.Signal, 8)
	signal.Notify(sig, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGINT)
	defer signal.Stop(sig)
	return serveCLI(ctx, *configPath, sig, Options{Stderr: stderr, Version: Version})
}

// serveCLI is the run loop, driven by sig (tests send signals directly).
func serveCLI(ctx context.Context, configPath string, sig <-chan os.Signal, opts Options) error {
	e := New(opts)
	rel, cfg, err := loadRelease(configPath)
	if err == nil {
		err = e.Start(cfg, rel.hash)
	}
	if err != nil {
		e.log.logf(levelEmerg, "%s", strings.Join(errorLines(err), "; "))
		return err
	}
	e.log.noticef("started hash=%s", rel.hash)

	for {
		select {
		case <-ctx.Done():
			return stop(e)
		case sg := <-sig:
			if sg != syscall.SIGHUP {
				return stop(e)
			}
			rel, cfg, err := loadRelease(configPath)
			if err == nil {
				err = e.Reload(cfg, rel.hash)
			}
			if err != nil {
				e.log.logf(levelEmerg, "reload failed: %s", strings.Join(errorLines(err), "; "))
				continue
			}
			e.log.noticef("config loaded hash=%s", rel.hash)
		}
	}
}

func stop(e *Engine) error {
	e.log.noticef("shutting down (draining up to %s)", e.t.drain)
	ctx, cancel := context.WithTimeout(context.Background(), e.t.drain+5*time.Second)
	defer cancel()
	e.Shutdown(ctx)
	e.log.noticef("exited")
	return nil
}
