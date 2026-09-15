// Load balancer slice: API hooks, draft factories and formatting helpers.
import { useEffect, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { api, ApiError, errorMessage, qs } from '../../lib/api'
import { keys } from '../../lib/queries'
import { rid } from '../../lib/format'
import type {
  Backend, BackendStats, Certificate, Condition, ConditionType, ConfigPreview, ForwardAuth, Frontend, GeoBlock,
  HAProxySettings, ProxyHost, RateLimit, Server, ServerStats, Version,
} from '../../lib/types'

// ---------------------------------------------------------------- API types

export interface PreviewResult extends ConfigPreview {
  checked?: 'haproxy' | 'local'
  fields?: Record<string, string>
}

export interface ValidationResult {
  valid: boolean
  output: string
  checked: 'haproxy' | 'local'
  durationMs: number
  lines: number
}

export interface HAProxyConfig {
  config: string
  rendered: string
  live?: string
  liveVersion?: number
  pending: boolean
  renderError?: string
  lines: number
}

export interface SeriesPoint { t: number; sessRate: number; errors: number; queue: number }
export interface LBSeries {
  range: string
  step: number
  backendId?: string
  points: SeriesPoint[]
  peak: number
  connErrors: number
  respErrors: number
  reqErrors: number
  retries: number
  redispatches: number
}

export interface StateResult { state: string; runtime: boolean; note?: string }
export interface ProbeResult {
  ok: boolean
  status: string
  check: string
  detail: string
  latencyMs: number
  httpStatus?: number
  checkedAt: string
}

export interface ExposeRequest {
  backendId: string
  domain: string
  certificate: { mode: 'request' | 'existing' | 'none'; certificateId?: string; challenge?: string; dnsProviderId?: string }
  forceHttps: boolean
  websockets: boolean
  access: { mode: 'public' | 'list'; accessListId?: string }
  forwardAuth?: ForwardAuth
  rateLimit?: RateLimit
  blockExploits: boolean
  geoBlock?: GeoBlock
  noIndex: boolean
  applyNow: boolean
}
export interface ExposePreview {
  /** nginx* fields describe the active proxy engine's files (see engine). */
  engine?: 'nginx' | 'edge'
  nginx: string
  nginxValid: boolean | null
  nginxOutput: string
  haproxy: string
  haproxyValid: boolean | null
  haproxyOutput: string
  port: number
  frontendName: string
  backendName: string
}
export interface ExposeResult {
  host: ProxyHost
  frontend: Frontend
  certificate?: Certificate
  version?: Version
  applyError?: string
}

// ---------------------------------------------------------------- hooks

/** Debounced POST preview; re-runs whenever body changes (null disables). */
export function usePreview(path: string, body: unknown, delay = 450) {
  const [state, setState] = useState<{ data?: PreviewResult; loading: boolean; error?: string }>({ loading: false })
  const seq = useRef(0)
  const key = body === null || body === undefined ? '' : JSON.stringify(body)
  useEffect(() => {
    if (!key) return
    const n = ++seq.current
    setState((s) => ({ ...s, loading: true }))
    const t = window.setTimeout(() => {
      api
        .post<PreviewResult>(path, JSON.parse(key))
        .then((data) => n === seq.current && setState({ data, loading: false }))
        .catch((err) => n === seq.current && setState({ loading: false, error: errorMessage(err) }))
    }, delay)
    return () => window.clearTimeout(t)
  }, [path, key, delay])
  return state
}

export function useLBSeries(range: string, backendId?: string, refetchMs = 10_000) {
  return useQuery({
    queryKey: ['lb', 'series', range, backendId ?? ''],
    queryFn: () => api.get<LBSeries>(`/api/lb/series${qs({ range, backend: backendId })}`),
    refetchInterval: refetchMs,
  })
}

export function useHAProxyConfig(enabled: boolean) {
  return useQuery({ queryKey: ['haproxy', 'config'], queryFn: () => api.get<HAProxyConfig>('/api/haproxy/config'), enabled })
}

export function useInvalidateLB() {
  const qc = useQueryClient()
  return () => {
    qc.invalidateQueries({ queryKey: keys.lbStats })
    qc.invalidateQueries({ queryKey: ['entities'] })
    qc.invalidateQueries({ queryKey: keys.pending })
    qc.invalidateQueries({ queryKey: ['haproxy'] })
  }
}

export function useServerActions() {
  const invalidate = useInvalidateLB()
  return {
    setState: async (backendId: string, serverId: string, state: Server['state'], graceSeconds?: number) => {
      const r = await api.post<StateResult>(`/api/backends/${backendId}/servers/${serverId}/state`, { state, graceSeconds })
      invalidate()
      return r
    },
    setWeight: async (backendId: string, serverId: string, weight: number) => {
      const r = await api.post<StateResult>(`/api/backends/${backendId}/servers/${serverId}/weight`, { weight })
      invalidate()
      return r
    },
    check: (backendId: string, serverId: string) => api.post<ProbeResult>(`/api/backends/${backendId}/servers/${serverId}/check`),
  }
}

// ---------------------------------------------------------------- toasts & errors

export const applyNowAction = {
  label: 'Apply now',
  primary: true,
  onClick: () => window.dispatchEvent(new CustomEvent('relay:apply')),
}

export function fieldErrors(err: unknown): Record<string, string> {
  return err instanceof ApiError && err.fields ? err.fields : {}
}

// ---------------------------------------------------------------- vocab

export const ALGORITHMS: { value: Backend['algorithm']; short: string; description: string }[] = [
  { value: 'roundrobin', short: 'roundrobin', description: 'each server in turn, by weight' },
  { value: 'leastconn', short: 'leastconn', description: 'fewest active connections' },
  { value: 'source', short: 'source hash', description: 'same client IP → same server' },
  { value: 'uri', short: 'uri hash', description: 'same path → same server (HTTP only)' },
  { value: 'random', short: 'random', description: 'random pick, weighted' },
  { value: 'first', short: 'first', description: 'fill the first server before the next' },
  { value: 'static-rr', short: 'static-rr', description: 'round robin, weights fixed at start' },
]

export function algorithmShort(a: string) {
  return ALGORITHMS.find((x) => x.value === a)?.short ?? a
}

export const HEALTH_TYPES: { value: Backend['healthCheck']['type']; label: string }[] = [
  { value: 'http', label: 'HTTP' },
  { value: 'tcp', label: 'TCP connect' },
  { value: 'pgsql', label: 'PostgreSQL' },
  { value: 'mysql', label: 'MySQL' },
  { value: 'redis', label: 'Redis' },
  { value: 'none', label: 'None' },
]

export function healthSummary(b: Backend, globalInterval = '2s'): { label: string; enabled: boolean } {
  const hc = b.healthCheck
  const every = hc.interval || globalInterval
  switch (hc.type) {
    case 'http':
      return { label: `check ${hc.method || 'GET'} ${hc.path || '/'} · ${every}`, enabled: true }
    case 'tcp':
      return { label: `tcp check · ${every}`, enabled: true }
    case 'pgsql':
    case 'mysql':
    case 'redis':
      return { label: `${hc.type}-check · ${every}`, enabled: true }
  }
  return { label: 'no health check', enabled: false }
}

export const CONDITION_TYPES: { value: ConditionType; label: string; mode: 'http' | 'tcp' | 'both'; placeholder: string }[] = [
  { value: 'host', label: 'Host is', mode: 'http', placeholder: 'api.home.lan' },
  { value: 'path_beg', label: 'Path starts', mode: 'http', placeholder: '/api/' },
  { value: 'path', label: 'Path is', mode: 'http', placeholder: '/health' },
  { value: 'path_reg', label: 'Path matches', mode: 'http', placeholder: '^/v[0-9]+/' },
  { value: 'header', label: 'Header', mode: 'http', placeholder: 'value · empty = present' },
  { value: 'src', label: 'Source IP', mode: 'both', placeholder: '10.0.0.0/8' },
  { value: 'sni', label: 'SNI', mode: 'tcp', placeholder: 'db.home.lan' },
]

export function conditionText(c: Condition): string {
  const not = c.negate ? 'not ' : ''
  switch (c.type) {
    case 'host':
      return `${not}host = ${c.value}`
    case 'path_beg':
      return `${not}path starts ${c.value}`
    case 'path':
      return `${not}path = ${c.value}`
    case 'path_reg':
      return `${not}path ~ ${c.value}`
    case 'header':
      return c.value ? `${not}header ${c.name} = ${c.value}` : `${not}header ${c.name} present`
    case 'src':
      return `${not}source ${c.value}`
    case 'sni':
      return `${not}SNI = ${c.value}`
  }
  return ''
}

// ---------------------------------------------------------------- drafts

export function newServer(address = '', port = 0): Server {
  return { id: rid(), name: '', address, port, weight: 100, role: 'active', check: true, state: 'ready' }
}

export function newBackendDraft(s?: HAProxySettings): Backend {
  return {
    id: '', createdAt: '', updatedAt: '',
    name: '', mode: 'http', algorithm: 'roundrobin',
    servers: [newServer()],
    healthCheck: { type: 'http', method: 'GET', path: '/', expectStatus: '2xx', interval: '', rise: s?.rise ?? 2, fall: s?.fall ?? 3 },
    sticky: { enabled: false, mode: 'insert', cookieName: 'SRVID' },
    forwardClientIp: true, sendProxy: false, tlsReencrypt: false, tlsVerify: false, retries: 3,
    timeouts: { connect: '', server: '', queue: '' },
    source: 'manual',
  }
}

export function cloneBackend(b: Backend, names: string[]): Backend {
  const c: Backend = JSON.parse(JSON.stringify(b))
  c.id = ''
  c.createdAt = ''
  c.updatedAt = ''
  c.source = 'manual'
  c.name = uniqueName(`${b.name}-copy`, names)
  c.servers = c.servers.map((s) => ({ ...s, id: rid(), name: '', state: 'ready' }))
  return c
}

export function newFrontendDraft(defaultBackendId = '', port = 10080): Frontend {
  return {
    id: '', createdAt: '', updatedAt: '',
    name: '', mode: 'http', bind: `127.0.0.1:${port}`, rules: [], defaultBackendId,
    acceptProxy: false, compression: false, enabled: true, source: 'manual',
  }
}

export function cloneFrontend(f: Frontend, names: string[], port: number): Frontend {
  const c: Frontend = JSON.parse(JSON.stringify(f))
  c.id = ''
  c.createdAt = ''
  c.updatedAt = ''
  c.hostId = undefined
  c.source = 'manual'
  c.name = uniqueName(`${f.name}-copy`, names)
  c.bind = `${splitBind(f.bind).addr || '127.0.0.1'}:${port}`
  c.rules = c.rules.map((r) => ({ ...r, id: rid() }))
  return c
}

export function uniqueName(base: string, taken: string[]) {
  const lower = new Set(taken.map((t) => t.toLowerCase()))
  let name = base
  for (let i = 2; lower.has(name.toLowerCase()); i++) name = `${base}-${i}`
  return name
}

// ---------------------------------------------------------------- binds & matching

export function splitBind(bind: string): { addr: string; port?: number } {
  const m = bind.trim().match(/^(\[[^\]]*\]|[^:]*):(\d+)$/)
  if (!m) return { addr: bind.trim() }
  return { addr: m[1].replace(/^\[|\]$/g, ''), port: Number(m[2]) }
}

export type BindKind = 'loopback' | 'lan' | 'public' | 'other'
export function bindKind(addr: string): BindKind {
  if (addr === '' || addr === '*' || addr === '0.0.0.0' || addr === '::') return 'public'
  if (addr.startsWith('127.') || addr === '::1' || addr === 'localhost') return 'loopback'
  if (/^10\./.test(addr) || /^192\.168\./.test(addr) || /^172\.(1[6-9]|2\d|3[01])\./.test(addr) || /^f[cd]/i.test(addr)) return 'lan'
  return 'other'
}

/** First port ≥ start not bound by another frontend (the server decides authoritatively). */
export function nextFreePort(frontends: Frontend[], start = 10080, selfId?: string) {
  const used = new Set(frontends.filter((f) => f.id !== selfId).map((f) => splitBind(f.bind).port))
  let p = start
  while (used.has(p)) p++
  return p
}

export function findServerStats(bs: BackendStats | undefined, srv: Server): ServerStats | undefined {
  if (!bs) return undefined
  return bs.servers.find((x) => x.id && x.id === srv.id) ?? bs.servers.find((x) => srv.name && x.name === srv.name)
}

export function frontendUsesBackend(f: Frontend, id: string) {
  return f.defaultBackendId === id || f.rules.some((r) => r.backendId === id)
}

/** Route chips for a backend card: ":443 → app.home.lan", ":5433". */
export function backendRoutes(b: Backend, frontends: Frontend[], hosts: ProxyHost[]): string[] {
  const out: string[] = []
  for (const f of frontends) {
    if (!f.enabled || !frontendUsesBackend(f, b.id)) continue
    const { addr, port } = splitBind(f.bind)
    const host = hosts.find((h) => h.id === f.hostId) ?? hosts.find((h) => h.upstream.backendId === b.id)
    if (host && bindKind(addr) === 'loopback') {
      out.push(`${host.certificateId ? ':443' : ':80'} → ${host.domains[0] ?? ''}`)
    } else if (port) {
      out.push(bindKind(addr) === 'public' || bindKind(addr) === 'loopback' ? `:${port}` : `${addr}:${port}`)
    }
  }
  return [...new Set(out)]
}

/** Names of everything that still references a backend (mirrors the delete guard). */
export function backendDependents(b: Backend, frontends: Frontend[], hosts: ProxyHost[], streams: { name: string; backendId?: string }[]): string[] {
  const out: string[] = []
  for (const f of frontends) if (frontendUsesBackend(f, b.id)) out.push(`frontend ${f.name}`)
  for (const h of hosts) {
    if (h.upstream.backendId === b.id || h.locations.some((l) => l.upstream.backendId === b.id)) out.push(h.domains[0] ?? 'a host')
  }
  for (const s of streams) if (s.backendId === b.id) out.push(`stream ${s.name}`)
  return out
}

export function certCovers(domains: string[], domain: string) {
  const d = domain.toLowerCase()
  return domains.some((c) => {
    const x = c.toLowerCase()
    if (x === d) return true
    if (x.startsWith('*.')) {
      const suffix = x.slice(1)
      return d.endsWith(suffix) && !d.slice(0, -suffix.length).includes('.') && d.length > suffix.length
    }
    return false
  })
}

export const DOMAIN_RE = /^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/
