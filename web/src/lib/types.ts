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
  /** "relay" = Relay login (built in); verifyUrl/signInUrl are ignored and sent empty. */
  provider: 'relay' | 'authelia' | 'authentik' | 'oauth2-proxy' | 'custom' | string
  verifyUrl: string
  signInUrl?: string
  /** Relay login only: Relay user ids allowed to sign in. Empty = every enabled Relay user. */
  allowedUsers?: string[]
  passRemoteUser: boolean
  passRemoteGroups: boolean
  skipWellKnown: boolean
}
export interface RateLimit { enabled: boolean; requestsPerSecond: number; burst: number; exemptAccessListId?: string }
export interface GeoBlock { enabled: boolean; allowCountries: string[] }
/** Maintenance mode: visitors get 503 with the maintenance page; empty title/message = Settings → Error pages. */
export interface HostMaintenance { enabled: boolean; title: string; message: string; bypassAccessListId?: string }
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
  maintenance: HostMaintenance
  noIndex: boolean
  maxBodySize: string
  proxyReadTimeout: number
  proxySendTimeout: number
  customNginx: string
  source: Source
  sourceRef?: string
  system?: boolean
  /** Published through this tunnel gateway ("" / absent = not published). */
  tunnelGatewayId?: string
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
  /** TCP ports published through this tunnel gateway. */
  tunnelGatewayId?: string
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
export type CertProvider = 'letsencrypt' | 'letsencrypt-staging' | 'acme' | 'custom' | 'selfsigned'
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
export type ProxyEngineName = 'nginx' | 'edge'
export const proxyEngineLabel: Record<ProxyEngineName, string> = { nginx: 'nginx', edge: 'Relay Edge' }
/** Load balancer engine: HAProxy (default) or Relay Balancer (beta, built into Relay). */
export type LBEngineName = 'haproxy' | 'balancer'
export const lbEngineLabel: Record<LBEngineName, string> = { haproxy: 'HAProxy', balancer: 'Relay Balancer' }
/** Command that validates the engine's config. */
export const lbCheckName: Record<LBEngineName, string> = { haproxy: 'haproxy -c', balancer: 'relay balancer check' }
/** File the engine's config is rendered to. */
export const lbConfigFile: Record<LBEngineName, string> = { haproxy: 'haproxy.cfg', balancer: 'balancer.json' }
/** Normalizes an API value to a load balancer engine (absent/unknown = HAProxy). */
export const asLBEngine = (v: unknown): LBEngineName => (v === 'balancer' ? 'balancer' : 'haproxy')

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
  /** Reverse proxy engine for the HTTP/HTTPS ports and streams. */
  proxyEngine: ProxyEngineName
  /** Load balancer engine for backends and frontends (default haproxy). */
  lbEngine?: LBEngineName
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
  acmeProvider: 'letsencrypt' | 'letsencrypt-staging' | 'custom'
  /** Custom ACME server (acmeProvider "custom"). */
  acmeDirectoryUrl?: string
  acmeCaBundle?: string
  eabKid?: string
  /** Secret: masked in responses, resend the mask to keep it. */
  eabHmacKey?: string
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
  type: 'smtp' | 'resend' | 'webhook'
  name: string
  config: Record<string, string>
  enabled: boolean
}
export type NotificationEvent =
  | 'upstream_down' | 'cert_renew_failed' | 'cert_expiring' | 'reload_failed'
  | 'unknown_sign_in' | 'mcp_write_executed' | 'weekly_summary' | 'engine_update_available' | 'backup_failed' | 'tunnel_down'
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
  s3: BackupS3Settings
}
/** Off-site copies of backups: AWS S3 or S3-compatible storage. */
export interface BackupS3Settings {
  enabled: boolean
  /** Empty for AWS S3. */
  endpoint: string
  region: string
  bucket: string
  prefix: string
  accessKeyId: string
  /** Write-only; empty keeps the saved key. */
  secretAccessKey?: string
  secretSet: boolean
  pathStyle: boolean
}
export type ErrorPageKey = '403' | '404' | '429' | '500' | '502' | '503' | '504' | 'maintenance'
export interface ErrorPage {
  title: string
  message: string
  /** Replaces the built-in design for this page (max 100 KB). */
  html?: string
}
/** GET/PUT /api/settings/error_pages */
export interface ErrorPagesSettings {
  /** Relay's pages replace the engine's plain pages for errors Relay generates. The maintenance page is always used. */
  enabled: boolean
  brandName: string
  /** #rrggbb, "" = default */
  accentColor: string
  pages: Record<ErrorPageKey, ErrorPage>
}
/** POST /api/preview/error-page → ErrorPagePreview */
export interface ErrorPagePreviewRequest {
  settings: ErrorPagesSettings
  page: ErrorPageKey
  maintenance?: { title: string; message: string }
}
export interface ErrorPagePreview { html: string }

/** Settings → Public DNS. GET/PUT /api/settings/public_dns (admin to change; applies immediately, not a pending change). */
export interface PublicDNSSettings {
  enabled: boolean
  /** DNS providers whose zones Relay manages (types from GET /api/dns/provider-types). */
  providerIds: string[]
  /** After each successful apply, create missing records for enabled proxy hosts. */
  autoCreate: boolean
  recordType: 'A' | 'CNAME'
  /** A: IPv4 ("" = Relay's detected public IP). CNAME: hostname (required). */
  target: string
  /** Seconds; GoDaddy minimum 600. */
  ttl: number
  /** Zones Relay never changes automatically. */
  excludedZones: string[]
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
  engines: EnginesSettings
  error_pages: ErrorPagesSettings
  public_dns: PublicDNSSettings
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
  gateways: Gateway
}
export type EntityKind = keyof EntityMap

// ---------------------------------------------------------------- tunnels
export type GatewayTransport = 'auto' | 'quic' | 'tcp'
/** GET /api/gateways: a public tunnel gateway this Relay dials out to (runtime state, no pending changes). */
export interface Gateway extends Meta {
  name: string
  /** host[:port] of the gateway's tunnel listener (port 7443 by default). */
  address: string
  transport: GatewayTransport
  enabled: boolean
  pairState: 'pending' | 'paired'
  gatewayPin?: string
  homeFingerprint?: string
  pairedAt?: string
  pairExpires?: string
  publicIps: string[]
  version?: string
}
export type GatewayState = 'disabled' | 'unpaired' | 'idle' | 'connecting' | 'connected' | 'disconnected' | 'incompatible'
export interface GatewayPortError { port: number; error: string }
/** The tunnel engine's view of one gateway. */
export interface GatewayStatus {
  id: string
  state: GatewayState
  transport?: 'quic' | 'tcp'
  address: string
  connectedAt?: string
  lastError?: string
  lastErrorAt?: string
  reconnects: number
  rttMs: number
  version?: string
  proto?: number
  publicIps?: string[]
  generation: number
  ackedGeneration: number
  portErrors?: GatewayPortError[]
  activeStreams: number
  streams: number
  rejected: number
  bytesIn: number
  bytesOut: number
}
/** GET /api/tunnels */
export interface TunnelOverview {
  /** null while the tunnel engine isn't running. */
  engine: { hash: string; startedAt: string; gateways: GatewayStatus[] } | null
  gateways: (Gateway & { status: GatewayStatus | null; published: { hosts: string[]; streams: string[] } })[]
}
/** POST /api/gateways/:id/pairing */
export interface GatewayPairing {
  token: string
  expiresAt: string
  homeFingerprint: string
  install: string
  manual: string
  ref: string
  env: string
  ports: string[]
}

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
/** member = "App access only": signs in to apps behind Relay login, not to the admin UI. */
export type Role = 'admin' | 'editor' | 'viewer' | 'member'
/** GET /api/users/directory (any signed-in user) */
export interface DirectoryUser { id: string; username: string; email: string; role: Role; disabled: boolean }
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
export type EngineKind = ProxyEngineName | LBEngineName | 'tunnel'
export interface EngineState {
  engine: EngineKind
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
  stopped?: boolean
  /** Engine container state when the agent is unreachable. */
  container?: 'running' | 'stopped' | 'missing'
  /** The container is stopped on purpose: the engine isn't needed right now. */
  standby?: boolean
}
export interface EnginesStatus {
  nginx: EngineState
  haproxy: EngineState
  edge: EngineState
  balancer?: EngineState
  /** The tunnel engine (runs while hosts or streams are published through a tunnel). */
  tunnel?: EngineState
  /** Active proxy engine: the live version's, or the General setting before the first apply. */
  proxy: ProxyEngineName
  /** Active load balancer engine, same rules as proxy. */
  lb?: LBEngineName
}

/** Settings → Updates (slice engine). GET/PUT /api/settings/engines */
export interface EnginesSettings {
  nginxChannel: 'stable' | 'mainline'
  haproxyChannel: 'lts' | 'latest'
  nginxImage: string
  haproxyImage: string
  checkIntervalHours: number
  autoCheck: boolean
}
export interface EngineRelease { version: string; tag: string; image: string; date?: string }
export interface EngineDrift { runningImage: string; runningVersion: string; desiredImage: string; desiredVersion: string }
/** GET /api/engines/updates (slice engine) */
export interface EngineUpdateInfo {
  engine: 'nginx' | 'haproxy'
  channel: string
  version: string
  reachable: boolean
  running: boolean
  image: string
  imageVersion: string
  container: string
  official: boolean
  desiredImage: string
  latest?: EngineRelease
  channels: Record<string, EngineRelease | null>
  updateAvailable: boolean
  drift?: EngineDrift
  canUpgrade: boolean
  upgradeBlocker?: string
  changesUrl: string
  modules: string[]
  missingModules: string[]
  /** True while the other engine is selected: Relay Edge (for nginx) or Relay Balancer (for HAProxy). */
  inactive?: boolean
  standby?: boolean
}
export interface RelayCommit { sha: string; short: string; author: string; date?: string; subject: string; url?: string }
/** Relay's own update state (part of GET /api/engines/updates). */
export interface RelayUpdateInfo {
  version: string
  commit: string
  branch: string
  container: string
  workingDir: string
  remote: string
  checkoutHead: string
  checkoutRef: string
  remoteHead: string
  /** versions derived from git history; '' when unknown */
  checkoutVersion: string
  remoteVersion: string
  behind: number
  ahead: number
  dirty: number
  commits: RelayCommit[]
  rebuildNeeded: boolean
  updateAvailable: boolean
  canUpdate: boolean
  blocker?: string
  checkedAt?: string
  checkError?: string
}
/** GET /api/engines/relay/update-status → {job}; bus topic relay.update */
export interface RelayUpdateJob {
  id: string
  from: string
  to: string
  actor: string
  restartEngines: boolean
  fromVersion: string
  toVersion: string
  status: 'running' | 'succeeded' | 'failed'
  message: string
  error?: string
  output?: string
  progress: number
  steps: UpgradeStep[]
  startedAt: string
  finishedAt?: string
}
export interface EngineUpdates {
  relay?: RelayUpdateInfo
  nginx: EngineUpdateInfo
  haproxy: EngineUpdateInfo
  /** Selected proxy engine (Relay Edge upgrades together with Relay). */
  proxyEngine?: ProxyEngineName
  autoCheck: boolean
  checkedAt?: string
  nextCheckAt?: string
  checkError?: string
  dockerError?: string
  composeProject?: string
}
export interface UpgradeStep { id: string; label: string; status: 'pending' | 'running' | 'done' | 'failed' | 'skipped'; detail?: string }
/** GET /api/engines/upgrade-status → {job}; bus topic engine.upgrade */
export interface UpgradeJob {
  id: string
  engine: 'nginx' | 'haproxy'
  from: string
  to: string
  fromImage: string
  toImage: string
  actor: string
  status: 'running' | 'succeeded' | 'rolled_back' | 'failed'
  message: string
  error?: string
  output?: string
  progress: number
  steps: UpgradeStep[]
  startedAt: string
  finishedAt?: string
}

/** POST /api/preview/* (proxy: host/stream on the active proxy engine; lb: backend/frontend on the active load balancer engine) */
export interface ConfigPreview { config: string; valid: boolean; output: string; engine?: EngineKind }

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
  /** Likely app ports usable with upstreamHost, best first. */
  candidatePorts?: number[]
  /** Stopped local container: a host can be created disabled and is enabled when it starts. */
  linkOnStart?: boolean
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

// ---------------------------------------------------------------- public DNS (slice dns)
/** A provider selected in Settings → Public DNS, with its zones fetched live. */
export interface DNSStatusProvider {
  id: string
  name: string
  type: string
  supported: boolean
  zones: string[]
  /** e.g. GoDaddy "Access denied" */
  error?: string
}
export interface DNSZone { name: string; providerId: string; providerName: string; providerType: string }
/** GET /api/dns/status */
export interface DNSStatus {
  enabled: boolean
  publicIp: string
  /** Effective target for automatic records. */
  target: string
  providers: DNSStatusProvider[]
  zones: DNSZone[]
}
export interface DNSRecord {
  id: string
  type: string
  /** Relative to the zone: "@", "www", "*.dev" */
  name: string
  fqdn: string
  data: string
  ttl: number
  priority?: number
  /** Cloudflare only */
  proxied?: boolean
  /** Proxy host ids served by this record. */
  hosts?: string[]
  /** e.g. apex NS, SOA, SRV */
  readOnly?: boolean
}
/** POST/PUT /api/dns/zones/{zone}/records */
export interface DNSRecordInput {
  type: string
  name: string
  data: string
  /** 0 = provider default */
  ttl: number
  priority?: number
  proxied?: boolean
}
/** GET /api/dns/zones/{zone}/records */
export interface DNSRecordList { zone: string; providerType: string; records: DNSRecord[] }
export type DNSCheckStatus = 'unmanaged' | 'excluded' | 'exists' | 'wildcard' | 'missing' | 'conflict' | 'error' | 'created'
/** POST /api/dns/check and POST /api/dns/sync → { results } */
export interface DNSDomainCheck {
  domain: string
  zone?: string
  providerId?: string
  providerType?: string
  status: DNSCheckStatus
  message?: string
  /** Existing A/AAAA/CNAME at that name (or the covering wildcard). */
  records: DNSRecord[]
  /** Records that point to Relay's target. */
  relayRecords: DNSRecord[]
  /** What Relay would create (status missing). */
  planned?: DNSRecordInput
}
export interface DNSCheckResponse { results: DNSDomainCheck[] }
