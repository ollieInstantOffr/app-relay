// Owner: slice observe — Topology page (design 31).
import { useCallback, useEffect, useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { Button, EmptyState } from '../../components/ui'
import { useContainers, useEngines, useEntities, useHealth, useLBEngine, useLBStats, useProxyEngine, useSettings } from '../../lib/queries'
import { FilterPanel, type NavMode, type TopoView } from './FilterPanel'
import { NodeTip } from './NodeTip'
import { SankeyView } from './SankeyView'
import { TopologyCanvas } from './TopologyCanvas'
import { LIVE_MS, useTopology, type FlowRange } from './api'
import { DEFAULT_FILTERS, buildGraph, type Metric, type TopoFilters, type TopoInput } from './graph'
import './topology.css'

interface Prefs {
  view: TopoView
  metric: Metric
  range: FlowRange
  live: boolean
  animate: boolean
  spotlight: boolean
  nav: NavMode
  panel: boolean
  filters: TopoFilters
}

const PREFS_KEY = 'relay.topology'
const DEFAULT_PREFS: Prefs = { view: 'topology', metric: 'requests', range: '15m', live: true, animate: true, spotlight: false, nav: 'select', panel: true, filters: DEFAULT_FILTERS }

function loadPrefs(): Prefs {
  try {
    const raw = localStorage.getItem(PREFS_KEY)
    if (!raw) return DEFAULT_PREFS
    const p = JSON.parse(raw) as Partial<Prefs>
    return { ...DEFAULT_PREFS, ...p, filters: { ...DEFAULT_FILTERS, ...p.filters, layers: { ...DEFAULT_FILTERS.layers, ...p.filters?.layers } } }
  } catch {
    return DEFAULT_PREFS
  }
}

export default function TopologyPage() {
  const navigate = useNavigate()
  const [prefs, setPrefs] = useState<Prefs>(loadPrefs)
  const set = useCallback(<K extends keyof Prefs>(k: K, v: Prefs[K]) => setPrefs((p) => ({ ...p, [k]: v })), [])
  useEffect(() => {
    try {
      localStorage.setItem(PREFS_KEY, JSON.stringify(prefs))
    } catch {
      /* storage unavailable */
    }
  }, [prefs])

  const [collapsed, setCollapsed] = useState<Set<string>>(() => new Set())
  const [showAllHosts, setShowAllHosts] = useState(false)
  const [selectedId, setSelectedId] = useState<string | null>(null)

  const refetch = prefs.live ? LIVE_MS : false
  const hosts = useEntities('hosts', { refetchInterval: prefs.live ? 30_000 : false })
  const backends = useEntities('backends', { refetchInterval: prefs.live ? 30_000 : false })
  const frontends = useEntities('frontends')
  const streams = useEntities('streams')
  const accessLists = useEntities('access-lists')
  const containers = useContainers()
  const health = useHealth()
  const lb = useLBStats(refetch === false ? 60_000 : refetch)
  const topo = useTopology(prefs.range, prefs.live)
  const general = useSettings('general').data
  const proxy = useProxyEngine()
  const lbEngine = useLBEngine()
  useEngines()

  const input: TopoInput = useMemo(() => ({
    hosts: hosts.data ?? [],
    backends: backends.data ?? [],
    frontends: frontends.data ?? [],
    streams: streams.data ?? [],
    accessLists: accessLists.data ?? [],
    containers: containers.data ?? [],
    health: health.data ?? {},
    lb: lb.data,
    topo: topo.data,
    proxy: { label: proxy.label, state: proxy.state },
    lbEngine: { label: lbEngine.label, state: lbEngine.state },
    ports: { http: general?.httpPort, https: general?.httpsPort },
    metric: prefs.metric,
  }), [hosts.data, backends.data, frontends.data, streams.data, accessLists.data, containers.data, health.data, lb.data, topo.data, proxy.label, proxy.state, lbEngine.label, lbEngine.state, general?.httpPort, general?.httpsPort, prefs.metric])

  const graph = useMemo(() => buildGraph(input, prefs.filters, collapsed, showAllHosts), [input, prefs.filters, collapsed, showAllHosts])

  // A selection whose node disappeared (filters, collapse, deletion) shows nothing.
  const selected = selectedId && graph.nodes.some((n) => n.id === selectedId) ? selectedId : null

  const toggleCollapse = useCallback((id: string) => {
    setCollapsed((c) => {
      const n = new Set(c)
      if (n.has(id)) n.delete(id)
      else n.add(id)
      return n
    })
  }, [])

  const loading = hosts.isLoading || backends.isLoading
  const empty = !loading && !input.hosts.length && !input.backends.length && !input.streams.length
  const openPanel = !prefs.panel && (
    <button type="button" className="topo-ctl topo-tb topo-panel-open" onClick={() => set('panel', true)} title="Show filters">⇥ Filters</button>
  )

  return (
    <div className="topo">
      {prefs.panel && (
        <FilterPanel
          view={prefs.view}
          onView={(v) => set('view', v)}
          filters={prefs.filters}
          onFilters={(f) => set('filters', f)}
          counts={graph.counts}
          liveTraffic={prefs.animate}
          onLiveTraffic={(v) => set('animate', v)}
          spotlight={prefs.spotlight}
          onSpotlight={(v) => set('spotlight', v)}
          nav={prefs.nav}
          onNav={(v) => set('nav', v)}
          onCollapse={() => set('panel', false)}
        />
      )}
      {prefs.view === 'sankey' ? (
        <SankeyView
          input={input}
          filters={prefs.filters}
          metric={prefs.metric}
          onMetric={(m) => set('metric', m)}
          range={prefs.range}
          onRange={(r) => set('range', r)}
          live={prefs.live}
          onLive={(v) => set('live', v)}
          animate={prefs.animate}
          leftControls={openPanel}
        />
      ) : (
        <TopologyCanvas
          graph={graph}
          metric={prefs.metric}
          onMetric={(m) => set('metric', m)}
          range={prefs.range}
          onRange={(r) => set('range', r)}
          live={prefs.live}
          onLive={(v) => set('live', v)}
          animate={prefs.animate && prefs.live}
          spotlight={prefs.spotlight}
          nav={prefs.nav}
          onToggleCollapse={toggleCollapse}
          onExpandMore={() => setShowAllHosts(true)}
          selectedId={selected}
          onSelect={setSelectedId}
          renderTip={(n, close) => <NodeTip node={n} input={input} range={prefs.range} onClose={close} />}
          leftControls={openPanel}
          overlay={
            loading ? (
              <div className="topo-empty"><span className="spinner lg" /></div>
            ) : empty ? (
              <div className="topo-empty">
                <EmptyState
                  icon="topology"
                  title="Nothing to draw yet"
                  description="Add a proxy host or a load balancer backend and its traffic shows up here."
                  actions={
                    <>
                      <Button variant="primary" icon="plus" onClick={() => navigate('/hosts?new=1')}>New proxy host</Button>
                      <Button onClick={() => navigate('/load-balancer/backends?new=1')}>New backend</Button>
                    </>
                  }
                />
              </div>
            ) : topo.isError && !topo.data ? (
              <div className="topo-ctl topo-stale">Traffic data unavailable — showing structure only</div>
            ) : null
          }
        />
      )}
    </div>
  )
}
