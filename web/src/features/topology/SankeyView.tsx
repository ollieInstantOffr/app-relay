// Owner: slice observe — Topology Sankey view: sources → Relay → hosts → backends/upstreams → servers.
import { useLayoutEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { useNavigate } from 'react-router-dom'
import { Menu, cx } from '../../components/ui'
import { FLOW_RANGES, LIVE_MS, fmtRate, shortAddr, type FlowRange } from './api'
import { METRICS, fmtValue, hostContainer, hostFrontends, hostTitle, serverStatus, valueOf, viaLB, type Metric, type SourceKey, type TopoFilters, type TopoInput } from './graph'

interface SNode { id: string; col: number; label: string; color: string; value: number; to?: string; err?: boolean; y: number; h: number }
interface SLink { from: string; to: string; value: number; err?: boolean; sy: number; ty: number; w: number }

const COLS = ['Source', 'Proxy', 'Hosts', 'Backends & upstreams', 'Servers']
const SOURCE_META: Record<SourceKey, { label: string; color: string }> = {
  lan: { label: 'LAN', color: '#141414' },
  vpn: { label: 'VPN', color: '#6b7280' },
  internet: { label: 'Internet', color: '#a8a6a0' },
  blocked: { label: 'Blocked', color: '#dc2626' },
}
const NODE_W = 12
const GAP = 14
const TOP = 88
const MAX_SANKEY_HOSTS = 14

export function SankeyView({
  input, filters, metric, onMetric, range, onRange, live, onLive, animate, leftControls,
}: {
  input: TopoInput
  filters: TopoFilters
  metric: Metric
  onMetric: (m: Metric) => void
  range: FlowRange
  onRange: (r: FlowRange) => void
  live: boolean
  onLive: (v: boolean) => void
  animate: boolean
  leftControls?: ReactNode
}) {
  const ref = useRef<HTMLDivElement>(null)
  const navigate = useNavigate()
  const [width, setWidth] = useState(1000)
  const [hover, setHover] = useState<string | null>(null)

  useLayoutEffect(() => {
    const el = ref.current
    if (!el) return
    const ro = new ResizeObserver(() => setWidth(el.clientWidth))
    ro.observe(el)
    setWidth(el.clientWidth)
    return () => ro.disconnect()
  }, [])

  const chart = useMemo(() => {
    const { topo, lb, frontends, containers } = input
    const win = topo?.windowSec ?? 900
    const nodes = new Map<string, SNode>()
    const links: SLink[] = []
    const add = (n: Omit<SNode, 'y' | 'h' | 'value'> & { value?: number }) => {
      if (!nodes.has(n.id)) nodes.set(n.id, { y: 0, h: 0, value: 0, ...n })
      return nodes.get(n.id)!
    }
    const link = (from: string, to: string, value: number, err?: boolean) => {
      if (value > 0) links.push({ from, to, value, err, sy: 0, ty: 0, w: 0 })
    }
    const statusOk = (s: string) => !filters.statuses.length || (filters.statuses as string[]).includes(s)

    // Hosts and their values
    const hostRows = input.hosts
      .filter((h) => filters.layers.hosts || (filters.layers.lb && viaLB(h, frontends)))
      .filter((h) => filters.layers.lb || !viaLB(h, frontends))
      .filter((h) => !filters.access.length || filters.access.includes(h.accessListId ?? 'public'))
      .map((h) => {
        const status = !h.enabled ? 'disabled' : input.health[`host:${h.id}`]?.status ?? 'unknown'
        return { h, status, value: valueOf(topo?.hosts[h.id], topo?.hostClients[h.id], metric, filters.sources, win) }
      })
      .filter((r) => statusOk(r.status) && r.value > 0)
      .sort((a, z) => z.value - a.value)
    const shown = hostRows.slice(0, MAX_SANKEY_HOSTS)
    const rest = hostRows.slice(MAX_SANKEY_HOSTS)
    const hostTotal = hostRows.reduce((a, r) => a + r.value, 0)
    const unknown = filters.sources.length || metric === 'errors' ? 0 : metric === 'requests' ? topo?.unknown.rps ?? 0 : topo?.unknown.bytesPerSec ?? 0
    const relayTotal = hostTotal + unknown

    // Sources → Relay (split relay traffic by the client mix)
    const cl = topo?.clients
    add({ id: 'relay', col: 1, label: `Relay · ${input.proxy.label}`, color: '#141414', to: undefined })
    const srcKeys = (filters.sources.length ? filters.sources : (['lan', 'vpn', 'internet', 'blocked'] as SourceKey[]))
    const srcSum = cl ? srcKeys.reduce((a, k) => a + cl[k], 0) : 0
    for (const k of srcKeys) {
      const v = cl && srcSum > 0 ? (relayTotal * cl[k]) / srcSum : 0
      if (v <= 0) continue
      add({ id: `src:${k}`, col: 0, label: SOURCE_META[k].label, color: SOURCE_META[k].color, err: k === 'blocked', to: k === 'blocked' ? '/logs/access?status=403' : undefined })
      link(`src:${k}`, 'relay', v, k === 'blocked')
    }

    // Relay → hosts
    for (const r of shown) {
      const down = r.status === 'down'
      add({ id: `host:${r.h.id}`, col: 2, label: r.h.domains[0] ?? hostTitle(r.h, containers), color: down ? '#dc2626' : '#8f8d87', err: down, to: `/topology/hosts/${r.h.id}` })
      const t = topo?.hosts[r.h.id]
      link('relay', `host:${r.h.id}`, r.value, down || (!!t && t.requests > 0 && t.s5xx / t.requests >= 0.05))
    }
    if (rest.length) {
      add({ id: 'hosts:other', col: 2, label: `${rest.length} other hosts`, color: '#c9c8c4', to: '/hosts' })
      link('relay', 'hosts:other', rest.reduce((a, r) => a + r.value, 0))
    }
    if (unknown > 0) {
      add({ id: 'unknown', col: 2, label: 'Unmatched host', color: '#c9c8c4', to: '/hosts/default' })
      link('relay', 'unknown', unknown)
    }

    // Hosts → backends / upstreams → servers
    if (filters.layers.upstreams) {
      const lbStats = new Map((lb?.backends ?? []).map((b) => [b.id, b]))
      const backendIn = new Map<string, number>()
      for (const r of shown) {
        if (viaLB(r.h, frontends)) {
          const fe = hostFrontends(r.h, frontends)[0]
          const bid = r.h.upstream.backendId || fe?.defaultBackendId
          const b = input.backends.find((x) => x.id === bid)
          if (!b) continue
          const st = lbStats.get(b.id)
          const down = st?.status === 'DOWN'
          add({ id: `backend:${b.id}`, col: 3, label: `${b.name} · ${b.algorithm}`, color: down ? '#dc2626' : '#141414', err: down, to: `/load-balancer/backends?edit=${b.id}` })
          link(`host:${r.h.id}`, `backend:${b.id}`, r.value, down)
          backendIn.set(b.id, (backendIn.get(b.id) ?? 0) + r.value)
        } else {
          const c = hostContainer(r.h, containers)
          const key = `${r.h.upstream.host}:${r.h.upstream.port}`
          const down = r.status === 'down'
          add({ id: `up:${key}`, col: 3, label: c ? c.name.replace(/^\//, '') : `${shortAddr(r.h.upstream.host)}:${r.h.upstream.port}`, color: down ? '#dc2626' : '#8f8d87', err: down })
          link(`host:${r.h.id}`, `up:${key}`, r.value, down)
        }
      }
      for (const [bid, v] of backendIn) {
        const b = input.backends.find((x) => x.id === bid)!
        const st = lbStats.get(bid)
        const servers = b.servers.map((s) => ({ s, ss: st?.servers.find((x) => x.id === s.id || x.name === s.name) }))
        const shareSum = servers.reduce((a, x) => a + (x.ss?.sharePct ?? 0), 0)
        for (const { s, ss } of servers) {
          const status = serverStatus(ss, s)
          if (!statusOk(status)) continue
          const share = shareSum > 0 ? (ss?.sharePct ?? 0) / shareSum : servers.length ? 1 / servers.length : 0
          if (share <= 0 && status !== 'down') continue
          add({ id: `server:${bid}/${s.id}`, col: 4, label: `${s.name} · ${shortAddr(s.address)}`, color: status === 'down' ? '#dc2626' : status === 'degraded' ? '#d97706' : '#1fa971', err: status === 'down', to: `/load-balancer/backends?edit=${bid}` })
          link(`backend:${bid}`, `server:${bid}/${s.id}`, v * share)
        }
      }
    }

    // Values, columns, layout
    for (const l of links) {
      nodes.get(l.from)!.value = Math.max(nodes.get(l.from)!.value, 0)
    }
    const inSum = new Map<string, number>()
    const outSum = new Map<string, number>()
    for (const l of links) {
      outSum.set(l.from, (outSum.get(l.from) ?? 0) + l.value)
      inSum.set(l.to, (inSum.get(l.to) ?? 0) + l.value)
    }
    for (const n of nodes.values()) n.value = Math.max(inSum.get(n.id) ?? 0, outSum.get(n.id) ?? 0)
    const all = [...nodes.values()].filter((n) => n.value > 0)
    const cols = Math.max(...all.map((n) => n.col), 2) + 1
    const byCol = Array.from({ length: cols }, (_, i) => all.filter((n) => n.col === i))
    const maxCount = Math.max(...byCol.map((c) => c.length), 1)
    const height = Math.max(420, maxCount * 40 + 40)
    const scale = Math.min(...byCol.filter((c) => c.length).map((c) => (height - GAP * (c.length - 1)) / c.reduce((a, n) => a + n.value, 0)))
    const colX = (i: number) => 150 + (i * (Math.max(width, 900) - 150 - 230 - NODE_W)) / Math.max(1, cols - 1)
    for (const c of byCol) {
      const total = c.reduce((a, n) => a + Math.max(2, n.value * scale), 0) + GAP * (c.length - 1)
      let y = TOP + (height - total) / 2
      for (const n of c) {
        n.h = Math.max(2, n.value * scale)
        n.y = y
        y += n.h + GAP
      }
    }
    const pos = new Map(all.map((n) => [n.id, n]))
    const visible = links.filter((l) => pos.has(l.from) && pos.has(l.to))
    const outs = new Map<string, number>()
    const ins = new Map<string, number>()
    for (const l of [...visible].sort((a, z) => pos.get(a.to)!.y - pos.get(z.to)!.y)) {
      const from = pos.get(l.from)!
      l.w = Math.max(1, l.value * scale)
      l.sy = from.y + (outs.get(l.from) ?? 0)
      outs.set(l.from, (outs.get(l.from) ?? 0) + l.w)
    }
    for (const l of [...visible].sort((a, z) => pos.get(a.from)!.y - pos.get(z.from)!.y)) {
      const to = pos.get(l.to)!
      l.ty = to.y + (ins.get(l.to) ?? 0)
      ins.set(l.to, (ins.get(l.to) ?? 0) + l.w)
    }
    return { nodes: all, links: visible, pos, cols, colX, height: TOP + height + 40, total: relayTotal }
  }, [input, filters, metric, width])

  const lit = (l: SLink) => hover && (l.from === hover || l.to === hover)
  const svgW = Math.max(width, 900)

  return (
    <div ref={ref} className="topo-sankey">
      {leftControls}
      <div className="topo-ctl topo-toolbar">
        <button type="button" className={cx('topo-tb', live ? 'live' : 'paused')} onClick={() => onLive(!live)}>
          <span className="live-dot" />
          {live ? `Live · ${Math.round(LIVE_MS / 1000)} s` : 'Paused'}
        </button>
        <Menu
          trigger={<button type="button" className="topo-tb">{METRICS.find((m) => m.value === metric)?.label}<span className="caret">▼</span></button>}
          items={[{ header: 'Band width shows' }, ...METRICS.map((m) => ({ label: m.label, icon: m.value === metric ? ('check' as const) : undefined, onSelect: () => onMetric(m.value) }))]}
        />
        <Menu
          trigger={<button type="button" className="topo-tb">{FLOW_RANGES.find((r) => r.value === range)?.label}<span className="caret">▼</span></button>}
          items={FLOW_RANGES.map((r) => ({ label: r.label, icon: r.value === range ? ('check' as const) : undefined, onSelect: () => onRange(r.value) }))}
        />
      </div>
      {chart.nodes.length <= 1 ? (
        <div className="topo-empty">
          <div className="flow-note">No {metric === 'errors' ? 'errors' : 'traffic'} in the {FLOW_RANGES.find((r) => r.value === range)?.label.toLowerCase()} matches the filters.</div>
        </div>
      ) : (
        <svg width={svgW} height={chart.height} role="img" aria-label="Traffic Sankey diagram">
          {COLS.slice(0, chart.cols).map((c, i) => (
            <text key={c} className="sk-col" x={chart.colX(i) + (i === chart.cols - 1 ? NODE_W : 0)} y={TOP - 22} textAnchor={i === chart.cols - 1 ? 'end' : 'start'}>{c}</text>
          ))}
          {chart.links.map((l, i) => {
            const a = chart.pos.get(l.from)!
            const b = chart.pos.get(l.to)!
            const x0 = chart.colX(a.col) + NODE_W
            const x1 = chart.colX(b.col)
            const xm = (x0 + x1) / 2
            const d = `M${x0} ${l.sy} C${xm} ${l.sy} ${xm} ${l.ty} ${x1} ${l.ty} L${x1} ${l.ty + l.w} C${xm} ${l.ty + l.w} ${xm} ${l.sy + l.w} ${x0} ${l.sy + l.w} Z`
            return (
              <path key={i} d={d} className={cx('sk-link', l.err && 'err', lit(l) && 'hl')} fill={l.err ? undefined : a.col === 0 ? a.color : '#141414'}>
                <title>{`${a.label} → ${b.label}: ${fmtValue(l.value, metric)}`}</title>
              </path>
            )
          })}
          {chart.nodes.map((n) => {
            const x = chart.colX(n.col)
            const last = n.col === chart.cols - 1
            const lx = last ? x - 8 : x + NODE_W + 8
            return (
              <g
                key={n.id}
                className="sk-node"
                onMouseEnter={() => setHover(n.id)}
                onMouseLeave={() => setHover(null)}
                onClick={() => n.to && navigate(n.to)}
                style={{ cursor: n.to ? 'pointer' : 'default' }}
              >
                <rect x={x} y={n.y} width={NODE_W} height={n.h} fill={n.color} className={cx(animate && n.col === 1 && 'sk-pulse')} />
                <text className="sk-label" x={lx} y={n.y + n.h / 2 - 1} textAnchor={last ? 'end' : 'start'} dominantBaseline="auto">{n.label}</text>
                <text className="sk-sub" x={lx} y={n.y + n.h / 2 + 12} textAnchor={last ? 'end' : 'start'}>
                  {fmtValue(n.value, metric)}{chart.total > 0 && n.col > 0 ? ` · ${Math.round((n.value / chart.total) * 100)}%` : ''}
                </text>
              </g>
            )
          })}
        </svg>
      )}
      {chart.nodes.length > 1 && metric === 'requests' && (
        <div className="flow-note" style={{ padding: '0 24px 20px' }}>
          Load balancer servers are split by each server’s share of its backend’s sessions. Total {fmtRate(chart.total)}.
        </div>
      )}
    </div>
  )
}
