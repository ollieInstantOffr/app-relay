// Owner: slice observe — Topology canvas (design 31): pipes, nodes, zoom & pan.
import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { Menu, cx } from '../../components/ui'
import { FLOW_RANGES, LIVE_MS, type FlowRange } from './api'
import { METRICS, lineage, type Metric, type TopoGraph, type TopoNode } from './graph'
import type { NavMode } from './FilterPanel'

interface View { x: number; y: number; k: number }
const MIN_K = 0.25
const MAX_K = 2

const pipeWidth = (share: number) => (share <= 0 ? 0 : 5 + 15 * Math.sqrt(share))
const pipeDuration = (share: number) => `${(1.6 - 1.15 * Math.sqrt(Math.min(1, share))).toFixed(2)}s`

export function Glyph({ kind }: { kind: TopoNode['kind'] }) {
  const common = { fill: 'none', stroke: 'currentColor', strokeWidth: 1.75, strokeLinecap: 'round' as const, strokeLinejoin: 'round' as const }
  switch (kind) {
    case 'clients':
      return (
        <svg width="22" height="22" viewBox="0 0 24 24" {...common}>
          <circle cx="12" cy="12" r="9" />
          <path d="M3 12h18" />
          <path d="M12 3c2.5 2.6 3.8 5.6 3.8 9s-1.3 6.4-3.8 9c-2.5-2.6-3.8-5.6-3.8-9S9.5 5.6 12 3z" />
        </svg>
      )
    case 'proxy':
      return (
        <svg width="26" height="26" viewBox="0 0 32 32">
          <circle cx="9.5" cy="16" r="3" fill="currentColor" />
          <path d="M12.5 16h6.5" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" />
          <circle cx="22.5" cy="16" r="3" fill="none" stroke="currentColor" strokeWidth="2.5" />
        </svg>
      )
    case 'lb':
      return (
        <svg width="26" height="26" viewBox="0 0 24 24" {...common}>
          <circle cx="6" cy="12" r="2.4" />
          <circle cx="18" cy="5.5" r="2.4" />
          <circle cx="18" cy="18.5" r="2.4" />
          <path d="M8.2 11l7.6-4.3M8.2 13l7.6 4.3" />
        </svg>
      )
    case 'server':
    case 'host':
      return (
        <svg width={kind === 'server' ? 18 : 20} height={kind === 'server' ? 18 : 20} viewBox="0 0 24 24" {...common}>
          <rect x="3.5" y="7" width="17" height="12.5" rx="2" />
          <path d="M9 7V5.5A1.5 1.5 0 0 1 10.5 4h3A1.5 1.5 0 0 1 15 5.5V7" />
          <path d="M3.5 12.5h17" />
        </svg>
      )
    case 'upstream':
      return (
        <svg width="18" height="18" viewBox="0 0 24 24" {...common}>
          <rect x="4" y="4" width="16" height="7" rx="1.5" />
          <rect x="4" y="13" width="16" height="7" rx="1.5" />
          <path d="M8 7.5h.01M8 16.5h.01" />
        </svg>
      )
    case 'stream':
      return (
        <svg width="20" height="20" viewBox="0 0 24 24" {...common}>
          <path d="M4 8h13M14 5l3 3-3 3M20 16H7M10 13l-3 3 3 3" />
        </svg>
      )
    default:
      return null
  }
}

function Lock() {
  return (
    <svg width="9" height="9" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" aria-label="TLS">
      <rect x="5" y="11" width="14" height="10" rx="2" />
      <path d="M8 11V8a4 4 0 0 1 8 0v3" />
    </svg>
  )
}

export function TopologyCanvas({
  graph, metric, onMetric, range, onRange, live, onLive, animate, spotlight, nav, onToggleCollapse, onExpandMore,
  selectedId, onSelect, renderTip, leftControls, overlay,
}: {
  graph: TopoGraph
  metric: Metric
  onMetric: (m: Metric) => void
  range: FlowRange
  onRange: (r: FlowRange) => void
  live: boolean
  onLive: (v: boolean) => void
  animate: boolean
  spotlight: boolean
  nav: NavMode
  onToggleCollapse: (id: string) => void
  onExpandMore: () => void
  selectedId: string | null
  onSelect: (id: string | null) => void
  renderTip: (n: TopoNode, close: () => void) => ReactNode
  leftControls?: ReactNode
  overlay?: ReactNode
}) {
  const ref = useRef<HTMLDivElement>(null)
  const tipRef = useRef<HTMLDivElement>(null)
  const [view, setView] = useState<View>({ x: 0, y: 0, k: 1 })
  const [size, setSize] = useState({ w: 0, h: 0 })
  const [dragging, setDragging] = useState(false)
  const drag = useRef<{ px: number; py: number; vx: number; vy: number; moved: boolean } | null>(null)
  const space = useRef(false)
  const fitted = useRef(false)
  const [tipBox, setTipBox] = useState<{ left: number; top: number; arrow: number; above: boolean } | null>(null)

  const selected = useMemo(() => graph.nodes.find((n) => n.id === selectedId), [graph, selectedId])
  const lit = useMemo(() => (spotlight && selected ? lineage(selected) : null), [spotlight, selected])

  // ---- size & fit
  useLayoutEffect(() => {
    const el = ref.current
    if (!el) return
    const ro = new ResizeObserver(() => setSize({ w: el.clientWidth, h: el.clientHeight }))
    ro.observe(el)
    setSize({ w: el.clientWidth, h: el.clientHeight })
    return () => ro.disconnect()
  }, [])

  const fit = useCallback(() => {
    if (!size.w || !size.h) return
    const k = Math.max(MIN_K, Math.min(1, (size.w - 40) / graph.width, (size.h - 70) / graph.height))
    setView({ k, x: Math.max(20, (size.w - graph.width * k) / 2), y: 56 })
  }, [size, graph.width, graph.height])

  const reset = useCallback(() => {
    setView({ k: 1, x: graph.width < size.w ? (size.w - graph.width) / 2 : 20, y: 56 })
  }, [graph.width, size.w])

  useEffect(() => {
    if (!fitted.current && size.w > 0 && graph.nodes.length > 1) {
      fitted.current = true
      fit()
    }
  }, [fit, size.w, graph.nodes.length])

  const zoomAt = useCallback((factor: number, cx: number, cy: number) => {
    setView((v) => {
      const k = Math.max(MIN_K, Math.min(MAX_K, v.k * factor))
      const f = k / v.k
      return { k, x: cx - (cx - v.x) * f, y: cy - (cy - v.y) * f }
    })
  }, [])

  // ---- wheel (non-passive so the page doesn't scroll)
  useEffect(() => {
    const el = ref.current
    if (!el) return
    const onWheel = (e: WheelEvent) => {
      if ((e.target as HTMLElement).closest('.topo-tip')) return
      e.preventDefault()
      const r = el.getBoundingClientRect()
      if (e.ctrlKey || e.metaKey || nav === 'pan') zoomAt(Math.exp(-e.deltaY * (e.ctrlKey ? 0.01 : 0.0015)), e.clientX - r.left, e.clientY - r.top)
      else setView((v) => ({ ...v, x: v.x - e.deltaX, y: v.y - e.deltaY }))
    }
    el.addEventListener('wheel', onWheel, { passive: false })
    return () => el.removeEventListener('wheel', onWheel)
  }, [nav, zoomAt])

  // ---- keyboard: space to pan, Esc to deselect
  useEffect(() => {
    const down = (e: KeyboardEvent) => {
      const t = e.target as HTMLElement
      if (t && (t.isContentEditable || ['INPUT', 'TEXTAREA', 'SELECT'].includes(t.tagName))) return
      if (e.key === ' ') space.current = true
      if (e.key === 'Escape') onSelect(null)
    }
    const up = (e: KeyboardEvent) => {
      if (e.key === ' ') space.current = false
    }
    window.addEventListener('keydown', down)
    window.addEventListener('keyup', up)
    return () => {
      window.removeEventListener('keydown', down)
      window.removeEventListener('keyup', up)
    }
  }, [onSelect])

  const onPointerDown = (e: React.PointerEvent) => {
    const t = e.target as HTMLElement
    if (t.closest('.topo-ctl, .topo-tip, .topo-empty > *')) return
    const onNode = !!t.closest('.topo-node, .topo-collapse')
    const panGesture = nav === 'pan' || space.current || e.button === 1
    if (onNode && !panGesture) return
    if (e.button !== 0 && e.button !== 1) return
    drag.current = { px: e.clientX, py: e.clientY, vx: view.x, vy: view.y, moved: false }
    ;(e.currentTarget as HTMLElement).setPointerCapture(e.pointerId)
  }
  const onPointerMove = (e: React.PointerEvent) => {
    const d = drag.current
    if (!d) return
    const dx = e.clientX - d.px
    const dy = e.clientY - d.py
    if (!d.moved && Math.hypot(dx, dy) < 4) return
    if (!d.moved) setDragging(true)
    d.moved = true
    setView((v) => ({ ...v, x: d.vx + dx, y: d.vy + dy }))
  }
  const onPointerUp = (e: React.PointerEvent) => {
    const d = drag.current
    drag.current = null
    setDragging(false)
    if (d && !d.moved && !(e.target as HTMLElement).closest('.topo-node, .topo-collapse')) onSelect(null)
  }

  // ---- tooltip placement (screen space, below the node or above when there's no room)
  useLayoutEffect(() => {
    if (!selected || !tipRef.current) {
      setTipBox(null)
      return
    }
    const tw = tipRef.current.offsetWidth
    const th = tipRef.current.offsetHeight
    const sx = selected.x * view.k + view.x
    const below = (selected.cardTop + selected.ch) * view.k + view.y + 12
    const above = selected.cardTop * view.k + view.y - 12 - th
    const useAbove = below + th > size.h - 8 && above > 8
    const left = Math.max(8, Math.min(sx - tw / 2, size.w - tw - 8))
    setTipBox({ left, top: useAbove ? above : below, arrow: Math.max(14, Math.min(tw - 14, sx - left)), above: useAbove })
  }, [selected, view, size])

  const liveLabel = `${Math.round(LIVE_MS / 1000)} s`
  const tipClose = useCallback(() => onSelect(null), [onSelect])

  // ---- pipes
  const pipes: ReactNode[] = []
  const dots: ReactNode[] = []
  for (const n of graph.nodes) {
    if (!n.children.length) continue
    const trunkDim = lit && !lit.has(n.id) ? ' dim' : ''
    const tw = pipeWidth(n.share)
    const trunk = n.kind === 'clients' ? null : `M${n.x} ${n.bottom} V${n.busY}`
    if (trunk) {
      if (tw > 0) {
        pipes.push(<path key={`t-${n.id}`} className={`topo-pipe base${trunkDim}`} strokeWidth={tw} d={trunk} />)
        dots.push(<path key={`td-${n.id}`} className={cx('topo-pipe dots', n.share < 0.3 && 'thin', trunkDim)} style={{ animationDuration: pipeDuration(n.share) }} d={trunk} />)
      } else pipes.push(<path key={`t-${n.id}`} className={`topo-pipe idle${trunkDim}`} d={trunk} />)
    }
    const startY = n.kind === 'clients' ? n.bottom : n.busY
    for (const c of n.children) {
      const d = Math.abs(c.x - n.x) < 0.5 ? `M${n.x} ${startY} V${c.cardTop}` : `M${n.x} ${startY} H${c.x} V${c.cardTop}`
      const dim = lit && !lit.has(c.id) ? ' dim' : ''
      const err = c.errorRate >= 0.05 || c.status === 'down'
      const w = pipeWidth(c.share)
      if (w > 0) {
        pipes.push(<path key={`p-${c.id}`} className={cx('topo-pipe base', err && 'err', dim)} strokeWidth={w} d={d} />)
        dots.push(<path key={`d-${c.id}`} className={cx('topo-pipe dots', c.share < 0.3 && 'thin', err && 'err', dim)} style={{ animationDuration: pipeDuration(c.share) }} d={d} />)
      } else {
        pipes.push(<path key={`p-${c.id}`} className={cx('topo-pipe idle', dim)} style={err ? { stroke: 'rgba(220,38,38,.55)' } : undefined} d={d} />)
      }
    }
  }

  const statusDot = (n: TopoNode) => (n.kind === 'host' || n.kind === 'server' || n.kind === 'stream' || n.kind === 'upstream') && <span className={cx('topo-sdot', n.status)} />

  return (
    <div
      ref={ref}
      className={cx('topo-canvas', (nav === 'pan' || dragging) && 'pan', dragging && 'dragging')}
      onPointerDown={onPointerDown}
      onPointerMove={onPointerMove}
      onPointerUp={onPointerUp}
      onPointerCancel={onPointerUp}
      tabIndex={-1}
    >
      <div className={cx('topo-layer', animate && 'topo-live')} style={{ width: graph.width, height: graph.height, transform: `translate(${view.x}px, ${view.y}px) scale(${view.k})` }}>
        <svg width={graph.width} height={graph.height} aria-hidden>
          {pipes}
          {dots}
        </svg>
        {graph.nodes.map((n) => {
          const dim = lit && !lit.has(n.id)
          const sel = n.id === selectedId
          const pick = (e: React.MouseEvent) => {
            e.stopPropagation()
            if (n.kind === 'more') onExpandMore()
            else onSelect(sel ? null : n.id)
          }
          return (
            <div
              key={n.id}
              className={cx('topo-node', `k-${n.kind}`, `s-${n.status}`, dim && 'dim', sel && 'sel')}
              style={{ left: n.x - n.cw / 2, top: n.cardTop, width: n.cw, height: n.ch }}
            >
              {n.badge && (
                <span className={cx('topo-badge', n.badge.danger && 'danger')}>
                  {n.badge.tls && <Lock />}
                  {n.badge.text}
                </span>
              )}
              {n.kind === 'backend' ? (
                <button type="button" className="topo-pill" onClick={pick} title={n.title}>{n.title}</button>
              ) : n.kind === 'more' ? (
                <button type="button" className="topo-more" onClick={pick}>{n.title}</button>
              ) : (
                <button type="button" className="topo-card" onClick={pick} aria-label={n.title}>
                  <Glyph kind={n.kind} />
                </button>
              )}
              {statusDot(n)}
              {n.labelSide !== 'none' && (
                <div className={cx('topo-label', n.labelSide)}>
                  <span className="t">{n.title}</span>
                  {n.lines.map((l, i) => (
                    <span key={i} className="l">
                      {l.map((s, j) => <span key={j} className={s.tone}>{s.text}</span>)}
                    </span>
                  ))}
                </div>
              )}
            </div>
          )
        })}
        {graph.nodes.map((n) => {
          if (!n.collapsible) return null
          if (n.collapsedCount > 0) {
            const y = n.bottom + 8
            return (
              <div key={`c-${n.id}`}>
                <button type="button" className="topo-collapse plus" style={{ left: n.x - 11, top: y }} onClick={() => onToggleCollapse(n.id)} aria-label={`Expand ${n.title}`} title="Expand" />
                <span className="topo-collapse-count" style={{ left: n.x + 16, top: y + 4 }}>{n.collapsedCount} hidden</span>
              </div>
            )
          }
          // Backends collapse from their pill (click opens details), so no button on the short bus.
          if (!n.children.length || n.kind === 'clients' || n.kind === 'backend') return null
          const y = (n.bottom + n.busY) / 2 - 11
          return <button key={`c-${n.id}`} type="button" className="topo-collapse" style={{ left: n.x - 11, top: y }} onClick={() => onToggleCollapse(n.id)} aria-label={`Collapse ${n.title}`} title="Collapse branch" />
        })}
      </div>

      {leftControls}
      {overlay}

      <div className="topo-ctl topo-toolbar">
        <button type="button" className={cx('topo-tb', live ? 'live' : 'paused')} onClick={() => onLive(!live)} title={live ? 'Pause updates' : 'Resume live updates'}>
          <span className="live-dot" />
          {live ? `Live · ${liveLabel}` : 'Paused'}
        </button>
        <Menu
          trigger={<button type="button" className="topo-tb">{METRICS.find((m) => m.value === metric)?.label}<span className="caret">▼</span></button>}
          items={[{ header: 'Pipe width shows' }, ...METRICS.map((m) => ({ label: m.label, icon: m.value === metric ? ('check' as const) : undefined, onSelect: () => onMetric(m.value) }))]}
        />
        <Menu
          trigger={<button type="button" className="topo-tb">{FLOW_RANGES.find((r) => r.value === range)?.label}<span className="caret">▼</span></button>}
          items={FLOW_RANGES.map((r) => ({ label: r.label, icon: r.value === range ? ('check' as const) : undefined, onSelect: () => onRange(r.value) }))}
        />
      </div>

      <div className="topo-ctl topo-zoom">
        <button type="button" onClick={reset} title="Reset zoom (100%)" aria-label="Reset zoom">↺</button>
        <button type="button" onClick={fit} title="Fit to screen" aria-label="Fit to screen">⌖</button>
        <div className="pct">{Math.round(view.k * 100)}%</div>
        <button type="button" onClick={() => zoomAt(1.2, size.w / 2, size.h / 2)} title="Zoom in" aria-label="Zoom in">+</button>
        <button type="button" onClick={() => zoomAt(1 / 1.2, size.w / 2, size.h / 2)} title="Zoom out" aria-label="Zoom out">−</button>
      </div>

      <div className="topo-ctl topo-legend">
        <span>
          <svg width="26" height="10"><path d="M3 5H23" stroke="#e0dfdb" strokeWidth="9" strokeLinecap="round" /><path d="M3 5H23" stroke="rgba(20,20,20,.6)" strokeWidth="2.5" strokeDasharray="2.5 5" strokeLinecap="round" /></svg>
          width &amp; dots = {metric === 'bandwidth' ? 'bytes/s' : metric === 'errors' ? '5xx/s' : 'req/s'}
        </span>
        <span><svg width="22" height="10"><path d="M3 5H19" stroke="#f6dcdc" strokeWidth="9" strokeLinecap="round" /></svg>errors</span>
        <span><svg width="22" height="10"><path d="M1 5H21" stroke="rgba(0,0,0,.35)" strokeWidth="1.5" strokeDasharray="4 3" /></svg>no traffic</span>
        <span><Lock />TLS</span>
      </div>

      {selected && selected.kind !== 'more' && (
        <div
          ref={tipRef}
          className={cx('topo-tip', tipBox?.above ? 'above' : 'below')}
          style={{ left: tipBox?.left ?? -9999, top: tipBox?.top ?? -9999, ['--arrow' as string]: `${tipBox?.arrow ?? 136}px` }}
          onPointerDown={(e) => e.stopPropagation()}
        >
          {renderTip(selected, tipClose)}
        </div>
      )}
    </div>
  )
}
