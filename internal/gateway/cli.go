package gateway

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
)

const cliUsage = `usage:
  relay gateway run [flags]                 serve the tunnel gateway (SIGTERM/SIGINT stop)
  relay gateway reset [--data-dir <dir>]    forget the paired home so the gateway can be paired again
  relay gateway info [--data-dir <dir>]     print the gateway fingerprint and pairing state
`

// RunCLI implements `relay gateway run|reset|info [flags]`.
//
// run serves until ctx is cancelled, logging to stderr. reset deletes the
// pairing state (the identity is kept); restart a running gateway with a new
// pairing token afterwards. info prints the fingerprint and pairing state.
func RunCLI(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, cliUsage)
		return errors.New("gateway: missing command")
	}
	switch args[0] {
	case "run":
		return runCommand(ctx, args[1:], stderr)
	case "reset":
		return resetCommand(args[1:], stdout, stderr)
	case "info":
		return infoCommand(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, cliUsage)
		return nil
	}
	fmt.Fprint(stderr, cliUsage)
	return fmt.Errorf("gateway: unknown command %q", args[0])
}

// stringList is a repeatable flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

func dataDirFlag(fs *flag.FlagSet) *string {
	def := os.Getenv("RELAY_DATA_DIR")
	if def == "" {
		def = "/data"
	}
	return fs.String("data-dir", def, "data directory (identity and pairing state in <dir>/gateway)")
}

func runCommand(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("gateway run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := dataDirFlag(fs)
	tunnel := fs.String("tunnel", DefaultTunnelAddr, "tunnel listen address (TCP and UDP/QUIC)")
	httpAddr := fs.String("http", DefaultHTTPAddr, `HTTP listen address, routed by Host ("" disables)`)
	httpsAddr := fs.String("https", DefaultHTTPSAddr, `HTTPS listen address, routed by TLS SNI ("" disables)`)
	bind := fs.String("bind", "", "host to bind published TCP ports on (default all interfaces)")
	allow := fs.String("allow-ports", DefaultAllowPorts, "TCP ports the home may publish, e.g. 1024-65535 or 8000-8100,9000 (tunnel port, 22, 80 and 443 are always excluded)")
	token := fs.String("pair-token", "", "one-time pairing token from Relay (default $RELAY_GATEWAY_PAIR_TOKEN); ignored once paired")
	var publicIPs stringList
	fs.Var(&publicIPs, "public-ip", "public IP address announced to the home (repeatable; default: interface addresses)")
	maxConns := fs.Int("max-conns", DefaultMaxConns, "maximum concurrent client connections")
	maxPerIP := fs.Int("max-conns-per-ip", DefaultMaxConnsPerIP, "maximum concurrent client connections per client IP")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		fmt.Fprint(stderr, cliUsage)
		return fmt.Errorf("gateway run: unexpected argument %q", fs.Arg(0))
	}
	if *token == "" {
		*token = os.Getenv("RELAY_GATEWAY_PAIR_TOKEN")
	}

	log := slog.New(slog.NewTextHandler(stderr, nil))
	srv, err := New(Options{
		DataDir:       *dataDir,
		TunnelAddr:    *tunnel,
		HTTPAddr:      *httpAddr,
		HTTPSAddr:     *httpsAddr,
		BindHost:      *bind,
		AllowPorts:    *allow,
		PairToken:     *token,
		PublicIPs:     publicIPs,
		MaxConns:      *maxConns,
		MaxConnsPerIP: *maxPerIP,
		Log:           log,
	})
	if err != nil {
		log.Error("gateway failed to start", "err", err)
		return err
	}
	if err := srv.Start(ctx); err != nil {
		log.Error("gateway failed to start", "err", err)
		return err
	}
	<-ctx.Done()
	return srv.Close()
}

func resetCommand(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("gateway reset", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := dataDirFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	removed, err := deletePairing(stateDir(*dataDir))
	if err != nil {
		return fmt.Errorf("gateway reset: %w", err)
	}
	if !removed {
		fmt.Fprintln(stdout, "the gateway is not paired")
		return nil
	}
	fmt.Fprintln(stdout, "pairing state removed; restart the gateway with a new pairing token to pair it again")
	return nil
}

func infoCommand(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("gateway info", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := dataDirFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := stateDir(*dataDir)
	id, err := loadIdentity(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		fmt.Fprintln(stdout, "fingerprint: none yet (created on the first run)")
	case err != nil:
		return fmt.Errorf("gateway info: %w", err)
	default:
		fmt.Fprintf(stdout, "fingerprint: %s\n", id.Fingerprint())
	}
	p, err := loadPairing(dir)
	if err != nil {
		return fmt.Errorf("gateway info: %w", err)
	}
	if p == nil {
		fmt.Fprintln(stdout, "paired:      no")
		return nil
	}
	fmt.Fprintln(stdout, "paired:      yes")
	fmt.Fprintf(stdout, "home pin:    %s\n", p.HomePin)
	fmt.Fprintf(stdout, "paired at:   %s\n", p.PairedAt.Format(time.RFC3339))
	return nil
}
