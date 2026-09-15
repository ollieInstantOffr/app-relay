// Owner: slice observe — Traffic flow for one host (design 32): hop-by-hop path and timings.
import { Fragment, type ReactNode } from 'react'
import { Link, useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { Button, Dot, EmptyState, Menu, Segmented, cx } from '../../components/ui'
import { TopBar } from '../../components/shell/TopBar'
import { useContainers, useEntities, useHealth, useLBEngine, useLBStats, useProxyEngine } from '../../lib/queries'
import type { ProxyHost } from '../../lib/types'
import { fmtCount, fmtMs, fmtPct, fmtRate, useHostFlow, type HostFlow } from './api'
import { hostFrontends, serverStatus, viaLB } from './graph'
import './topology.css'

type Mode = 'live' | 'hour'

function Hop({ label, sub, limited, stack, live }: { label: ReactNode; sub: ReactNode; limited?: number; stack?: number; live?: boolean }) {
  return (
    <div className="flow-hop">
      <span className="h">{label}</span>
      {stack ? (
        <div className="bar stack">{Array.from({ length: Math.min(stack, 6) }, (_, i) => <span key={i} />)}</div>
      ) : (
        <div className={cx('bar', !live && 'still')}>{limited ? <i style={{ width: `${Math.min(100, Math.max(3, limited))}%` }} /> : null}</div>
      )}
      <span className="sub">{sub}</span>
    </div>
  )
}

function Col({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div style={{ minWidth: 0 }}>
      <div className="flow-col-label">{label}</div>
      {children}
    </div>
  )
}

const tone = (s: string) => (s === 'healthy' ? 'ok' : s === 'degraded' ? 'warn' : s === 'down' ? 'danger' : 'muted')

function hostFeatures(h: ProxyHost, aclName?: string): string {
  const x = h as ProxyHost & { rateLimit?: { enabled?: boolean }; geoBlock?: { enabled?: boolean } }
  return [
    h.certificateId ? ':443 · TLS' : ':80',
    aclName,
    h.forwardAuth?.enabled ? 'login' : undefined,
    x.rateLimit?.enabled ? 'rate-limit' : undefined,
    x.geoBlock?.enabled ? 'geo-block' : undefined,
  ].filter(Boolean).join(' · ')
}

function dominantSource(f: HostFlow | undefined): string {
  const c = f?.clients
  if (!c || !c.requests) return 'Clients'
  const list: [string, number][] = [['LAN', c.lan], ['VPN', c.vpn], ['Internet', c.internet]]
  list.sort((a, b) => b[1] - a[1])
  return list[0][1] >= c.requests * 0.6 ? list[0][0] : 'Mixed'
}

export default function HostFlowPage() {
  const { id } = useParams()
  const navigate = useNavigate()
  const [mode, setMode] = useModeState()
  const live = mode === 'live'
  const range = live ? '15m' : '1h'
  const hosts = useEntities('hosts')
  const backends = useEntities('backends').data ?? []
  const frontends = useEntities('frontends').data ?? []
  const accessLists = useEntities('access-lists').data ?? []
  const containers = useContainers().data ?? []
  const health = useHealth().data ?? {}
  const lb = useLBStats(live ? 5_000 : 60_000).data
  const proxy = useProxyEngine()
  const lbEngine = useLBEngine()
  const flowQ = useHostFlow(id, range, live)
  const flow = flowQ.data

  const host = hosts.data?.find((h) => h.id === id)
  if (hosts.isLoading) return <div className="page"><span className="spinner lg" /></div>
  if (!host) {
    return (
      <>
        <TopBar title="Traffic flow" />
        <div className="page">
          <EmptyState icon="topology" title="Host not found" description="It may have been deleted." actions={<Button onClick={() => navigate('/topology')}>Back to topology</Button>} />
        </div>
      </>
    )
  }

  const domain = host.domains[0] ?? host.id
  const acl = accessLists.find((a) => a.id === host.accessListId)
  const hs = health[`host:${host.id}`]
  const status = !host.enabled ? 'disabled' : hs?.status ?? 'unknown'
  const throughLB = viaLB(host, frontends)
  const fe = throughLB ? hostFrontends(host, frontends)[0] : undefined
  const backendId = host.upstream.backendId || fe?.defaultBackendId
  const backend = throughLB ? backends.find((b) => b.id === backendId) : undefined
  const bs = lb?.backends.find((b) => b.id === backend?.id)
  const t = flow?.traffic
  const tm = flow?.timing
  const proxyMs = tm?.requestMs != null && tm.upstreamMs != null ? Math.max(0, tm.requestMs - tm.upstreamMs) : null
  const upstreamTarget = `${host.upstream.host}:${host.upstream.port}`
  const rule = fe?.rules.find((r) => r.backendId === backendId)
  const ruleText = rule ? rule.conditions.map((c) => `${c.negate ? '!' : ''}${c.type} ${c.value}`).join(' & ') || 'rule' : 'default_backend'
  const shares = bs?.servers.map((s) => Math.round(s.sharePct)) ?? []

  const columns = throughLB ? '140px 1fr 200px 1fr 170px 1fr 170px 1fr 240px' : '150px 1fr 230px 1fr 280px'

  // Where time goes (share of the request time at the proxy)
  const req = tm?.requestMs ?? 0
  const connect = Math.max(0, tm?.connectMs ?? 0)
  const upstream = Math.max(0, (tm?.upstreamMs ?? 0) - connect)
  const proxyShare = Math.max(0, req - connect - upstream)
  const split = req > 0 ? [connect / req, upstream / req, proxyShare / req].map((v) => Math.round(v * 100)) : [0, 0, 0]

  const pct = (n: number) => (flow?.clients.requests ? Math.round((n / flow.clients.requests) * 100) : 0)
  const errTone = flow && flow.errorPct >= 1 ? 'danger' : undefined

  const logsLink = `/logs/access?host=${encodeURIComponent(domain)}&range=${range}`

  const containerRows: ReactNode[] = []
  if (throughLB && backend) {
    for (const s of backend.servers) {
      const ss = bs?.servers.find((x) => x.id === s.id || x.name === s.name)
      const st = serverStatus(ss, s)
      containerRows.push(
        <Link key={s.id} to={`/load-balancer/backends?edit=${backend.id}`} className={cx('flow-box', st === 'down' && 'danger')}>
          <span className="t"><Dot tone={tone(st)} />{s.name} · {s.address}</span>
          <span className="s">{st === 'down' ? 'down' : ss ? fmtMs(ss.respAvgMs) : '—'}</span>
        </Link>,
      )
    }
  } else if (flow?.upstreams.length) {
    for (const u of flow.upstreams.slice(0, 6)) {
      const ip = u.addr.replace(/:\d+$/, '')
      const c = containers.find((x) => x.ip === ip || x.name === ip)
      containerRows.push(
        <div key={u.addr} className={cx('flow-box', u.errors > 0 && u.errors / u.requests > 0.05 && 'danger')}>
          <span className="t"><Dot tone={u.errors && u.errors / u.requests > 0.05 ? 'danger' : 'ok'} />{c ? `${c.name.replace(/^\//, '')} · ` : ''}{u.addr}</span>
          <span className="s">{fmtMs(u.responseMs)} · {Math.round(u.sharePct)}%</span>
        </div>,
      )
    }
  } else {
    const c = containers.find((x) => x.hostId === host.id || x.ip === host.upstream.host || x.name === host.upstream.host)
    containerRows.push(
      <div key="cfg" className={cx('flow-box', status === 'down' && 'danger')}>
        <span className="t"><Dot tone={tone(status)} />{c ? `${c.name.replace(/^\//, '')} · ` : ''}{upstreamTarget}</span>
        <span className="s">{hs ? fmtMs(hs.latencyMs) : '—'}</span>
      </div>,
    )
  }

  return (
    <>
      <TopBar
        title={
          <div className="flow-crumb">
            <Link to="/topology">Traffic flow</Link>
            <span className="sep">/</span>
            <Menu
              align="start"
              trigger={<button type="button" className="cur">{domain}<span style={{ fontSize: 9, color: 'var(--ink-faint)' }}>▼</span></button>}
              items={(hosts.data ?? []).slice().sort((a, b) => (a.domains[0] ?? '').localeCompare(b.domains[0] ?? '')).slice(0, 40).map((h) => ({
                label: h.domains[0] ?? h.id, icon: h.id === host.id ? ('check' as const) : undefined, onSelect: () => navigate(`/topology/hosts/${h.id}`),
              }))}
            />
          </div>
        }
        actions={
          <div className="row gap-8">
            <Segmented<Mode> value={mode} onChange={setMode} options={[{ value: 'live', label: 'Live' }, { value: 'hour', label: 'Last hour' }]} />
            <Button size="sm" onClick={() => navigate(`/hosts?edit=${host.id}`)}>Open host</Button>
            <Button size="sm" variant="ghost" icon="logs" onClick={() => navigate(logsLink)}>Logs</Button>
          </div>
        }
      />
      <div className="page">
        <div className={cx('card flow-card', live && 'flow-live')}>
          <div className="flow-grid" style={{ gridTemplateColumns: columns }}>
            <Col label="Clients">
              <div className="flow-box">
                <span className="t">{dominantSource(flow)}</span>
                <span className="s">{flow ? `${fmtCount(flow.clients.unique)} unique IPs` : '—'}</span>
              </div>
            </Col>
            <Hop
              live={live}
              label={flow && flow.tlsPct > 0 ? `tls ${fmtPct(flow.tlsPct)}` : 'http'}
              limited={flow ? flow.rateLimitedPct + pct(flow.clients.blocked) : 0}
              sub={`${fmtRate(t?.rps ?? 0)} · ${fmtPct(flow?.rateLimitedPct ?? 0)} limited`}
            />
            <Col label={`Reverse proxy · ${proxy.label}`}>
              <Link to={`/hosts?edit=${host.id}`} className={cx('flow-box', status === 'down' && 'danger')}>
                <span className="t"><Dot tone={tone(status)} />{domain}</span>
                <span className="s">{hostFeatures(host, acl?.name)}</span>
              </Link>
            </Col>
            <Hop live={live} label={`proxy ${fmtMs(proxyMs)}`} sub={throughLB && fe ? fe.bind : upstreamTarget} />
            {throughLB && (
              <>
                <Col label={`Frontend · ${lbEngine.label}`}>
                  {fe ? (
                    <Link to={`/load-balancer/frontends?edit=${fe.id}`} className="flow-box">
                      <span className="t">{fe.name}</span>
                      <span className="s">{fe.mode}{backend?.forwardClientIp ? ' · forwardfor' : ''}{fe.acceptProxy ? ' · proxy-protocol' : ''}</span>
                    </Link>
                  ) : (
                    <div className="flow-box"><span className="t">{upstreamTarget}</span><span className="s">direct to backend</span></div>
                  )}
                </Col>
                <Hop live={live} label={`route${bs ? ` · queue ${bs.queue}` : ''}`} sub={ruleText} />
                <Col label="Backend">
                  {backend ? (
                    <Link to={`/load-balancer/backends?edit=${backend.id}`} className="flow-box dark">
                      <span className="t">{backend.name}</span>
                      <span className="s">{backend.algorithm}{backend.sticky.enabled ? ` · ${backend.sticky.mode === 'source' ? 'sticky source' : `cookie ${backend.sticky.cookieName || 'SRVID'}`}` : ''}</span>
                    </Link>
                  ) : (
                    <div className="flow-box danger"><span className="t">missing backend</span></div>
                  )}
                </Col>
                <Hop live={live} label={`upstream ${fmtMs(bs?.respP95Ms ?? tm?.upstreamMs)}`} stack={backend?.servers.length ?? 0} sub={shares.length ? `${shares.join(' · ')} %` : '—'} />
              </>
            )}
            {!throughLB && <Hop live={live} label={`upstream ${fmtMs(tm?.upstreamMs)}`} sub={`connect ${fmtMs(tm?.connectMs)}`} />}
            <Col label={throughLB ? 'Servers' : 'Containers'}>
              <div className="flow-list">
                {containerRows}
                {!throughLB && flow && flow.upstreams.length > 6 && <span className="more">+{flow.upstreams.length - 6} more</span>}
              </div>
            </Col>
          </div>
        </div>

        <div className="flow-stats">
          <div className="card flow-stat">
            <span className="k">End-to-end p50 / p95</span>
            <span className="v">{t?.p50Ms != null ? Math.round(t.p50Ms) : '—'} / {t?.p95Ms != null ? `${Math.round(t.p95Ms)} ms` : '—'}</span>
            <span className="n">{fmtCount(t?.requests ?? 0)} requests · {range === '15m' ? '15 min' : '1 hour'}</span>
          </div>
          <div className="card flow-stat">
            <span className="k">Where time goes</span>
            <div className="flow-split" aria-hidden>
              <span style={{ width: `${split[0]}%`, background: '#8f8d87' }} />
              <span style={{ width: `${split[1]}%`, background: '#141414' }} />
              <span style={{ width: `${split[2]}%`, background: '#c9c8c4' }} />
            </div>
            <span className="n">{req > 0 ? `connect ${split[0]} % · ${throughLB ? 'lb+app' : 'app'} ${split[1]} % · proxy ${split[2]} %` : 'no proxied requests'}</span>
          </div>
          <div className="card flow-stat">
            <span className="k">Errors · {range === '15m' ? '15 min' : '1 hour'}</span>
            <span className={cx('v', errTone)}>{fmtPct(flow?.errorPct ?? 0, 1)}</span>
            <span className="n">{fmtCount(t?.s5xx ?? 0)} 5xx · {fmtCount(t?.s4xx ?? 0)} 4xx</span>
          </div>
          <div className="card flow-stat">
            <span className="k">Rate-limited</span>
            <span className={cx('v', flow && flow.rateLimitedPct > 0 && 'warn')}>{fmtPct(flow?.rateLimitedPct ?? 0, 1)}</span>
            <span className="n">429 responses</span>
          </div>
          <div className="card flow-stat">
            <span className="k">Unique clients</span>
            <span className="v">{fmtCount(flow?.clients.unique ?? 0)}</span>
            <span className="n">{flow?.clients.requests ? `LAN ${pct(flow.clients.lan)} % · VPN ${pct(flow.clients.vpn)} % · Net ${pct(flow.clients.internet)} %` : 'no requests'}</span>
          </div>
        </div>
        <p className="flow-note">
          Timings are averages from {proxy.label}’s access log: <em>proxy</em> is the time spent in Relay outside the upstream call, <em>upstream</em> runs from connecting to the
          upstream until its response ended{throughLB ? ', including the load balancer' : ''}. Server response times come from the {lbEngine.label} stats.
          {flowQ.isError && <Fragment> Traffic data is unavailable right now.</Fragment>}
        </p>
      </div>
    </>
  )
}

function useModeState(): [Mode, (m: Mode) => void] {
  const [sp, setSp] = useSearchParams()
  const mode: Mode = sp.get('range') === '1h' ? 'hour' : 'live'
  return [mode, (m) => setSp(m === 'hour' ? { range: '1h' } : {}, { replace: true })]
}
