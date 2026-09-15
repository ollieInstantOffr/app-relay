// Owner: slice hosts. Helpers, local API types and query hooks for the hosts feature.
import { useCallback, useEffect, useMemo, useState } from 'react'
import { keepPreviousData, useQueries, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, ApiError, errorMessage } from '../../lib/api'
import { keys } from '../../lib/queries'
import type {
  AccessList, Certificate, ConfigPreview, HealthState, HealthStatus, HostDefaults, HostMetrics, Location, ProxyHost, Redirect,
  TLSSettings,
} from '../../lib/types'
import { daysUntil, rid, upstreamUrl } from '../../lib/format'
import type { ToastAction } from '../../components/ui'

// ---------------------------------------------------------------- local API types

export type HostDraft = Omit<ProxyHost, 'id' | 'createdAt' | 'updatedAt'> & Partial<Pick<ProxyHost, 'id' | 'createdAt' | 'updatedAt'>>
export type RedirectDraft = Omit<Redirect, 'id' | 'createdAt' | 'updatedAt'> & Partial<Pick<Redirect, 'id' | 'createdAt' | 'updatedAt'>>

/** POST /api/hosts/check-domain */
export interface DomainCheck {
  domain: string
  valid: boolean
  error?: string
  conflict?: { kind: 'host' | 'redirect'; id: string; name: string }
  message?: string
}

/** GET /api/hosts/{id}/usage */
export interface HostUsage {
  certificateId: string
  certificateName: string
  certificateSharedWith: string[]
  locations: number
  defaultHostAction: 'close' | '404' | 'redirect' | 'host' | string
  defaultHostTarget?: string
  isDefaultHost: boolean
}

/** POST /api/hosts/bulk */
export type BulkAction = 'enable' | 'disable' | 'delete' | 'attach_access_list' | 'set_certificate'
export interface BulkResult {
  results: { id: string; name: string; ok: boolean; error?: string }[]
  ok: number
  failed: number
}

export type HostTab = 'details' | 'ssl' | 'locations' | 'advanced'
export const HOST_TABS: HostTab[] = ['details', 'ssl', 'locations', 'advanced']

// ---------------------------------------------------------------- drafts

export function newLocation(): Location {
  return {
    id: rid(), path: '', kind: 'proxy', upstream: { scheme: 'http', host: '', port: 0 },
    websockets: false, stripPrefix: false, cache: false, noAuth: false, headers: [],
  }
}

export function newHost(d?: Partial<HostDefaults>): HostDraft {
  return {
    domains: [],
    enabled: true,
    upstream: { scheme: 'http', host: '', port: 0 },
    websockets: d?.websockets ?? true,
    blockExploits: d?.blockExploits ?? true,
    cacheAssets: false,
    accessListId: d?.accessListId || undefined,
    certificateId: d?.certificateId || undefined,
    forceHttps: d?.forceHttps ?? true,
    http2: d?.http2 ?? true,
    http3: null,
    hsts: 'inherit',
    upstreamTlsVerify: false,
    cipherProfile: '',
    locations: [],
    forwardAuth: { enabled: false, provider: 'authelia', verifyUrl: '', signInUrl: '', passRemoteUser: true, passRemoteGroups: true, skipWellKnown: true },
    rateLimit: { enabled: false, requestsPerSecond: 30, burst: 60 },
    geoBlock: { enabled: false, allowCountries: [] },
    noIndex: false,
    maxBodySize: '',
    proxyReadTimeout: 0,
    proxySendTimeout: 0,
    customNginx: '',
    source: 'manual',
  }
}

/** Fills missing nested fields of a stored host so forms never see undefined. */
export function hostToDraft(h: ProxyHost): HostDraft {
  const base = newHost()
  return {
    ...base,
    ...h,
    domains: h.domains ?? [],
    upstream: { ...base.upstream, ...h.upstream },
    locations: (h.locations ?? []).map((l) => ({ ...newLocation(), ...l, upstream: { ...newLocation().upstream, ...l.upstream }, headers: l.headers ?? [] })),
    forwardAuth: { ...base.forwardAuth, ...h.forwardAuth },
    rateLimit: { ...base.rateLimit, ...h.rateLimit },
    geoBlock: { ...base.geoBlock, ...h.geoBlock, allowCountries: h.geoBlock?.allowCountries ?? [] },
    hsts: h.hsts || 'inherit',
    http3: h.http3 ?? null,
    cipherProfile: h.cipherProfile ?? '',
  }
}

export function newRedirect(): RedirectDraft {
  return { domains: [], fromPath: '', to: '', code: 301, keepPath: true, forceHttps: false, enabled: true }
}

// ---------------------------------------------------------------- validation (mirrors internal/model/validate_host.go)

const DOMAIN_LABEL = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/
const IPV4 = /^(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}$/

export function isIPv4(s: string): boolean {
  return IPV4.test(s)
}

export function isIPv6(s: string): boolean {
  if (!s.includes(':') || !/^[0-9a-fA-F:.]+$/.test(s)) return false
  try {
    new URL(`http://[${s}]/`)
    return true
  } catch {
    return false
  }
}

export function normalizeDomain(d: string): string {
  return d.trim().toLowerCase().replace(/\.$/, '')
}

export function domainError(d: string): string {
  if (!d) return 'Domain is required'
  if (d.length > 253) return 'Domain is too long (max 253 characters)'
  if (d.includes('://') || /[/:]/.test(d)) return 'Enter the domain only — no scheme, port or path'
  if (/\s/.test(d)) return 'Domain must not contain spaces'
  if (d !== d.toLowerCase()) return 'Domain must be lowercase'
  let name = d
  const wild = name.startsWith('*.')
  if (wild) name = name.slice(2)
  if (name.includes('*')) return 'Wildcards are only allowed as the first label (*.home.lan)'
  if (!name) return 'Wildcard needs a domain after *.'
  if (!wild && isIPv4(name)) return ''
  for (const l of name.split('.')) {
    if (!l) return 'Domain has an empty label (two dots in a row?)'
    if (l.length > 63) return 'Each part of a domain must be at most 63 characters'
    if (!DOMAIN_LABEL.test(l)) return `${d} is not a valid domain name`
  }
  return ''
}

export function upstreamHostError(h: string): string {
  if (!h) return 'Upstream host is required'
  if (h.includes('://')) return 'Enter the host only — pick the scheme separately'
  if (/[\s/"'{};]/.test(h)) return 'Not a valid host or IP address'
  if (h.startsWith('[') || h.endsWith(']')) {
    const inner = h.replace(/^\[/, '').replace(/\]$/, '')
    return isIPv6(inner) ? '' : 'Not a valid IPv6 address'
  }
  if (isIPv4(h) || isIPv6(h)) return ''
  if (h.includes(':')) return 'Not a valid IPv6 address'
  if (/^[0-9.]+$/.test(h)) return 'Not a valid IPv4 address'
  if (h.length > 253) return 'Host name is too long'
  for (const l of h.split('.')) {
    if (!l || !/^[a-zA-Z0-9_][a-zA-Z0-9_-]{0,62}$/.test(l) || l.endsWith('-')) return 'Not a valid host name or IP address'
  }
  return ''
}

export function portError(p: number): string {
  if (!p) return 'Port is required'
  return p >= 1 && p <= 65535 && Number.isInteger(p) ? '' : 'Port must be between 1 and 65535'
}

/** Lenient absolute-URL check; nginx variables ($host, $request_uri) are allowed like on the server. */
export function urlError(s: string): string {
  if (!s) return 'URL is required'
  if (/[\s"'{};\\]/.test(s)) return 'URL must not contain spaces, quotes, braces or semicolons'
  if (!/^https?:\/\/[^/?#]+/i.test(s)) return 'Enter a full URL starting with http:// or https://'
  return ''
}

/** Maps a server field path to the drawer tab that renders it. */
export function tabForField(field: string): HostTab {
  if (field.startsWith('locations')) return 'locations'
  if (/^(certificateId|forceHttps|http3|hsts|upstreamTlsVerify|cipherProfile)/.test(field)) return 'ssl'
  if (/^(forwardAuth|rateLimit|geoBlock|noIndex|maxBodySize|proxyReadTimeout|proxySendTimeout|customNginx)/.test(field)) return 'advanced'
  return 'details'
}

/** Errors for fields no tab renders inline. */
export const UNRENDERED_FIELDS = /^(enabled|source|upstream\.(backendId|path|scheme))$/

// ---------------------------------------------------------------- certificates

export function certCovers(c: Certificate, domain: string): boolean {
  const d = domain.toLowerCase()
  return (c.domains ?? []).some((san) => {
    const s = san.toLowerCase()
    if (s === d) return true
    if (!s.startsWith('*.')) return false
    const base = s.slice(2)
    if (d.startsWith('*.')) return d.slice(2) === base
    const i = d.indexOf('.')
    return i > 0 && d.slice(i + 1) === base
  })
}

export function certCoversAll(c: Certificate, domains: string[]): boolean {
  return domains.length > 0 && domains.every((d) => certCovers(c, d))
}

export const isWildcardCert = (c: Certificate) => (c.domains ?? []).some((d) => d.startsWith('*.'))

export function providerLabel(p: string): string {
  switch (p) {
    case 'letsencrypt': return "Let's Encrypt"
    case 'letsencrypt-staging': return "Let's Encrypt staging"
    case 'custom': return 'Custom'
    case 'selfsigned': return 'Self-signed'
  }
  return p
}

export function providerShort(p: string): string {
  switch (p) {
    case 'letsencrypt': return 'LE'
    case 'letsencrypt-staging': return 'LE staging'
    case 'custom': return 'custom'
    case 'selfsigned': return 'self-signed'
  }
  return p
}

export function certDays(c: Certificate): number | undefined {
  return daysUntil(c.notAfter)
}

/** "cloud.home.lan · Let's Encrypt · 59 days · auto-renew" */
export function certOptionLabel(c: Certificate): string {
  const parts = [c.name, providerLabel(c.provider)]
  const d = certDays(c)
  if (c.status === 'pending' && d === undefined) parts.push('issuing…')
  else if (c.status === 'failed' && d === undefined) parts.push('failed')
  else if (d !== undefined) parts.push(d < 0 ? 'expired' : `${d} ${d === 1 ? 'day' : 'days'}`)
  if (c.autoRenew && c.provider.startsWith('letsencrypt')) parts.push('auto-renew')
  return parts.join(' · ')
}

/** "Let's Encrypt · expires in 88 d · auto-renews" */
export function certTooltip(c: Certificate): string {
  const parts = [providerLabel(c.provider)]
  const d = certDays(c)
  if (d !== undefined) parts.push(d < 0 ? `expired ${-d} d ago` : `expires in ${d} d`)
  if (c.status === 'pending') parts.push('issuing…')
  if (c.status === 'failed') parts.push(c.lastError ? `renewal failed: ${c.lastError}` : 'renewal failed')
  if (c.autoRenew && c.provider.startsWith('letsencrypt')) parts.push('auto-renews')
  return parts.join(' · ')
}

/** Certificates covering all domains first (valid, exact before wildcard), then the rest. */
export function rankCerts(certs: Certificate[], domains: string[]): { matching: Certificate[]; other: Certificate[] } {
  const score = (c: Certificate) => (certCoversAll(c, domains) ? 0 : 4) + (c.status === 'valid' ? 0 : 2) + (isWildcardCert(c) ? 1 : 0)
  const sorted = [...certs].sort((a, b) => score(a) - score(b) || a.name.localeCompare(b.name))
  const matching = sorted.filter((c) => certCoversAll(c, domains))
  return { matching, other: sorted.filter((c) => !matching.includes(c)) }
}

export function bestCert(certs: Certificate[], domains: string[]): Certificate | undefined {
  return rankCerts(certs, domains).matching.find((c) => c.status === 'valid')
}

// ---------------------------------------------------------------- access lists

/** "192.168.0.0/16, 10.0.0.0/8 · basic auth" */
export function accessSummary(l: AccessList): string {
  const rules = l.rules ?? []
  const allows = rules.filter((r) => r.action === 'allow' && r.cidr !== 'all').map((r) => r.cidr)
  const parts: string[] = []
  if (allows.length) parts.push(allows.slice(0, 3).join(', ') + (allows.length > 3 ? ` +${allows.length - 3}` : ''))
  else if (rules.length) parts.push(`${rules.length} ${rules.length === 1 ? 'rule' : 'rules'}`)
  if (l.basicAuth?.enabled) parts.push('basic auth')
  return parts.join(' · ')
}

// ---------------------------------------------------------------- health & probe

export function hostHealth(h: ProxyHost, health?: Record<string, HealthStatus>): { state: HealthState; status?: HealthStatus } {
  if (!h.enabled) return { state: 'disabled' }
  const s = health?.[`host:${h.id}`]
  const state = !s || s.status === 'disabled' ? 'unknown' : s.status
  return { state, status: s }
}

const STATUS_TEXT: Record<number, string> = {
  200: 'OK', 201: 'Created', 204: 'No Content', 301: 'Moved Permanently', 302: 'Found', 303: 'See Other', 304: 'Not Modified',
  307: 'Temporary Redirect', 308: 'Permanent Redirect', 400: 'Bad Request', 401: 'Unauthorized', 403: 'Forbidden', 404: 'Not Found',
  405: 'Method Not Allowed', 408: 'Request Timeout', 429: 'Too Many Requests', 500: 'Internal Server Error', 502: 'Bad Gateway',
  503: 'Service Unavailable', 504: 'Gateway Timeout',
}

export function httpStatusText(code: number): string {
  return STATUS_TEXT[code] ? `${code} ${STATUS_TEXT[code]}` : String(code)
}

/** "Upstream reachable · 200 OK · 12 ms" */
export function probeMessage(s: HealthStatus): { tone: 'ok' | 'warn' | 'danger'; text: string } {
  const code = s.httpStatus ? httpStatusText(s.httpStatus) : ''
  const latency = s.latencyMs ? `${Math.round(s.latencyMs)} ms` : ''
  switch (s.status) {
    case 'healthy':
      return { tone: 'ok', text: ['Upstream reachable', code, latency].filter(Boolean).join(' · ') }
    case 'degraded':
      return { tone: 'warn', text: ['Upstream responding with problems', code, s.detail, latency].filter(Boolean).join(' · ') }
    case 'down':
      return { tone: 'danger', text: ['Upstream unreachable', code, s.detail].filter(Boolean).join(' · ') }
  }
  return { tone: 'warn', text: ["Couldn't check upstream", s.detail].filter(Boolean).join(' · ') }
}

/** "18 min", "3 h", "2 d" since an ISO time. */
export function sinceShort(iso?: string): string {
  if (!iso) return ''
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000)
  if (s < 60) return 'just now'
  if (s < 3600) return `${Math.round(s / 60)} min`
  if (s < 86400) return `${Math.round(s / 3600)} h`
  return `${Math.round(s / 86400)} d`
}

// ---------------------------------------------------------------- sizes & durations

export type SizeUnit = '' | 'k' | 'm' | 'g' | '0'

/** "10g" → {num: "10", unit: "g"}; "" → default; "0" → unlimited. */
export function parseSize(s: string): { num: string; unit: SizeUnit } {
  if (!s) return { num: '', unit: '' }
  if (s === '0') return { num: '', unit: '0' }
  const m = /^(\d+)([kmg]?)$/i.exec(s.trim())
  if (!m) return { num: s, unit: 'm' }
  if (!m[2]) return { num: String(Math.max(1, Math.ceil(Number(m[1]) / 1024))), unit: 'k' }
  return { num: m[1], unit: m[2].toLowerCase() as SizeUnit }
}

export function formatSize(num: string, unit: SizeUnit): string {
  if (unit === '' || unit === '0') return unit
  return num ? `${num}${unit}` : ''
}

/** "10g" → "10 GB" */
export function sizeLabel(s: string, space = true): string {
  if (s === '0') return 'unlimited'
  const { num, unit } = parseSize(s)
  const u = unit === 'k' ? 'KB' : unit === 'g' ? 'GB' : 'MB'
  return `${num}${space ? ' ' : ''}${u}`
}

/** 15768000 → "6 months" */
export function humanAge(sec: number): string {
  if (sec >= 31_536_000) {
    const y = Math.round(sec / 31_536_000)
    return `${y} ${y === 1 ? 'year' : 'years'}`
  }
  if (sec >= 2_592_000) {
    const m = Math.round(sec / 2_628_000)
    return `${m} ${m === 1 ? 'month' : 'months'}`
  }
  const d = Math.max(1, Math.round(sec / 86400))
  return `${d} ${d === 1 ? 'day' : 'days'}`
}

export function capitalize(s?: string): string {
  return s ? s[0].toUpperCase() + s.slice(1) : ''
}

// ---------------------------------------------------------------- list presentation

export interface ViewCtx {
  health?: Record<string, HealthStatus>
  metrics?: HostMetrics
  metricsFailed: boolean
  certs: Map<string, Certificate>
  lists: Map<string, AccessList>
  backends: Map<string, string>
  tls?: TLSSettings
}

export interface Chip {
  label: string
  tone?: 'ok' | 'warn' | 'danger'
  tooltip?: string
}

export function tlsChip(h: ProxyHost, ctx: ViewCtx): Chip {
  if (!h.certificateId) return { label: 'HTTP only' }
  const c = ctx.certs.get(h.certificateId)
  if (!c) return { label: 'TLS · missing', tone: 'danger', tooltip: 'The selected certificate no longer exists' }
  const tooltip = certTooltip(c)
  const d = certDays(c)
  if (d === undefined) {
    if (c.status === 'failed') return { label: 'TLS · failed', tone: 'danger', tooltip }
    return { label: 'TLS · pending', tone: 'warn', tooltip }
  }
  if (d < 0) return { label: 'TLS · expired', tone: 'danger', tooltip }
  if (d < 14) return { label: `TLS · ${d}d left`, tone: 'warn', tooltip }
  return { label: `TLS · ${providerShort(c.provider)}`, tone: 'ok', tooltip }
}

export function effectiveHsts(h: ProxyHost | HostDraft, tls?: TLSSettings): boolean {
  if (!h.certificateId) return false
  return h.hsts === 'on' || (h.hsts === 'inherit' && !!tls?.hsts?.enabled)
}

function listIsBasicAuthOnly(l: AccessList) {
  return (l.rules ?? []).length === 0 && !!l.basicAuth?.enabled
}

export function featureChips(h: ProxyHost, ctx: ViewCtx): Chip[] {
  const out: string[] = []
  if (h.websockets) out.push('websockets')
  const list = h.accessListId ? ctx.lists.get(h.accessListId) : undefined
  if (list) out.push(listIsBasicAuthOnly(list) ? 'basic auth' : list.name)
  if (h.forwardAuth?.enabled) out.push('sso')
  if (effectiveHsts(h, ctx.tls)) out.push('HSTS')
  if (h.maxBodySize && h.maxBodySize !== '0') out.push(`${sizeLabel(h.maxBodySize)} body`)
  if (h.rateLimit?.enabled) out.push('rate limit')
  if (h.geoBlock?.enabled) out.push('geo-block')
  if (h.locations?.length) out.push(`${h.locations.length} loc`)
  if (h.noIndex) out.push('noindex')
  if (h.blockExploits) out.push('block exploits')
  return out.map((label) => ({ label }))
}

/** Table "Features" column: "ws · h2", "hsts · 10GB · 3 loc", "rate · noindex". */
export function featureSummary(h: ProxyHost, ctx: ViewCtx): string {
  const parts: string[] = []
  if (h.websockets) parts.push('ws')
  if (h.http2 && h.certificateId) parts.push('h2')
  if (effectiveHsts(h, ctx.tls)) parts.push('hsts')
  if (h.maxBodySize && h.maxBodySize !== '0') parts.push(sizeLabel(h.maxBodySize, false))
  if (h.locations?.length) parts.push(`${h.locations.length} loc`)
  const list = h.accessListId ? ctx.lists.get(h.accessListId) : undefined
  if (list?.basicAuth?.enabled) parts.push('basic auth')
  if (h.forwardAuth?.enabled) parts.push('sso')
  if (h.rateLimit?.enabled) parts.push('rate')
  if (h.geoBlock?.enabled) parts.push('geo')
  if (h.noIndex) parts.push('noindex')
  return parts.join(' · ')
}

/** Table "TLS" column: "LE · 88d" / "none". */
export function tlsSummary(h: ProxyHost, ctx: ViewCtx): { text: string; tone?: 'warn' | 'danger' } {
  if (!h.certificateId) return { text: 'none' }
  const c = ctx.certs.get(h.certificateId)
  if (!c) return { text: 'missing', tone: 'danger' }
  const d = certDays(c)
  if (d === undefined) return { text: `${providerShort(c.provider)} · ${c.status}`, tone: c.status === 'failed' ? 'danger' : 'warn' }
  if (d < 0) return { text: `${providerShort(c.provider)} · expired`, tone: 'danger' }
  return { text: `${providerShort(c.provider)} · ${d}d`, tone: d < 14 ? 'warn' : undefined }
}

export function upstreamText(u: ProxyHost['upstream'], backends: Map<string, string>): string {
  if (u.backendId) return `lb → ${backends.get(u.backendId) ?? u.backendId}`
  return upstreamUrl(u)
}

/** First non-wildcard domain (for "Open in new tab"). */
export function openableDomain(h: ProxyHost): string | undefined {
  return h.domains.find((d) => !d.startsWith('*.'))
}

/** Extracts `location … { … }` blocks from rendered nginx config. */
export function extractLocationBlocks(config: string): string {
  const lines = config.split('\n')
  const blocks: string[][] = []
  for (let i = 0; i < lines.length; i++) {
    if (!/^\s*location\s/.test(lines[i])) continue
    const block: string[] = []
    let depth = 0
    for (let j = i; j < lines.length; j++) {
      block.push(lines[j])
      depth += (lines[j].match(/\{/g) ?? []).length - (lines[j].match(/\}/g) ?? []).length
      if (depth <= 0 && lines[j].includes('}')) {
        i = j
        break
      }
    }
    blocks.push(block)
  }
  if (!blocks.length) return ''
  const indent = Math.min(...blocks.map((b) => /^\s*/.exec(b[0])![0].length))
  return blocks.map((b) => b.map((l) => l.slice(Math.min(indent, /^\s*/.exec(l)![0].length))).join('\n')).join('\n')
}

// ---------------------------------------------------------------- misc UI helpers

export const applyNowAction: ToastAction = {
  label: 'Apply now',
  primary: true,
  onClick: () => window.dispatchEvent(new CustomEvent('relay:apply')),
}

export function isTyping(e: KeyboardEvent): boolean {
  const el = e.target as HTMLElement | null
  if (!el) return false
  return el.isContentEditable || ['INPUT', 'TEXTAREA', 'SELECT'].includes(el.tagName)
}

export function overlayOpen(): boolean {
  return !!document.querySelector('.drawer, .dialog, .menu')
}

// ---------------------------------------------------------------- hooks

export function useDebounced<T>(value: T, ms: number): T {
  const [v, setV] = useState(value)
  useEffect(() => {
    const t = window.setTimeout(() => setV(value), ms)
    return () => window.clearTimeout(t)
  }, [value, ms])
  return v
}

export function useLocalStorage<T>(key: string, initial: T): [T, (v: T | ((prev: T) => T)) => void] {
  const [value, setValue] = useState<T>(() => {
    try {
      const raw = localStorage.getItem(key)
      return raw ? (JSON.parse(raw) as T) : initial
    } catch {
      return initial
    }
  })
  const set = useCallback(
    (next: T | ((prev: T) => T)) => {
      setValue((prev) => {
        const v = typeof next === 'function' ? (next as (p: T) => T)(prev) : next
        try {
          localStorage.setItem(key, JSON.stringify(v))
        } catch {
          /* storage unavailable */
        }
        return v
      })
    },
    [key],
  )
  return [value, set]
}

export function useInvalidateHosts() {
  const qc = useQueryClient()
  return useCallback(() => {
    qc.invalidateQueries({ queryKey: keys.entities('hosts') })
    qc.invalidateQueries({ queryKey: keys.pending })
    qc.invalidateQueries({ queryKey: ['hosts'] })
  }, [qc])
}

export function useHostMetrics() {
  return useQuery({
    queryKey: ['metrics', 'hosts'],
    queryFn: () => api.get<HostMetrics>('/api/metrics/hosts'),
    refetchInterval: 60_000,
    retry: false,
  })
}

/** Unknown-host hits (24 h) from the observe slice's overview metrics, or null when not reported. */
export function useUnknownHostHits() {
  return useQuery({
    queryKey: ['hosts', 'unknown-host-hits'],
    queryFn: async () => {
      const o = await api.get<Record<string, unknown>>('/api/metrics/overview?range=24h')
      const n = o?.unknownHostHits
      return typeof n === 'number' ? n : null
    },
    retry: false,
    staleTime: 60_000,
  })
}

/** Debounced live upstream probe ("Upstream reachable · 200 OK · 12 ms"). */
export function useProbe(u: { scheme: string; host: string; port: number } | null, enabled = true) {
  const key = enabled && u && !upstreamHostError(u.host) && !portError(u.port) ? `${u.scheme}://${u.host}:${u.port}` : ''
  const debounced = useDebounced(key, 700)
  const q = useQuery({
    queryKey: ['hosts', 'probe', debounced],
    queryFn: () => {
      const [scheme, rest] = debounced.split('://')
      const i = rest.lastIndexOf(':')
      return api.post<HealthStatus>('/api/health/probe', { upstream: { scheme, host: rest.slice(0, i), port: Number(rest.slice(i + 1)) } })
    },
    enabled: debounced !== '' && debounced === key,
    retry: false,
    staleTime: 15_000,
  })
  const settled = key !== '' && key === debounced
  return {
    active: key !== '',
    checking: key !== '' && (!settled || q.isFetching),
    result: settled && !q.isFetching ? q.data : undefined,
    unavailable: settled && q.isError,
    refetch: q.refetch,
  }
}

export interface PreviewState {
  status: 'idle' | 'loading' | 'ready' | 'unavailable'
  preview?: ConfigPreview
  error?: string
  hint?: string
}

/** Debounced POST /api/preview/nginx/host for the drawer draft. */
export function useConfigPreview(host: HostDraft, enabled: boolean): PreviewState {
  const incomplete = host.domains.length === 0 || !host.upstream.host || !host.upstream.port
  const json = useMemo(() => JSON.stringify(host), [host])
  const debounced = useDebounced(json, 600)
  const q = useQuery({
    queryKey: ['hosts', 'preview', debounced],
    queryFn: () => api.post<ConfigPreview>('/api/preview/nginx/host', { host: JSON.parse(debounced) }),
    enabled: enabled && !incomplete,
    retry: false,
    staleTime: Infinity,
    gcTime: 60_000,
    placeholderData: keepPreviousData,
  })
  if (!enabled) return { status: 'idle' }
  if (incomplete) return { status: 'idle', hint: 'Add a domain and an upstream to preview the generated config.' }
  if (q.isError) {
    if (q.error instanceof ApiError && q.error.status === 422) {
      return { status: 'ready', preview: { config: '', valid: false, output: q.error.message } }
    }
    return { status: 'unavailable', error: errorMessage(q.error) }
  }
  if (!q.data || json !== debounced || q.isFetching) return { status: 'loading', preview: q.data }
  return { status: 'ready', preview: q.data }
}

/** Live domain conflict checks; returns domain → message for conflicting domains. */
export function useDomainChecks(
  domains: string[],
  opts: { kind: 'host' | 'redirect'; excludeId?: string; fromPath?: string; enabled?: boolean },
): Record<string, string> {
  const results = useQueries({
    queries: domains.map((d) => ({
      queryKey: ['hosts', 'check-domain', opts.kind, d, opts.excludeId ?? '', opts.fromPath ?? ''],
      queryFn: () =>
        api.post<DomainCheck>('/api/hosts/check-domain', { domain: d, excludeId: opts.excludeId ?? '', kind: opts.kind, fromPath: opts.fromPath ?? '' }),
      enabled: opts.enabled !== false && !domainError(d),
      retry: false,
      staleTime: 5_000,
    })),
  })
  const out: Record<string, string> = {}
  results.forEach((r, i) => {
    if (r.data?.conflict && r.data.message) out[domains[i]] = r.data.message
  })
  return out
}
