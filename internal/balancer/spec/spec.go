// Package spec is the Relay Balancer configuration schema (balancer.json).
//
// It is the contract between the renderer (internal/render/balancer), which
// resolves Relay's backends, frontends and load-balancer settings into this
// form, and the data plane (internal/balancer), which only executes it.
// Behaviour must match the haproxy.cfg Relay renders for the same snapshot
// (see docs/BALANCER.md for the parity rules).
package spec

import (
	"encoding/json"
	"fmt"
	"os"
)

// SchemaVersion is the current Config.Schema.
const SchemaVersion = 1

// Engine modes.
const (
	ModeHTTP = "http"
	ModeTCP  = "tcp"
)

// Config is the complete Relay Balancer configuration.
type Config struct {
	Schema int `json:"schema"`
	// Notes are informational lines from the renderer; the data plane ignores them.
	Notes []string `json:"notes,omitempty"`

	// RuntimeSocket is a unix socket that answers the subset of the HAProxy
	// runtime API Relay uses, in HAProxy's output format:
	//   show stat | show info |
	//   set server <backend>/<server> state ready|drain|maint |
	//   set server <backend>/<server> weight <n>
	// "" disables it.
	RuntimeSocket string `json:"runtimeSocket"`

	// MaxConn caps concurrent client connections across all frontends
	// (HAProxy global maxconn). 0 = unlimited.
	MaxConn int `json:"maxConn"`
	// Timeouts are the defaults for every frontend and backend.
	Timeouts Timeouts `json:"timeouts"`
	// Check holds the default health check interval/rise/fall.
	Check CheckDefaults `json:"check"`
	// CAFile verifies TLS servers of backends with TLSVerify ("" = system roots).
	CAFile string `json:"caFile,omitempty"`

	// Stats is the optional stats listener (nil = off).
	Stats *Stats `json:"stats,omitempty"`

	Frontends []Frontend `json:"frontends"` // enabled frontends only
	Backends  []Backend  `json:"backends"`  // every backend, used or not
}

// Timeouts in milliseconds. 0 inherits (backend → global → built-in default).
type Timeouts struct {
	ConnectMs int64 `json:"connectMs,omitempty"`
	ClientMs  int64 `json:"clientMs,omitempty"`
	ServerMs  int64 `json:"serverMs,omitempty"`
	QueueMs   int64 `json:"queueMs,omitempty"`
}

// CheckDefaults apply to health checks that don't set their own values.
type CheckDefaults struct {
	IntervalMs int64 `json:"intervalMs"`
	Rise       int   `json:"rise"`
	Fall       int   `json:"fall"`
}

// Stats is an HTTP listener with a stats page ("/"), CSV ("/;csv") and,
// when Prometheus is set, "/metrics".
type Stats struct {
	Bind       string `json:"bind"` // host:port
	Prometheus bool   `json:"prometheus"`
	// Access rules are evaluated top to bottom, first match wins; a client
	// that matches no rule is allowed. Denied clients get 403.
	Access []IPRule `json:"access,omitempty"`
}

// IPRule allows or denies a CIDR, a single IP or "all".
type IPRule struct {
	Allow bool   `json:"allow"`
	CIDR  string `json:"cidr"`
}

// Frontend is one listener.
type Frontend struct {
	ID   string `json:"id"`
	Name string `json:"name"` // pxname in stats and logs
	Mode string `json:"mode"` // http | tcp
	Bind string `json:"bind"` // host:port

	// AcceptProxy expects the PROXY protocol (v1 or v2) from clients.
	AcceptProxy bool `json:"acceptProxy,omitempty"`
	// ForwardFor adds X-Forwarded-For with the client address (http) unless
	// the client matches ForwardForExcept (CIDRs).
	ForwardFor       bool     `json:"forwardFor,omitempty"`
	ForwardForExcept []string `json:"forwardForExcept,omitempty"`
	// Compression gzips compressible responses (http).
	Compression bool `json:"compression,omitempty"`
	// InspectDelayMs waits up to this long for a TLS ClientHello before
	// evaluating rules (tcp frontends with SNI rules).
	InspectDelayMs int64 `json:"inspectDelayMs,omitempty"`

	// Rules are evaluated in order; the first rule whose conditions all
	// match picks the backend.
	Rules []Rule `json:"rules,omitempty"`
	// DefaultBackend is the backend ID used when no rule matches. Without
	// one, http answers 503 and tcp closes the connection.
	DefaultBackend string `json:"defaultBackend,omitempty"`

	Timeouts Timeouts `json:"timeouts,omitempty"`
}

// Rule routes to a backend when all conditions match.
type Rule struct {
	Conditions []Condition `json:"conditions"`
	Backend    string      `json:"backend"` // backend ID
}

// Condition types.
const (
	CondHost    = "host"     // Host header without port; Match exact|suffix; case-insensitive
	CondPathBeg = "path_beg" // path prefix
	CondPath    = "path"     // exact path
	CondPathReg = "path_reg" // Go (RE2) regular expression on the path
	CondHeader  = "header"   // Name; Match found|exact (case-sensitive values)
	CondSrc     = "src"      // client address in any CIDR/IP of Values
	CondSNI     = "sni"      // TLS SNI (tcp); Match exact|suffix; case-insensitive
)

// Match kinds.
const (
	MatchExact  = "exact"
	MatchSuffix = "suffix"
	MatchFound  = "found"
)

// Condition is one ACL test. Any of Values matching counts as a match;
// Negate inverts the result.
type Condition struct {
	Type   string   `json:"type"`
	Name   string   `json:"name,omitempty"`
	Match  string   `json:"match,omitempty"`
	Values []string `json:"values,omitempty"`
	Negate bool     `json:"negate,omitempty"`
}

// Algorithms.
const (
	AlgoRoundRobin = "roundrobin"
	AlgoStaticRR   = "static-rr"
	AlgoLeastConn  = "leastconn"
	AlgoSource     = "source"
	AlgoURI        = "uri"
	AlgoRandom     = "random"
	AlgoFirst      = "first"
)

// Backend is a pool of servers.
type Backend struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"` // pxname in stats, runtime commands and logs
	Mode      string   `json:"mode"` // http | tcp
	Algorithm string   `json:"algorithm"`
	Servers   []Server `json:"servers"`

	// Check is the health check for servers with Check set (nil = no checks).
	Check *HealthCheck `json:"check,omitempty"`
	// Sticky keeps clients on the same server (nil = off).
	Sticky *Sticky `json:"sticky,omitempty"`

	// ForwardFor adds X-Forwarded-For (http), like HAProxy's backend
	// "option forwardfor".
	ForwardFor bool `json:"forwardFor,omitempty"`
	// SendProxy sends a PROXY protocol v2 header to servers.
	SendProxy bool `json:"sendProxy,omitempty"`
	// TLS connects to servers over TLS; TLSVerify checks the chain against
	// Config.CAFile (not the host name, like HAProxy "verify required").
	TLS       bool `json:"tls,omitempty"`
	TLSVerify bool `json:"tlsVerify,omitempty"`
	// Retries is how many times a failed connection is retried; with
	// Redispatch the last retry may go to another server.
	Retries    int  `json:"retries"`
	Redispatch bool `json:"redispatch,omitempty"`

	Timeouts Timeouts `json:"timeouts,omitempty"`
}

// Server admin states.
const (
	StateReady = "ready"
	StateDrain = "drain"
	StateMaint = "maint"
)

// Server is one member of a backend.
type Server struct {
	ID      string `json:"id"`
	Name    string `json:"name"` // svname in stats, runtime commands and logs
	Address string `json:"address"`
	Port    int    `json:"port"`
	Weight  int    `json:"weight"` // 0–256
	Backup  bool   `json:"backup,omitempty"`
	Check   bool   `json:"check,omitempty"`
	// State is the admin state after (re)load: ready | drain | maint.
	State string `json:"state"`
	// Cookie is the sticky cookie value identifying this server (insert/prefix).
	Cookie string `json:"cookie,omitempty"`
}

// Health check types.
const (
	CheckTCP   = "tcp"
	CheckHTTP  = "http"
	CheckPgSQL = "pgsql"
	CheckMySQL = "mysql"
	CheckRedis = "redis"
)

// HealthCheck describes how servers are probed.
type HealthCheck struct {
	Type string `json:"type"`
	// http
	Method string `json:"method,omitempty"`
	Path   string `json:"path,omitempty"`
	Host   string `json:"host,omitempty"`
	// Expect is the accepted status: "200", "2xx" or a range "200-399".
	Expect string `json:"expect,omitempty"`
	// pgsql
	User string `json:"user,omitempty"`

	IntervalMs int64 `json:"intervalMs,omitempty"`
	Rise       int   `json:"rise,omitempty"`
	Fall       int   `json:"fall,omitempty"`
}

// Sticky session modes.
const (
	StickyInsert = "insert" // set a cookie naming the server
	StickyPrefix = "prefix" // prefix the application's cookie with the server
	StickySource = "source" // remember client address → server
)

// Sticky keeps a client on one server.
type Sticky struct {
	Mode   string `json:"mode"`
	Cookie string `json:"cookie,omitempty"`
	// ExpireMs and TableSize bound the source table (HAProxy stick-table).
	ExpireMs  int64 `json:"expireMs,omitempty"`
	TableSize int   `json:"tableSize,omitempty"`
}

// Load reads a configuration file. Unknown fields are ignored so older data
// planes accept newer renderers' optional fields.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}
