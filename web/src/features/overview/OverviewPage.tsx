// Owner: slice observe — Overview dashboard (design 01, 22a empty, 22c proxy engine down).
import { useEffect, useMemo, useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { TopBar } from '../../components/shell/TopBar'
import { Button, Dot, EmptyState, Segmented, Skeleton, StatCard, Status, cx, healthTone } from '../../components/ui'
import { ago, bytes, clock, compact, dateTime, duration, ms } from '../../lib/format'
import { useContainers, useEntities, useHealth, useProxyEngine, useRole } from '../../lib/queries'
import { proxyEngineLabel } from '../../lib/types'
import type { ActivityRow, HealthState, HealthStatus, ProxyHost } from '../../lib/types'
import DockerSuggestionsDialog from '../docker/DockerSuggestionsDialog'
import { OVERVIEW_RANGES, useActivity, useNow, useOverview, type Overview, type OverviewRange } from './api'
import { TrafficChart } from './TrafficChart'
import UpdateBanner from './UpdateBanner'
import './overview.css'

const RANGE_KEY = 'relay.overview.range'
const HEALTH_CHIPS = 12

function loadRange(): OverviewRange {
  try {
    const v = localStorage.getItem(RANGE_KEY)
    if (v === '1h' || v === '24h' || v === '7d') return v
  } catch {
    /* storage unavailable */
  }
  return '24h'
}

interface HostRow {
  host: ProxyHost
  state: HealthState
  detail?: string
}

function hostState(h: ProxyHost, health: Record<string, HealthStatus> | undefined, nginxDown: boolean): HealthState {
  if (!h.enabled) return 'disabled'
  if (nginxDown) return 'down'
  return health?.[`host:${h.id}`]?.status ?? 'unknown'
}

function pct(v: number): string {
  if (v === 0) return '0%'
  if (v < 0.01) return '<0.01%'
  if (v < 10) return `${v.toFixed(2).replace(/\.?0+$/, '')}%`
  return `${v.toFixed(1).replace(/\.0$/, '')}%`
}

function signedPct(v: number): string {
  return `${v >= 0 ? '+' : '−'}${Math.abs(v).toFixed(1)}%`
}

function sinceLabel(iso: string): string {
  const d = new Date(iso)
  return d.toDateString() === new Date().toDateString() ? clock(iso).slice(0, 5) : dateTime(iso)
}

export default function OverviewPage() {
  const navigate = useNavigate()
  const { canWrite } = useRole()
  const [range, setRange] = useState<OverviewRange>(loadRange)
  const [dockerOpen, setDockerOpen] = useState(false)
  const stats = useOverview('24h')
  const traffic = useOverview(range)
  const hostsQ = useEntities('hosts')
  const health = useHealth().data
  const { state: engine, label: liveLabel, loaded: enginesLoaded } = useProxyEngine()
  const ov = stats.data

  useEffect(() => {
    try {
      localStorage.setItem(RANGE_KEY, range)
    } catch {
      /* storage unavailable */
    }
  }, [range])

  const nginx = engine
    ? { reachable: engine.reachable, running: engine.running, version: engine.version, startedAt: engine.startedAt, uptimeSec: null as number | null }
    : ov
      ? { ...ov.nginx, startedAt: undefined }
      : undefined
  const nginxDown = !!nginx && nginx.reachable && !nginx.running
  const proxyLabel = enginesLoaded ? liveLabel : proxyEngineLabel[ov?.proxyEngine ?? 'nginx']

  const hosts = useMemo(() => hostsQ.data ?? [], [hostsQ.data])
  const rows = useMemo<HostRow[]>(
    () => hosts.map((h) => ({ host: h, state: hostState(h, health, nginxDown), detail: health?.[`host:${h.id}`]?.detail })),
    [hosts, health, nginxDown],
  )
  const noHosts = hostsQ.isSuccess && hosts.length === 0

  return (
    <>
      <UpdateBanner />
      <TopBar
        title="Overview"
        meta={<ProxyMeta label={proxyLabel} nginx={nginx} />}
        search
        actions={canWrite ? <Button variant="primary" icon="plus" onClick={() => navigate('/hosts?new=1')}>New host</Button> : undefined}
      />
      <div className="page overview">
        <StatCards rows={rows} hostsLoaded={hostsQ.isSuccess} ov={ov} nginxDown={nginxDown} />
        {noHosts ? (
          <div className="ov-grid">
            <Onboarding canWrite={canWrite} onDocker={() => setDockerOpen(true)} />
            <ActivityCard />
          </div>
        ) : (
          <>
            <div className="ov-grid">
              <TrafficCard range={range} onRange={setRange} data={traffic.data} stale={traffic.isPlaceholderData} error={traffic.isError} />
              <ActivityCard />
            </div>
            <HostHealth rows={rows} loading={!hostsQ.data} />
          </>
        )}
      </div>
      <DockerSuggestionsDialog open={dockerOpen} onClose={() => setDockerOpen(false)} />
    </>
  )
}

/** Status of the active proxy engine (nginx or Relay Edge). */
function ProxyMeta({ label, nginx }: { label: string; nginx?: { reachable: boolean; running: boolean; version: string; startedAt?: string; uptimeSec: number | null } }) {
  const now = useNow(60_000)
  if (!nginx) return null
  const name = [label, nginx.version].filter(Boolean).join(' ')
  if (!nginx.reachable) return <Status tone="muted">{label} agent unreachable</Status>
  if (!nginx.running) return <Status tone="danger">{name} · not running</Status>
  const up = nginx.startedAt ? (now - Date.parse(nginx.startedAt)) / 1000 : nginx.uptimeSec
  return <Status tone="ok">{name}{up != null && up >= 0 ? ` · uptime ${duration(up)}` : ''}</Status>
}

function StatCards({ rows, hostsLoaded, ov, nginxDown }: { rows: HostRow[]; hostsLoaded: boolean; ov?: Overview; nginxDown: boolean }) {
  const count = (s: HealthState) => rows.filter((r) => r.state === s).length
  const down = count('down')
  const degraded = count('degraded')
  const healthy = count('healthy')
  const unknown = count('unknown')

  let hostMeta: string | undefined
  let hostTone: 'ok' | 'warn' | 'danger' | 'muted' = 'muted'
  if (hostsLoaded) {
    if (rows.length === 0) hostMeta = 'none yet'
    else if (down) [hostMeta, hostTone] = [`${down} down`, 'danger']
    else if (degraded) [hostMeta, hostTone] = [`${degraded} degraded`, 'warn']
    else if (healthy) [hostMeta, hostTone] = [`${healthy} healthy`, 'ok']
    else if (unknown) hostMeta = 'checking…'
    else hostMeta = `${rows.length} disabled`
  }

  const req = ov?.requests
  const noTraffic = !!ov && ov.requests.total === 0
  const certs = ov?.certificates
  const e5 = ov?.upstream5xx

  return (
    <div className="grid-4">
      <StatCard label="Proxy hosts" value={hostsLoaded ? rows.length : '—'} meta={hostMeta} metaTone={hostTone} />
      {nginxDown ? (
        <StatCard label="Requests · 24h" value="—" meta={ov?.lastDataAt ? `no data since ${sinceLabel(ov.lastDataAt)}` : 'no data'} metaTone="danger" />
      ) : (
        <StatCard
          label="Requests · 24h"
          value={req ? compact(req.total) : '—'}
          meta={req?.deltaPct != null ? signedPct(req.deltaPct) : undefined}
          metaTone="muted"
        />
      )}
      <StatCard
        label="Certificates"
        value={certs ? certs.total : '—'}
        meta={certs?.expiringSoon ? `${certs.expiringSoon} expiring <14d` : certs?.expired ? `${certs.expired} expired` : undefined}
        metaTone={certs?.expiringSoon ? 'warn' : 'danger'}
      />
      <StatCard
        label="5xx from upstreams"
        value={e5 && !noTraffic ? pct(e5.pct) : '—'}
        meta={!ov ? undefined : noTraffic ? 'no traffic' : `${compact(e5!.count)} req`}
        metaTone={e5 && e5.pct >= 5 ? 'danger' : e5 && e5.pct >= 1 ? 'warn' : 'muted'}
      />
    </div>
  )
}

function TrafficCard({ range, onRange, data, stale, error }: { range: OverviewRange; onRange: (r: OverviewRange) => void; data?: Overview; stale: boolean; error: boolean }) {
  return (
    <div className="card ov-card">
      <div className="ov-card-head">
        <div className="ov-card-title">Traffic</div>
        <div className="spacer" />
        <Segmented value={range} onChange={onRange} options={OVERVIEW_RANGES} />
      </div>
      {data ? (
        <TrafficChart buckets={data.traffic.buckets} stepSeconds={data.traffic.stepSeconds} stale={stale} />
      ) : error ? (
        <div className="ov-note">Traffic metrics are unavailable right now.</div>
      ) : (
        <Skeleton height={268} />
      )}
      <div className="ov-kpis">
        <Kpi label="p50 latency" value={data?.latency.p50Ms != null ? ms(data.latency.p50Ms) : '—'} />
        <Kpi label="p95 latency" value={data?.latency.p95Ms != null ? ms(data.latency.p95Ms) : '—'} />
        <Kpi label="Bandwidth" value={data ? bytes(data.bandwidthBytes) : '—'} />
      </div>
    </div>
  )
}

function Kpi({ label, value }: { label: string; value: string }) {
  return (
    <div className="ov-kpi">
      <div className="k">{label}</div>
      <div className="v">{value}</div>
    </div>
  )
}

const activityDot: Record<string, string> = { ok: 'ok', error: 'danger', warn: 'warn', info: 'ink' }

function ActivityCard() {
  const q = useActivity(6)
  const now = useNow(30_000)
  return (
    <div className="card ov-card">
      <div className="ov-card-head">
        <div className="ov-card-title">Activity</div>
        <Link to="/logs/audit" className="ov-link">View all</Link>
      </div>
      {q.isLoading ? (
        <div className="col gap-10">
          {[0, 1, 2, 3].map((i) => <Skeleton key={i} height={34} />)}
        </div>
      ) : q.isError ? (
        <div className="ov-note">Activity is unavailable right now.</div>
      ) : q.data && q.data.length > 0 ? (
        <div className="ov-activity">
          {q.data.map((a) => <ActivityItem key={a.id} a={a} now={now} />)}
        </div>
      ) : (
        <div className="ov-note">No activity yet. Certificate renewals, upstream outages, new hosts and config reloads show up here.</div>
      )}
    </div>
  )
}

function ActivityItem({ a, now }: { a: ActivityRow; now: number }) {
  return (
    <div className="ov-activity-row">
      <span className={cx('dot', activityDot[a.level] ?? 'ink')} />
      <div className="grow">
        <div className="ov-activity-title">
          {a.title}
          {a.subject && <> <span className="mono">{a.subject}</span></>}
        </div>
        <div className="ov-activity-meta">{[ago(a.at, now), a.detail].filter(Boolean).join(' · ')}</div>
      </div>
    </div>
  )
}

const stateOrder: Record<HealthState, number> = { down: 0, degraded: 1, unknown: 2, healthy: 3, disabled: 4 }

function primaryDomain(h: ProxyHost): string {
  return h.domains[0] ?? h.id
}

/** "grafana.home.lan" → "grafana"; falls back to the full domain when ambiguous. */
function shortLabels(hosts: ProxyHost[]): string[] {
  const short = hosts.map((h) => {
    const d = primaryDomain(h)
    return d.startsWith('*') || /^[\d.:]+$/.test(d) ? d : d.split('.')[0]
  })
  return short.map((s, i) => (short.indexOf(s) !== short.lastIndexOf(s) ? primaryDomain(hosts[i]) : s))
}

function HostHealth({ rows, loading }: { rows: HostRow[]; loading: boolean }) {
  const sorted = [...rows].sort((a, b) => stateOrder[a.state] - stateOrder[b.state] || primaryDomain(a.host).localeCompare(primaryDomain(b.host)))
  const visible = sorted.slice(0, HEALTH_CHIPS)
  const labels = shortLabels(visible.map((r) => r.host))
  return (
    <div className="card ov-health">
      <div className="ov-card-title nowrap">Host health</div>
      <div className="ov-chips">
        {loading ? (
          <Skeleton height={28} />
        ) : (
          visible.map((r, i) => (
            <Link
              key={r.host.id}
              to={`/hosts?edit=${r.host.id}`}
              className={cx('ov-chip', r.state === 'disabled' && 'dim')}
              title={[primaryDomain(r.host), r.state, r.detail].filter(Boolean).join(' · ')}
            >
              <Dot tone={healthTone[r.state]} />
              {labels[i]}
            </Link>
          ))
        )}
        {sorted.length > visible.length && (
          <Link to="/hosts" className="ov-chip more">+{sorted.length - visible.length} more</Link>
        )}
      </div>
    </div>
  )
}

function Onboarding({ canWrite, onDocker }: { canWrite: boolean; onDocker: () => void }) {
  const navigate = useNavigate()
  const containers = useContainers(canWrite)
  const found = containers.data?.filter((c) => c.http && !c.hostId && !c.backendId).length ?? 0
  return (
    <div className="card ov-card ov-onboarding">
      <EmptyState
        icon="hosts"
        title="No proxy hosts yet"
        description="A host maps a domain to something on your network. Start with one, or pull them in from Docker or an existing Nginx Proxy Manager."
        actions={
          canWrite ? (
            <>
              <Button variant="primary" icon="plus" onClick={() => navigate('/hosts?new=1')}>New host</Button>
              <Button icon="docker" onClick={onDocker}>From Docker{found ? ` · ${found} found` : ''}</Button>
              <Button icon="upload" onClick={() => navigate('/settings/backup')}>Import NPM</Button>
            </>
          ) : undefined
        }
      />
    </div>
  )
}
