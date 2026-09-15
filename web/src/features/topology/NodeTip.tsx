// Owner: slice observe — Topology node details (design 31, dark tooltip).
import { useState, type ReactNode } from 'react'
import { useNavigate } from 'react-router-dom'
import { useQueryClient } from '@tanstack/react-query'
import { useToast } from '../../components/ui'
import { api } from '../../lib/api'
import { keys, useRole } from '../../lib/queries'
import type { HealthStatus } from '../../lib/types'
import { healthSummary, useServerActions } from '../loadbalancer/lbApi'
import { useNow } from '../overview/api'
import { fmtBytesRate, fmtCount, fmtDuration, fmtMs, fmtPct, fmtRate, type FlowRange } from './api'
import { fmtValue, hostFrontends, type TopoInput, type TopoNode } from './graph'

type Field = [label: string, value: ReactNode, tone?: 'ok' | 'warn' | 'danger', wide?: boolean]
interface Action { label: string; onClick: () => void | Promise<void>; danger?: boolean; disabled?: boolean }

const dotTone = (s: TopoNode['status']) => (s === 'healthy' ? 'ok' : s === 'degraded' ? 'warn' : s === 'down' ? 'danger' : 'muted')
const logRange = (r: FlowRange) => (r === '5m' ? '15m' : r)

function clock(now: number, msAgo: number): string {
  const d = new Date(now - msAgo)
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false })
}

export function NodeTip({ node, input, range, onClose }: { node: TopoNode; input: TopoInput; range: FlowRange; onClose: () => void }) {
  const navigate = useNavigate()
  const toast = useToast()
  const qc = useQueryClient()
  const { canWrite } = useRole()
  const serverActions = useServerActions()
  const [busy, setBusy] = useState(false)
  const now = useNow(5_000)
  const go = (to: string) => {
    onClose()
    navigate(to)
  }
  const { topo, lb, metric } = input
  const win = topo?.windowSec ?? 900

  let title = node.title
  let tag: string | undefined
  let fields: Field[] = []
  let actions: Action[] = []

  switch (node.kind) {
    case 'clients': {
      const c = topo?.clients
      const pct = (n: number) => (c && c.requests ? fmtPct((n / c.requests) * 100) : '0%')
      title = 'Clients'
      tag = `last ${range}`
      fields = c
        ? [
            ['Unique addresses', fmtCount(c.unique)],
            ['Requests', `${fmtCount(c.requests)} · ${fmtRate(topo!.totals.rps)}`],
            ['LAN', `${fmtCount(c.lan)} · ${pct(c.lan)}`],
            ['VPN', `${fmtCount(c.vpn)} · ${pct(c.vpn)}`],
            ['Internet', `${fmtCount(c.internet)} · ${pct(c.internet)}`],
            ['Blocked', `${fmtCount(c.blocked)} · ${pct(c.blocked)}`, c.blocked ? 'danger' : undefined],
            ['Unmatched hosts', `${fmtCount(topo!.unknown.requests)} req`, undefined, true],
          ]
        : [['Traffic', 'No requests yet', undefined, true]]
      actions = [
        { label: 'Access logs', onClick: () => go(`/logs/access?range=${logRange(range)}`) },
        { label: 'Blocked', onClick: () => go(`/logs/access?status=403&range=${logRange(range)}`) },
      ]
      break
    }
    case 'proxy': {
      const st = input.proxy.state
      const t = topo?.totals
      title = `Relay · ${input.proxy.label}`
      tag = st?.version
      fields = [
        ['State', st ? (st.running ? 'RUNNING' : st.reachable ? 'STOPPED' : 'UNREACHABLE') : '—', st?.running ? 'ok' : 'danger'],
        ['Up since', st?.startedAt ? fmtDuration((now - Date.parse(st.startedAt)) / 1000) : '—'],
        ['Traffic', `${fmtRate(t?.rps ?? 0)} · ${fmtBytesRate(t?.bytesPerSec ?? 0)}`],
        ['Latency p50 / p95', `${fmtMs(t?.p50Ms)} / ${fmtMs(t?.p95Ms)}`],
        ['5xx', t ? `${fmtCount(t.s5xx)} · ${fmtPct(t.requests ? (t.s5xx / t.requests) * 100 : 0, 2)}` : '—', t && t.s5xx ? 'danger' : undefined],
        ['Last reload', st?.lastReloadAt ? `${fmtDuration((now - Date.parse(st.lastReloadAt)) / 1000)} ago` : '—'],
      ]
      actions = [
        { label: 'Hosts', onClick: () => go('/hosts') },
        { label: 'Logs', onClick: () => go(`/logs/error?source=${input.proxy.label.toLowerCase().includes('edge') ? 'edge' : 'nginx'}`) },
        { label: 'Engine settings', onClick: () => go('/settings/general') },
      ]
      break
    }
    case 'lb': {
      const st = input.lbEngine.state
      const binds = input.frontends.filter((f) => f.enabled).map((f) => `${f.name} ${f.bind}`)
      const viaHosts = input.hosts.filter((h) => hostFrontends(h, input.frontends).length > 0 || !!h.upstream.backendId).map((h) => h.domains[0])
      title = `Load balancer · ${input.lbEngine.label}`
      tag = st?.version
      fields = [
        ['State', lb ? (lb.running ? 'RUNNING' : 'NOT RUNNING') : '—', lb?.running ? 'ok' : 'danger'],
        ['Sessions', lb ? `${fmtRate(lb.sessRate, 'sess/s')} · peak ${lb.peakSessRate}` : '—'],
        ['Queue · retries', lb ? `${lb.queue} · ${lb.retries}` : '—', lb && lb.queue ? 'warn' : undefined],
        ['Errors', lb ? `conn ${lb.connErrors} · resp ${lb.respErrors}` : '—', lb && lb.connErrors + lb.respErrors ? 'warn' : undefined],
        ['Frontends', binds.length ? binds.slice(0, 3).join(', ') + (binds.length > 3 ? ` +${binds.length - 3}` : '') : 'none', undefined, true],
        ['Proxy hosts', viaHosts.length ? viaHosts.slice(0, 3).join(', ') + (viaHosts.length > 3 ? ` +${viaHosts.length - 3}` : '') : 'none (clients connect directly)', undefined, true],
      ]
      actions = [
        { label: 'Backends', onClick: () => go('/load-balancer/backends') },
        { label: 'Frontends', onClick: () => go('/load-balancer/frontends') },
        { label: 'Stats', onClick: () => go('/load-balancer/stats') },
      ]
      break
    }
    case 'backend': {
      const b = input.backends.find((x) => x.id === node.backendId)!
      const bs = lb?.backends.find((x) => x.id === node.backendId)
      const up = bs ? bs.servers.filter((s) => s.status === 'UP' || s.status === 'NOCHECK').length : 0
      title = b.name
      tag = `${b.mode} · ${b.algorithm}`
      fields = [
        ['State', bs?.status ?? '—', node.status === 'healthy' ? 'ok' : node.status === 'down' ? 'danger' : 'warn'],
        ['Servers up', bs ? `${up} / ${b.servers.length}` : `${b.servers.length} configured`],
        ['Sessions', bs ? `${fmtRate(bs.sessRate, 'sess/s')} · ${bs.current} now` : '—'],
        ['Response p95', fmtMs(bs?.respP95Ms)],
        ['Queue', bs ? String(bs.queue) : '—', bs && bs.queue ? 'warn' : undefined],
        ['Health check', healthSummary(b).label],
        ['Sticky', b.sticky.enabled ? `${b.sticky.mode}${b.sticky.cookieName ? ` · ${b.sticky.cookieName}` : ''}` : 'off', undefined, true],
      ]
      actions = [
        { label: 'Open backend', onClick: () => go(`/load-balancer/backends?edit=${b.id}`) },
        { label: 'Stats', onClick: () => go('/load-balancer/stats') },
        { label: 'Logs', onClick: () => go(`/logs/error?source=${input.lbEngine.label.toLowerCase().includes('balancer') ? 'balancer' : 'haproxy'}`) },
      ]
      break
    }
    case 'server': {
      const b = input.backends.find((x) => x.id === node.backendId)!
      const s = b.servers.find((x) => x.id === node.serverId)!
      const bs = lb?.backends.find((x) => x.id === b.id)
      const ss = bs?.servers.find((x) => x.id === s.id || x.name === s.name)
      const others = bs?.servers.filter((x) => x !== ss && (x.status === 'UP' || x.status === 'NOCHECK')).map((x) => x.name) ?? []
      title = `${s.name} · ${s.address}:${s.port}`
      tag = b.name
      const down = ss?.status === 'DOWN'
      fields = [
        ['State', ss ? `${ss.status}${down ? ` · ${fmtDuration(ss.downSec)}` : ss.uptimeSec ? ` · ${fmtDuration(ss.uptimeSec)}` : ''}` : s.state.toUpperCase(), down ? 'danger' : ss?.status === 'UP' ? 'ok' : 'warn'],
        ['Health check', ss?.checkDetail || (s.check ? healthSummary(b).label : 'not checked')],
        ['Last OK', down ? clock(now, ss!.downSec * 1000) : ss ? 'now' : '—'],
        ['Traffic now', down ? `0 sess/s${others.length ? ` → ${others.length === 1 ? others[0] : `${others[0]} +${others.length - 1}`}` : ''}` : ss ? `${fmtRate(ss.sessRate, 'sess/s')} · ${Math.round(ss.sharePct)}%` : '—'],
        ['Response avg / p95', ss ? `${fmtMs(ss.respAvgMs)} / ${fmtMs(ss.respP95Ms)}` : '—'],
        ['Weight · role', `${s.weight} · ${s.role}`],
      ]
      actions = [
        { label: 'Open server', onClick: () => go(`/load-balancer/backends?edit=${b.id}`) },
        { label: 'Logs', onClick: () => go(`/logs/error?source=${input.lbEngine.label.toLowerCase().includes('balancer') ? 'balancer' : 'haproxy'}`) },
      ]
      if (canWrite && s.check) {
        actions.push({
          label: busy ? 'Checking…' : 'Retry check',
          danger: true,
          disabled: busy,
          onClick: async () => {
            setBusy(true)
            try {
              const r = await serverActions.check(b.id, s.id)
              toast.show({ kind: r.ok ? 'success' : 'error', title: r.ok ? `${s.name} passed its check` : `${s.name} failed its check`, message: r.detail || r.check })
              qc.invalidateQueries({ queryKey: keys.lbStats })
            } catch (err) {
              toast.show({ kind: 'error', title: 'Check failed', message: err instanceof Error ? err.message : String(err) })
            } finally {
              setBusy(false)
            }
          },
        })
      }
      break
    }
    case 'host':
    case 'upstream': {
      const h = input.hosts.find((x) => x.id === node.hostId)!
      const t = topo?.hosts[h.id]
      const hs: HealthStatus | undefined = input.health[`host:${h.id}`]
      const acl = input.accessLists.find((a) => a.id === h.accessListId)
      title = h.domains[0] ?? node.title
      tag = acl?.name ?? 'public'
      fields = [
        ['State', `${node.status.toUpperCase()}${hs?.httpStatus ? ` · ${hs.httpStatus}` : ''}${hs?.changedAt && node.status === 'down' ? ` · ${fmtDuration((now - Date.parse(hs.changedAt)) / 1000)}` : ''}`, node.status === 'healthy' ? 'ok' : node.status === 'down' ? 'danger' : node.status === 'degraded' ? 'warn' : undefined],
        ['Upstream', `${h.upstream.scheme}://${h.upstream.host}:${h.upstream.port}`],
        ['Traffic now', t ? fmtValue(metric === 'requests' ? t.rps : metric === 'bandwidth' ? t.bytesPerSec : t.s5xx / win, metric) : '0 r/s'],
        ['p50 / p95', t ? `${fmtMs(t.p50Ms)} / ${fmtMs(t.p95Ms)}` : '—'],
        ['5xx', t ? `${fmtCount(t.s5xx)} · ${fmtPct(t.requests ? (t.s5xx / t.requests) * 100 : 0, 1)}` : '—', t && t.s5xx ? 'danger' : undefined],
        ['TLS', h.certificateId ? 'certificate' : 'http only'],
      ]
      if (hs?.detail && node.status !== 'healthy') fields.push(['Check', hs.detail, 'danger', true])
      if (node.kind === 'upstream') fields.splice(1, 0, ['Location', node.lines[0]?.[0]?.text ?? '', undefined, true])
      actions = [
        { label: 'Open host', onClick: () => go(`/hosts?edit=${h.id}`) },
        { label: 'View flow', onClick: () => go(`/topology/hosts/${h.id}`) },
        { label: 'Logs', onClick: () => go(`/logs/access?host=${encodeURIComponent(h.domains[0] ?? '')}&range=${logRange(range)}`) },
      ]
      if (canWrite && h.upstream.host) {
        actions.push({
          label: busy ? 'Testing…' : 'Test',
          danger: node.status === 'down',
          disabled: busy,
          onClick: async () => {
            setBusy(true)
            try {
              const r = await api.post<HealthStatus>('/api/health/probe', { upstream: h.upstream })
              toast.show({ kind: r.status === 'healthy' ? 'success' : r.status === 'degraded' ? 'warning' : 'error', title: `Upstream ${r.status}`, message: r.detail || `${fmtMs(r.latencyMs)}` })
              qc.invalidateQueries({ queryKey: keys.health })
            } catch (err) {
              toast.show({ kind: 'error', title: 'Upstream test failed', message: err instanceof Error ? err.message : String(err) })
            } finally {
              setBusy(false)
            }
          },
        })
      }
      break
    }
    case 'stream': {
      const s = input.streams.find((x) => x.id === node.streamId)!
      const t = topo?.streams[s.id]
      title = s.name
      tag = s.protocol
      fields = [
        ['State', node.status.toUpperCase(), node.status === 'healthy' ? 'ok' : node.status === 'down' ? 'danger' : undefined],
        ['Listen', `${s.listenAddress || '*'}:${s.listenPorts}`],
        ['Forward', `${s.forwardHost}:${s.forwardPorts || s.listenPorts}`],
        ['Sessions', t ? `${fmtCount(t.requests)} · ${fmtRate(t.rps, 'sess/s')}` : '0'],
        ['Bandwidth', fmtBytesRate(t?.bytesPerSec ?? 0), undefined, true],
      ]
      actions = [
        { label: 'Open stream', onClick: () => go(`/streams?edit=${s.id}`) },
        { label: 'Logs', onClick: () => go(`/logs/access?q=${encodeURIComponent(`stream:${s.id}`)}&kind=stream`) },
      ]
      break
    }
    default:
      break
  }

  return (
    <>
      <div className="topo-tip-head">
        <span className={`dot ${dotTone(node.status)}`} />
        <span className="t" title={title}>{title}</span>
        {tag && <span className="tag" title={tag}>{tag}</span>}
        <button type="button" className="x" onClick={onClose} aria-label="Close">×</button>
      </div>
      {fields.length > 0 && (
        <div className="topo-tip-grid">
          {fields.map(([k, v, tone, wide]) => (
            <div key={k} className={wide ? 'wide' : undefined}>
              <div className="k">{k}</div>
              <div className={`v ${tone ?? ''}`}>{v}</div>
            </div>
          ))}
        </div>
      )}
      {actions.length > 0 && (
        <div className="topo-tip-actions">
          {actions.map((a) => (
            <button key={a.label} type="button" className={a.danger ? 'danger' : undefined} disabled={a.disabled} onClick={() => void a.onClick()}>
              {a.label}
            </button>
          ))}
        </div>
      )}
    </>
  )
}
