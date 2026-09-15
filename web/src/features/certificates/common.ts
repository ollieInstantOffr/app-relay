// Shared helpers for the certs slice UI (certificates, access lists, streams, TLS settings).
import { useMemo } from 'react'
import { useQuery } from '@tanstack/react-query'
import { ApiError, api } from '../../lib/api'
import { useEntities, useSettings } from '../../lib/queries'
import { agoShort, daysUntil } from '../../lib/format'
import type { Certificate, Challenge, DNSProvider } from '../../lib/types'
import type { useToast } from '../../components/ui'

export type CertView = Certificate & { renewing?: boolean }
type Toasts = ReturnType<typeof useToast>

export const applyNow = () => window.dispatchEvent(new CustomEvent('relay:apply'))

/** "Host saved · … added to pending changes · Apply now" style toast. */
export function pendingToast(toast: Toasts, title: string, subject: string) {
  toast.show({
    kind: 'success',
    title,
    message: `${subject} added to pending changes.`,
    actions: [{ label: 'Apply now', onClick: applyNow, primary: true }],
  })
}

export function fieldErrors(err: unknown): Record<string, string> {
  return err instanceof ApiError && err.fields ? err.fields : {}
}

/** Toast for errors that aren't field validation errors. */
export function toastUnlessFields(toast: Toasts, err: unknown, title: string) {
  if (err instanceof ApiError && err.fields && Object.keys(err.fields).length) return
  toast.error(err, title)
}

export const isWildcard = (d: string) => d.startsWith('*.')

export function challengeLabel(ch?: Challenge | string): string {
  switch (ch) {
    case 'dns-01': return 'DNS-01'
    case 'http-01': return 'HTTP-01'
    case 'tls-alpn-01': return 'TLS-ALPN-01'
  }
  return ''
}

export function providerName(p: string): string {
  switch (p) {
    case 'letsencrypt': return "Let's Encrypt"
    case 'letsencrypt-staging': return "Let's Encrypt staging"
    case 'acme': return 'Custom ACME'
    case 'custom': return 'Custom'
    case 'selfsigned': return 'Self-signed'
  }
  return p
}

export const isACME = (c: Pick<Certificate, 'provider'>) => c.provider === 'letsencrypt' || c.provider === 'letsencrypt-staging' || c.provider === 'acme'

/** Host of an ACME directory URL ("ca.internal:9000"). */
export function directoryHost(url?: string): string {
  try {
    return url ? new URL(url).host : ''
  } catch {
    return url ?? ''
  }
}

/** "Let's Encrypt · DNS-01" */
export function providerText(c: Certificate): string {
  return isACME(c) && c.challenge ? `${providerName(c.provider)} · ${challengeLabel(c.challenge)}` : providerName(c.provider)
}

export function daysText(days: number): string {
  if (days < 0) return `expired ${Math.abs(days)} ${Math.abs(days) === 1 ? 'day' : 'days'} ago`
  return `${days} ${days === 1 ? 'day' : 'days'}`
}

export type Tone = 'ok' | 'warn' | 'danger' | 'muted'

export function certExpiry(c: CertView): { days?: number; tone: Tone; pct: number } {
  const days = daysUntil(c.notAfter)
  if (days === undefined) return { tone: 'muted', pct: 0 }
  const total = c.notBefore && c.notAfter ? (new Date(c.notAfter).getTime() - new Date(c.notBefore).getTime()) / 86_400_000 : 90
  const pct = Math.max(0, Math.min(100, (days / Math.max(1, total)) * 100))
  let tone: Tone
  if (days < 0 || c.status === 'expired') tone = 'danger'
  else if (days < 14 || c.status === 'failed') tone = 'warn'
  else tone = isACME(c) ? 'ok' : 'muted'
  return { days, tone, pct: tone === 'muted' ? 100 : pct }
}

/** Short reason for the table row: "DNS challenge timed out". */
export function shortError(msg?: string): string {
  if (!msg) return 'unknown error'
  if (msg.includes('TXT record not visible')) return 'DNS challenge timed out'
  if (msg.startsWith('Revoked')) return msg.split(' — ')[0]
  if (msg.includes('rate limit')) return 'rate limit reached'
  const stripped = msg.replace(/^(DNS-01|HTTP-01)( for [^:]+)?: /, '')
  return stripped.length > 64 ? stripped.slice(0, 63) + '…' : stripped
}

export function certSubtitle(c: CertView): { text: string; tone?: 'warn' | 'danger' } {
  const wildcard = c.domains.some(isWildcard)
  if (c.renewing) return { text: 'Renewing…' }
  if (c.status === 'pending') return { text: c.notAfter ? 'Retrying renewal…' : 'Requesting from ' + providerName(c.provider) + '…' }
  if (c.status === 'failed') return { text: (c.notAfter ? 'Renewal failed · ' : 'Request failed · ') + shortError(c.lastError), tone: 'warn' }
  if (c.status === 'expired') return { text: 'Expired · ' + (isACME(c) ? 'renew or request again' : 'upload a new certificate'), tone: 'danger' }
  if (!isACME(c)) return { text: `Custom upload${c.issuer ? ' · ' + c.issuer : ''}` }
  const issued = c.notBefore ? agoShort(c.notBefore) : ''
  return { text: `${wildcard ? 'Wildcard · issued' : 'Issued'} ${issued === 'now' ? 'just now' : issued + ' ago'}` }
}

export function downloadUrl(id: string, part: 'fullchain' | 'cert' | 'chain' | 'key') {
  return `/api/certificates/${id}/download?part=${part}`
}

export function triggerDownload(url: string) {
  const a = document.createElement('a')
  a.href = url
  a.rel = 'noopener'
  document.body.appendChild(a)
  a.click()
  a.remove()
}

// ---------------------------------------------------------------- DNS providers
export interface DNSFieldOption { value: string; label: string; envPrefix?: string; docsUrl?: string }
export interface DNSProviderField { key: string; label: string; secret: boolean; required: boolean; placeholder?: string; hint?: string; options?: DNSFieldOption[] }
export interface DNSProviderTypeDef {
  type: string
  label: string
  docsUrl: string
  fields: DNSProviderField[]
  requireOneOf?: string[]
  /** Caveats / what Test can verify. */
  note?: string
  /** Only admins can create or change providers of this type. */
  adminOnly?: boolean
  /** Credentials also take lego environment variables (NAME → secret value). */
  envPairs?: boolean
}
export interface DNSTestResult { status: 'ok' | 'failed' | 'unknown'; zones: string[]; error?: string }

export function useDNSProviderTypes() {
  return useQuery({ queryKey: ['dns-provider-types'], queryFn: () => api.get<DNSProviderTypeDef[]>('/api/dns-providers/types'), staleTime: Infinity })
}

export function dnsTypeLabel(types: DNSProviderTypeDef[] | undefined, type: string) {
  return types?.find((t) => t.type === type)?.label ?? type
}

/** "API token ••••8c1f · zones: example.com, home.lan" */
export function dnsSummary(p: DNSProvider, types?: DNSProviderTypeDef[]): string {
  const def = types?.find((t) => t.type === p.type)
  const secret = def?.fields.find((f) => f.secret && p.credentials[f.key])
  const parts: string[] = []
  if (secret) parts.push(`${secret.label.replace(/ \(.*\)$/, '')} ${p.credentials[secret.key]}`)
  if (p.zones?.length) parts.push(`zones: ${p.zones.slice(0, 4).join(', ')}${p.zones.length > 4 ? ` +${p.zones.length - 4}` : ''}`)
  return parts.join(' · ')
}

// ---------------------------------------------------------------- usage
export interface CertUsage { hosts: { id: string; domain: string }[]; redirects: { id: string; domain: string }[]; defaultHost: boolean }

/** Certificate usage computed from hosts, redirects and the default host settings. */
export function useCertUsage(): Map<string, CertUsage> {
  const hosts = useEntities('hosts').data
  const redirects = useEntities('redirects').data
  const defaultHost = useSettings('default_host').data
  return useMemo(() => {
    const m = new Map<string, CertUsage>()
    const get = (id: string) => {
      let u = m.get(id)
      if (!u) m.set(id, (u = { hosts: [], redirects: [], defaultHost: false }))
      return u
    }
    hosts?.forEach((h) => h.certificateId && get(h.certificateId).hosts.push({ id: h.id, domain: h.domains[0] ?? h.id }))
    redirects?.forEach((r) => r.certificateId && get(r.certificateId).redirects.push({ id: r.id, domain: (r.domains[0] ?? '') + (r.fromPath || '') }))
    if (defaultHost?.certificateId) get(defaultHost.certificateId).defaultHost = true
    return m
  }, [hosts, redirects, defaultHost])
}

export function usageCount(u?: CertUsage): number {
  return u ? u.hosts.length + u.redirects.length + (u.defaultHost ? 1 : 0) : 0
}

export function usageText(u?: CertUsage): string {
  if (!u) return '—'
  const n = u.hosts.length
  const parts: string[] = []
  if (n) parts.push(`${n} ${n === 1 ? 'host' : 'hosts'}`)
  if (u.redirects.length) parts.push(`${u.redirects.length} ${u.redirects.length === 1 ? 'redirect' : 'redirects'}`)
  if (u.defaultHost) parts.push('default host')
  return parts.length ? parts.join(' · ') : '—'
}

export function randomPassword(): string {
  const words = ['brisk', 'otter', 'amber', 'cedar', 'delta', 'ember', 'fjord', 'glade', 'harbor', 'indigo', 'juniper', 'kestrel', 'lumen', 'meadow', 'nimbus', 'onyx', 'pebble', 'quartz', 'raven', 'sierra', 'tundra', 'umber', 'vale', 'willow']
  const buf = new Uint32Array(3)
  crypto.getRandomValues(buf)
  return `${words[buf[0] % words.length]}-${words[buf[1] % words.length]}-${1000 + (buf[2] % 9000)}`
}
