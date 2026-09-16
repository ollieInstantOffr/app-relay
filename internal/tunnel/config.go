// Package tunnel is the home side of Relay tunnels: the data plane run by the
// relay-tunnel engine container (`relay tunnel run`). It dials every paired
// gateway, accepts the streams the gateway opens for its clients and hands
// them to the proxy engine's tunnel ingress sockets with a PROXY protocol
// header.
//
// Configuration comes in two files:
//
//	tunnel.json    the release (versioned with the proxy config): what is
//	               published through which gateway and where it goes
//	gateways.json  runtime state written by Relay (like certificate files):
//	               gateway addresses, pins and key files; re-read on SIGHUP
package tunnel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/instantoffr/relay/internal/tunnel/wire"
)

// File and socket names.
const (
	ConfigFile        = "tunnel.json"
	GatewaysFileName  = "gateways.json"       // in <data>/tunnel
	RuntimeSocketName = "tunnel-runtime.sock" // in the run directory
)

// Config is tunnel.json.
type Config struct {
	Schema int `json:"schema"` // 1
	// GatewaysFile is the runtime gateway list (absolute path).
	GatewaysFile string `json:"gatewaysFile"`
	// RuntimeSocket answers "status" with Status as JSON (absolute path).
	RuntimeSocket string `json:"runtimeSocket"`
	// Targets are the proxy engine's tunnel ingress sockets for hosts.
	Targets Targets `json:"targets"`
	// Routes lists what each gateway publishes.
	Routes []Route `json:"routes"`
}

// Targets are unix sockets requiring a PROXY protocol header.
type Targets struct {
	HTTP  string `json:"http"`
	HTTPS string `json:"https"`
}

// Route is what one gateway publishes.
type Route struct {
	GatewayID string `json:"gatewayId"`
	// Names are published host names ("*.example.com" wildcards allowed);
	// they are accepted on the gateway's ports 80 and 443.
	Names []string   `json:"names"`
	TCP   []TCPRoute `json:"tcp"`
}

// TCPRoute is a published TCP stream port.
type TCPRoute struct {
	Port   uint16 `json:"port"`
	Socket string `json:"socket"` // proxy engine stream ingress
}

// Gateways is gateways.json.
type Gateways struct {
	Schema   int       `json:"schema"` // 1
	Gateways []Gateway `json:"gateways"`
}

// Gateway is one gateway to dial.
type Gateway struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Address   string `json:"address"`   // host:port
	Transport string `json:"transport"` // auto | quic | tcp
	Enabled   bool   `json:"enabled"`
	// Pin is the gateway key fingerprint; empty = not paired (not dialed).
	Pin      string `json:"pin"`
	CertFile string `json:"certFile"` // this Relay's identity for the gateway
	KeyFile  string `json:"keyFile"`
}

// LoadConfig reads and validates tunnel.json (path may be the release directory).
func LoadConfig(path string) (*Config, error) {
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		path = filepath.Join(path, ConfigFile)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

// Validate checks the static rules of a release.
func (c *Config) Validate() error {
	var errs []error
	if c.Schema != 1 {
		errs = append(errs, fmt.Errorf("unsupported schema %d (expected 1)", c.Schema))
	}
	if !filepath.IsAbs(c.GatewaysFile) {
		errs = append(errs, fmt.Errorf("gatewaysFile %q must be an absolute path", c.GatewaysFile))
	}
	if c.RuntimeSocket != "" && !filepath.IsAbs(c.RuntimeSocket) {
		errs = append(errs, fmt.Errorf("runtimeSocket %q must be an absolute path", c.RuntimeSocket))
	}
	seen := map[string]bool{}
	ports := map[uint16]string{}
	for _, r := range c.Routes {
		if r.GatewayID == "" {
			errs = append(errs, errors.New("route without gatewayId"))
			continue
		}
		if seen[r.GatewayID] {
			errs = append(errs, fmt.Errorf("gateway %s: duplicate route", r.GatewayID))
		}
		seen[r.GatewayID] = true
		if len(r.Names) > 0 && (c.Targets.HTTP == "" || c.Targets.HTTPS == "") {
			errs = append(errs, fmt.Errorf("gateway %s publishes hosts but the http/https targets are missing", r.GatewayID))
		}
		for _, n := range r.Names {
			if wire.NormalizeName(n) == "" || len(n) > wire.MaxNameLen {
				errs = append(errs, fmt.Errorf("gateway %s: invalid name %q", r.GatewayID, n))
			}
		}
		for _, t := range r.TCP {
			switch {
			case t.Port == 0:
				errs = append(errs, fmt.Errorf("gateway %s: TCP port 0", r.GatewayID))
			case t.Socket == "" || !filepath.IsAbs(t.Socket):
				errs = append(errs, fmt.Errorf("gateway %s: TCP port %d needs an absolute socket path", r.GatewayID, t.Port))
			}
			if other, dup := ports[t.Port]; dup && other == r.GatewayID {
				errs = append(errs, fmt.Errorf("gateway %s: TCP port %d published twice", r.GatewayID, t.Port))
			}
			ports[t.Port] = r.GatewayID
		}
	}
	return errors.Join(errs...)
}

// LoadGateways reads gateways.json; a missing file means no gateways.
func LoadGateways(path string) (*Gateways, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Gateways{Schema: 1}, nil
	}
	if err != nil {
		return nil, err
	}
	var g Gateways
	if err := json.Unmarshal(data, &g); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if g.Schema != 1 {
		return nil, fmt.Errorf("%s: unsupported schema %d (expected 1)", path, g.Schema)
	}
	return &g, nil
}

// ---------------------------------------------------------------- status

// Gateway connection states.
const (
	StateDisabled     = "disabled"     // gateway disabled in Relay
	StateUnpaired     = "unpaired"     // no pin yet
	StateIdle         = "idle"         // nothing published through it
	StateConnecting   = "connecting"   // dialing / handshaking
	StateConnected    = "connected"    // session up, routes pushed
	StateDisconnected = "disconnected" // lost; retrying (see LastError)
	StateIncompatible = "incompatible" // protocol versions don't overlap
)

// Status is the runtime status (answer to "status" on the runtime socket).
type Status struct {
	Hash      string          `json:"hash"` // active release
	StartedAt time.Time       `json:"startedAt"`
	Gateways  []GatewayStatus `json:"gateways"`
}

// GatewayStatus is one gateway's connection and traffic.
type GatewayStatus struct {
	ID          string     `json:"id"`
	State       string     `json:"state"`
	Transport   string     `json:"transport,omitempty"` // quic | tcp while connected
	Address     string     `json:"address"`
	ConnectedAt *time.Time `json:"connectedAt,omitempty"`
	LastError   string     `json:"lastError,omitempty"`
	LastErrorAt *time.Time `json:"lastErrorAt,omitempty"`
	Reconnects  uint64     `json:"reconnects"`

	RTTMs     float64  `json:"rttMs"`
	Version   string   `json:"version,omitempty"` // gateway build
	Proto     int      `json:"proto,omitempty"`
	PublicIPs []string `json:"publicIps,omitempty"`

	Generation      uint64           `json:"generation"`      // routes pushed
	AckedGeneration uint64           `json:"ackedGeneration"` // routes confirmed by the gateway
	PortErrors      []wire.PortError `json:"portErrors,omitempty"`

	ActiveStreams int64  `json:"activeStreams"`
	Streams       uint64 `json:"streams"`  // accepted since start
	Rejected      uint64 `json:"rejected"` // streams for names/ports not published here
	BytesIn       uint64 `json:"bytesIn"`  // client → home
	BytesOut      uint64 `json:"bytesOut"` // home → client

	GatewayStats *wire.Stats `json:"gatewayStats,omitempty"`
}
