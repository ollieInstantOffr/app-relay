// Public DNS (slice dns): zones and records at the user's DNS provider, domain checks and sync.
import { useCallback, useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ApiError, api } from '../../lib/api'
import { useSettings } from '../../lib/queries'
import type { DNSCheckResponse, DNSDomainCheck, DNSRecord, DNSRecordInput, DNSRecordList, DNSStatus } from '../../lib/types'
import type { Tone } from '../../components/ui'

export const dnsKeys = {
  all: ['dns'] as const,
  providerTypes: ['dns', 'provider-types'] as const,
  status: ['dns', 'status'] as const,
  records: (zone: string) => ['dns', 'records', zone] as const,
  checks: ['dns', 'check'] as const,
  check: (domains: string[]) => ['dns', 'check', ...domains] as const,
}

const enc = encodeURIComponent

/** Public DNS settings (readable by every signed-in user). */
export function usePublicDNS() {
  const q = useSettings('public_dns')
  return { settings: q.data, enabled: !!q.data?.enabled, isLoading: q.isLoading }
}

/** Provider types that support Public DNS, e.g. ["godaddy", "cloudflare"]. */
export function usePublicDNSProviderTypes(enabled = true) {
  return useQuery({
    queryKey: dnsKeys.providerTypes,
    queryFn: () => api.get<string[]>('/api/dns/provider-types'),
    enabled,
    staleTime: Infinity,
    retry: false,
  })
}

export function useDNSStatus(enabled = true) {
  return useQuery({ queryKey: dnsKeys.status, queryFn: () => api.get<DNSStatus>('/api/dns/status'), enabled, staleTime: 30_000, retry: false })
}

export function useDNSRecords(zone: string | undefined) {
  return useQuery({
    queryKey: dnsKeys.records(zone ?? ''),
    queryFn: () => api.get<DNSRecordList>(`/api/dns/zones/${enc(zone!)}/records`),
    enabled: !!zone,
    staleTime: 15_000,
    retry: false,
  })
}

export const createDNSRecord = (zone: string, input: DNSRecordInput) => api.post<DNSRecord>(`/api/dns/zones/${enc(zone)}/records`, input)
export const updateDNSRecord = (zone: string, id: string, input: DNSRecordInput) => api.put<DNSRecord>(`/api/dns/zones/${enc(zone)}/records/${enc(id)}`, input)
export const deleteDNSRecord = (zone: string, id: string) => api.del(`/api/dns/zones/${enc(zone)}/records/${enc(id)}`)

export const checkDNS = (domains: string[]) => api.post<DNSCheckResponse>('/api/dns/check', { domains }).then((r) => r?.results ?? [])
export const syncDNS = (hostIds?: string[]) => api.post<DNSCheckResponse>('/api/dns/sync', hostIds ? { hostIds } : {}).then((r) => r?.results ?? [])

/** Refetch records, checks and status after a write (record ids may change). */
export function useInvalidateDNS() {
  const qc = useQueryClient()
  return useCallback(() => {
    qc.invalidateQueries({ queryKey: dnsKeys.all })
  }, [qc])
}

export function useSaveDNSRecord(zone: string) {
  const invalidate = useInvalidateDNS()
  return useMutation({
    mutationFn: ({ id, input }: { id?: string; input: DNSRecordInput }) => (id ? updateDNSRecord(zone, id, input) : createDNSRecord(zone, input)),
    onSettled: invalidate,
  })
}

export function useDeleteDNSRecord(zone: string) {
  const invalidate = useInvalidateDNS()
  return useMutation({ mutationFn: (id: string) => deleteDNSRecord(zone, id), onSettled: invalidate })
}

export function useDebounced<T>(value: T, ms: number): T {
  const [v, setV] = useState(value)
  useEffect(() => {
    const t = window.setTimeout(() => setV(value), ms)
    return () => window.clearTimeout(t)
  }, [value, ms])
  return v
}

/** Debounced POST /api/dns/check for a list of (valid) domains. */
export function useDNSCheck(domains: string[], enabled: boolean) {
  const key = useDebounced(domains.join(','), 600)
  const list = key ? key.split(',') : []
  return useQuery({
    queryKey: dnsKeys.check(list),
    queryFn: () => checkDNS(list),
    enabled: enabled && list.length > 0,
    staleTime: 10_000,
    retry: false,
    placeholderData: (prev) => prev,
  })
}

export const isProviderError = (err: unknown) => err instanceof ApiError && err.code === 'provider_error'

// ---------------------------------------------------------------- records
export const EDITABLE_TYPES = ['A', 'AAAA', 'CNAME', 'MX', 'TXT', 'CAA', 'NS'] as const

export const RECORD_TYPE_INFO: Record<string, { placeholder: string; hint: string }> = {
  A: { placeholder: '203.0.113.10', hint: 'An IPv4 address' },
  AAAA: { placeholder: '2001:db8::10', hint: 'An IPv6 address' },
  CNAME: { placeholder: 'app.example.com', hint: 'Another hostname · most providers don’t allow it on @' },
  MX: { placeholder: 'mail.example.com', hint: 'Your mail server · lower priority is tried first' },
  TXT: { placeholder: 'v=spf1 include:_spf.example.com ~all', hint: 'Free text, e.g. SPF or a site verification code' },
  CAA: { placeholder: '0 issue "letsencrypt.org"', hint: 'Which certificate authorities may issue certificates' },
  NS: { placeholder: 'ns1.example.net', hint: 'Hands a sub-domain to other name servers · not allowed on @' },
}

export const PROXIABLE_TYPES = new Set(['A', 'AAAA', 'CNAME'])

/** 600 → "10 min", 3600 → "1 h", 0/1 → "Auto". */
export function ttlText(ttl: number | undefined): string {
  if (!ttl || ttl === 1) return 'Auto'
  if (ttl < 60 || ttl % 60 !== 0) return `${ttl}s`
  if (ttl < 3600) return `${ttl / 60} min`
  if (ttl < 86400 && ttl % 3600 === 0) return `${ttl / 3600} h`
  if (ttl < 86400) return `${Math.round(ttl / 60)} min`
  const d = Math.round(ttl / 86400)
  return `${d} ${d === 1 ? 'day' : 'days'}`
}

export const TTL_CHOICES = [0, 60, 300, 600, 1800, 3600, 14400, 86400]

/** "app.example.com" in "example.com" → "app"; the zone itself → "@". */
export function relativeName(domain: string, zone: string): string {
  if (domain === zone) return '@'
  return domain.endsWith('.' + zone) ? domain.slice(0, -(zone.length + 1)) : domain
}

// ---------------------------------------------------------------- checks
export function checkMeta(c: DNSDomainCheck, autoCreate: boolean): { tone: Tone; label: string; text: string } {
  const other = c.records[0]
  switch (c.status) {
    case 'exists':
      return { tone: 'ok', label: 'ok', text: 'Points to Relay' }
    case 'created':
      return { tone: 'ok', label: 'created', text: c.message || `Created ${c.planned ? `${c.planned.type} → ${c.planned.data}` : 'record'}` }
    case 'wildcard':
      return { tone: 'ok', label: 'wildcard', text: `Covered by ${other?.fqdn || 'a wildcard record'}` }
    case 'missing':
      return { tone: 'warn', label: 'missing', text: autoCreate ? 'Created after you apply' : 'No record yet' }
    case 'conflict':
      return { tone: 'warn', label: 'conflict', text: `Points to ${other?.data || 'something else'} — not changed` }
    case 'excluded':
      return { tone: undefined, label: 'excluded', text: `${c.zone ?? 'This domain'} is excluded from automatic changes` }
    case 'error':
      return { tone: 'danger', label: 'error', text: c.message || 'Your DNS provider returned an error' }
    case 'unmanaged':
    default:
      return { tone: undefined, label: 'not managed', text: 'Not in a connected domain' }
  }
}

// ---------------------------------------------------------------- remembered zone
const ZONE_KEY = 'relay.dns.zone'

export function readStoredZone(): string {
  try {
    return localStorage.getItem(ZONE_KEY) ?? ''
  } catch {
    return ''
  }
}

export function storeZone(zone: string) {
  try {
    localStorage.setItem(ZONE_KEY, zone)
  } catch {
    // storage unavailable (private mode etc.)
  }
}
