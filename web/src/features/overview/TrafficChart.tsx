// Owner: slice observe — requests bar chart with 5xx overlay (design 01).
import { useState } from 'react'
import { cx } from '../../components/ui'
import { compact } from '../../lib/format'
import type { TrafficBucket } from './api'

const pad = (n: number) => String(n).padStart(2, '0')
const hhmm = (d: Date) => `${pad(d.getHours())}:${pad(d.getMinutes())}`
const weekday = (d: Date) => d.toLocaleDateString([], { weekday: 'short' })

function tick(iso: string, stepSeconds: number): string {
  const d = new Date(iso)
  return stepSeconds >= 6 * 3600 ? `${weekday(d)} ${hhmm(d)}` : hhmm(d)
}

function bucketRange(iso: string, stepSeconds: number): string {
  const start = new Date(iso)
  const end = new Date(start.getTime() + stepSeconds * 1000)
  if (stepSeconds < 3600) return hhmm(start)
  if (stepSeconds < 86400 && start.getDate() === end.getDate()) return `${hhmm(start)}–${hhmm(end)}`
  return `${weekday(start)} ${hhmm(start)}–${weekday(end)} ${hhmm(end)}`
}

/** Five evenly spaced axis labels, the last one "now". */
export function axisTicks(buckets: { t: string }[], stepSeconds: number): string[] {
  const n = buckets.length
  if (n === 0) return []
  const idx = [0, Math.round(n / 4), Math.round(n / 2), Math.round((3 * n) / 4)].filter((i) => i < n - 1)
  return [...idx.map((i) => tick(buckets[i].t, stepSeconds)), 'now']
}

export function TrafficChart({ buckets, stepSeconds, stale }: { buckets: TrafficBucket[]; stepSeconds: number; stale?: boolean }) {
  const [hover, setHover] = useState<number | null>(null)
  const max = Math.max(0, ...buckets.map((b) => b.requests))
  const hovered = hover !== null ? buckets[hover] : undefined
  return (
    <div className={cx('ov-chart-wrap', stale && 'stale')}>
      <div className="ov-chart" onMouseLeave={() => setHover(null)}>
        {buckets.map((b, i) => (
          <div
            key={b.t}
            className={cx('ov-bar', i === buckets.length - 1 && 'current', hover === i && 'hover')}
            onMouseEnter={() => setHover(i)}
          >
            <div className="ov-bar-fill" style={{ height: max ? `${(b.requests / max) * 100}%` : 0, minHeight: b.requests ? 2 : 0 }}>
              {b.s5xx > 0 && <div className="ov-bar-5xx" style={{ height: `${(b.s5xx / b.requests) * 100}%` }} />}
            </div>
          </div>
        ))}
        {max === 0 && <div className="ov-chart-empty">No requests in this range</div>}
        {hovered && (
          <div className="chart-tip" style={{ left: `${((hover! + 0.5) / buckets.length) * 100}%` }}>
            {bucketRange(hovered.t, stepSeconds)} · {compact(hovered.requests)} req
            {hovered.s5xx > 0 && <span className="tip-err"> · {compact(hovered.s5xx)} 5xx</span>}
          </div>
        )}
      </div>
      <div className="ov-axis">
        {axisTicks(buckets, stepSeconds).map((l, i) => (
          <span key={i}>{l}</span>
        ))}
      </div>
    </div>
  )
}
