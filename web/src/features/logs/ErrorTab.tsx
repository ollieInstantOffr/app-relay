// Owner: slice observe — Error log tab (nginx / Relay Edge error.log, HAProxy / Relay Balancer engine output).
import { useEffect, useMemo, useRef, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { useInfiniteQuery } from '@tanstack/react-query'
import { Badge, Button, Callout, EmptyState, Input, Pagination, Select, cx, useFitGrid, usePagination } from '../../components/ui'
import { api, errorMessage, qs } from '../../lib/api'
import { Topics, useTopicStream } from '../../lib/events'
import type { ErrorEntry, ErrorPage } from './api'
import {
  ERROR_LEVEL_OPTIONS, ERROR_RANGES, ERROR_SOURCE_OPTIONS, errorApiParams, levelSet, readErrorFilters, type ErrorFilters,
} from './filters'
import { timeCell } from './util'

const PAGE_SIZE = 100
const MAX_LIVE_ROWS = 500
const DEFAULT_RANGE = '24h'

function levelClass(level: string): string {
  if (['emerg', 'alert', 'crit', 'error'].includes(level)) return 'st-danger'
  if (level === 'warn') return 'st-warn'
  return 'st-muted'
}

const sourceTone: Record<string, 'info' | 'dark' | undefined> = { haproxy: 'info', balancer: 'info', relay: 'dark' }

export default function ErrorTab({ live }: { live: boolean }) {
  const [sp, setSp] = useSearchParams()
  const filters = useMemo(() => readErrorFilters(sp), [sp])
  const params = useMemo(() => errorApiParams(filters), [filters])
  const key = JSON.stringify(params)

  const update = (patch: Partial<ErrorFilters>) => {
    const next = new URLSearchParams(sp)
    for (const [k, v] of Object.entries(patch)) {
      if (v && !(k === 'range' && v === DEFAULT_RANGE)) next.set(k, v)
      else next.delete(k)
    }
    setSp(next, { replace: true })
  }

  // Search box: debounced into the URL.
  const [draft, setDraft] = useState(filters.q ?? '')
  const inputRef = useRef<HTMLInputElement>(null)
  useEffect(() => setDraft(filters.q ?? ''), [filters.q])
  useEffect(() => {
    if ((filters.q ?? '') === draft.trim()) return
    const t = window.setTimeout(() => update({ q: draft.trim() || undefined }), 350)
    return () => window.clearTimeout(t)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [draft])
  useEffect(() => {
    const focus = () => inputRef.current?.focus()
    window.addEventListener('relay:focus-search', focus)
    return () => window.removeEventListener('relay:focus-search', focus)
  }, [])

  const list = useInfiniteQuery({
    queryKey: ['logs', 'error', 'list', key],
    queryFn: ({ pageParam }) => api.get<ErrorPage>(`/api/logs/error${qs({ ...params, before: pageParam || undefined, limit: PAGE_SIZE })}`),
    initialPageParam: 0,
    getNextPageParam: (last) => last.nextBeforeId || undefined,
    refetchOnWindowFocus: false,
  })

  const [liveRows, setLiveRows] = useState<ErrorEntry[]>([])
  useEffect(() => setLiveRows([]), [key])
  const wasLive = useRef(live)
  const { refetch } = list
  useEffect(() => {
    if (live && !wasLive.current) {
      setLiveRows([])
      void refetch()
    }
    wasLive.current = live
  }, [live, refetch])

  const matcher = useMemo(() => {
    const levels = levelSet(filters.level)
    const q = filters.q?.toLowerCase()
    return (e: ErrorEntry) =>
      (!filters.source || e.source === filters.source) && (!levels || levels.has(e.level)) && (!q || e.message.toLowerCase().includes(q))
  }, [filters])
  useTopicStream<ErrorEntry>(Topics.ErrorLog, live, (ev) => {
    if (ev.topic !== Topics.ErrorLog || !ev.data || !matcher(ev.data)) return
    setLiveRows((rows) => [ev.data, ...rows].slice(0, MAX_LIVE_ROWS))
  })

  // Past page 1 the live rows are frozen so new lines don't shift the page being read.
  const [frozenLive, setFrozenLive] = useState<ErrorEntry[] | null>(null)
  const shownLive = frozenLive ?? liveRows
  const liveIds = useMemo(() => new Set(shownLive.map((e) => e.id)), [shownLive])
  const rows = useMemo(() => {
    const seen = new Set<number>()
    const out: ErrorEntry[] = []
    for (const e of [...shownLive, ...(list.data?.pages.flatMap((p) => p.entries) ?? [])]) {
      if (!seen.has(e.id)) {
        seen.add(e.id)
        out.push(e)
      }
    }
    return out.sort((a, b) => b.id - a.id)
  }, [shownLive, list.data])

  // Rows that fit the window: below them sit the pager (41px), the card border and the table's bottom margin.
  // Messages wrap, so the tallest row seen sets the page size.
  const rowsRef = useRef<HTMLDivElement>(null)
  const { pageSize } = useFitGrid(rowsRef, { reserve: 41 + 1 + 28, itemHeight: 36, min: 5 })
  const pg = usePagination(rows, pageSize, [key])
  useEffect(() => {
    if (pg.page === 1) setFrozenLive(null)
  }, [pg.page])
  const goPage = (p: number) => {
    if (p > 1) setFrozenLive((f) => f ?? liveRows)
    pg.setPage(p)
  }

  // Load the next server page while the user is on the last two loaded pages.
  const { hasNextPage, isFetchingNextPage, isFetchNextPageError, fetchNextPage } = list
  useEffect(() => {
    if (hasNextPage && !isFetchingNextPage && !isFetchNextPageError && pg.page >= pg.pages - 1) void fetchNextPage()
  }, [hasNextPage, isFetchingNextPage, isFetchNextPageError, fetchNextPage, pg.page, pg.pages])

  const filtered = !!(filters.source || filters.level || filters.q)

  return (
    <div className="logs-body">
      <div className="logs-filterbar">
        <Input
          ref={inputRef}
          inputSize="sm"
          mono
          placeholder="Search messages…"
          aria-label="Search error log"
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          style={{ flex: 1 }}
        />
        <Select inputSize="sm" aria-label="Source" value={filters.source ?? ''} options={ERROR_SOURCE_OPTIONS} onChange={(v) => update({ source: v || undefined })} />
        <Select inputSize="sm" aria-label="Level" value={filters.level ?? ''} options={ERROR_LEVEL_OPTIONS} onChange={(v) => update({ level: v || undefined })} />
        <Select inputSize="sm" aria-label="Time range" value={filters.range} options={ERROR_RANGES} onChange={(v) => update({ range: v })} />
      </div>
      {list.isError && (
        <div className="logs-callout">
          <Callout tone="danger" title="Couldn't load the error log">{errorMessage(list.error)}</Callout>
        </div>
      )}
      <div className="logs-table">
        <div className="lt-head et-head">
          <span>Time</span><span>Source</span><span>Level</span><span>Message</span>
        </div>
        {list.isLoading ? (
          <div className="lt-foot">Loading…</div>
        ) : rows.length === 0 ? (
          !list.isError && (
            <EmptyState
              icon="logs"
              title={filtered ? 'No log lines match these filters' : 'No errors'}
              description={
                filtered
                  ? 'Widen the time range or remove a filter.'
                  : 'Reverse proxy and load balancer warnings and errors show up here — a quiet log is a good log.'
              }
              actions={
                filtered ? (
                  <Button onClick={() => update({ source: undefined, level: undefined, q: undefined })}>Clear filters</Button>
                ) : undefined
              }
            />
          )
        ) : (
          <>
            <div className="lt-rows" ref={rowsRef}>
              {pg.rows.map((e) => (
                <div key={e.id} className={cx('lt-row et-row', liveIds.has(e.id) && 'fresh')}>
                  <span className="st-muted">{timeCell(e.ts)}</span>
                  <span><Badge tone={sourceTone[e.source]}>{e.source}</Badge></span>
                  <span className={levelClass(e.level)}>{e.level}</span>
                  <span className="msg">{e.message}</span>
                </div>
              ))}
            </div>
            <Pagination page={pg.page} pageSize={pg.pageSize} total={pg.total} onPage={goPage} label={hasNextPage ? 'loaded lines' : 'lines'} />
          </>
        )}
      </div>
    </div>
  )
}
