// Owner: slice observe — URL-backed filters for the Logs tabs and client-side
// matching of live rows (mirrors internal/logs/filter.go).
import type { AccessEntry } from '../../lib/types'

// ---------------------------------------------------------------- access
export const ACCESS_TOKEN_KEYS = ['host', 'status', 'ip', 'method', 'kind'] as const
export type AccessTokenKey = (typeof ACCESS_TOKEN_KEYS)[number]

export interface AccessFilters {
  host?: string
  status?: string
  ip?: string
  method?: string
  kind?: string
  q?: string
  range: string
}

export const ACCESS_RANGES = [
  { value: '15m', label: 'Last 15m' },
  { value: '1h', label: 'Last 1h' },
  { value: '24h', label: 'Last 24h' },
  { value: '7d', label: 'Last 7d' },
]
export const DEFAULT_ACCESS_RANGE = '1h'

const param = (sp: URLSearchParams, k: string) => sp.get(k)?.trim() || undefined

export function readAccessFilters(sp: URLSearchParams): AccessFilters {
  const range = param(sp, 'range')
  return {
    host: param(sp, 'host'),
    status: param(sp, 'status'),
    ip: param(sp, 'ip'),
    method: param(sp, 'method'),
    kind: param(sp, 'kind'),
    q: param(sp, 'q'),
    range: range && ACCESS_RANGES.some((r) => r.value === range) ? range : DEFAULT_ACCESS_RANGE,
  }
}

export function accessApiParams(f: AccessFilters) {
  return { host: f.host, status: f.status, ip: f.ip, method: f.method, kind: f.kind, q: f.q, since: f.range }
}

const TOKEN_RE = /^(host|status|ip|method|kind):(.+)$/i

/** "host:a.lan status:>=500 wp-login" → tokens {host, status} + rest "wp-login". */
export function parseFilterText(text: string): { tokens: Partial<Record<AccessTokenKey, string>>; rest: string } {
  const tokens: Partial<Record<AccessTokenKey, string>> = {}
  const rest: string[] = []
  for (const word of text.split(/\s+/)) {
    if (!word) continue
    const m = TOKEN_RE.exec(word)
    if (m) tokens[m[1].toLowerCase() as AccessTokenKey] = m[2]
    else rest.push(word)
  }
  return { tokens, rest: rest.join(' ') }
}

/** Status expression → predicate; null when the expression is invalid. */
export function statusMatcher(expr: string | undefined): ((s: number) => boolean) | null {
  const norm = (expr ?? '').replace(/≥/g, '>=').replace(/≤/g, '<=').replace(/\s+/g, '')
  if (!norm) return () => true
  const pos: ((s: number) => boolean)[] = []
  const neg: ((s: number) => boolean)[] = []
  for (const part of norm.split(',')) {
    if (!part) continue
    let op = '='
    let rest = part
    for (const p of ['>=', '<=', '!=', '>', '<', '=', '!']) {
      if (part.startsWith(p)) {
        op = p === '!' ? '!=' : p
        rest = part.slice(p.length)
        break
      }
    }
    const cls = /^([1-5])xx$/i.exec(rest)
    if (cls) {
      const lo = Number(cls[1]) * 100
      const hi = lo + 100
      if (op === '=') pos.push((s) => s >= lo && s < hi)
      else if (op === '!=') neg.push((s) => s < lo || s >= hi)
      else if (op === '>=') pos.push((s) => s >= lo)
      else if (op === '>') pos.push((s) => s >= hi)
      else if (op === '<') pos.push((s) => s < lo)
      else if (op === '<=') pos.push((s) => s < hi)
      continue
    }
    if (!/^\d{1,3}$/.test(rest)) return null
    const n = Number(rest)
    if (op === '!=') neg.push((s) => s !== n)
    else if (op === '>=') pos.push((s) => s >= n)
    else if (op === '>') pos.push((s) => s > n)
    else if (op === '<') pos.push((s) => s < n)
    else if (op === '<=') pos.push((s) => s <= n)
    else pos.push((s) => s === n)
  }
  return (s) => (pos.length === 0 || pos.some((f) => f(s))) && neg.every((f) => f(s))
}

export function hostMatches(filter: string, host: string): boolean {
  const f = filter.toLowerCase()
  const h = host.toLowerCase()
  if (f.startsWith('*.') || f.startsWith('.')) {
    const base = f.replace(/^\*?\./, '')
    return h === base || h.endsWith('.' + base)
  }
  return h === f
}

export function accessMatcher(f: AccessFilters): (e: AccessEntry) => boolean {
  const status = statusMatcher(f.status)
  const q = f.q?.toLowerCase()
  return (e) => {
    if (f.host && !hostMatches(f.host, e.host)) return false
    if (f.status && (!status || !status(e.status))) return false
    if (f.ip && e.clientIp !== f.ip) return false
    if (f.method && e.method.toUpperCase() !== f.method.toUpperCase()) return false
    if (f.kind && f.kind !== 'all' && e.kind !== f.kind) return false
    if (q && ![e.path, e.userAgent, e.clientIp, e.host, e.referer].some((v) => v?.toLowerCase().includes(q))) return false
    return true
  }
}

// ---------------------------------------------------------------- error log
export const LEVELS = ['emerg', 'alert', 'crit', 'error', 'warn', 'notice', 'info', 'debug']

export const ERROR_RANGES = [
  { value: '1h', label: 'Last 1h' },
  { value: '24h', label: 'Last 24h' },
  { value: '7d', label: 'Last 7d' },
  { value: '30d', label: 'Last 30d' },
]

export const ERROR_LEVEL_OPTIONS = [
  { value: '', label: 'All levels' },
  { value: 'warn+', label: 'Warnings & worse' },
  { value: 'error+', label: 'Errors & worse' },
]

export const ERROR_SOURCE_OPTIONS = [
  { value: '', label: 'All sources' },
  { value: 'nginx', label: 'nginx' },
  { value: 'edge', label: 'Relay Edge' },
  { value: 'haproxy', label: 'haproxy' },
  { value: 'relay', label: 'relay' },
]

export interface ErrorFilters {
  source?: string
  level?: string
  q?: string
  range: string
}

export function readErrorFilters(sp: URLSearchParams): ErrorFilters {
  const range = param(sp, 'range')
  return {
    source: param(sp, 'source'),
    level: param(sp, 'level'),
    q: param(sp, 'q'),
    range: range && ERROR_RANGES.some((r) => r.value === range) ? range : '24h',
  }
}

export function errorApiParams(f: ErrorFilters) {
  return { source: f.source, level: f.level, q: f.q, since: f.range }
}

/** "warn+" → emerg…warn; "error" → {error}; empty → null (all). */
export function levelSet(spec: string | undefined): Set<string> | null {
  if (!spec) return null
  if (spec.endsWith('+')) {
    const i = LEVELS.indexOf(spec.slice(0, -1))
    return new Set(i >= 0 ? LEVELS.slice(0, i + 1) : [])
  }
  return new Set(spec.split(',').map((s) => s.trim()))
}

// ---------------------------------------------------------------- audit
export const AUDIT_RANGES = [
  { value: '24h', label: 'Last 24 hours' },
  { value: '7d', label: 'Last 7 days' },
  { value: '30d', label: 'Last 30 days' },
  { value: '90d', label: 'Last 90 days' },
  { value: 'all', label: 'All time' },
]

export const AUDIT_ACTOR_OPTIONS = [
  { value: '', label: 'All actors' },
  { value: 'user', label: 'People' },
  { value: 'mcp', label: 'AI clients (MCP)' },
  { value: 'token', label: 'API tokens' },
  { value: 'system', label: 'System' },
  { value: 'docker', label: 'Docker' },
]

export interface AuditFilters {
  q?: string
  actor?: string
  range: string
}

export function readAuditFilters(sp: URLSearchParams): AuditFilters {
  const range = param(sp, 'range')
  return { q: param(sp, 'q'), actor: param(sp, 'actor'), range: range && AUDIT_RANGES.some((r) => r.value === range) ? range : '7d' }
}

export function auditApiParams(f: AuditFilters) {
  return { q: f.q, actorType: f.actor, since: f.range === 'all' ? undefined : f.range }
}
