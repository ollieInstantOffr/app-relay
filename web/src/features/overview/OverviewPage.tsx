// Owner: slice observe — Overview dashboard (design 01, 22a empty, 22c proxy engine down).
import { useEffect, useMemo, useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { TopBar } from '../../components/shell/TopBar'
import { ClientSourcesChart, LBSessionsChart, ResponseMixChart, TopHostsChart } from '../../components/charts'
import { Button, Dot, EmptyState, Segmented, Skeleton, StatCard, Status, cx, healthTone } from '../../components/ui'
import { ago, bytes, clock, compact, dateTime, duration, ms } from '../../lib/format'
import { useContainers, useEntities, useHealth, useLBEngine, useLBStats, useProxyEngine, useRole } from '../../lib/queries'
import { proxyEngineLabel } from '../../lib/types'
import type { ActivityRow, BackendStats, HealthState, HealthStatus, ProxyHost } from '../../lib/types'
import DockerSuggestionsDialog from '../docker/DockerSuggestionsDialog'
import { useLBSeries } from '../loadbalancer/lbApi'
import { useTopology } from '../topology/api'
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
  const backendsQ = useEntities('backends')
  const hasBackends = (backendsQ.data?.length ?? 0) > 0

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
          <>
            <div className="ov-grid">
              <Onboarding canWrite={canWrite} onDocker={() => setDockerOpen(true)} />
              <ActivityCard />
            </div>
            {hasBackends && <LoadBalancerCard range={range} />}
          </>
        ) : (
          <>
            <div className="ov-grid">
              <TrafficCard range={range} onRange={setRange} data={traffic.data} stale={traffic.isPlaceholderData} error={traffic.isError} />
              <ActivityCard />
            </div>
            <TrafficBreakdown range={range} hosts={hosts} />
            <HostHealth rows={rows} loading={!hostsQ.data} />
            {hasBackends && <LoadBalancerCard range={range} />}
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

const RANGE_LABEL: Record<OverviewRange, string> = { '1h': 'last hour', '24h': 'last 24 hours', '7d': 'last 7 days' }
const TOP_HOSTS = 6

/** Axis label for a load balancer series point (unix seconds). */
function seriesLabel(t: number, stepSeconds: number): string {
  const d = new Date(t * 1000)
  const hm = d.toTimeString().slice(0, 5)
  return stepSeconds >= 3600 ? `${d.toLocaleDateString([], { weekday: 'short' })} ${hm}` : hm
}

/**
 * Where traffic comes from, how it was answered and which hosts get it
 * (GET /api/metrics/topology). Topology covers at most 24 h, so 7d shows the last day.
 */
function TrafficBreakdown({ range, hosts }: { range: OverviewRange; hosts: ProxyHost[] }) {
  const navigate = useNavigate()
  const flowRange = range === '1h' ? '1h' : '24h'
  const q = useTopology(flowRange, false)
  const t = q.data
  const span = RANGE_LABEL[flowRange]

  const top = useMemo(() => {
    if (!t) return []
    const byId = new Map(hosts.map((h) => [h.id, h]))
    const ranked = Object.entries(t.hosts)
      .filter(([id, f]) => byId.has(id) && f.requests > 0)
      .sort((a, b) => b[1].requests - a[1].requests)
      .slice(0, TOP_HOSTS)
    const labels = shortLabels(ranked.map(([id]) => byId.get(id)!))
    return ranked.map(([id, f], i) => ({ id, label: labels[i], requests: f.requests }))
  }, [t, hosts])

  if (q.isError && !t) {
    return <div className="card ov-card"><div className="ov-note">Traffic breakdown is unavailable right now.</div></div>
  }
  const loading = !t
  const total = t?.totals.requests ?? 0
  const empty = (title: string) => (
    <div className="col gap-8">
      <div className="ov-card-title">{title} <span className="ov-card-sub">· {span}</span></div>
      <div className="ov-note">No requests in the {span}.</div>
    </div>
  )
  const clients = t?.clients
  const success = t ? Math.max(0, total - t.totals.s4xx - t.totals.s5xx) : 0

  return (
    <div className={cx('ov-breakdown', q.isPlaceholderData && 'stale')}>
      <div className="card ov-card">
        <TopHostsChart
          loading={loading}
          data={top}
          subtitle={span}
          headline={compact(total)}
          formatValue={compact}
          onPointClick={(d) => {
            const hit = top.find((r) => r.label === d.label)
            if (hit) navigate(`/topology/hosts/${hit.id}`)
          }}
          emptyState={empty('Top hosts')}
        />
      </div>
      <div className="card ov-card">
        <ClientSourcesChart
          loading={loading}
          data={clients && clients.requests > 0
            ? [
                { name: 'LAN', requests: clients.lan },
                { name: 'VPN', requests: clients.vpn },
                { name: 'Internet', requests: clients.internet },
                { name: 'Blocked', requests: clients.blocked },
              ]
            : []}
          subtitle={span}
          headline={clients ? compact(clients.unique) : undefined}
          unit="unique IPs"
          formatValue={compact}
          emptyState={empty('Client sources')}
        />
      </div>
      <div className="card ov-card">
        <ResponseMixChart
          loading={loading}
          data={total > 0
            ? [
                { name: '2xx · 3xx', requests: success },
                { name: '4xx', requests: t!.totals.s4xx },
                { name: '5xx', requests: t!.totals.s5xx },
              ]
            : []}
          subtitle={span}
          headline={t?.totals.p95Ms != null ? ms(t.totals.p95Ms) : undefined}
          unit="p95"
          formatValue={compact}
          emptyState={empty('Responses')}
        />
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

const backendTone: Record<string, 'ok' | 'warn' | 'danger'> = { UP: 'ok', DEGRADED: 'warn', DOWN: 'danger' }

/** Load balancer summary: engine, traffic and backend health (from /api/lb/stats). */
function LoadBalancerCard({ range }: { range: OverviewRange }) {
  const backends = useEntities('backends').data ?? []
  const frontends = useEntities('frontends').data ?? []
  const stats = useLBStats(10_000).data
  const series = useLBSeries(range, undefined, range === '1h' ? 15_000 : 60_000).data
  const hasSeries = !!series && series.points.length > 1
  const { label, state } = useLBEngine()
  const now = useNow(60_000)

  const byId = new Map<string, BackendStats>((stats?.backends ?? []).map((b) => [b.id, b]))
  const running = !!stats?.running
  const live = backends.map((b) => ({ b, st: running ? byId.get(b.id) : undefined }))
  const servers = live.flatMap(({ st }) => st?.servers ?? [])
  const serversUp = servers.filter((sv) => sv.status === 'UP' || sv.status === 'NOCHECK').length
  const totalServers = backends.reduce((n, b) => n + b.servers.length, 0)
  const backendsUp = live.filter(({ st }) => st?.status === 'UP').length
  const degraded = live.filter(({ st }) => st?.status === 'DEGRADED').length
  const down = live.filter(({ st }) => st?.status === 'DOWN').length
  const current = live.reduce((n, { st }) => n + (st?.current ?? 0), 0)
  const enabledFrontends = frontends.filter((f) => f.enabled).length

  let engineStatus
  if (!state) engineStatus = null
  else if (!state.reachable) engineStatus = <Status tone="muted">{label} agent unreachable</Status>
  else if (!state.running) engineStatus = <Status tone="danger">{label} · not running</Status>
  else {
    const up = state.startedAt ? (now - Date.parse(state.startedAt)) / 1000 : null
    engineStatus = <Status tone="ok">{[label, state.version].filter(Boolean).join(' ')}{up != null && up >= 0 ? ` · uptime ${duration(up)}` : ''}</Status>
  }

  const sorted = [...live].sort((x, y) => {
    const rank = (st?: BackendStats) => (!st ? 3 : st.status === 'DOWN' ? 0 : st.status === 'DEGRADED' ? 1 : 2)
    return rank(x.st) - rank(y.st) || x.b.name.localeCompare(y.b.name)
  })
  const visible = sorted.slice(0, HEALTH_CHIPS)

  return (
    <div className="card ov-card ov-lb">
      <div className="ov-card-head">
        <div className="ov-card-title">Load balancer</div>
        {engineStatus}
        <Link to="/load-balancer" className="ov-link">Open</Link>
      </div>
      <div className="ov-kpis ov-lb-kpis">
        <Kpi label="Sessions / s" value={running ? compact(stats!.sessRate) : '—'} />
        <Kpi label="Current sessions" value={running ? compact(current) : '—'} />
        <Kpi
          label="Backends up"
          value={running ? `${backendsUp} / ${backends.length}` : `— / ${backends.length}`}
        />
        <Kpi label="Servers up" value={running ? `${serversUp} / ${totalServers}` : `— / ${totalServers}`} />
        <Kpi label="Frontends" value={String(enabledFrontends)} />
      </div>
      <div className={cx('ov-lb-body', hasSeries && 'with-chart')}>
        {series && hasSeries && (
          <div className="ov-lb-chart">
            <LBSessionsChart
              data={series.points.map((p) => ({ label: seriesLabel(p.t, series.step), sessions: p.sessRate, errors: p.errors }))}
              subtitle={RANGE_LABEL[range]}
              headline={false}
              formatValue={compact}
            />
          </div>
        )}
        <div className="col gap-12">
          {running && (down > 0 || degraded > 0) && (
            <div className={cx('ov-lb-alert', down > 0 ? 'danger' : 'warn')}>
              <Dot tone={down > 0 ? 'danger' : 'warn'} />
              {[down > 0 ? `${down} backend${down === 1 ? '' : 's'} down` : '', degraded > 0 ? `${degraded} degraded` : ''].filter(Boolean).join(' · ')}
            </div>
          )}
          <div className="ov-chips">
            {visible.map(({ b, st }) => {
              const up = st?.servers.filter((sv) => sv.status === 'UP' || sv.status === 'NOCHECK').length ?? 0
              const title = st
                ? `${b.name} · ${st.status} · ${up}/${b.servers.length} servers up · ${compact(st.sessRate)} sessions/s`
                : `${b.name} · ${running ? 'not in the running config yet (apply)' : `${label} not running`}`
              return (
                <Link key={b.id} to={`/load-balancer/backends?edit=${b.id}`} className={cx('ov-chip', !st && 'dim')} title={title}>
                  <Dot tone={st ? (backendTone[st.status] ?? 'muted') : 'muted'} />
                  {b.name}
                  {st && <span className="ov-chip-meta">{up}/{b.servers.length}</span>}
                </Link>
              )
            })}
            {sorted.length > visible.length && (
              <Link to="/load-balancer/backends" className="ov-chip more">+{sorted.length - visible.length} more</Link>
            )}
          </div>
        </div>
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
