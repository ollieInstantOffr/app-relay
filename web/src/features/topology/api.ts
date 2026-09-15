// Owner: slice observe — Topology (design 31) and host traffic flow (design 32).
import { keepPreviousData, useQuery } from '@tanstack/react-query'
import { api } from '../../lib/api'

export type FlowRange = '5m' | '15m' | '1h' | '24h'
export const FLOW_RANGES: { value: FlowRange; label: string }[] = [
  { value: '5m', label: 'Last 5 min' },
  { value: '15m', label: 'Last 15 min' },
  { value: '1h', label: 'Last hour' },
  { value: '24h', label: 'Last 24 hours' },
]

/** Refresh interval while live. */
export const LIVE_MS = 5_000

export interface FlowTraffic {
  requests: number
  rps: number
  s4xx: number
  s5xx: number
  bytesIn: number
  bytesOut: number
  /** In + out. */
  bytesPerSec: number
  p50Ms: number | null
  p95Ms: number | null
}

/** Requests by client source; blocked requests (403/444) are only in blocked. */
export interface FlowClients {
  unique: number
  requests: number
  lan: number
  vpn: number
  internet: number
  blocked: number
}

/** GET /api/metrics/topology?range= */
export interface Topology {
  range: FlowRange
  windowSec: number
  generatedAt: string
  totals: FlowTraffic
  unknown: FlowTraffic
  clients: FlowClients
  hosts: Record<string, FlowTraffic>
  hostClients: Record<string, FlowClients>
  /** requests = sessions */
  streams: Record<string, FlowTraffic>
  lastDataAt: string | null
}

export interface FlowUpstream {
  addr: string
  requests: number
  sharePct: number
  errors: number
  responseMs: number | null
}

/** GET /api/metrics/topology/hosts/:id?range= */
export interface HostFlow {
  hostId: string
  range: FlowRange
  windowSec: number
  generatedAt: string
  traffic: FlowTraffic
  clients: FlowClients
  tlsPct: number
  rateLimitedPct: number
  errorPct: number
  timing: { requestMs: number | null; connectMs: number | null; headerMs: number | null; upstreamMs: number | null }
  upstreams: FlowUpstream[]
}

export function useTopology(range: FlowRange, live: boolean) {
  return useQuery({
    queryKey: ['metrics', 'topology', range],
    queryFn: () => api.get<Topology>(`/api/metrics/topology?range=${range}`),
    refetchInterval: live ? LIVE_MS : false,
    placeholderData: keepPreviousData,
  })
}

export function useHostFlow(hostId: string | undefined, range: FlowRange, live: boolean) {
  return useQuery({
    queryKey: ['metrics', 'topology', 'host', hostId, range],
    queryFn: () => api.get<HostFlow>(`/api/metrics/topology/hosts/${hostId}?range=${range}`),
    enabled: !!hostId,
    refetchInterval: live ? LIVE_MS : false,
    placeholderData: keepPreviousData,
  })
}

// ---------------------------------------------------------------- formatting

export function fmtRate(v: number, unit = 'r/s'): string {
  if (!isFinite(v) || v <= 0) return `0 ${unit}`
  if (v < 0.1) return `<0.1 ${unit}`
  if (v < 10) return `${v.toFixed(1).replace(/\.0$/, '')} ${unit}`
  if (v < 1000) return `${Math.round(v)} ${unit}`
  return `${(v / 1000).toFixed(1).replace(/\.0$/, '')}k ${unit}`
}

export function fmtBytesRate(v: number): string {
  if (!isFinite(v) || v <= 0) return '0 B/s'
  const units = ['B/s', 'KB/s', 'MB/s', 'GB/s']
  let i = 0
  while (v >= 1000 && i < units.length - 1) {
    v /= 1000
    i++
  }
  return `${v < 10 && i > 0 ? v.toFixed(1) : Math.round(v)} ${units[i]}`
}

export function fmtCount(v: number): string {
  if (v < 1000) return String(v)
  if (v < 1_000_000) return `${(v / 1000).toFixed(1).replace(/\.0$/, '')}k`
  return `${(v / 1_000_000).toFixed(1).replace(/\.0$/, '')}M`
}

export function fmtMs(v: number | null | undefined): string {
  if (v === null || v === undefined || !isFinite(v)) return '—'
  if (v < 1) return `${v.toFixed(1)} ms`
  if (v < 1000) return `${Math.round(v)} ms`
  return `${(v / 1000).toFixed(1)} s`
}

export function fmtPct(v: number, digits = 0): string {
  if (!isFinite(v)) return '0%'
  if (v > 0 && v < 0.1 && digits < 2) return `${v.toFixed(2)}%`
  return `${v.toFixed(digits)}%`
}

export function fmtDuration(sec: number): string {
  if (sec < 60) return `${Math.max(0, Math.round(sec))} s`
  if (sec < 3600) return `${Math.round(sec / 60)} min`
  if (sec < 86400) return `${Math.round(sec / 3600)} h`
  return `${Math.round(sec / 86400)} d`
}

/** "10.0.0.30" → ".30"; hostnames stay (shortened). */
export function shortAddr(host: string): string {
  if (/^\d+\.\d+\.\d+\.\d+$/.test(host)) return '.' + host.split('.')[3]
  return host.length > 18 ? host.slice(0, 17) + '…' : host
}
