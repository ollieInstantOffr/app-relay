// Owner: slice hosts. Hosts table view (design 27).
import type { ReactNode } from 'react'
import { Badge, Checkbox, IconButton, Menu, Skeleton, cx } from '../../components/ui'
import { agoShort, compact } from '../../lib/format'
import type { ProxyHost } from '../../lib/types'
import type { HostActions } from './HostActions'
import { HealthDot } from './HostsGrid'
import { featureSummary, hostHealth, tlsSummary, upstreamText, type ViewCtx } from './lib'

export type ColumnId = 'upstream' | 'tls' | 'access' | 'features' | 'requests' | 'updated'
export const COLUMNS: { id: ColumnId; label: string }[] = [
  { id: 'upstream', label: 'Upstream' },
  { id: 'tls', label: 'TLS' },
  { id: 'access', label: 'Access' },
  { id: 'features', label: 'Features' },
  { id: 'requests', label: 'Req · 24h' },
  { id: 'updated', label: 'Updated' },
]

export type SortKey = 'updated' | 'domain' | 'requests'
export interface SortState {
  key: SortKey
  dir: 'asc' | 'desc'
}

export function HostsTable({
  hosts, ctx, hidden, sort, onSort, selected, focusedId, canWrite, actions, onToggleSelect, onSelectAll, onFocus, loading,
}: {
  hosts: ProxyHost[]
  ctx: ViewCtx
  hidden: ColumnId[]
  sort: SortState
  onSort: (key: SortKey) => void
  selected: Set<string>
  focusedId: string | null
  canWrite: boolean
  actions: HostActions
  onToggleSelect: (id: string) => void
  onSelectAll: (ids: string[]) => void
  onFocus: (id: string) => void
  loading: boolean
}) {
  const show = (c: ColumnId) => !hidden.includes(c)
  const allSelected = hosts.length > 0 && hosts.every((h) => selected.has(h.id))
  const someSelected = !allSelected && hosts.some((h) => selected.has(h.id))
  const colCount = 3 + (canWrite ? 1 : 0) + COLUMNS.filter((c) => show(c.id)).length - 1

  const sortTh = (key: SortKey, label: ReactNode, className?: string) => (
    <th className={cx('sortable', className)} onClick={() => onSort(key)} aria-sort={sort.key === key ? (sort.dir === 'asc' ? 'ascending' : 'descending') : undefined}>
      {label}
      {sort.key === key && <span className="arrow">{sort.dir === 'asc' ? '↑' : '↓'}</span>}
    </th>
  )

  return (
    <div className="card hosts-table-card">
      <div className="table-wrap">
        <table className="table hosts-table">
          <thead>
            <tr>
              {canWrite && (
                <th>
                  <Checkbox checked={allSelected} indeterminate={someSelected} onChange={(v) => onSelectAll(v ? hosts.map((h) => h.id) : [])} />
                </th>
              )}
              {sortTh('domain', 'Domain')}
              {show('upstream') && <th>Upstream</th>}
              {show('tls') && <th>TLS</th>}
              {show('access') && <th>Access</th>}
              {show('features') && <th>Features</th>}
              {show('requests') && sortTh('requests', 'Req · 24h', 'num')}
              {show('updated') && sortTh('updated', 'Updated')}
              <th className="col-menu" aria-label="Actions" />
            </tr>
          </thead>
          <tbody>
            {loading &&
              Array.from({ length: 5 }, (_, i) => (
                <tr key={i}>
                  <td colSpan={colCount + 1}>
                    <Skeleton height={14} />
                  </td>
                </tr>
              ))}
            {!loading &&
              hosts.map((h) => {
                const { state, status } = hostHealth(h, ctx.health)
                const tls = tlsSummary(h, ctx)
                const list = h.accessListId ? ctx.lists.get(h.accessListId) : undefined
                const m = ctx.metrics?.[h.id]
                const features = featureSummary(h, ctx)
                return (
                  <tr
                    key={h.id}
                    data-host-id={h.id}
                    className={cx('clickable', !h.enabled && 'dim', selected.has(h.id) && 'selected', focusedId === h.id && 'focused')}
                    onClick={() => {
                      onFocus(h.id)
                      actions.edit(h)
                    }}
                  >
                    {canWrite && (
                      <td onClick={(e) => e.stopPropagation()}>
                        <Checkbox checked={selected.has(h.id)} onChange={() => onToggleSelect(h.id)} />
                      </td>
                    )}
                    <td>
                      <div className="domain-cell">
                        <HealthDot state={state} inline />
                        <span>{h.domains[0] ?? '—'}</span>
                        {h.domains.length > 1 && (
                          <span className="faint mono small" title={h.domains.slice(1).join(', ')}>+{h.domains.length - 1}</span>
                        )}
                        {h.system && <Badge tone="dark">system</Badge>}
                        {h.maintenance?.enabled && <Badge tone="warn" title="Visitors see the maintenance page (503)">maintenance</Badge>}
                      </div>
                    </td>
                    {show('upstream') && (
                      <td>
                        <span className="mono-cell">
                          {upstreamText(h.upstream, ctx.backends)}
                          {h.enabled && state === 'down' && <span className="down-text"> · {status?.httpStatus || 'down'}</span>}
                        </span>
                      </td>
                    )}
                    {show('tls') && (
                      <td>
                        <span className={cx('mono-cell', tls.tone === 'warn' && 'warn-text', tls.tone === 'danger' && 'danger-text')}>{tls.text}</span>
                      </td>
                    )}
                    {show('access') && (
                      <td>
                        <span className="mono-cell">{list?.name ?? (h.accessListId ? 'missing' : '—')}</span>
                      </td>
                    )}
                    {show('features') && (
                      <td>
                        <span className="mono-cell">{features || '—'}</span>
                      </td>
                    )}
                    {show('requests') && <td className="num">{m ? compact(m.requests24h) : '—'}</td>}
                    {show('updated') && (
                      <td>
                        <span className="mono-cell" title={h.updatedAt}>{agoShort(h.updatedAt)}</span>
                      </td>
                    )}
                    <td className="col-menu" onClick={(e) => e.stopPropagation()}>
                      <Menu trigger={<IconButton icon="more" bare label={`Actions for ${h.domains[0] ?? 'host'}`} />} items={actions.menuItems(h)} />
                    </td>
                  </tr>
                )
              })}
          </tbody>
        </table>
      </div>
    </div>
  )
}
