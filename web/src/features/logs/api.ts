// Owner: slice observe — response shapes of the logs endpoints not in lib/types.
import type { AccessEntry } from '../../lib/types'

/** GET /api/logs/access/{id} */
export interface AccessDetail {
  entry: AccessEntry
  /** Same client IP within ±10 min, newest first. */
  sameClient: AccessEntry[]
  host: { id: string; domain: string; kind: 'host' | 'stream' } | null
  /** The client IP is covered by the global blocklist. */
  blocked: boolean
}

export interface HistogramBucket {
  t: string
  total: number
  s2xx: number
  s3xx: number
  s4xx: number
  s5xx: number
}

/** GET /api/logs/access/histogram */
export interface Histogram {
  buckets: HistogramBucket[]
  stepSeconds: number
  since: string
  until: string
}

export interface ErrorEntry {
  id: number
  ts: string
  source: 'nginx' | 'edge' | 'haproxy' | 'balancer' | 'relay' | 'acme' | string
  level: string
  message: string
}

/** GET /api/logs/error */
export interface ErrorPage {
  entries: ErrorEntry[]
  nextBeforeId: number
}
