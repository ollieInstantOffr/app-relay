// Owner: slice observe — Audit log with the MCP approvals inbox (design 21).
import { useEffect, useMemo, useRef, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { useInfiniteQuery } from '@tanstack/react-query'
import { AIChip, Avatar, Badge, Callout, EmptyState, Input, Pagination, Select, Skeleton, useFitRows, usePagination, type Tone } from '../../components/ui'
import { api, errorMessage, qs } from '../../lib/api'
import { clock, dateTime } from '../../lib/format'
import { useSettings } from '../../lib/queries'
import type { AuditRow } from '../../lib/types'
import ApprovalsPanel from '../mcp/ApprovalsPanel'
import { AUDIT_ACTOR_OPTIONS, AUDIT_RANGES, auditApiParams, readAuditFilters, type AuditFilters } from './filters'

const PAGE_SIZE = 100
const DEFAULT_RANGE = '7d'

const resultTone: Record<string, Tone> = {
  applied: 'ok',
  pending: 'warn',
  denied: 'danger',
  blocked: 'danger',
  failed: 'danger',
  auto: 'info',
  reverted: 'warn',
}

export default function AuditTab() {
  const [sp, setSp] = useSearchParams()
  const filters = useMemo(() => readAuditFilters(sp), [sp])
  const params = useMemo(() => auditApiParams(filters), [filters])
  const key = JSON.stringify(params)
  const timeout = useSettings('mcp').data?.approvalTimeoutMinutes ?? 10

  const update = (patch: Partial<AuditFilters>) => {
    const next = new URLSearchParams(sp)
    for (const [k, v] of Object.entries(patch)) {
      if (v && !(k === 'range' && v === DEFAULT_RANGE)) next.set(k, v)
      else next.delete(k)
    }
    setSp(next, { replace: true })
  }

  const [draft, setDraft] = useState(filters.q ?? '')
  const inputRef = useRef<HTMLInputElement>(null)
  useEffect(() => setDraft(filters.q ?? ''), [filters.q])
  useEffect(() => {
    if ((filters.q ?? '') === draft.trim()) return
    const t = window.setTimeout(() => update({ q: draft.trim() || undefined }), 300)
    return () => window.clearTimeout(t)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [draft])
  useEffect(() => {
    const focus = () => inputRef.current?.focus()
    window.addEventListener('relay:focus-search', focus)
    return () => window.removeEventListener('relay:focus-search', focus)
  }, [])

  // Key starts with "audit" so audit.appended events refresh it (lib/events).
  const list = useInfiniteQuery({
    queryKey: ['audit', 'list', key],
    queryFn: ({ pageParam }) => api.get<AuditRow[]>(`/api/audit${qs({ ...params, before: pageParam || undefined, limit: PAGE_SIZE })}`),
    initialPageParam: 0,
    getNextPageParam: (last) => (last.length === PAGE_SIZE ? last[last.length - 1].id : undefined),
  })
  const rows = useMemo(() => list.data?.pages.flat() ?? [], [list.data])
  const filtered = !!(filters.q || filters.actor)

  // Rows that fit the window, measured from the table (below the filter bar): below them sit the
  // pager (41px), the card border and the layout's bottom padding.
  const tableRef = useRef<HTMLDivElement>(null)
  const pageSize = useFitRows(tableRef, { reserve: 41 + 1 + 24, min: 5 })
  const pg = usePagination(rows, pageSize, [key])

  // Load the next server page while the user is on the last two loaded pages.
  const { hasNextPage, isFetchingNextPage, isFetchNextPageError, fetchNextPage } = list
  useEffect(() => {
    if (hasNextPage && !isFetchingNextPage && !isFetchNextPageError && pg.page >= pg.pages - 1) void fetchNextPage()
  }, [hasNextPage, isFetchingNextPage, isFetchNextPageError, fetchNextPage, pg.page, pg.pages])

  return (
    <div className="audit-layout">
      <div className="card">
        <div className="audit-filters">
          <Input
            ref={inputRef}
            inputSize="sm"
            placeholder="actor, action, target…"
            aria-label="Filter audit log"
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            style={{ flex: 1 }}
          />
          <Select inputSize="sm" aria-label="Actor" value={filters.actor ?? ''} options={AUDIT_ACTOR_OPTIONS} onChange={(v) => update({ actor: v || undefined })} />
          <Select inputSize="sm" aria-label="Time range" value={filters.range} options={AUDIT_RANGES} onChange={(v) => update({ range: v })} />
        </div>
        {list.isError && (
          <div className="card-body">
            <Callout tone="danger" title="Couldn't load the audit log">{errorMessage(list.error)}</Callout>
          </div>
        )}
        {list.isLoading ? (
          <div className="card-body col gap-10">
            {[0, 1, 2, 3, 4].map((i) => <Skeleton key={i} height={22} />)}
          </div>
        ) : rows.length === 0 ? (
          !list.isError && (
            <EmptyState
              icon="history"
              title={filtered ? 'No entries match' : 'No audit entries'}
              description={filtered ? 'Try another actor, action or a longer time range.' : 'Sign-ins, config changes, applies and MCP tool calls are recorded here.'}
            />
          )
        ) : (
          <div ref={tableRef}>
            <div className="table-wrap">
              <table className="table compact audit-table">
                <thead>
                  <tr>
                    <th>Time</th><th>Actor</th><th>Action</th><th>Target / detail</th><th>Version</th><th>Result</th>
                  </tr>
                </thead>
                <tbody>
                  {pg.rows.map((a) => <AuditTr key={a.id} a={a} />)}
                </tbody>
              </table>
            </div>
            <Pagination page={pg.page} pageSize={pg.pageSize} total={pg.total} onPage={pg.setPage} label={hasNextPage ? 'loaded entries' : 'entries'} />
          </div>
        )}
      </div>
      <div className="audit-side">
        <div className="card">
          <div className="card-header">
            Pending approvals
            <span className="sub">Write tools wait here up to {timeout} min</span>
          </div>
          <div className="card-body">
            <ApprovalsPanel compact />
          </div>
        </div>
      </div>
    </div>
  )
}

function AuditTr({ a }: { a: AuditRow }) {
  const today = new Date(a.at).toDateString() === new Date().toDateString()
  return (
    <tr>
      <td className="mono small faint nowrap">{today ? clock(a.at) : dateTime(a.at)}</td>
      <td><Actor a={a} /></td>
      <td className="mono small nowrap">{a.action}</td>
      <td className="small">
        {a.target && <span className="mono">{a.target}</span>}
        {a.detail && <span className="detail">{a.target ? ' · ' : ''}{a.detail}</span>}
        {!a.target && !a.detail && <span className="faint">—</span>}
      </td>
      <td className="mono small faint">{a.version != null ? `v${a.version}` : '—'}</td>
      <td><Badge tone={resultTone[a.result]}>{a.result}</Badge></td>
    </tr>
  )
}

function Actor({ a }: { a: AuditRow }) {
  const title = a.ip ? `${a.actorName} · ${a.ip}` : a.actorName
  switch (a.actorType) {
    case 'mcp':
      return <span className="actor" title={title}><AIChip /><span className="mono small">mcp · {a.actorName}</span></span>
    case 'user':
      return <span className="actor" title={title}><Avatar name={a.actorName} />{a.actorName}</span>
    case 'token':
      return <span className="actor" title={title}><span className="ai-chip sys">API</span>{a.actorName}</span>
    default:
      return <span className="actor" title={title}><AIChip system />{a.actorName || a.actorType}</span>
  }
}
