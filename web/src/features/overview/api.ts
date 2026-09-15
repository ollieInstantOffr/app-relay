// Owner: slice observe — dashboard data (GET /api/metrics/overview, /api/activity).
import { useEffect, useState } from 'react'
import { keepPreviousData, useQuery } from '@tanstack/react-query'
import { api } from '../../lib/api'
import type { ActivityRow } from '../../lib/types'

export type OverviewRange = '1h' | '24h' | '7d'
export const OVERVIEW_RANGES: OverviewRange[] = ['1h', '24h', '7d']

export interface TrafficBucket {
  /** Bucket start (UTC). */
  t: string
  requests: number
  s5xx: number
}

/** GET /api/metrics/overview?range= */
export interface Overview {
  range: OverviewRange
  hosts: { total: number; healthy: number; down: number; degraded: number; disabled: number; unknown: number }
  requests: { total: number; previousTotal: number; deltaPct: number | null }
  certificates: { total: number; expiringSoon: number; expired: number }
  upstream5xx: { pct: number; count: number }
  traffic: { buckets: TrafficBucket[]; stepSeconds: number }
  latency: { p50Ms: number | null; p95Ms: number | null }
  bandwidthBytes: number
  unknownHostHits: number
  /** The active proxy engine (see proxyEngine), not necessarily nginx. */
  nginx: { version: string; uptimeSec: number | null; running: boolean; reachable: boolean }
  proxyEngine?: 'nginx' | 'edge'
  lastDataAt: string | null
}

export function useOverview(range: OverviewRange) {
  return useQuery({
    queryKey: ['metrics', 'overview', range],
    queryFn: () => api.get<Overview>(`/api/metrics/overview?range=${range}`),
    refetchInterval: range === '1h' ? 15_000 : 60_000,
    placeholderData: keepPreviousData,
  })
}

export function useActivity(limit: number) {
  return useQuery({
    queryKey: ['activity', limit],
    queryFn: () => api.get<ActivityRow[]>(`/api/activity?limit=${limit}`),
    refetchInterval: 60_000,
  })
}

/** Re-renders every intervalMs so relative times stay fresh. */
export function useNow(intervalMs: number): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const t = window.setInterval(() => setNow(Date.now()), intervalMs)
    return () => window.clearInterval(t)
  }, [intervalMs])
  return now
}
