package core

// Cross-slice service contracts. The methods declared here are relied on by
// other feature areas (API handlers, MCP tools, the apply pipeline) and must
// keep these signatures. Owners may add methods, but add them in their own
// core/<area>_ext.go file to avoid merge conflicts.

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/instantoffr/relay/internal/agent"
	"github.com/instantoffr/relay/internal/model"
)

// ---------------------------------------------------------------- auth (slice: auth)

var (
	ErrUnauthenticated = errors.New("unauthenticated")
	ErrForbidden       = errors.New("forbidden")
)

type Auth interface {
	Service
	// Authenticate resolves a session cookie or `Authorization: Bearer` REST
	// token. Returns ErrUnauthenticated when neither is valid.
	Authenticate(r *http.Request) (Actor, error)
	// AuthenticateToken validates a raw API token for a surface ("mcp" | "rest").
	AuthenticateToken(ctx context.Context, raw, surface string) (Actor, error)
}

// ---------------------------------------------------------------- engine (slice: engine)

type PendingItem struct {
	Kind   string `json:"kind"` // model.Kind* or "settings"
	ID     string `json:"id"`
	Name   string `json:"name"`
	Action string `json:"action"` // created | updated | deleted
}

type Pending struct {
	Count       int           `json:"count"`
	Items       []PendingItem `json:"items"`
	LiveVersion int64         `json:"liveVersion"`
}

type ApplyOptions struct {
	Summary string `json:"summary"`
}

type Version struct {
	ID           int64         `json:"id"`
	CreatedAt    time.Time     `json:"createdAt"`
	Actor        string        `json:"actor"`
	Summary      string        `json:"summary"`
	Status       string        `json:"status"` // live | superseded | rolled_back | failed | draft
	Error        string        `json:"error,omitempty"`
	ValidateMs   int64         `json:"validateMs"`
	ReloadMs     int64         `json:"reloadMs"`
	RolledBackTo *int64        `json:"rolledBackTo,omitempty"`
	Changes      []PendingItem `json:"changes"`
}

type EngineState struct {
	agent.Status
	Reachable bool   `json:"reachable"`
	Error     string `json:"error,omitempty"`
	// Container is the engine container's state when the agent is
	// unreachable: running | stopped | missing ("" = not checked).
	Container string `json:"container,omitempty"`
	// Standby: the container is stopped on purpose because the engine isn't
	// needed (not the selected proxy engine, or HAProxy stopped or idle).
	Standby bool `json:"standby,omitempty"`
}

type EnginesStatus struct {
	Nginx   EngineState `json:"nginx"`
	HAProxy EngineState `json:"haproxy"`
	Edge    EngineState `json:"edge"`
	// Proxy is the active proxy engine: nginx | edge.
	Proxy string `json:"proxy"`
}

// EngineContainers starts and stops engine containers (internal/engines).
type EngineContainers interface {
	// ContainerState is "running", "stopped", "missing" or "" (Docker unavailable).
	ContainerState(ctx context.Context, engine string) string
	// StartContainer starts a stopped engine container and waits for its agent.
	StartContainer(ctx context.Context, engine string) error
	// StopContainer stops an engine container; reason goes to the activity feed.
	StopContainer(ctx context.Context, engine, reason string) error
}

type Engine interface {
	Service
	Pending(ctx context.Context) (*Pending, error)
	// Apply renders, validates, swaps, reloads and health-checks the current
	// configuration as a new version, rolling back automatically on failure.
	Apply(ctx context.Context, opts ApplyOptions) (*Version, error)
	Status(ctx context.Context) (*EnginesStatus, error)
}

// ---------------------------------------------------------------- load balancer (slice: lb)

type ServerStats struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Address     string `json:"address"` // ip:port
	Status      string `json:"status"`  // UP | DOWN | DRAIN | MAINT | NOLB | NOCHECK
	CheckDetail string `json:"checkDetail,omitempty"`
	Role        string `json:"role"`
	Weight      int    `json:"weight"`
	SharePct    int    `json:"sharePct"`
	SessRate    int64  `json:"sessRate"`
	Current     int64  `json:"current"`
	Max         int64  `json:"max"`
	Queue       int64  `json:"queue"`
	Errors      int64  `json:"errors"`
	RespAvgMs   int64  `json:"respAvgMs"`
	RespP95Ms   int64  `json:"respP95Ms"`
	UptimeSec   int64  `json:"uptimeSec"`
	DownSec     int64  `json:"downSec"`
}

type BackendStats struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Status    string        `json:"status"` // UP | DEGRADED | DOWN
	SessRate  int64         `json:"sessRate"`
	Current   int64         `json:"current"`
	Max       int64         `json:"max"`
	Queue     int64         `json:"queue"`
	Errors    int64         `json:"errors"`
	RespP95Ms int64         `json:"respP95Ms"`
	UptimeSec int64         `json:"uptimeSec"`
	Servers   []ServerStats `json:"servers"`
}

type FrontendStats struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	SessRate int64  `json:"sessRate"`
	Current  int64  `json:"current"`
}

type LBStats struct {
	Running      bool            `json:"running"`
	SessRate     int64           `json:"sessRate"`
	PeakSessRate int64           `json:"peakSessRate"`
	Queue        int64           `json:"queue"`
	Retries      int64           `json:"retries"`
	Redispatches int64           `json:"redispatches"`
	ConnErrors   int64           `json:"connErrors"`
	RespErrors   int64           `json:"respErrors"`
	ReqErrors    int64           `json:"reqErrors"`
	Backends     []BackendStats  `json:"backends"`
	Frontends    []FrontendStats `json:"frontends"`
}

type LoadBalancer interface {
	Service
	Stats(ctx context.Context) (*LBStats, error)
	// SetServerState changes a server's admin state at runtime and persists
	// it on the backend: model.ServerStateReady | Drain | Maint.
	SetServerState(ctx context.Context, backendID, serverID, state string) error
}

// ---------------------------------------------------------------- certificates (slice: certs)

type CertRequest struct {
	Domains       []string `json:"domains"`
	Challenge     string   `json:"challenge"`
	DNSProviderID string   `json:"dnsProviderId,omitempty"`
	Staging       bool     `json:"staging"`
	AutoRenew     bool     `json:"autoRenew"`
}

type Certs interface {
	Service
	// Request creates a pending certificate and issues it in the background.
	Request(ctx context.Context, req CertRequest) (*model.Certificate, error)
	Renew(ctx context.Context, id string) error
}

// ---------------------------------------------------------------- logs & health (slice: observe)

type AccessQuery struct {
	HostID   string    `json:"hostId"`
	Host     string    `json:"host"`
	Status   string    `json:"status"` // "502", ">=500", "4xx"
	ClientIP string    `json:"clientIp"`
	Method   string    `json:"method"`
	Search   string    `json:"search"` // path, ua, ip…
	Kind     string    `json:"kind"`   // http | stream
	Since    time.Time `json:"since"`
	Until    time.Time `json:"until"`
	BeforeID int64     `json:"beforeId"`
	Limit    int       `json:"limit"`
}

type AccessEntry struct {
	ID                   int64             `json:"id"`
	TS                   time.Time         `json:"ts"`
	Kind                 string            `json:"kind"`
	HostID               string            `json:"hostId"`
	Host                 string            `json:"host"`
	Method               string            `json:"method"`
	Path                 string            `json:"path"`
	Protocol             string            `json:"protocol"`
	Status               int               `json:"status"`
	ClientIP             string            `json:"clientIp"`
	UpstreamAddr         string            `json:"upstreamAddr"`
	UpstreamStatus       string            `json:"upstreamStatus"`
	RequestTime          float64           `json:"requestTime"`
	UpstreamConnectTime  *float64          `json:"upstreamConnectTime,omitempty"`
	UpstreamHeaderTime   *float64          `json:"upstreamHeaderTime,omitempty"`
	UpstreamResponseTime *float64          `json:"upstreamResponseTime,omitempty"`
	BytesSent            int64             `json:"bytesSent"`
	BytesReceived        int64             `json:"bytesReceived"`
	UserAgent            string            `json:"userAgent"`
	Referer              string            `json:"referer"`
	RequestID            string            `json:"requestId"`
	SSLProtocol          string            `json:"sslProtocol"`
	Extra                map[string]string `json:"extra"`
}

type AccessPage struct {
	Entries      []AccessEntry `json:"entries"`
	NextBeforeID int64         `json:"nextBeforeId"`
}

type Logs interface {
	Service
	QueryAccess(ctx context.Context, q AccessQuery) (*AccessPage, error)
}

const (
	HealthHealthy  = "healthy"
	HealthDegraded = "degraded"
	HealthDown     = "down"
	HealthDisabled = "disabled"
	HealthUnknown  = "unknown"
)

type HealthStatus struct {
	Target     string    `json:"target"` // host:<id> | stream:<id>
	Status     string    `json:"status"`
	LatencyMs  int64     `json:"latencyMs"`
	HTTPStatus int       `json:"httpStatus,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	CheckedAt  time.Time `json:"checkedAt"`
	ChangedAt  time.Time `json:"changedAt"`
}

func HostTarget(id string) string   { return "host:" + id }
func StreamTarget(id string) string { return "stream:" + id }

type Health interface {
	Service
	Get(target string) (HealthStatus, bool)
	All() map[string]HealthStatus
	// Probe checks an upstream right now ("Upstream reachable · 200 OK · 12 ms").
	Probe(ctx context.Context, u model.Upstream) HealthStatus
}

// ---------------------------------------------------------------- docker (slice: ops)

type ContainerPort struct {
	Private int    `json:"private"`
	Public  int    `json:"public,omitempty"`
	Proto   string `json:"proto"`
}

type Container struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Image         string            `json:"image"`
	State         string            `json:"state"`
	IP            string            `json:"ip"`
	Ports         []ContainerPort   `json:"ports"`
	Labels        map[string]string `json:"labels"`
	HTTP          bool              `json:"http"`                // looks like a web app
	SuggestedPort int               `json:"suggestedPort"`       // best HTTP port guess
	HostID        string            `json:"hostId,omitempty"`    // already proxied by this host
	BackendID     string            `json:"backendId,omitempty"` // member of this backend
	Reason        string            `json:"reason,omitempty"`    // why not suggested ("not HTTP (6379)")
	EndpointID    string            `json:"endpointId"`          // Docker host the container runs on
	EndpointName  string            `json:"endpointName"`
	UpstreamHost  string            `json:"upstreamHost,omitempty"` // address to proxy to (with SuggestedPort); "" = not reachable
	// CandidatePorts are likely app ports usable with UpstreamHost (container
	// ports for container IPs / host networking, published ports otherwise), best first.
	CandidatePorts []int `json:"candidatePorts"`
	// LinkOnStart: stopped local container without an address yet; a host can be
	// created disabled and is linked + enabled when the container starts.
	LinkOnStart bool `json:"linkOnStart,omitempty"`
}

type Docker interface {
	Service
	Containers(ctx context.Context) ([]Container, error)
}

// ---------------------------------------------------------------- notifications (slice: ops)

type Notification struct {
	Event   string `json:"event"` // model.Event*
	Level   string `json:"level"` // info | warn | error
	Title   string `json:"title"`
	Message string `json:"message"`
	URL     string `json:"url,omitempty"`
}

type Notifier interface {
	Service
	// Notify routes an event to the configured channels (non-blocking).
	Notify(ctx context.Context, n Notification)
}

// ---------------------------------------------------------------- backups & import (slice: ops)

type Backups interface{ Service }

type Importer interface{}

// ---------------------------------------------------------------- MCP (slice: mcp)

type MCP interface {
	Service
	// Handler serves the MCP streamable-HTTP endpoint at /mcp (auth included).
	Handler() http.Handler
}

// ---------------------------------------------------------------- geo-blocking (internal/geoip)

// GeoIP keeps the IP → country database used for geo-blocking.
type GeoIP interface {
	Service
	// Database is the country database path ("" when there is none yet).
	Database() string
	// Ensure downloads the database when there is none.
	Ensure(ctx context.Context) error
	// CountryFile returns the nginx geo include with the networks of countries.
	CountryFile(ctx context.Context, countries []string) (string, error)
}
