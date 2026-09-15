// Package agent is the control plane between the relay app and the nginx /
// haproxy containers. Each engine container runs `relay agent --engine X` as
// PID 1: it supervises the engine process and serves this protocol as HTTP
// over a unix socket in the shared /run/relay volume.
//
// The relay container never touches engine config files directly; it sends
// rendered files to the agent, which validates, atomically swaps and reloads.
package agent

import "time"

const (
	EngineNginx   = "nginx"
	EngineHAProxy = "haproxy"
)

// SocketPath returns the agent socket for an engine inside runDir.
func SocketPath(runDir, engine string) string { return runDir + "/" + engine + ".sock" }

// Files maps a path relative to the engine's config root to file content.
// nginx: "nginx.conf", "conf.d/hosts/<id>.conf", "htpasswd/<id>", …
// haproxy: "haproxy.cfg".
type Files map[string]string

// Endpoints (all JSON):
//
//	GET  /v1/status              → Status
//	POST /v1/validate            ValidateRequest → ValidateResponse
//	POST /v1/apply               ApplyRequest → ApplyResponse (validate + swap + reload/start)
//	POST /v1/rollback            → ApplyResponse (re-activate the previous release)
//	POST /v1/start | /v1/stop | /v1/reload → ActionResponse
//	GET  /v1/logs?since=<unix-ms>&limit=N → LogsResponse (engine stdout/stderr)
//	GET  /v1/listeners           → ListenersResponse (sockets bound on the host)
//	POST /v1/runtime             RuntimeRequest → RuntimeResponse (haproxy runtime API)
const (
	PathStatus    = "/v1/status"
	PathValidate  = "/v1/validate"
	PathApply     = "/v1/apply"
	PathRollback  = "/v1/rollback"
	PathStart     = "/v1/start"
	PathStop      = "/v1/stop"
	PathReload    = "/v1/reload"
	PathLogs      = "/v1/logs"
	PathListeners = "/v1/listeners"
	PathRuntime   = "/v1/runtime"
)

type Status struct {
	Engine       string     `json:"engine"`
	Running      bool       `json:"running"`
	PID          int        `json:"pid"`
	Version      string     `json:"version"`
	Modules      []string   `json:"modules"` // nginx: http_v3, stream, geoip2, auth_request …
	StartedAt    *time.Time `json:"startedAt,omitempty"`
	ExitedAt     *time.Time `json:"exitedAt,omitempty"`
	ExitError    string     `json:"exitError,omitempty"`
	LastReloadAt *time.Time `json:"lastReloadAt,omitempty"`
	LastReloadMs int64      `json:"lastReloadMs"`
	ConfigHash   string     `json:"configHash"` // hash of the active release
	ConfigLines  int        `json:"configLines"`
	Configured   bool       `json:"configured"` // a release has been applied
}

type ValidateRequest struct {
	Files Files `json:"files"`
}

type ValidateResponse struct {
	OK         bool   `json:"ok"`
	Output     string `json:"output"`
	DurationMs int64  `json:"durationMs"`
}

type ApplyRequest struct {
	Files Files  `json:"files"`
	Hash  string `json:"hash"`
	// Stop the engine instead of reloading when true (e.g. haproxy with no backends).
	Stop bool `json:"stop"`
}

type ApplyResponse struct {
	OK           bool   `json:"ok"`
	Stage        string `json:"stage"` // validate | swap | reload | start
	Output       string `json:"output"`
	ValidateMs   int64  `json:"validateMs"`
	ReloadMs     int64  `json:"reloadMs"`
	PreviousHash string `json:"previousHash"`
	Running      bool   `json:"running"`
}

type ActionResponse struct {
	OK     bool   `json:"ok"`
	Output string `json:"output"`
}

type LogLine struct {
	At     time.Time `json:"at"`
	Stream string    `json:"stream"` // stdout | stderr
	Text   string    `json:"text"`
}

type LogsResponse struct {
	Lines []LogLine `json:"lines"`
}

type Listener struct {
	Proto   string `json:"proto"` // tcp | udp
	Address string `json:"address"`
	Port    int    `json:"port"`
	Process string `json:"process,omitempty"`
}

type ListenersResponse struct {
	Listeners []Listener `json:"listeners"`
}

type RuntimeRequest struct {
	Command string `json:"command"`
}

type RuntimeResponse struct {
	Output string `json:"output"`
}
