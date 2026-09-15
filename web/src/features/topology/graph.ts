// Owner: slice observe — Topology tree (design 31): model, filters and layout.
import type { AccessList, Backend, BackendStats, Container, EngineState, Frontend, HealthStatus, LBStats, ProxyHost, ServerStats, Stream } from '../../lib/types'
import { fmtBytesRate, fmtCount, fmtMs, fmtPct, fmtRate, shortAddr, type FlowClients, type FlowTraffic, type Topology } from './api'

export type NodeKind = 'clients' | 'proxy' | 'lb' | 'host' | 'upstream' | 'backend' | 'server' | 'stream' | 'more'
export type NodeStatus = 'healthy' | 'degraded' | 'down' | 'disabled' | 'unknown'
export type Metric = 'requests' | 'bandwidth' | 'errors'
export type SourceKey = 'lan' | 'vpn' | 'internet' | 'blocked'
export type LayerKey = 'hosts' | 'lb' | 'upstreams' | 'streams'
export type Tone = 'ok' | 'warn' | 'danger' | 'muted'
export interface Seg { text: string; tone?: Tone }

export const METRICS: { value: Metric; label: string }[] = [
  { value: 'requests', label: 'Requests' },
  { value: 'bandwidth', label: 'Bandwidth' },
  { value: 'errors', label: 'Errors' },
]

export interface TopoNode {
  id: string
  kind: NodeKind
  title: string
  lines: Seg[][]
  /** Pill on the incoming link (host domain, stream port, LB bind). */
  badge?: { text: string; tls?: boolean; danger?: boolean }
  status: NodeStatus
  /** Traffic in the node's own unit (r/s, bytes/s, sess/s). */
  value: number
  errorRate: number
  children: TopoNode[]
  /** Visible children hidden by collapsing. */
  collapsedCount: number
  collapsible: boolean
  hostId?: string
  backendId?: string
  serverId?: string
  streamId?: string
  parent?: TopoNode
  // ---- layout (canvas px)
  /** Traffic as a share of all traffic, 0..1 (pipe width and speed). */
  share: number
  x: number
  y: number
  cw: number
  ch: number
  cardTop: number
  /** Below the card and its label: where the trunk to children starts. */
  bottom: number
  busY: number
  sw: number
  labelSide: 'right' | 'left' | 'below' | 'none'
}

export interface TopoFilters {
  /** Empty = every status. */
  statuses: ('healthy' | 'degraded' | 'down')[]
  layers: Record<LayerKey, boolean>
  /** Access list ids or "public"; empty = all. */
  access: string[]
  /** Empty = all sources. */
  sources: SourceKey[]
  hideIdle: boolean
}

export const DEFAULT_FILTERS: TopoFilters = {
  statuses: [],
  layers: { hosts: true, lb: true, upstreams: true, streams: false },
  access: [],
  sources: [],
  hideIdle: false,
}

export function filtersActive(f: TopoFilters): boolean {
  return f.statuses.length > 0 || f.access.length > 0 || f.sources.length > 0 || f.hideIdle ||
    (Object.keys(DEFAULT_FILTERS.layers) as LayerKey[]).some((k) => f.layers[k] !== DEFAULT_FILTERS.layers[k])
}

export interface TopoInput {
  hosts: ProxyHost[]
  backends: Backend[]
  frontends: Frontend[]
  streams: Stream[]
  accessLists: AccessList[]
  containers: Container[]
  health: Record<string, HealthStatus>
  lb?: LBStats
  topo?: Topology
  proxy: { label: string; state?: EngineState }
  lbEngine: { label: string; state?: EngineState }
  ports: { http?: number; https?: number }
  metric: Metric
}

export interface TopoCounts {
  status: Record<'healthy' | 'degraded' | 'down', number>
  layers: Record<LayerKey, number>
  access: { id: string; name: string; count: number }[]
  /** Percent of requests. */
  sources: Record<SourceKey, number>
}

export interface TopoGraph {
  root: TopoNode
  nodes: TopoNode[]
  counts: TopoCounts
  width: number
  height: number
  /** Hosts that reach backends through the load balancer. */
  lbHosts: ProxyHost[]
}

export const MAX_HOSTS = 7

// ---------------------------------------------------------------- status helpers

export function serverStatus(st: ServerStats | undefined, s?: Backend['servers'][number]): NodeStatus {
  if (!st) return s?.state === 'drain' || s?.state === 'maint' ? 'degraded' : 'unknown'
  switch (st.status) {
    case 'UP':
    case 'NOCHECK':
      return 'healthy'
    case 'DOWN':
      return 'down'
    default:
      return 'degraded'
  }
}

export function backendStatus(st: BackendStats | undefined): NodeStatus {
  if (!st) return 'unknown'
  return st.status === 'UP' ? 'healthy' : st.status === 'DOWN' ? 'down' : 'degraded'
}

function engineDown(state?: EngineState): boolean {
  return !!state && state.reachable && !state.running
}

function hostStatus(h: ProxyHost, inp: TopoInput): NodeStatus {
  if (!h.enabled) return 'disabled'
  if (engineDown(inp.proxy.state)) return 'down'
  return inp.health[`host:${h.id}`]?.status ?? 'unknown'
}

function streamStatus(s: Stream, inp: TopoInput): NodeStatus {
  if (!s.enabled) return 'disabled'
  if (engineDown(inp.proxy.state)) return 'down'
  return inp.health[`stream:${s.id}`]?.status ?? 'unknown'
}

/** Frontend(s) a host reaches through the load balancer. */
export function hostFrontends(h: ProxyHost, frontends: Frontend[]): Frontend[] {
  const byHost = frontends.filter((f) => f.hostId === h.id)
  if (byHost.length) return byHost
  const byBind = frontends.filter((f) => bindMatches(f.bind, h.upstream.host, h.upstream.port))
  if (byBind.length) return byBind
  const b = h.upstream.backendId
  return b ? frontends.filter((f) => f.defaultBackendId === b || f.rules.some((r) => r.backendId === b)) : []
}

export function bindMatches(bind: string, host: string, port: number): boolean {
  const i = bind.lastIndexOf(':')
  if (i < 0 || Number(bind.slice(i + 1)) !== port) return false
  const addr = bind.slice(0, i).replace(/^\[|\]$/g, '')
  const local = ['127.0.0.1', 'localhost', '::1']
  if (['', '*', '0.0.0.0', '::'].includes(addr)) return local.includes(host) || host === ''
  return addr === host
}

export function viaLB(h: ProxyHost, frontends: Frontend[]): boolean {
  return !!h.upstream.backendId || (!h.upstream.backendId && frontends.some((f) => bindMatches(f.bind, h.upstream.host, h.upstream.port)))
}

export function hostContainer(h: ProxyHost, containers: Container[]): Container | undefined {
  return containers.find((c) => c.hostId === h.id) ??
    containers.find((c) => !!h.upstream.host && (c.ip === h.upstream.host || c.name === h.upstream.host || c.upstreamHost === h.upstream.host))
}

export function serverContainer(b: Backend, address: string, containers: Container[]): Container | undefined {
  return containers.find((c) => c.backendId === b.id && (c.ip === address || c.name === address || c.upstreamHost === address)) ??
    containers.find((c) => c.ip === address || c.name === address)
}

export function hostTitle(h: ProxyHost, containers: Container[]): string {
  const c = hostContainer(h, containers)
  if (c) return c.name.replace(/^\//, '')
  const up = h.upstream.host
  if (up && !/^[\d.:[\]]+$/.test(up) && up !== 'localhost') return up.split('.')[0]
  return (h.domains[0] ?? 'host').split('.')[0]
}

// ---------------------------------------------------------------- values

export function valueOf(t: FlowTraffic | undefined, clients: FlowClients | undefined, metric: Metric, sources: SourceKey[], windowSec: number): number {
  if (!t) return 0
  let v = metric === 'requests' ? t.rps : metric === 'bandwidth' ? t.bytesPerSec : t.s5xx / Math.max(1, windowSec)
  if (sources.length) {
    if (!clients || clients.requests <= 0) return 0
    v *= sources.reduce((a, s) => a + clients[s], 0) / clients.requests
  }
  return v
}

export function fmtValue(v: number, metric: Metric): string {
  if (metric === 'bandwidth') return fmtBytesRate(v)
  return fmtRate(v, metric === 'errors' ? '5xx/s' : 'r/s')
}

// ---------------------------------------------------------------- build

function node(id: string, kind: NodeKind, title: string, status: NodeStatus, extra: Partial<TopoNode> = {}): TopoNode {
  return {
    id, kind, title, status, lines: [], value: 0, errorRate: 0, children: [], collapsedCount: 0, collapsible: false,
    share: 0, x: 0, y: 0, cw: 0, ch: 0, cardTop: 0, bottom: 0, busY: 0, sw: 0, labelSide: 'below', ...extra,
  }
}

export function buildGraph(inp: TopoInput, f: TopoFilters, collapsed: ReadonlySet<string>, showAllHosts: boolean): TopoGraph {
  const { topo, lb, metric } = inp
  const win = topo?.windowSec ?? 900
  const statusOk = (s: NodeStatus) => !f.statuses.length || (f.statuses as NodeStatus[]).includes(s)
  const idleOk = (v: number) => !f.hideIdle || v > 0
  const lbStatsById = new Map((lb?.backends ?? []).map((b) => [b.id, b]))
  const lbHosts = inp.hosts.filter((h) => viaLB(h, inp.frontends))
  const directHosts = inp.hosts.filter((h) => !viaLB(h, inp.frontends))

  // ---- counts (unfiltered)
  const counts: TopoCounts = {
    status: { healthy: 0, degraded: 0, down: 0 },
    layers: { hosts: inp.hosts.length, lb: inp.backends.length, upstreams: 0, streams: inp.streams.length },
    access: [],
    sources: { lan: 0, vpn: 0, internet: 0, blocked: 0 },
  }
  const countStatus = (s: NodeStatus) => {
    if (s === 'healthy' || s === 'degraded' || s === 'down') counts.status[s]++
  }
  inp.hosts.forEach((h) => countStatus(hostStatus(h, inp)))
  inp.streams.forEach((s) => countStatus(streamStatus(s, inp)))
  const upstreamKeys = new Set<string>()
  directHosts.forEach((h) => upstreamKeys.add(`${h.upstream.host}:${h.upstream.port}`))
  for (const b of inp.backends) {
    const bs = lbStatsById.get(b.id)
    for (const s of b.servers) {
      upstreamKeys.add(`${b.id}/${s.id}`)
      countStatus(serverStatus(bs?.servers.find((x) => x.id === s.id || x.name === s.name), s))
    }
  }
  counts.layers.upstreams = upstreamKeys.size
  const aclCount = new Map<string, number>()
  inp.hosts.forEach((h) => aclCount.set(h.accessListId ?? 'public', (aclCount.get(h.accessListId ?? 'public') ?? 0) + 1))
  counts.access = [
    { id: 'public', name: 'public', count: aclCount.get('public') ?? 0 },
    ...inp.accessLists.map((a) => ({ id: a.id, name: a.name, count: aclCount.get(a.id) ?? 0 })),
  ]
  const cl = topo?.clients
  if (cl && cl.requests > 0) {
    ;(['lan', 'vpn', 'internet', 'blocked'] as SourceKey[]).forEach((k) => (counts.sources[k] = (cl[k] / cl.requests) * 100))
  }

  // ---- root: clients → relay
  const totals = topo?.totals
  const clients = node('clients', 'clients', 'Clients', 'healthy', { labelSide: 'right' })
  const pctOf = (k: SourceKey) => (cl && cl.requests ? Math.round((cl[k] / cl.requests) * 100) : 0)
  clients.lines = [
    [{ text: cl ? `${fmtCount(cl.unique)} active · LAN ${pctOf('lan')}% · VPN ${pctOf('vpn')}% · Internet ${pctOf('internet')}%` : 'no traffic yet', tone: 'muted' }],
    [
      { text: `↓ ${fmtRate(totals?.rps ?? 0)}`, tone: 'ok' },
      { text: ` ↑ ${fmtBytesRate(totals ? totals.bytesOut / Math.max(1, win) : 0)}` },
    ],
  ]

  const pState = inp.proxy.state
  const relay = node('proxy', 'proxy', 'Relay · reverse proxy', engineDown(pState) ? 'down' : pState?.running ? 'healthy' : 'unknown', {
    labelSide: 'right', collapsible: true,
  })
  const ports = [inp.ports.http, inp.ports.https].filter(Boolean).map((p) => `:${p}`).join(' ')
  relay.lines = [
    [{ text: [[inp.proxy.label, pState?.version].filter(Boolean).join(' '), ports, `${inp.hosts.length} hosts`].filter(Boolean).join(' · '), tone: 'muted' }],
    engineDown(pState)
      ? [{ text: 'stopped', tone: 'danger' }]
      : [
          { text: `↓ ${fmtRate(totals?.rps ?? 0)}`, tone: 'ok' },
          { text: ` · p95 ${fmtMs(totals?.p95Ms)}` },
          { text: ` · ${fmtPct(totals && totals.requests ? (totals.s5xx / totals.requests) * 100 : 0, 2)} 5xx`, tone: totals && totals.requests && totals.s5xx / totals.requests > 0.01 ? 'danger' : undefined },
        ],
  ]
  clients.children = [relay]

  const relayKids: TopoNode[] = []

  // ---- load balancer
  if (f.layers.lb && inp.backends.length > 0) {
    const lState = inp.lbEngine.state
    const lbNode = node('lb', 'lb', 'Load balancer', !lb?.running && engineDown(lState) ? 'down' : 'healthy', { labelSide: 'left', collapsible: true })
    let down = 0
    let drain = 0
    let serverTotal = 0
    const backendNodes: TopoNode[] = []
    for (const b of [...inp.backends].sort((a, z) => a.name.localeCompare(z.name))) {
      const bs = lbStatsById.get(b.id)
      const bStatus = backendStatus(bs)
      const bNode = node(`backend:${b.id}`, 'backend', `${b.name} · ${b.algorithm} · ${fmtRate(bs?.sessRate ?? 0, 'sess/s')}`, bStatus, {
        backendId: b.id, value: bs?.sessRate ?? 0, errorRate: bStatus === 'down' ? 1 : 0, labelSide: 'none', collapsible: true,
      })
      const servers: TopoNode[] = []
      for (const s of b.servers) {
        serverTotal++
        const ss = bs?.servers.find((x) => x.id === s.id || x.name === s.name)
        const st = serverStatus(ss, s)
        if (ss?.status === 'DOWN') down++
        else if (ss && ss.status !== 'UP' && ss.status !== 'NOCHECK') drain++
        if (!f.layers.upstreams || !statusOk(st) || !idleOk(ss?.sessRate ?? 0)) continue
        const sNode = node(`server:${b.id}/${s.id}`, 'server', `${s.name} · ${shortAddr(s.address)}`, st, {
          backendId: b.id, serverId: s.id, value: ss?.sessRate ?? 0, errorRate: st === 'down' ? 1 : 0,
        })
        sNode.lines = [[
          ss?.status === 'DOWN'
            ? { text: 'down', tone: 'danger' }
            : ss && ss.status !== 'UP' && ss.status !== 'NOCHECK'
              ? { text: ss.status.toLowerCase(), tone: 'warn' }
              : { text: fmtRate(ss?.sessRate ?? 0, 'sess/s'), tone: 'muted' },
        ]]
        servers.push(sNode)
      }
      if (f.layers.upstreams) {
        if (!servers.length && (f.statuses.length || f.hideIdle)) continue
      } else if (!statusOk(bStatus) || !idleOk(bNode.value)) continue
      if (collapsed.has(bNode.id)) bNode.collapsedCount = servers.length
      else bNode.children = servers
      bNode.collapsible = servers.length > 0
      backendNodes.push(bNode)
    }
    lbNode.value = backendNodes.reduce((a, b) => a + b.value, 0)
    if (lb && !lb.running && inp.backends.length) lbNode.status = 'down'
    else if (down) lbNode.status = 'degraded'
    const binds = [...new Set(lbHosts.flatMap((h) => hostFrontends(h, inp.frontends)).map((fe) => fe.bind))]
    const enabledFe = inp.frontends.filter((fe) => fe.enabled)
    const bindText = binds[0] ?? enabledFe[0]?.bind
    lbNode.badge = { text: [bindText ? (binds.length > 1 ? `${bindText} +${binds.length - 1}` : bindText) : 'no frontend', `${enabledFe.length} frontend${enabledFe.length === 1 ? '' : 's'}`].join(' · ') }
    const lblVer = [inp.lbEngine.label, lState?.version].filter(Boolean).join(' ')
    lbNode.lines = [
      [{ text: `${lblVer} · ${inp.backends.length} backend${inp.backends.length === 1 ? '' : 's'} · ${serverTotal} server${serverTotal === 1 ? '' : 's'}`, tone: 'muted' }],
      lb && !lb.running
        ? [{ text: 'not running', tone: 'danger' }]
        : [
            { text: `↓ ${fmtRate(lb?.sessRate ?? 0, 'sess/s')}`, tone: 'ok' },
            ...(down ? [{ text: ` · ${down} down`, tone: 'danger' as Tone }] : []),
            ...(drain ? [{ text: ` · ${drain} drain`, tone: 'warn' as Tone }] : []),
          ],
    ]
    // Traffic into the load balancer, in the proxy's unit, for pipe widths.
    const viaHostsValue = lbHosts.reduce((a, h) => a + valueOf(topo?.hosts[h.id], topo?.hostClients[h.id], metric, f.sources, win), 0)
    lbNode.value = viaHostsValue > 0 ? viaHostsValue : metric === 'requests' ? lb?.sessRate ?? 0 : 0
    const lbShown = backendNodes.length > 0 || (!f.statuses.length && !f.hideIdle)
    if (lbShown) {
      if (collapsed.has(lbNode.id)) lbNode.collapsedCount = backendNodes.length
      else lbNode.children = backendNodes
      lbNode.collapsible = backendNodes.length > 0
      relayKids.push(lbNode)
    }
  }

  // ---- direct hosts
  if (f.layers.hosts) {
    const hostNodes: TopoNode[] = []
    for (const h of directHosts) {
      const st = hostStatus(h, inp)
      if (!statusOk(st)) continue
      if (f.access.length && !f.access.includes(h.accessListId ?? 'public')) continue
      const t = topo?.hosts[h.id]
      const v = valueOf(t, topo?.hostClients[h.id], metric, f.sources, win)
      if (!idleOk(v)) continue
      const hs = inp.health[`host:${h.id}`]
      const hn = node(`host:${h.id}`, 'host', hostTitle(h, inp.containers), st, {
        hostId: h.id, value: v, errorRate: st === 'down' ? 1 : t && t.requests ? t.s5xx / t.requests : 0,
        badge: { text: h.domains[0] ?? '(no domain)', tls: !!h.certificateId, danger: st === 'down' },
      })
      const short = shortAddr(h.upstream.host || '—')
      hn.lines = [[
        st === 'down'
          ? { text: `${short} · ${hs?.httpStatus ?? 'down'}`, tone: 'danger' }
          : st === 'disabled'
            ? { text: `${short} · disabled`, tone: 'muted' }
            : { text: `${short} · ${fmtValue(v, metric)}`, tone: 'muted' },
      ]]
      if (f.layers.upstreams) {
        const seen = new Set([`${h.upstream.host}:${h.upstream.port}`])
        const ups: TopoNode[] = []
        for (const loc of h.locations ?? []) {
          if (loc.kind !== 'proxy' || !loc.upstream?.host) continue
          const key = `${loc.upstream.host}:${loc.upstream.port}`
          if (seen.has(key)) continue
          seen.add(key)
          const un = node(`upstream:${h.id}:${loc.id}`, 'upstream', `${shortAddr(loc.upstream.host)}:${loc.upstream.port}`, st === 'disabled' ? 'disabled' : 'unknown', { hostId: h.id })
          un.lines = [[{ text: loc.path, tone: 'muted' }]]
          ups.push(un)
        }
        if (ups.length) {
          hn.collapsible = true
          if (collapsed.has(hn.id)) hn.collapsedCount = ups.length
          else hn.children = ups
        }
      }
      hostNodes.push(hn)
    }
    hostNodes.sort((a, z) => z.value - a.value || a.title.localeCompare(z.title))
    if (!showAllHosts && hostNodes.length > MAX_HOSTS) {
      const rest = hostNodes.splice(MAX_HOSTS - 1)
      const more = node('more', 'more', `+${rest.length} hosts`, 'healthy', { value: rest.reduce((a, n) => a + n.value, 0), labelSide: 'none' })
      relayKids.push(...hostNodes, more)
    } else relayKids.push(...hostNodes)
  }

  // ---- streams
  if (f.layers.streams && !f.access.length) {
    for (const s of inp.streams) {
      const st = streamStatus(s, inp)
      if (!statusOk(st)) continue
      const t = topo?.streams[s.id]
      const v = metric === 'bandwidth' ? t?.bytesPerSec ?? 0 : metric === 'requests' ? t?.rps ?? 0 : 0
      if (!idleOk(v)) continue
      const sn = node(`stream:${s.id}`, 'stream', s.name, st, {
        streamId: s.id, value: v, errorRate: st === 'down' ? 1 : 0,
        badge: { text: `${s.protocol === 'both' ? 'tcp+udp' : s.protocol} :${s.listenPorts}`, danger: st === 'down' },
      })
      sn.lines = [[
        st === 'down'
          ? { text: `${shortAddr(s.forwardHost)} · down`, tone: 'danger' }
          : { text: `${shortAddr(s.forwardHost)} · ${metric === 'bandwidth' ? fmtBytesRate(v) : fmtRate(v, 'sess/s')}`, tone: 'muted' },
      ]]
      relayKids.push(sn)
    }
  }

  if (collapsed.has(relay.id)) relay.collapsedCount = relayKids.length
  else relay.children = relayKids
  relay.collapsible = relayKids.length > 0
  const kidsValue = relayKids.reduce((a, n) => a + n.value, 0)
  relay.value = f.sources.length || metric !== 'requests' ? kidsValue : Math.max(totals?.rps ?? 0, kidsValue)
  clients.value = relay.value

  // ---- shares (pipe widths) and parents
  const nodes: TopoNode[] = []
  const walk = (n: TopoNode, share: number, parent?: TopoNode) => {
    n.share = Math.max(0, Math.min(1, share))
    n.parent = parent
    nodes.push(n)
    const sum = n.children.reduce((a, c) => a + Math.max(0, c.value), 0)
    // The LB subtree counts sessions, not requests: share out by its own total.
    const denom = n.kind === 'lb' || n.kind === 'backend' ? sum : Math.max(sum, n.value)
    for (const c of n.children) walk(c, denom > 0 ? (n.share * Math.max(0, c.value)) / denom : 0, n)
  }
  walk(clients, clients.value > 0 ? 1 : 0)

  const { width, height } = layout(clients, nodes)
  return { root: clients, nodes, counts, width, height, lbHosts }
}

// ---------------------------------------------------------------- layout

const CARD: Record<NodeKind, number> = { clients: 56, proxy: 64, lb: 64, host: 56, upstream: 48, server: 46, stream: 52, backend: 22, more: 24 }
const SLOT: Partial<Record<NodeKind, number>> = { host: 128, upstream: 112, server: 100, stream: 128, more: 104, lb: 150, proxy: 150, clients: 150 }
const LABEL_BELOW = 38
const BADGE = 30
const PAD = 48

export const monoWidth = (s: string, px = 10.5) => s.length * px * 0.6

function textWidth(n: TopoNode): number {
  const titleW = n.title.length * 7
  const lineW = Math.max(0, ...n.lines.map((l) => monoWidth(l.map((s) => s.text).join(''), 11)))
  return Math.max(titleW, lineW)
}

function layout(root: TopoNode, nodes: TopoNode[]): { width: number; height: number } {
  for (const n of nodes) {
    if (n.kind === 'backend') {
      n.cw = monoWidth(n.title) + 18
      n.ch = CARD.backend
    } else if (n.kind === 'more') {
      n.cw = monoWidth(n.title, 11) + 20
      n.ch = CARD.more
    } else {
      n.cw = CARD[n.kind]
      n.ch = CARD[n.kind]
    }
  }
  const measure = (n: TopoNode): number => {
    const own = n.kind === 'backend' ? n.cw + 24 : n.kind === 'more' ? Math.max(SLOT.more ?? 0, n.cw + 16) : Math.max(SLOT[n.kind] ?? 100, n.badge ? monoWidth(n.badge.text) + 34 : 0)
    const kids = n.children.reduce((a, c) => a + measure(c), 0)
    n.sw = Math.max(own, kids)
    return n.sw
  }
  const placeX = (n: TopoNode, left: number) => {
    n.x = left + n.sw / 2
    const kids = n.children.reduce((a, c) => a + c.sw, 0)
    let l = left + (n.sw - kids) / 2
    for (const c of n.children) {
      placeX(c, l)
      l += c.sw
    }
  }
  const placeY = (n: TopoNode, top: number) => {
    n.cardTop = top
    n.y = top + n.ch / 2
    n.bottom = top + n.ch + (n.labelSide === 'below' ? LABEL_BELOW : 0)
    if (!n.children.length) {
      n.busY = n.bottom + 22
      return
    }
    const single = n.children.length === 1
    n.busY = n.bottom + (n.kind === 'clients' ? 30 : n.kind === 'backend' ? 22 : 44)
    const badge = n.children.some((c) => c.badge) ? BADGE : 0
    const childTop = n.busY + (n.kind === 'clients' || (single && n.kind !== 'proxy') ? 26 : 22) + badge
    for (const c of n.children) placeY(c, c.kind === 'more' ? childTop + (CARD.host - CARD.more) / 2 : c.kind === 'backend' ? n.busY + 22 : childTop)
  }
  measure(root)
  placeX(root, 0)
  placeY(root, 0)

  let minX = Infinity
  let maxX = -Infinity
  let maxY = 0
  for (const n of nodes) {
    let l = n.x - n.cw / 2
    let r = n.x + n.cw / 2
    const tw = textWidth(n)
    if (n.labelSide === 'right') r += 14 + tw
    if (n.labelSide === 'left') l -= 14 + tw
    if (n.labelSide === 'below') {
      l = Math.min(l, n.x - tw / 2)
      r = Math.max(r, n.x + tw / 2)
    }
    if (n.badge) {
      const bw = monoWidth(n.badge.text) + 30
      l = Math.min(l, n.x - bw / 2)
      r = Math.max(r, n.x + bw / 2)
    }
    minX = Math.min(minX, l)
    maxX = Math.max(maxX, r)
    maxY = Math.max(maxY, n.bottom + (n.collapsedCount ? 30 : 0))
  }
  const dx = PAD - minX
  for (const n of nodes) {
    n.x += dx
    n.y += PAD
    n.cardTop += PAD
    n.bottom += PAD
    n.busY += PAD
  }
  return { width: maxX - minX + PAD * 2, height: maxY + PAD * 2 }
}

// ---------------------------------------------------------------- selection

/** Ids of a node, its ancestors and its descendants. */
export function lineage(n: TopoNode | undefined): Set<string> {
  const out = new Set<string>()
  if (!n) return out
  for (let p: TopoNode | undefined = n; p; p = p.parent) out.add(p.id)
  const down = (x: TopoNode) => x.children.forEach((c) => (out.add(c.id), down(c)))
  down(n)
  return out
}
