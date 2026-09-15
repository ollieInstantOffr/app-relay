// Owner: slice hosts. Hosts grid (design 02).
import { useState, type RefObject } from 'react'
import { Bars, Checkbox, IconButton, Menu, Skeleton, Tooltip, cx } from '../../components/ui'
import { compact } from '../../lib/format'
import type { HealthState, ProxyHost } from '../../lib/types'
import type { HostActions } from './HostActions'
import { featureChips, hostHealth, sinceShort, tlsChip, upstreamText, type Chip, type ViewCtx } from './lib'

export function HealthDot({ state, inline }: { state: HealthState; inline?: boolean }) {
  const cls = state === 'healthy' ? 'ok' : state === 'degraded' ? 'warn' : state === 'down' ? 'danger' : state === 'disabled' ? 'off' : ''
  return <span className={cx('hosts-dot', cls, inline && 'inline')} title={state} aria-label={state} />
}

export function ChipView({ chip }: { chip: Chip }) {
  const el = <span className={cx('badge', chip.tone)}>{chip.label}</span>
  return chip.tooltip ? <Tooltip content={chip.tooltip}>{el}</Tooltip> : el
}

function padSeries(series: number[] | undefined): number[] {
  const s = (series ?? []).slice(-24)
  return s.length >= 24 ? s : [...Array<number>(24 - s.length).fill(0), ...s]
}

export function HostsGrid({ gridRef, hosts, ctx, selected, focusedId, canWrite, actions, onToggleSelect, onFocus, onNew, loading }: {
  gridRef?: RefObject<HTMLDivElement | null>
  hosts: ProxyHost[]
  ctx: ViewCtx
  selected: Set<string>
  focusedId: string | null
  canWrite: boolean
  actions: HostActions
  onToggleSelect: (id: string) => void
  onFocus: (id: string) => void
  onNew: () => void
  loading: boolean
}) {
  if (loading) {
    return (
      <div className="hosts-grid" ref={gridRef}>
        {Array.from({ length: 6 }, (_, i) => (
          <Skeleton key={i} height={156} />
        ))}
      </div>
    )
  }
  return (
    <div className={cx('hosts-grid', selected.size > 0 && 'selecting')} ref={gridRef}>
      {hosts.map((h) => (
        <HostCard
          key={h.id}
          host={h}
          ctx={ctx}
          selected={selected.has(h.id)}
          focused={focusedId === h.id}
          canWrite={canWrite}
          actions={actions}
          onToggleSelect={onToggleSelect}
          onFocus={onFocus}
        />
      ))}
      {canWrite && (
        <button type="button" className="hosts-add-card" onClick={onNew}>
          <span className="plus">+</span>
          Add proxy host
        </button>
      )}
    </div>
  )
}

function HostCard({ host: h, ctx, selected, focused, canWrite, actions, onToggleSelect, onFocus }: {
  host: ProxyHost
  ctx: ViewCtx
  selected: boolean
  focused: boolean
  canWrite: boolean
  actions: HostActions
  onToggleSelect: (id: string) => void
  onFocus: (id: string) => void
}) {
  const [retrying, setRetrying] = useState(false)
  const { state, status } = hostHealth(h, ctx.health)
  const m = ctx.metrics?.[h.id]
  const features = featureChips(h, ctx).slice(0, h.system ? 2 : 3)
  const failing = h.enabled && (state === 'down' || state === 'degraded') && status

  let mid: React.ReactNode
  if (!h.enabled) {
    mid = <span className="hosts-card-note">Disabled · no traffic</span>
  } else if (failing) {
    const text = [status.httpStatus, status.detail || (state === 'down' ? 'upstream down' : 'upstream degraded'), sinceShort(status.changedAt)]
      .filter(Boolean)
      .join(' · ')
    mid = (
      <div className={cx('hosts-strip', state === 'down' ? 'danger' : 'warn')}>
        <span className={cx('dot', state === 'down' ? 'danger' : 'warn')} />
        <span className="hosts-strip-text" title={text}>{text}</span>
        {canWrite && (
          <button
            type="button"
            disabled={retrying}
            onClick={async (e) => {
              e.stopPropagation()
              setRetrying(true)
              try {
                await actions.probe(h)
              } finally {
                setRetrying(false)
              }
            }}
          >
            {retrying ? 'Retrying…' : 'Retry'}
          </button>
        )}
      </div>
    )
  } else if (m) {
    mid = <Bars values={padSeries(m.series)} height={28} highlightLast />
  } else {
    mid = <span className="hosts-card-note">{ctx.metricsFailed ? 'No traffic data' : ctx.metrics ? 'No requests in the last 24 h' : ''}</span>
  }

  return (
    <div
      data-host-id={h.id}
      className={cx(
        'card hosts-card',
        h.enabled && state === 'down' && 'down',
        h.enabled && state === 'degraded' && 'degraded',
        !h.enabled && 'disabled',
        selected && 'selected',
        focused && 'focused',
      )}
      onClick={() => {
        onFocus(h.id)
        actions.edit(h)
      }}
    >
      <div className="hosts-card-head">
        <HealthDot state={state} />
        <div className="grow">
          <div className="hosts-card-domain truncate">
            {h.domains[0] ?? '—'}
            {h.domains.length > 1 && <span className="more" title={h.domains.slice(1).join(', ')}>+{h.domains.length - 1}</span>}
          </div>
          <div className="hosts-card-upstream truncate">→ {upstreamText(h.upstream, ctx.backends)}</div>
        </div>
        {canWrite && (
          <span className="hosts-card-check" onClick={(e) => e.stopPropagation()}>
            <Checkbox checked={selected} onChange={() => onToggleSelect(h.id)} />
          </span>
        )}
        <Menu trigger={<IconButton icon="more" bare label={`Actions for ${h.domains[0] ?? 'host'}`} />} items={actions.menuItems(h)} />
      </div>
      <div className="hosts-card-mid">{mid}</div>
      <div className="hosts-chips">
        {h.maintenance?.enabled && <span className="badge warn" title="Visitors see the maintenance page (503)">maintenance</span>}
        <ChipView chip={tlsChip(h, ctx)} />
        {h.system && <span className="badge dark">system</span>}
        {features.map((c) => (
          <span key={c.label} className="badge">{c.label}</span>
        ))}
        <span className="hosts-req">{m ? `${compact(m.requests24h)} req` : ''}</span>
      </div>
    </div>
  )
}
