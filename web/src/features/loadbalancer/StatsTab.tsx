import { Fragment, useState } from 'react'
import { Badge, Card, EmptyState, Select, Skeleton, Sparkline, StatCard, Status, Tooltip, cx, type Tone } from '../../components/ui'
import { useEntities, useLBStats } from '../../lib/queries'
import { compact, duration, ms } from '../../lib/format'
import type { ServerStats } from '../../lib/types'
import { useLBSeries } from './lbApi'

const RANGES = [
  { value: '1h', label: 'Last 1h' },
  { value: '24h', label: 'Last 24h' },
  { value: '7d', label: 'Last 7d' },
]

function statusTone(s: string): Tone {
  if (s === 'UP' || s === 'NOCHECK') return 'ok'
  if (s === 'DEGRADED' || s === 'DRAIN' || s === 'NOLB') return 'warn'
  if (s === 'DOWN') return 'danger'
  return undefined
}

function uptime(sv: ServerStats) {
  if (sv.status === 'DOWN') return `${duration(sv.downSec)} ↓`
  if (sv.status === 'MAINT') return '—'
  return duration(sv.uptimeSec)
}

export default function StatsTab({ onEdit }: { onEdit: (id: string) => void }) {
  const [range, setRange] = useState('1h')
  const statsQ = useLBStats(2_000)
  const series = useLBSeries(range, undefined, range === '1h' ? 10_000 : 60_000).data
  const backends = useEntities('backends').data ?? []

  if (statsQ.isLoading) return <Skeleton height={220} />
  const s = statsQ.data
  const running = !!s?.running
  const liveIds = new Set((s?.backends ?? []).map((b) => b.id))
  const notApplied = backends.filter((b) => !liveIds.has(b.id))
  const errors = series ? series.connErrors + series.respErrors + series.reqErrors : undefined
  const peak = Math.max(series?.peak ?? 0, s?.peakSessRate ?? 0)

  return (
    <>
      <div className="row between">
        <Status tone={running ? 'ok' : 'muted'} pulse={running}>
          {running ? 'Live · 2 s refresh' : 'HAProxy is not running'}
        </Status>
        <div style={{ width: 140 }}>
          <Select inputSize="sm" value={range} options={RANGES} onChange={setRange} />
        </div>
      </div>

      <div className="grid-3">
        <StatCard label="Sessions / s" value={running ? compact(s!.sessRate) : '—'} meta={`now · peak ${compact(peak)}`}>
          <Sparkline values={(series?.points ?? []).map((p) => p.sessRate)} height={36} width={320} />
        </StatCard>
        <StatCard
          label={`Errors · ${range}`}
          value={errors === undefined ? '—' : compact(errors)}
          meta={series ? `${compact(series.connErrors)} conn · ${compact(series.respErrors)} resp · ${compact(series.reqErrors)} req` : undefined}
          metaTone={errors ? 'danger' : 'muted'}
        >
          <Sparkline values={(series?.points ?? []).map((p) => p.errors)} height={36} width={320} stroke="var(--danger)" fill="rgba(220,38,38,.06)" />
        </StatCard>
        <StatCard
          label="Queue · retries"
          value={`${running ? compact(s!.queue) : '—'} · ${series ? compact(series.retries) : '—'}`}
          meta={series ? `redispatched ${compact(series.redispatches)}` : undefined}
        >
          <Sparkline values={(series?.points ?? []).map((p) => p.queue)} height={36} width={320} />
        </StatCard>
      </div>

      {!running ? (
        <Card>
          <EmptyState
            icon="stats"
            title="No live statistics"
            description={
              backends.length
                ? 'HAProxy is not running or not reachable. Stats appear once the load balancer config is applied and the engine is up.'
                : 'HAProxy starts once you create the first backend and apply.'
            }
          />
        </Card>
      ) : (
        <Card>
          <div className="table-wrap">
            <table className="table compact lb-stats-table">
              <thead>
                <tr>
                  <th>Backend / server</th>
                  <th>Status</th>
                  <th className="num">Sess/s</th>
                  <th className="num">Current</th>
                  <th className="num">Max</th>
                  <th className="num">Queue</th>
                  <th className="num">
                    <Tooltip content="Connection + response errors since the last reload">
                      <span>Errors</span>
                    </Tooltip>
                  </th>
                  <th className="num">
                    <Tooltip content="95th percentile of HAProxy's rolling response-time average (last 1024 requests), sampled every 2 s over the last hour">
                      <span>Resp p95 ≈</span>
                    </Tooltip>
                  </th>
                  <th className="num">Uptime</th>
                </tr>
              </thead>
              <tbody>
                {s!.backends.map((b) => (
                  <Fragment key={b.name}>
                    <tr className="backend">
                      <td className={cx('name', b.id && 'clickable')} onClick={() => b.id && onEdit(b.id)}>
                        {b.name}
                        {!b.id && <span className="faint" style={{ fontWeight: 400 }}> · removed, pending apply</span>}
                      </td>
                      <td><Badge tone={statusTone(b.status)}>{b.status}</Badge></td>
                      <td className="num">{compact(b.sessRate)}</td>
                      <td className="num">{compact(b.current)}</td>
                      <td className="num">{compact(b.max)}</td>
                      <td className="num">{compact(b.queue)}</td>
                      <td className={cx('num', b.errors > 0 && 'danger-text')}>{compact(b.errors)}</td>
                      <td className="num">{b.respP95Ms ? ms(b.respP95Ms) : '—'}</td>
                      <td className="num">{b.status === 'DOWN' ? '—' : duration(b.uptimeSec)}</td>
                    </tr>
                    {b.servers.map((sv) => (
                      <tr key={sv.name} className={cx('server', sv.status === 'DOWN' && 'down', sv.status === 'MAINT' && 'dim')}>
                        <td className="name" title={sv.name + (sv.checkDetail ? ` · ${sv.checkDetail}` : '')}>
                          {sv.address || sv.name}
                          {sv.role === 'backup' && <span className="faint"> · backup</span>}
                        </td>
                        <td><Badge tone={statusTone(sv.status)}>{sv.status}</Badge></td>
                        <td className="num">{compact(sv.sessRate)}</td>
                        <td className="num">{compact(sv.current)}</td>
                        <td className="num">{compact(sv.max)}</td>
                        <td className="num">{compact(sv.queue)}</td>
                        <td className="num">{compact(sv.errors)}</td>
                        <td className="num">{sv.respP95Ms ? ms(sv.respP95Ms) : '—'}</td>
                        <td className="num">{uptime(sv)}</td>
                      </tr>
                    ))}
                  </Fragment>
                ))}
                {notApplied.map((b) => (
                  <tr key={b.id} className="dim">
                    <td className="name clickable" onClick={() => onEdit(b.id)}>{b.name}</td>
                    <td colSpan={8}><Badge tone="pending">not applied yet</Badge></td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Card>
      )}
    </>
  )
}
