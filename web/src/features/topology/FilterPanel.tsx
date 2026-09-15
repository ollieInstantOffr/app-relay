// Owner: slice observe — Topology filter panel (design 31, left column).
import { useState, type ReactNode } from 'react'
import { Checkbox, Icon, Segmented, Toggle, cx } from '../../components/ui'
import { DEFAULT_FILTERS, filtersActive, type LayerKey, type SourceKey, type TopoCounts, type TopoFilters } from './graph'

export type TopoView = 'topology' | 'sankey'
export type NavMode = 'select' | 'pan'

const SOURCE_COLORS: Record<SourceKey, string> = { lan: '#141414', vpn: '#6b7280', internet: '#b8b6b0', blocked: '#dc2626' }

function Section({ title, children }: { title: string; children: ReactNode }) {
  const [open, setOpen] = useState(true)
  return (
    <div className="topo-sec">
      <button type="button" className={cx('topo-sec-head', !open && 'closed')} onClick={() => setOpen(!open)} aria-expanded={open}>
        {title}
        <Icon name="chevron" size={14} />
      </button>
      {open && <div className="topo-sec-body">{children}</div>}
    </div>
  )
}

function toggleIn<T>(list: T[], v: T, on: boolean): T[] {
  return on ? [...list, v] : list.filter((x) => x !== v)
}

export function FilterPanel({
  view, onView, filters, onFilters, counts, liveTraffic, onLiveTraffic, spotlight, onSpotlight, nav, onNav, onCollapse,
}: {
  view: TopoView
  onView: (v: TopoView) => void
  filters: TopoFilters
  onFilters: (f: TopoFilters) => void
  counts: TopoCounts
  liveTraffic: boolean
  onLiveTraffic: (v: boolean) => void
  spotlight: boolean
  onSpotlight: (v: boolean) => void
  nav: NavMode
  onNav: (v: NavMode) => void
  onCollapse: () => void
}) {
  const set = (patch: Partial<TopoFilters>) => onFilters({ ...filters, ...patch })
  const layer = (k: LayerKey, label: string) => (
    <Checkbox
      checked={filters.layers[k]}
      onChange={(v) => set({ layers: { ...filters.layers, [k]: v } })}
      label={`${label} (${counts.layers[k]})`}
    />
  )
  const sources: { k: SourceKey; label: string }[] = [
    { k: 'lan', label: 'LAN' },
    { k: 'vpn', label: 'VPN' },
    { k: 'internet', label: 'Internet' },
    { k: 'blocked', label: 'Blocked' },
  ]
  const fmtShare = (v: number) => (v > 0 && v < 1 ? '<1%' : `${Math.round(v)}%`)
  const anySource = sources.some((s) => counts.sources[s.k] > 0)

  return (
    <aside className="topo-panel" aria-label="Topology filters">
      <div className="topo-panel-head">
        <Segmented<TopoView> value={view} onChange={onView} options={[{ value: 'topology', label: 'Topology' }, { value: 'sankey', label: 'Sankey' }]} />
        <button type="button" className="topo-icon-btn" onClick={onCollapse} title="Hide filters" aria-label="Hide filters">⇤</button>
      </div>
      <div className="topo-panel-body">
        <div className="topo-toggles">
          <label className="topo-toggle">
            Show live traffic
            <Toggle checked={liveTraffic} onChange={onLiveTraffic} label="Show live traffic" />
          </label>
          {view === 'topology' && (
            <label className="topo-toggle">
              Selection spotlight
              <Toggle checked={spotlight} onChange={onSpotlight} label="Selection spotlight" />
            </label>
          )}
          <label className="topo-toggle">
            Hide idle nodes
            <Toggle checked={filters.hideIdle} onChange={(v) => set({ hideIdle: v })} label="Hide idle nodes" />
          </label>
        </div>

        <Section title="Status">
          {(['healthy', 'degraded', 'down'] as const).map((s) => (
            <Checkbox
              key={s}
              checked={filters.statuses.includes(s)}
              onChange={(v) => set({ statuses: toggleIn(filters.statuses, s, v) })}
              label={`${s[0].toUpperCase()}${s.slice(1)} (${counts.status[s]})`}
            />
          ))}
        </Section>

        <Section title="Layers">
          {layer('hosts', 'Reverse proxy hosts')}
          {layer('lb', 'Load balancer backends')}
          {layer('upstreams', 'Containers & upstreams')}
          {layer('streams', 'Streams TCP/UDP')}
        </Section>

        <Section title="Access">
          {counts.access.map((a) => (
            <span key={a.id} className={cx(a.count === 0 && 'zero')} style={{ display: 'contents' }}>
              <Checkbox checked={filters.access.includes(a.id)} onChange={(v) => set({ access: toggleIn(filters.access, a.id, v) })} label={`${a.name} (${a.count})`} />
            </span>
          ))}
        </Section>

        <Section title="Traffic source">
          {sources.map((s) => (
            <Checkbox
              key={s.k}
              checked={filters.sources.includes(s.k)}
              onChange={(v) => set({ sources: toggleIn(filters.sources, s.k, v) })}
              label={anySource ? `${s.label} · ${fmtShare(counts.sources[s.k])}` : s.label}
            />
          ))}
          {anySource && (
            <div className="topo-src-bar" aria-hidden>
              {sources.map((s) => <span key={s.k} style={{ width: `${counts.sources[s.k]}%`, background: SOURCE_COLORS[s.k] }} />)}
            </div>
          )}
        </Section>
      </div>
      {view === 'topology' && (
        <div className="topo-panel-foot">
          Navigation
          <Segmented<NavMode>
            value={nav}
            onChange={onNav}
            options={[
              { value: 'select', label: <span title="Select (V)">↖</span> },
              { value: 'pan', label: <span title="Pan (H)">✋</span> },
            ]}
          />
        </div>
      )}
      <button type="button" className="topo-clear" disabled={!filtersActive(filters)} onClick={() => onFilters(DEFAULT_FILTERS)}>
        Clear filters
      </button>
    </aside>
  )
}
