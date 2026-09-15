package edge

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

const cliUsage = `usage:
  relay edge run --config <path/to/edge.json>   serve (SIGHUP reloads, SIGTERM/SIGQUIT stop)
  relay edge check <dir | path/to/edge.json>    validate a configuration without binding
`

// RunCLI implements `relay edge <command>`.
func RunCLI(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, cliUsage)
		return errors.New("edge: missing command")
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
	return fmt.Errorf("edge: unknown command %q", args[0])
}

// Check loads and compiles a configuration exactly like `run` would,
// without binding any socket.
func Check(path string) error {
	_, _, err := loadRelease(path)
	return err
}

// loadRelease resolves, loads and compiles path in check mode.
func loadRelease(path string) (release, *Config, error) {
	rel, err := resolveRelease(path)
	if err != nil {
		return rel, nil, err
	}
	cfg, err := Load(rel.path)
	if err != nil {
		return rel, nil, err
	}
	if _, err := compile(cfg, rel.baseDir, compileEnv{}); err != nil {
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
		return errors.New("edge check: expected one path")
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
	fs := flag.NewFlagSet("edge run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to edge.json (resolved through symlinks on every reload)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *configPath == "" {
		fmt.Fprint(stderr, cliUsage)
		return errors.New("edge run: --config is required")
	}
	// Subscribe before starting so a SIGHUP sent right after "started" is
	// never lost.
	sig := make(chan os.Signal, 8)
	signal.Notify(sig, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGINT)
	defer signal.Stop(sig)

	srv, err := NewServer(Options{Stderr: stderr})
	if err != nil {
		return err
	}
	defer srv.Close()
	load := func() (release, *Config, error) {
		rel, err := resolveRelease(*configPath)
		if err != nil {
			return rel, nil, err
		}
		cfg, err := Load(rel.path)
		return rel, cfg, err
	}
	rel, cfg, err := load()
	if err == nil {
		err = srv.Start(cfg, rel.baseDir, rel.hash)
	}
	if err != nil {
		srv.errlog.sync(levelEmerg, "%s", strings.Join(errorLines(err), "; "))
		return err
	}
	srv.errlog.sync(levelNotice, "started hash=%s", rel.hash)

	for {
		select {
		case <-ctx.Done():
			return stop(srv)
		case sg := <-sig:
			if sg != syscall.SIGHUP {
				return stop(srv)
			}
			rel, cfg, err := load()
			if err == nil {
				err = srv.Reload(cfg, rel.baseDir, rel.hash)
			}
			if err != nil {
				srv.errlog.sync(levelEmerg, "reload failed: %s", strings.Join(errorLines(err), "; "))
				continue
			}
			srv.errlog.sync(levelNotice, "config loaded hash=%s", rel.hash)
		}
	}
}

func stop(srv *Server) error {
	srv.errlog.sync(levelNotice, "shutting down (draining up to %s)", drainTimeout)
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	srv.Shutdown(ctx)
	srv.errlog.sync(levelNotice, "exited")
	return nil
}
