// Package model holds Relay's configuration entities. These structs are the
// source of truth for the database documents, the REST API (JSON) and the
// config renderers. Keep web/src/lib/types.ts in sync when changing them.
package model

import "time"

// Meta is embedded in every configuration entity.
type Meta struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (m *Meta) GetMeta() *Meta { return m }

// Entity is implemented by pointers to every config entity (via embedded Meta).
type Entity interface{ GetMeta() *Meta }

// Kinds of configuration entities (also the SQLite table names).
const (
	KindHost        = "hosts"
	KindRedirect    = "redirects"
	KindStream      = "streams"
	KindAccessList  = "access_lists"
	KindCertificate = "certificates"
	KindDNSProvider = "dns_providers"
	KindBackend     = "backends"
	KindFrontend    = "frontends"
)

// Source records who created an entity.
const (
	SourceManual = "manual"
	SourceDocker = "docker"
	SourceExpose = "expose"
	SourceImport = "import"
	SourceMCP    = "mcp"
)

// ---------------------------------------------------------------- proxy hosts

type Upstream struct {
	Scheme string `json:"scheme"` // http | https
	Host   string `json:"host"`
	Port   int    `json:"port"`
	Path   string `json:"path,omitempty"`
	// BackendID routes the host through a HAProxy backend (via its localhost
	// frontend) instead of Host/Port. Set by the Expose wizard.
	BackendID string `json:"backendId,omitempty"`
}

type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

const (
	LocationProxy = "proxy" // proxy to Upstream
	LocationSame  = "same"  // host's default upstream, optionally without auth
	LocationDeny  = "deny"  // 403
)

type Location struct {
	ID           string   `json:"id"`
	Path         string   `json:"path"`
	Kind         string   `json:"kind"` // proxy | same | deny
	Upstream     Upstream `json:"upstream"`
	Websockets   bool     `json:"websockets"`
	StripPrefix  bool     `json:"stripPrefix"`
	Cache        bool     `json:"cache"`
	AccessListID string   `json:"accessListId,omitempty"` // "" inherit host list
	NoAuth       bool     `json:"noAuth"`                 // skip access list + forward auth
	Headers      []Header `json:"headers"`
}

type ForwardAuth struct {
	Enabled          bool   `json:"enabled"`
	Provider         string `json:"provider"` // authelia | authentik | oauth2-proxy | custom
	VerifyURL        string `json:"verifyUrl"`
	SignInURL        string `json:"signInUrl,omitempty"`
	PassRemoteUser   bool   `json:"passRemoteUser"`
	PassRemoteGroups bool   `json:"passRemoteGroups"`
	SkipWellKnown    bool   `json:"skipWellKnown"`
}

type RateLimit struct {
	Enabled            bool   `json:"enabled"`
	RequestsPerSecond  int    `json:"requestsPerSecond"`
	Burst              int    `json:"burst"`
	ExemptAccessListID string `json:"exemptAccessListId,omitempty"`
}

type GeoBlock struct {
	Enabled        bool     `json:"enabled"`
	AllowCountries []string `json:"allowCountries"` // ISO 3166-1 alpha-2
}

type ProxyHost struct {
	Meta
	Domains  []string `json:"domains"`
	Enabled  bool     `json:"enabled"`
	Upstream Upstream `json:"upstream"`

	Websockets    bool   `json:"websockets"`
	BlockExploits bool   `json:"blockExploits"`
	CacheAssets   bool   `json:"cacheAssets"`
	AccessListID  string `json:"accessListId,omitempty"`

	// SSL tab
	CertificateID     string `json:"certificateId,omitempty"` // "" = HTTP only
	ForceHTTPS        bool   `json:"forceHttps"`
	HTTP2             bool   `json:"http2"`
	HTTP3             *bool  `json:"http3"` // nil = inherit global
	HSTS              string `json:"hsts"`  // inherit | on | off
	UpstreamTLSVerify bool   `json:"upstreamTlsVerify"`
	CipherProfile     string `json:"cipherProfile"` // "" inherit | modern | intermediate | old

	Locations []Location `json:"locations"`

	// Advanced tab
	ForwardAuth      ForwardAuth `json:"forwardAuth"`
	RateLimit        RateLimit   `json:"rateLimit"`
	GeoBlock         GeoBlock    `json:"geoBlock"`
	NoIndex          bool        `json:"noIndex"`
	MaxBodySize      string      `json:"maxBodySize"`      // nginx size, e.g. "10g"; "" = 1m default, "0" = unlimited
	ProxyReadTimeout int         `json:"proxyReadTimeout"` // seconds, 0 = default 60
	ProxySendTimeout int         `json:"proxySendTimeout"`
	CustomNginx      string      `json:"customNginx"`

	Source    string `json:"source"`              // manual | docker | expose | import | mcp
	SourceRef string `json:"sourceRef,omitempty"` // container name, backend id…
	System    bool   `json:"system,omitempty"`    // Relay's own admin UI host
}

// ---------------------------------------------------------------- redirects

type Redirect struct {
	Meta
	Domains       []string `json:"domains"`
	FromPath      string   `json:"fromPath"` // "" or "/" = whole domain
	To            string   `json:"to"`
	Code          int      `json:"code"` // 301 | 302 | 307 | 308
	KeepPath      bool     `json:"keepPath"`
	CertificateID string   `json:"certificateId,omitempty"`
	ForceHTTPS    bool     `json:"forceHttps"`
	Enabled       bool     `json:"enabled"`
}

// ---------------------------------------------------------------- streams

type Stream struct {
	Meta
	Name          string `json:"name"`
	Protocol      string `json:"protocol"`      // tcp | udp | both
	ListenAddress string `json:"listenAddress"` // 0.0.0.0, 10.0.0.1 …
	ListenPorts   string `json:"listenPorts"`   // "25565" or "2456-2458"
	ForwardHost   string `json:"forwardHost"`
	ForwardPorts  string `json:"forwardPorts"` // "" = same as listen
	BackendID     string `json:"backendId,omitempty"`
	ProxyProtocol bool   `json:"proxyProtocol"`
	IdleTimeout   string `json:"idleTimeout"` // nginx time, e.g. "10m"
	Enabled       bool   `json:"enabled"`
}

// ---------------------------------------------------------------- access lists

type IPRule struct {
	ID     string `json:"id"`
	Action string `json:"action"` // allow | deny
	CIDR   string `json:"cidr"`   // CIDR, IP, or "all"
	Note   string `json:"note"`
}

type BasicAuthUser struct {
	Username     string     `json:"username"`
	PasswordHash string     `json:"passwordHash,omitempty"` // bcrypt; never returned by the API
	Password     string     `json:"password,omitempty"`     // write-only: plaintext on create/reset
	LastUsedAt   *time.Time `json:"lastUsedAt,omitempty"`
}

type BasicAuth struct {
	Enabled bool            `json:"enabled"`
	Realm   string          `json:"realm"`
	Users   []BasicAuthUser `json:"users"`
}

type AccessList struct {
	Meta
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Rules       []IPRule  `json:"rules"`
	BasicAuth   BasicAuth `json:"basicAuth"`
	SatisfyAny  bool      `json:"satisfyAny"` // IP match OR password
}

// ---------------------------------------------------------------- certificates

const (
	CertLetsEncrypt        = "letsencrypt"
	CertLetsEncryptStaging = "letsencrypt-staging"
	CertCustom             = "custom"
	CertSelfSigned         = "selfsigned"

	ChallengeHTTP01    = "http-01"
	ChallengeDNS01     = "dns-01"
	ChallengeTLSALPN01 = "tls-alpn-01"

	CertStatusValid   = "valid"
	CertStatusPending = "pending"
	CertStatusFailed  = "failed"
	CertStatusExpired = "expired"
)

type CertEvent struct {
	At         time.Time `json:"at"`
	Message    string    `json:"message"`
	Result     string    `json:"result"` // ok | failed | retried
	DurationMs int64     `json:"durationMs"`
}

type Certificate struct {
	Meta
	Name          string      `json:"name"` // display name, usually the primary domain
	Domains       []string    `json:"domains"`
	Provider      string      `json:"provider"`  // letsencrypt | letsencrypt-staging | custom | selfsigned
	Challenge     string      `json:"challenge"` // http-01 | dns-01 | tls-alpn-01
	DNSProviderID string      `json:"dnsProviderId,omitempty"`
	AutoRenew     bool        `json:"autoRenew"`
	Status        string      `json:"status"`
	LastError     string      `json:"lastError,omitempty"`
	NotBefore     *time.Time  `json:"notBefore,omitempty"`
	NotAfter      *time.Time  `json:"notAfter,omitempty"`
	Issuer        string      `json:"issuer,omitempty"`
	KeyType       string      `json:"keyType,omitempty"`
	Fingerprint   string      `json:"fingerprint,omitempty"` // SHA-256, colon hex
	Chain         []string    `json:"chain,omitempty"`       // subject CNs leaf → root
	History       []CertEvent `json:"history"`
}

type DNSProvider struct {
	Meta
	Name          string            `json:"name"`
	Type          string            `json:"type"`        // cloudflare | route53 | digitalocean | hetzner | duckdns | …
	Credentials   map[string]string `json:"credentials"` // secrets are masked on read
	Zones         []string          `json:"zones"`
	Status        string            `json:"status"` // ok | failed | unknown
	LastError     string            `json:"lastError,omitempty"`
	LastCheckedAt *time.Time        `json:"lastCheckedAt,omitempty"`
}

// ---------------------------------------------------------------- load balancer

const (
	ServerActive = "active"
	ServerBackup = "backup"

	ServerStateReady = "ready"
	ServerStateDrain = "drain"
	ServerStateMaint = "maint"
)

type Server struct {
	ID      string `json:"id"`
	Name    string `json:"name"` // haproxy server name, e.g. api-1
	Address string `json:"address"`
	Port    int    `json:"port"`
	Weight  int    `json:"weight"` // 1–256
	Role    string `json:"role"`   // active | backup
	Check   bool   `json:"check"`
	State   string `json:"state"` // ready | drain | maint (admin state)
}

type HealthCheck struct {
	Type         string `json:"type"` // none | tcp | http | pgsql | mysql | redis
	Method       string `json:"method"`
	Path         string `json:"path"`
	ExpectStatus string `json:"expectStatus"` // "200" or "2xx" style
	Host         string `json:"host,omitempty"`
	Interval     string `json:"interval"` // "" = global default
	Rise         int    `json:"rise"`
	Fall         int    `json:"fall"`
}

type Sticky struct {
	Enabled    bool   `json:"enabled"`
	Mode       string `json:"mode"` // insert | prefix | source
	CookieName string `json:"cookieName"`
}

type BackendTimeouts struct {
	Connect string `json:"connect"`
	Server  string `json:"server"`
	Queue   string `json:"queue"`
}

type Backend struct {
	Meta
	Name            string          `json:"name"`
	Mode            string          `json:"mode"`      // http | tcp
	Algorithm       string          `json:"algorithm"` // roundrobin | leastconn | source | uri | random | first | static-rr
	Servers         []Server        `json:"servers"`
	HealthCheck     HealthCheck     `json:"healthCheck"`
	Sticky          Sticky          `json:"sticky"`
	ForwardClientIP bool            `json:"forwardClientIp"` // option forwardfor (http)
	SendProxy       bool            `json:"sendProxy"`       // PROXY protocol to servers
	TLSReencrypt    bool            `json:"tlsReencrypt"`
	TLSVerify       bool            `json:"tlsVerify"`
	Retries         int             `json:"retries"`
	Timeouts        BackendTimeouts `json:"timeouts"`
	Source          string          `json:"source"`
}

const (
	CondHost      = "host"
	CondPathBeg   = "path_beg"
	CondPath      = "path"
	CondHeader    = "header"
	CondSrc       = "src"
	CondSNI       = "sni"
	CondPathRegex = "path_reg"
)

type Condition struct {
	Type   string `json:"type"`
	Name   string `json:"name,omitempty"` // header name
	Value  string `json:"value"`
	Negate bool   `json:"negate"`
}

type FrontendRule struct {
	ID         string      `json:"id"`
	Conditions []Condition `json:"conditions"` // AND
	BackendID  string      `json:"backendId"`
}

type Frontend struct {
	Meta
	Name             string         `json:"name"`
	Mode             string         `json:"mode"` // http | tcp
	Bind             string         `json:"bind"` // 127.0.0.1:10080
	Rules            []FrontendRule `json:"rules"`
	DefaultBackendID string         `json:"defaultBackendId,omitempty"`
	AcceptProxy      bool           `json:"acceptProxy"`
	Compression      bool           `json:"compression"`
	Enabled          bool           `json:"enabled"`
	Source           string         `json:"source"`
	HostID           string         `json:"hostId,omitempty"` // proxy host created together (expose)
}

// ---------------------------------------------------------------- settings

const (
	SettingsGeneral       = "general"
	SettingsSecurity      = "security"
	SettingsDocker        = "docker"
	SettingsTLS           = "tls"
	SettingsDefaultHost   = "default_host"
	SettingsHAProxy       = "haproxy"
	SettingsMCP           = "mcp"
	SettingsNotifications = "notifications"
	SettingsBackup        = "backup"
)

type HostDefaults struct {
	ForceHTTPS    bool   `json:"forceHttps"`
	BlockExploits bool   `json:"blockExploits"`
	Websockets    bool   `json:"websockets"`
	HTTP2         bool   `json:"http2"`
	AccessListID  string `json:"accessListId,omitempty"`
	CertificateID string `json:"certificateId,omitempty"`
}

type GeneralSettings struct {
	InstanceName string       `json:"instanceName"`
	AdminDomain  string       `json:"adminDomain"`
	Timezone     string       `json:"timezone"`
	PublicIP     string       `json:"publicIp"` // detected, read-only
	HTTPPort     int          `json:"httpPort"`
	HTTPSPort    int          `json:"httpsPort"`
	AdminPort    int          `json:"adminPort"`
	HTTP3        bool         `json:"http3"`
	LANCIDR      string       `json:"lanCidr"`
	Defaults     HostDefaults `json:"defaults"`
	SetupDone    bool         `json:"setupDone"`
}

type SecuritySettings struct {
	Require2FAForAdmins bool   `json:"require2faForAdmins"`
	AdminAccessListID   string `json:"adminAccessListId,omitempty"` // "" = unrestricted
	SessionTTLHours     int    `json:"sessionTtlHours"`
	LoginMaxAttempts    int    `json:"loginMaxAttempts"`
	LoginLockoutMinutes int    `json:"loginLockoutMinutes"`
}

type DockerSettings struct {
	Enabled             bool   `json:"enabled"`
	Endpoint            string `json:"endpoint"`      // legacy single endpoint: unix:///var/run/docker.sock | tcp://…
	AutoCreate          bool   `json:"autoCreate"`    // default for new endpoints
	AutoRemove          bool   `json:"autoRemove"`    // default for new endpoints
	DomainPattern       string `json:"domainPattern"` // {name}.home.lan
	DefaultCertID       string `json:"defaultCertificateId,omitempty"`
	DefaultAccessListID string `json:"defaultAccessListId,omitempty"`
	KeepInSync          bool   `json:"keepInSync"`
	// Endpoints are the Docker hosts Relay watches. When empty, the legacy
	// Endpoint is treated as one local socket endpoint (migrated on load).
	Endpoints []DockerEndpoint `json:"endpoints"`
}

// Docker endpoint connection types.
const (
	DockerSocket = "socket"
	DockerTCP    = "tcp"
	DockerTLS    = "tls"
	DockerSSH    = "ssh"
)

// DockerEndpoint is one Docker host watched by discovery.
type DockerEndpoint struct {
	ID   string `json:"id"`
	Name string `json:"name"` // local, nas, pve-docker
	Type string `json:"type"` // socket | tcp | tls | ssh
	URL  string `json:"url"`  // unix:///var/run/docker.sock | tcp://10.0.0.5:2375 | ssh://user@10.0.0.5:22
	// UpstreamAddress is the IP/host Relay uses to reach this host's published
	// ports; defaults to the host part of URL, "" for the local socket
	// (containers are reached by their IP).
	UpstreamAddress string `json:"upstreamAddress"`
	TLSCA           string `json:"tlsCa,omitempty"`   // PEM
	TLSCert         string `json:"tlsCert,omitempty"` // PEM
	TLSKey          string `json:"tlsKey,omitempty"`  // PEM, secret (write-only)
	TLSKeySet       bool   `json:"tlsKeySet,omitempty"`
	SSHKey          string `json:"sshKey,omitempty"` // private key PEM, secret (write-only)
	SSHKeySet       bool   `json:"sshKeySet,omitempty"`
	SSHKnownHost    string `json:"sshKnownHost,omitempty"` // SHA256 host key fingerprint (trusted on first connect)
	Enabled         bool   `json:"enabled"`
	AutoCreate      bool   `json:"autoCreate"`
	AutoRemove      bool   `json:"autoRemove"`
}

type HSTSSettings struct {
	Enabled           bool `json:"enabled"`
	MaxAgeSeconds     int  `json:"maxAgeSeconds"`
	IncludeSubdomains bool `json:"includeSubdomains"`
	Preload           bool `json:"preload"`
}

type TLSSettings struct {
	ACMEProvider       string       `json:"acmeProvider"` // letsencrypt | letsencrypt-staging | custom
	Email              string       `json:"email"`
	PreferredChallenge string       `json:"preferredChallenge"`
	RenewDaysBefore    int          `json:"renewDaysBefore"`
	CipherProfile      string       `json:"cipherProfile"` // modern | intermediate | old
	HSTS               HSTSSettings `json:"hsts"`
	OCSPStapling       bool         `json:"ocspStapling"`
	// Custom ACME server (acmeProvider "custom": step-ca, ZeroSSL, internal CAs).
	ACMEDirectoryURL string `json:"acmeDirectoryUrl,omitempty"`
	ACMECABundle     string `json:"acmeCaBundle,omitempty"` // PEM roots trusted for the directory's TLS
	EABKid           string `json:"eabKid,omitempty"`
	EABHMACKey       string `json:"eabHmacKey,omitempty"` // secret (redacted)
}

type DefaultHostSettings struct {
	Action        string `json:"action"` // close | 404 | redirect | host
	RedirectTo    string `json:"redirectTo,omitempty"`
	HostID        string `json:"hostId,omitempty"`
	CertificateID string `json:"certificateId,omitempty"` // "" = self-signed placeholder
}

type HAProxySettings struct {
	TimeoutConnect  string `json:"timeoutConnect"`
	TimeoutClient   string `json:"timeoutClient"`
	TimeoutServer   string `json:"timeoutServer"`
	MaxConn         int    `json:"maxConn"`
	CheckInterval   string `json:"checkInterval"`
	Rise            int    `json:"rise"`
	Fall            int    `json:"fall"`
	SeamlessReload  bool   `json:"seamlessReload"`
	StatsEnabled    bool   `json:"statsEnabled"`
	StatsBind       string `json:"statsBind"`
	StatsAccessList string `json:"statsAccessListId,omitempty"`
	Prometheus      bool   `json:"prometheus"`
	ExposePortStart int    `json:"exposePortStart"` // first port for localhost expose frontends (10080)
}

const (
	ToolRead     = "read"     // runs without confirmation
	ToolAllow    = "allow"    // write tool, runs without confirmation
	ToolConfirm  = "confirm"  // write tool, waits in the approvals inbox
	ToolDisabled = "disabled" // not exposed
)

type MCPSettings struct {
	Enabled                bool              `json:"enabled"`
	Transports             []string          `json:"transports"` // http, stdio
	AccessListID           string            `json:"accessListId,omitempty"`
	Tools                  map[string]string `json:"tools"` // tool name → read|allow|confirm|disabled
	ApprovalTimeoutMinutes int               `json:"approvalTimeoutMinutes"`
}

type NotificationChannel struct {
	ID      string            `json:"id"`
	Type    string            `json:"type"` // ntfy | smtp | resend | webhook
	Name    string            `json:"name"`
	Config  map[string]string `json:"config"`
	Enabled bool              `json:"enabled"`
}

// Notification event keys.
const (
	EventUpstreamDown     = "upstream_down"
	EventCertRenewFailed  = "cert_renew_failed"
	EventCertExpiring     = "cert_expiring"
	EventReloadFailed     = "reload_failed"
	EventUnknownSignIn    = "unknown_sign_in"
	EventMCPWriteExecuted = "mcp_write_executed"
	EventWeeklySummary    = "weekly_summary"
	// EventEngineUpdateAvailable fires once per new nginx/HAProxy release (engine slice).
	EventEngineUpdateAvailable = "engine_update_available"
)

type QuietHours struct {
	Enabled bool   `json:"enabled"`
	Start   string `json:"start"` // "23:00"
	End     string `json:"end"`   // "07:00"
}

type NotificationSettings struct {
	Channels   []NotificationChannel `json:"channels"`
	Routes     map[string][]string   `json:"routes"` // event key → channel ids
	QuietHours QuietHours            `json:"quietHours"`
}

type BackupSettings struct {
	Enabled            bool   `json:"enabled"`
	Time               string `json:"time"` // daily "03:00"
	Keep               int    `json:"keep"`
	Passphrase         string `json:"passphrase,omitempty"` // write-only via API
	PassphraseSet      bool   `json:"passphraseSet"`
	IncludePrivateKeys bool   `json:"includePrivateKeys"`
}

// BlockEntry is a globally denied client IP/CIDR (Logs → "Block IP").
type BlockEntry struct {
	CIDR      string    `json:"cidr"`
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"createdAt"`
	CreatedBy string    `json:"createdBy"`
}

type BlocklistSettings struct {
	Entries []BlockEntry `json:"entries"`
}

const SettingsBlocklist = "blocklist"

// SettingsEngines holds engine image update preferences (engine slice).
const SettingsEngines = "engines"

// EnginesSettings controls nginx/HAProxy version checks and upgrades.
type EnginesSettings struct {
	NginxChannel       string `json:"nginxChannel"`   // stable | mainline
	HAProxyChannel     string `json:"haproxyChannel"` // lts | latest
	NginxImage         string `json:"nginxImage"`     // image Relay last installed ("" = adopt the running one)
	HAProxyImage       string `json:"haproxyImage"`
	CheckIntervalHours int    `json:"checkIntervalHours"`
	AutoCheck          bool   `json:"autoCheck"`
}

// ---------------------------------------------------------------- snapshots

// Snapshot is the complete desired configuration. Every applied config
// version stores one; rollback restores it.
type Snapshot struct {
	Hosts        []ProxyHost         `json:"hosts"`
	Redirects    []Redirect          `json:"redirects"`
	Streams      []Stream            `json:"streams"`
	AccessLists  []AccessList        `json:"accessLists"`
	Certificates []Certificate       `json:"certificates"`
	Backends     []Backend           `json:"backends"`
	Frontends    []Frontend          `json:"frontends"`
	General      GeneralSettings     `json:"general"`
	TLS          TLSSettings         `json:"tls"`
	DefaultHost  DefaultHostSettings `json:"defaultHost"`
	HAProxy      HAProxySettings     `json:"haproxy"`
	Blocklist    BlocklistSettings   `json:"blocklist"`
}
