package edge

// Config is the complete Relay Edge configuration (edge.json). It is rendered
// by internal/render/edge from a snapshot and is deliberately "resolved": the
// renderer applies Relay's modelling rules (synthesised locations, effective
// access lists, usable certificates, redirect grouping and injection, default
// host selection) so the data plane only has to execute it.
//
// Behaviour must match the nginx configuration Relay renders for the same
// snapshot (see docs/EDGE.md for the parity rules). Relative paths are
// resolved against the directory containing edge.json.
type Config struct {
	Schema int `json:"schema"` // 1
	// Notes are informational lines from the renderer (e.g. features that
	// were skipped). The data plane ignores them.
	Notes []string `json:"notes,omitempty"`

	HTTPPort  int `json:"httpPort"`  // plain HTTP listener (TCP, all interfaces)
	HTTPSPort int `json:"httpsPort"` // TLS listener (TCP) and, when HTTP3, UDP for QUIC
	// HTTP3 opens the QUIC listener on HTTPSPort (any host with HTTP3 and a cert).
	HTTP3 bool `json:"http3"`

	// StatusAddr is a loopback listener with /healthz, /stub_status (nginx
	// format) and /metrics (Prometheus). "" disables it.
	StatusAddr string `json:"statusAddr"`
	// LogDir receives access.log, stream-access.log (JSON, nginx relay_json
	// formats) and error.log (nginx-style lines). "" logs errors to stderr only.
	LogDir string `json:"logDir"`
	// ACMEWebroot serves /.well-known/acme-challenge/<token> from
	// <ACMEWebroot>/.well-known/acme-challenge/<token>.
	ACMEWebroot string `json:"acmeWebroot"`
	// TLSProfile applies to the default server and redirect groups: modern | intermediate | old.
	TLSProfile string `json:"tlsProfile"`
	// AssetCacheMB bounds the in-memory asset cache (0 = 256).
	AssetCacheMB int `json:"assetCacheMb"`

	// Blocklist addresses/CIDRs get 403 on every HTTP server (not streams).
	Blocklist []string `json:"blocklist"`
	// AccessLists by id, referenced from locations.
	AccessLists map[string]AccessList `json:"accessLists"`
	// GeoIPDatabase is a MaxMind DB country database (mmdb) for hosts with
	// AllowCountries. It is re-read when the file changes.
	GeoIPDatabase string `json:"geoipDatabase,omitempty"`
	// ErrorPages replace the plain pages of errors the engine generates
	// (status code → HTML). Responses from upstreams pass through.
	ErrorPages map[string]string `json:"errorPages,omitempty"`

	Default   DefaultServer   `json:"default"`
	Hosts     []Host          `json:"hosts"`     // enabled hosts; for duplicate names the first wins
	Redirects []RedirectGroup `json:"redirects"` // domains not served by a host
	Streams   []Stream        `json:"streams"`
}

// CertRef points at a PEM certificate chain and key on disk.
type CertRef struct {
	ID       string `json:"id"`
	CertFile string `json:"certFile"`
	KeyFile  string `json:"keyFile"`
	// OCSPStapling fetches and staples OCSP responses (custom certificates only).
	OCSPStapling bool `json:"ocspStapling,omitempty"`
}

// AccessList is IP rules plus optional basic auth.
type AccessList struct {
	Name string `json:"name"`
	// Rules are evaluated top to bottom, first match wins, and an address that
	// matches no rule is allowed. The renderer already appends the implicit
	// "deny all" nginx adds and drops rules after an "all" rule.
	Rules     []IPRule   `json:"rules"`
	BasicAuth *BasicAuth `json:"basicAuth,omitempty"`
	// SatisfyAny: IP allow OR valid basic auth OR forward auth passes. Only
	// set when both rules and basic auth exist (nginx "satisfy any").
	SatisfyAny bool `json:"satisfyAny,omitempty"`
}

type BasicAuth struct {
	Realm string `json:"realm"`
	// UsersFile is an htpasswd file relative to edge.json (htpasswd/<id>):
	// "# comment" lines and user:hash lines (bcrypt $2a$/$2b$/$2y$).
	UsersFile string `json:"usersFile"`
}

// Host is one enabled proxy host.
type Host struct {
	ID      string   `json:"id"`
	Domains []string `json:"domains"` // lowercase; "*.example.com" wildcard; IP literals allowed

	// Cert is nil for HTTP-only hosts. With a cert the host is served on the
	// HTTPS port, and also on the HTTP port unless ForceHTTPS.
	Cert *CertRef `json:"cert,omitempty"`
	// ForceHTTPS (with Cert): the HTTP port answers ACME challenges and
	// redirects everything else with 301 to https.
	ForceHTTPS    bool   `json:"forceHttps,omitempty"`
	HTTP2         bool   `json:"http2,omitempty"`
	HTTP3         bool   `json:"http3,omitempty"`         // Alt-Svc + QUIC
	CipherProfile string `json:"cipherProfile,omitempty"` // modern | intermediate | old
	HSTS          string `json:"hsts,omitempty"`          // full Strict-Transport-Security value, "" = none

	BlockExploits bool `json:"blockExploits,omitempty"`
	NoIndex       bool `json:"noIndex,omitempty"`
	// AllowCountries (upper-case ISO codes) answers other public addresses
	// with 403. Local and private addresses are always allowed.
	AllowCountries []string `json:"allowCountries,omitempty"`
	// MaxBodyBytes limits request bodies (413). 0 = unlimited.
	MaxBodyBytes int64 `json:"maxBodyBytes"`
	// ReadTimeoutSec / SendTimeoutSec are the upstream idle read/send timeouts (0 = 60).
	ReadTimeoutSec int `json:"readTimeoutSec,omitempty"`
	SendTimeoutSec int `json:"sendTimeoutSec,omitempty"`

	RateLimit         *RateLimit   `json:"rateLimit,omitempty"`
	UpstreamTLSVerify bool         `json:"upstreamTlsVerify,omitempty"`
	ForwardAuth       *ForwardAuth `json:"forwardAuth,omitempty"`
	// Maintenance answers requests with 503 and its page, except bypassed addresses.
	Maintenance *Maintenance `json:"maintenance,omitempty"`

	// Locations are prefix matches, sorted longest path first; "/" is always present.
	Locations []Location `json:"locations"`
	// PathRedirects (from redirect rules on this host's domains) win over
	// prefix locations but not over the ACME challenge path.
	PathRedirects []PathRedirect `json:"pathRedirects,omitempty"`
}

// RateLimit is per client address: RequestsPerSecond with Burst extra requests
// served immediately; excess requests get 429.
type RateLimit struct {
	RequestsPerSecond int `json:"requestsPerSecond"`
	Burst             int `json:"burst"`
	// Exempt uses longest-prefix match (nginx geo): the most specific matching
	// CIDR decides; ExemptDefault applies when none match.
	Exempt        []ExemptRule `json:"exempt,omitempty"`
	ExemptDefault bool         `json:"exemptDefault,omitempty"`
}

// Maintenance: Bypass uses longest-prefix match like RateLimit.Exempt; an
// Exempt rule bypasses the maintenance page. BypassDefault applies when none match.
type Maintenance struct {
	Page          string       `json:"page"`
	Bypass        []ExemptRule `json:"bypass,omitempty"`
	BypassDefault bool         `json:"bypassDefault,omitempty"`
}

type ExemptRule struct {
	CIDR   string `json:"cidr"`
	Exempt bool   `json:"exempt"`
}

// ForwardAuth asks VerifyURL before serving a request (auth_request).
type ForwardAuth struct {
	VerifyURL        string `json:"verifyUrl"`
	SignInURL        string `json:"signInUrl,omitempty"` // 401 → 302 SignInURL?rd=<original URL>
	PassRemoteUser   bool   `json:"passRemoteUser,omitempty"`
	PassRemoteGroups bool   `json:"passRemoteGroups,omitempty"`
}

// Location is a case-sensitive path prefix rule.
type Location struct {
	Path string `json:"path"`
	Kind string `json:"kind"` // proxy | deny ("same" is resolved to proxy with the host upstream)

	Upstream    Upstream `json:"upstream"`
	StripPrefix bool     `json:"stripPrefix,omitempty"`
	Websockets  bool     `json:"websockets,omitempty"`
	Cache       bool     `json:"cache,omitempty"` // cache static assets for 30 days
	Headers     []Header `json:"headers,omitempty"`

	AccessListID string `json:"accessListId,omitempty"`
	// DenyAll: the referenced access list no longer exists (403 for everyone).
	DenyAll bool `json:"denyAll,omitempty"`
	// ForwardAuth applies the host's forward auth on this path.
	ForwardAuth bool `json:"forwardAuth,omitempty"`
	// SkipMaintenance serves the path during maintenance (the Relay login page).
	SkipMaintenance bool `json:"skipMaintenance,omitempty"`
}

type Upstream struct {
	Scheme string `json:"scheme"` // http | https
	Host   string `json:"host"`
	Port   int    `json:"port"`
	Path   string `json:"path,omitempty"` // base path prefix, e.g. "/grafana"
}

// Header is a request header sent upstream. An empty Value removes the header.
// Values may contain nginx-style variables ($host, $remote_addr, $http_x_y …).
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// PathRedirect matches "^<From>(/.*)?$" against the decoded path.
type PathRedirect struct {
	From     string `json:"from"` // no trailing slash
	To       string `json:"to"`
	Code     int    `json:"code"` // 301 | 302 | 307 | 308
	KeepPath bool   `json:"keepPath,omitempty"`
}

// Redirect is a whole-domain redirect.
type Redirect struct {
	To       string `json:"to"`
	Code     int    `json:"code"`
	KeepPath bool   `json:"keepPath,omitempty"` // append the request URI (path + query)
}

// RedirectGroup serves redirect rules for domains no proxy host serves.
type RedirectGroup struct {
	Domains    []string       `json:"domains"`
	Cert       *CertRef       `json:"cert,omitempty"`
	ForceHTTPS bool           `json:"forceHttps,omitempty"`
	Whole      *Redirect      `json:"whole,omitempty"` // nil: 404 unless a path redirect matches
	Paths      []PathRedirect `json:"paths,omitempty"`
}

// DefaultServer answers requests no host or redirect group claims.
type DefaultServer struct {
	Action     string `json:"action"` // close | 404 | redirect | host
	RedirectTo string `json:"redirectTo,omitempty"`
	// Host is served for action "host" (without HSTS and Alt-Svc).
	Host *Host `json:"host,omitempty"`
	// Cert is presented for unknown server names (DefaultHost cert or the
	// self-signed placeholder).
	Cert *CertRef `json:"cert,omitempty"`
	// HTTP3 announces QUIC on the default server (any HTTP/3 host exists).
	HTTP3 bool `json:"http3,omitempty"`
}

// Stream forwards TCP and/or UDP ports.
type Stream struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	TCP        bool   `json:"tcp"`
	UDP        bool   `json:"udp"`
	ListenAddr string `json:"listenAddr,omitempty"` // "" = all interfaces
	ListenLo   int    `json:"listenLo"`
	ListenHi   int    `json:"listenHi"`
	// ForwardLo == ForwardHi: every listen port goes to that port; otherwise
	// port p goes to ForwardLo + (p - ListenLo).
	ForwardHost      string `json:"forwardHost"`
	ForwardLo        int    `json:"forwardLo"`
	ForwardHi        int    `json:"forwardHi"`
	ProxyProtocol    bool   `json:"proxyProtocol,omitempty"` // send PROXY v1 (TCP)
	IdleTimeoutMs    int    `json:"idleTimeoutMs"`           // 0 = 600000
	ConnectTimeoutMs int    `json:"connectTimeoutMs"`        // 0 = 10000
}
