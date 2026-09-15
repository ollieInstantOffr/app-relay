// Mirrors internal/model and the cross-slice contracts in internal/core.
// Keep in sync with the Go structs.

export interface Meta {
  id: string
  createdAt: string
  updatedAt: string
}

// ---------------------------------------------------------------- hosts
export interface Upstream {
  scheme: 'http' | 'https'
  host: string
  port: number
  path?: string
  backendId?: string
}
export interface Header { name: string; value: string }
export type LocationKind = 'proxy' | 'same' | 'deny'
export interface Location {
  id: string
  path: string
  kind: LocationKind
  upstream: Upstream
  websockets: boolean
  stripPrefix: boolean
  cache: boolean
  accessListId?: string
  noAuth: boolean
  headers: Header[]
}
export interface ForwardAuth {
  enabled: boolean
  provider: 'authelia' | 'authentik' | 'oauth2-proxy' | 'custom' | string
  verifyUrl: string
  signInUrl?: string
  passRemoteUser: boolean
  passRemoteGroups: boolean
  skipWellKnown: boolean
}
export interface RateLimit { enabled: boolean; requestsPerSecond: number; burst: number; exemptAccessListId?: string }
export interface GeoBlock { enabled: boolean; allowCountries: string[] }
export type Source = 'manual' | 'docker' | 'expose' | 'import' | 'mcp'

export interface ProxyHost extends Meta {
  domains: string[]
  enabled: boolean
  upstream: Upstream
  websockets: boolean
  blockExploits: boolean
  cacheAssets: boolean
  accessListId?: string
  certificateId?: string
  forceHttps: boolean
  http2: boolean
  http3: boolean | null
  hsts: 'inherit' | 'on' | 'off'
  upstreamTlsVerify: boolean
  cipherProfile: '' | 'modern' | 'intermediate' | 'old'
  locations: Location[]
  forwardAuth: ForwardAuth
  rateLimit: RateLimit
  geoBlock: GeoBlock
  noIndex: boolean
  maxBodySize: string
  proxyReadTimeout: number
  proxySendTimeout: number
  customNginx: string
  source: Source
  sourceRef?: string
  system?: boolean
}

export interface Redirect extends Meta {
  domains: string[]
  fromPath: string
  to: string
  code: 301 | 302 | 307 | 308
  keepPath: boolean
  certificateId?: string
  forceHttps: boolean
  enabled: boolean
}

export interface Stream extends Meta {
  name: string
  protocol: 'tcp' | 'udp' | 'both'
  listenAddress: string
  listenPorts: string
  forwardHost: string
  forwardPorts: string
  backendId?: string
  proxyProtocol: boolean
  idleTimeout: string
  enabled: boolean
}

export interface IPRule { id: string; action: 'allow' | 'deny'; cidr: string; note: string }
export interface BasicAuthUser { username: string; password?: string; lastUsedAt?: string }
export interface AccessList extends Meta {
  name: string
  description: string
  rules: IPRule[]
  basicAuth: { enabled: boolean; realm: string; users: BasicAuthUser[] }
  satisfyAny: boolean
}

// ---------------------------------------------------------------- certificates
export type CertProvider = 'letsencrypt' | 'letsencrypt-staging' | 'custom' | 'selfsigned'
export type Challenge = 'http-01' | 'dns-01' | 'tls-alpn-01'
export interface CertEvent { at: string; message: string; result: 'ok' | 'failed' | 'retried'; durationMs: number }
export interface Certificate extends Meta {
  name: string
  domains: string[]
  provider: CertProvider
  challenge: Challenge
  dnsProviderId?: string
  autoRenew: boolean
  status: 'valid' | 'pending' | 'failed' | 'expired'
  lastError?: string
  notBefore?: string
  notAfter?: string
  issuer?: string
  keyType?: string
  fingerprint?: string
  chain?: string[]
  history: CertEvent[]
}
export interface DNSProvider extends Meta {
  name: string
  type: string
  credentials: Record<string, string>
  zones: string[]
  status: 'ok' | 'failed' | 'unknown' | ''
  lastError?: string
  lastCheckedAt?: string
}

// ---------------------------------------------------------------- load balancer
export interface Server {
  id: string
  name: string
  address: string
  port: number
  weight: number
  role: 'active' | 'backup'
  check: boolean
  state: 'ready' | 'drain' | 'maint'
}
export interface HealthCheck {
  type: 'none' | 'tcp' | 'http' | 'pgsql' | 'mysql' | 'redis'
  method: string
  path: string
  expectStatus: string
  host?: string
  interval: string
  rise: number
  fall: number
}
export interface Backend extends Meta {
  name: string
  mode: 'http' | 'tcp'
  algorithm: 'roundrobin' | 'leastconn' | 'source' | 'uri' | 'random' | 'first' | 'static-rr'
  servers: Server[]
  healthCheck: HealthCheck
  sticky: { enabled: boolean; mode: 'insert' | 'prefix' | 'source'; cookieName: string }
  forwardClientIp: boolean
  sendProxy: boolean
  tlsReencrypt: boolean
  tlsVerify: boolean
  retries: number
  timeouts: { connect: string; server: string; queue: string }
  source: Source
}
export type ConditionType = 'host' | 'path_beg' | 'path' | 'header' | 'src' | 'sni' | 'path_reg'
export interface Condition { type: ConditionType; name?: string; value: string; negate: boolean }
export interface FrontendRule { id: string; conditions: Condition[]; backendId: string }
export interface Frontend extends Meta {
  name: string
  mode: 'http' | 'tcp'
  bind: string
  rules: FrontendRule[]
  defaultBackendId?: string
  acceptProxy: boolean
  compression: boolean
  enabled: boolean
  source: Source
  hostId?: string
}

// ---------------------------------------------------------------- settings
export interface HostDefaults {
  forceHttps: boolean
  blockExploits: boolean
  websockets: boolean
  http2: boolean
  accessListId?: string
  certificateId?: string
}
export interface GeneralSettings {
  instanceName: string
  adminDomain: string
  timezone: string
  publicIp: string
  httpPort: number
  httpsPort: number
  adminPort: number
  http3: boolean
  lanCidr: string
  defaults: HostDefaults
  setupDone: boolean
}
export interface SecuritySettings {
  require2faForAdmins: boolean
  adminAccessListId?: string
  sessionTtlHours: number
  loginMaxAttempts: number
  loginLockoutMinutes: number
}
export interface DockerSettings {
  enabled: boolean
  endpoint: string
  autoCreate: boolean
  autoRemove: boolean
  domainPattern: string
  defaultCertificateId?: string
  defaultAccessListId?: string
  keepInSync: boolean
  endpoints: DockerEndpoint[]
}
export type DockerEndpointType = 'socket' | 'tcp' | 'tls' | 'ssh'
export interface DockerEndpoint {
  id: string
  name: string
  type: DockerEndpointType
  url: string
  /** Address Relay uses to reach published ports; "" for the local socket. */
  upstreamAddress: string
  tlsCa?: string
  tlsCert?: string
  /** Write-only; tlsKeySet reports whether one is stored. */
  tlsKey?: string
  tlsKeySet?: boolean
  /** Write-only; sshKeySet reports whether one is stored. */
  sshKey?: string
  sshKeySet?: boolean
  sshKnownHost?: string
  enabled: boolean
  autoCreate: boolean
  autoRemove: boolean
}
export interface TLSSettings {
  acmeProvider: 'letsencrypt' | 'letsencrypt-staging'
  email: string
  preferredChallenge: Challenge
  renewDaysBefore: number
  cipherProfile: 'modern' | 'intermediate' | 'old'
  hsts: { enabled: boolean; maxAgeSeconds: number; includeSubdomains: boolean; preload: boolean }
  ocspStapling: boolean
}
export interface DefaultHostSettings {
  action: 'close' | '404' | 'redirect' | 'host'
  redirectTo?: string
  hostId?: string
  certificateId?: string
}
export interface HAProxySettings {
  timeoutConnect: string
  timeoutClient: string
  timeoutServer: string
  maxConn: number
  checkInterval: string
  rise: number
  fall: number
  seamlessReload: boolean
  statsEnabled: boolean
  statsBind: string
  statsAccessListId?: string
  prometheus: boolean
  exposePortStart: number
}
export type ToolPermission = 'read' | 'allow' | 'confirm' | 'disabled'
export interface MCPSettings {
  enabled: boolean
  transports: ('http' | 'stdio')[]
  accessListId?: string
  tools: Record<string, ToolPermission>
  approvalTimeoutMinutes: number
}
export interface NotificationChannel {
  id: string
  type: 'ntfy' | 'smtp' | 'webhook'
  name: string
  config: Record<string, string>
  enabled: boolean
}
export type NotificationEvent =
  | 'upstream_down' | 'cert_renew_failed' | 'cert_expiring' | 'reload_failed'
  | 'unknown_sign_in' | 'mcp_write_executed' | 'weekly_summary'
export interface NotificationSettings {
  channels: NotificationChannel[]
  routes: Partial<Record<NotificationEvent, string[]>>
  quietHours: { enabled: boolean; start: string; end: string }
}
export interface BackupSettings {
  enabled: boolean
  time: string
  keep: number
  passphrase?: string
  passphraseSet: boolean
  includePrivateKeys: boolean
}
export interface BlockEntry { cidr: string; note: string; createdAt: string; createdBy: string }
export interface BlocklistSettings { entries: BlockEntry[] }

export interface SettingsMap {
  general: GeneralSettings
  security: SecuritySettings
  docker: DockerSettings
  tls: TLSSettings
  default_host: DefaultHostSettings
  haproxy: HAProxySettings
  mcp: MCPSettings
  notifications: NotificationSettings
  backup: BackupSettings
  blocklist: BlocklistSettings
}
export type SettingsKey = keyof SettingsMap

// ---------------------------------------------------------------- entity kinds
export interface EntityMap {
  hosts: ProxyHost
  redirects: Redirect
  streams: Stream
  'access-lists': AccessList
  certificates: Certificate
  'dns-providers': DNSProvider
  backends: Backend
  frontends: Frontend
}
export type EntityKind = keyof EntityMap

// ---------------------------------------------------------------- cross-slice contracts

/** GET /api/auth/session (slice auth) */
export interface SessionUser {
  id: string
  username: string
  email: string
  role: Role
  totpEnabled: boolean
  mustChangePassword: boolean
}
export type Role = 'admin' | 'editor' | 'viewer'
export interface Session {
  authenticated: boolean
  setupRequired: boolean
  user?: SessionUser
  version: string
  instanceName: string
}

/** GET /api/pending (slice engine) */
export interface PendingItem { kind: string; id: string; name: string; action: 'created' | 'updated' | 'deleted' }
export interface Pending { count: number; items: PendingItem[]; liveVersion: number }

/** GET /api/versions (slice engine) */
export interface Version {
  id: number
  createdAt: string
  actor: string
  summary: string
  status: 'live' | 'superseded' | 'rolled_back' | 'failed' | 'draft'
  error?: string
  validateMs: number
  reloadMs: number
  rolledBackTo?: number
  changes: PendingItem[]
}

/** GET /api/engines (slice engine) */
export interface EngineState {
  engine: 'nginx' | 'haproxy'
  reachable: boolean
  error?: string
  running: boolean
  pid: number
  version: string
  modules: string[]
  startedAt?: string
  exitedAt?: string
  exitError?: string
  lastReloadAt?: string
  lastReloadMs: number
  configHash: string
  configLines: number
  configured: boolean
}
export interface EnginesStatus { nginx: EngineState; haproxy: EngineState }

/** POST /api/preview/* (engine: nginx host/stream; lb: haproxy backend/frontend) */
export interface ConfigPreview { config: string; valid: boolean; output: string }

/** GET /api/health (slice observe) → Record<target, HealthStatus>; target = host:<id> | stream:<id> */
export type HealthState = 'healthy' | 'degraded' | 'down' | 'disabled' | 'unknown'
export interface HealthStatus {
  target: string
  status: HealthState
  latencyMs: number
  httpStatus?: number
  detail?: string
  checkedAt: string
  changedAt: string
}

/** GET /api/lb/stats (slice lb) */
export interface ServerStats {
  id: string; name: string; address: string
  status: 'UP' | 'DOWN' | 'DRAIN' | 'MAINT' | 'NOLB' | 'NOCHECK' | string
  checkDetail?: string; role: string; weight: number; sharePct: number
  sessRate: number; current: number; max: number; queue: number; errors: number
  respAvgMs: number; respP95Ms: number; uptimeSec: number; downSec: number
}
export interface BackendStats {
  id: string; name: string; status: 'UP' | 'DEGRADED' | 'DOWN' | string
  sessRate: number; current: number; max: number; queue: number; errors: number
  respP95Ms: number; uptimeSec: number; servers: ServerStats[]
}
export interface LBStats {
  running: boolean; sessRate: number; peakSessRate: number; queue: number; retries: number; redispatches: number
  connErrors: number; respErrors: number; reqErrors: number
  backends: BackendStats[]
  frontends: { id: string; name: string; sessRate: number; current: number }[]
}

/** GET /api/docker/containers (slice ops) */
export interface Container {
  id: string; name: string; image: string; state: string; ip: string
  ports: { private: number; public?: number; proto: string }[]
  labels: Record<string, string>
  http: boolean; suggestedPort: number; hostId?: string; backendId?: string; reason?: string
  /** Docker host the container runs on. */
  endpointId: string; endpointName: string
  /** Address to proxy to together with suggestedPort; absent when not reachable. */
  upstreamHost?: string
}

/** GET /api/approvals (slice mcp) */
export interface Approval {
  id: string
  createdAt: string
  expiresAt: string
  tokenId: string
  clientName: string
  tool: string
  args: Record<string, unknown>
  summary: string
  reason: string
  preview: string
  status: 'pending' | 'approved' | 'denied' | 'expired'
  decidedBy: string
  decidedAt?: string
  result: string
}

/** POST /api/certificates/request (slice certs) */
export interface CertRequest {
  domains: string[]
  challenge: Challenge
  dnsProviderId?: string
  staging: boolean
  autoRenew: boolean
}

/** GET /api/logs/access (slice observe) */
export interface AccessEntry {
  id: number; ts: string; kind: 'http' | 'stream'; hostId: string; host: string; method: string; path: string
  protocol: string; status: number; clientIp: string; upstreamAddr: string; upstreamStatus: string
  requestTime: number; upstreamConnectTime?: number; upstreamHeaderTime?: number; upstreamResponseTime?: number
  bytesSent: number; bytesReceived: number; userAgent: string; referer: string; requestId: string
  sslProtocol: string; extra: Record<string, string>
}
export interface AccessPage { entries: AccessEntry[]; nextBeforeId: number }

/** GET /api/audit, GET /api/activity (slice observe) */
export interface AuditRow {
  id: number; at: string; actorType: 'user' | 'mcp' | 'token' | 'system' | 'docker'; actorId: string; actorName: string
  action: string; target: string; detail: string; version?: number; result: string; ip: string
}
export interface ActivityRow { id: number; at: string; kind: string; level: 'ok' | 'info' | 'warn' | 'error'; title: string; subject: string; detail: string }

/** GET /api/metrics/hosts (slice observe): per host id, 24 hourly request buckets (oldest first) */
export type HostMetrics = Record<string, { requests24h: number; series: number[] }>

/** GET /api/metrics/streams (slice observe): per stream id */
export type StreamMetrics = Record<string, { active: number; bytesPerSec: number }>

/** GET /api/tokens (slice auth) */
export interface ApiToken {
  id: string
  name: string
  prefix: string
  last4: string
  scope: 'read' | 'write'
  surfaces: ('mcp' | 'rest')[]
  limitTo: string[]
  expiresAt?: string
  lastUsedAt?: string
  createdBy: string
  createdAt: string
  revokedAt?: string
  /** Only present in the POST /api/tokens response. */
  token?: string
}

/** SSE /api/events */
export interface BusEvent<T = unknown> { topic: string; at: string; data: T }
