// Owner: slice observe — Access log tab (design 07): token filters, histogram, live table.
import { memo, useEffect, useMemo, useRef, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { keepPreviousData, useInfiniteQuery, useQuery } from '@tanstack/react-query'
import { Button, Callout, EmptyState, Pagination, Select, cx, useFitGrid, usePagination } from '../../components/ui'
import { api, errorMessage, qs } from '../../lib/api'
import { Topics, useTopicStream } from '../../lib/events'
import { compact } from '../../lib/format'
import type { AccessEntry, AccessPage } from '../../lib/types'
import type { Histogram } from './api'
import {
  ACCESS_RANGES, ACCESS_TOKEN_KEYS, DEFAULT_ACCESS_RANGE, accessApiParams, accessMatcher, parseFilterText, readAccessFilters,
  statusMatcher, type AccessFilters,
} from './filters'
import LogDetailDrawer from './LogDetailDrawer'
import { requestLabel, sizeLabel, statusTone, tickLabel, timeCell, upstreamInfo } from './util'

const PAGE_SIZE = 100
const MAX_LIVE_ROWS = 500

export default function AccessTab({ live }: { live: boolean }) {
  const [sp, setSp] = useSearchParams()
  const filters = useMemo(() => readAccessFilters(sp), [sp])
  const params = useMemo(() => accessApiParams(filters), [filters])
  const key = JSON.stringify(params)
  const statusInvalid = !!filters.status && statusMatcher(filters.status) === null
  const [selected, setSelected] = useState<number | null>(null)

  const update = (patch: Partial<AccessFilters>) => {
    const next = new URLSearchParams(sp)
    for (const [k, v] of Object.entries(patch)) {
      if (v && !(k === 'range' && v === DEFAULT_ACCESS_RANGE)) next.set(k, v)
      else next.delete(k)
    }
    setSp(next, { replace: true })
  }

  const list = useInfiniteQuery({
    queryKey: ['logs', 'access', 'list', key],
    queryFn: ({ pageParam }) =>
      api.get<AccessPage>(`/api/logs/access${qs({ ...params, before: pageParam || undefined, limit: PAGE_SIZE })}`),
    initialPageParam: 0,
    getNextPageParam: (last) => last.nextBeforeId || undefined,
    enabled: !statusInvalid,
    refetchOnWindowFocus: false,
  })

  const hist = useQuery({
    queryKey: ['logs', 'access', 'histogram', key],
    queryFn: () => api.get<Histogram>(`/api/logs/access/histogram${qs({ ...params, buckets: 60 })}`),
    enabled: !statusInvalid,
    refetchInterval: live ? 10_000 : false,
    placeholderData: keepPreviousData,
  })

  // Live tail: prepend committed rows that match the current filters.
  const [liveRows, setLiveRows] = useState<AccessEntry[]>([])
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

  const matcher = useMemo(() => accessMatcher(filters), [filters])
  useTopicStream<AccessEntry>(Topics.AccessLog, live && !statusInvalid, (ev) => {
    if (ev.topic !== Topics.AccessLog || !ev.data || !matcher(ev.data)) return
    setLiveRows((rows) => [ev.data, ...rows].slice(0, MAX_LIVE_ROWS))
  })

  // Past page 1 the live rows are frozen so new requests don't shift the page being read.
  const [frozenLive, setFrozenLive] = useState<AccessEntry[] | null>(null)
  const shownLive = frozenLive ?? liveRows
  const liveIds = useMemo(() => new Set(shownLive.map((e) => e.id)), [shownLive])
  const rows = useMemo(() => {
    const seen = new Set<number>()
    const out: AccessEntry[] = []
    const push = (e: AccessEntry) => {
      if (!seen.has(e.id)) {
        seen.add(e.id)
        out.push(e)
      }
    }
    shownLive.forEach(push)
    list.data?.pages.forEach((p) => p.entries.forEach(push))
    return out.sort((a, b) => b.id - a.id)
  }, [shownLive, list.data])

  // Rows that fit the window: below them sit the pager (41px), the card border and the table's bottom margin.
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

  const filtered = ACCESS_TOKEN_KEYS.some((k) => filters[k]) || !!filters.q
  const rangeLabel = ACCESS_RANGES.find((r) => r.value === filters.range)?.label.toLowerCase() ?? filters.range

  return (
    <div className="logs-body">
      <AccessFilterBar filters={filters} invalid={statusInvalid} onChange={update} />
      {statusInvalid && (
        <div className="logs-callout">
          <Callout tone="danger">Invalid status filter “{filters.status}” — try 502, &gt;=500, 4xx or !200.</Callout>
        </div>
      )}
      {list.isError && (
        <div className="logs-callout">
          <Callout tone="danger" title="Couldn't load the access log">{errorMessage(list.error)}</Callout>
        </div>
      )}
      <HistogramStrip data={statusInvalid ? undefined : hist.data} />
      <div className="logs-table">
        <div className="lt-head">
          <span>Time</span><span>Status</span><span>Host</span><span>Request</span><span>Client</span><span>Upstream</span><span>Size</span>
        </div>
        {list.isLoading ? (
          <div className="lt-foot">Loading…</div>
        ) : rows.length === 0 ? (
          !list.isError &&
          !statusInvalid && (
            <EmptyState
              icon="logs"
              title={filtered ? 'No requests match these filters' : 'No requests yet'}
              description={
                filtered
                  ? `Nothing in the ${rangeLabel} matches. Widen the time range or remove a filter.`
                  : `Nothing was served in the ${rangeLabel}. Requests through the reverse proxy show up here within a second.`
              }
              actions={
                filtered ? (
                  <Button onClick={() => update({ host: undefined, status: undefined, ip: undefined, method: undefined, kind: undefined, q: undefined })}>
                    Clear filters
                  </Button>
                ) : undefined
              }
            />
          )
        ) : (
          <>
            <div className="lt-rows" ref={rowsRef}>
              {pg.rows.map((e) => (
                <AccessRow key={e.id} e={e} fresh={liveIds.has(e.id)} selected={selected === e.id} onOpen={setSelected} />
              ))}
            </div>
            <Pagination page={pg.page} pageSize={pg.pageSize} total={pg.total} onPage={goPage} label={hasNextPage ? 'loaded requests' : 'requests'} />
          </>
        )}
      </div>
      <LogDetailDrawer
        id={selected}
        onClose={() => setSelected(null)}
        onSelect={setSelected}
        onFilterIp={(ip) => {
          update({ ip })
          setSelected(null)
        }}
      />
    </div>
  )
}

function AccessFilterBar({ filters, invalid, onChange }: { filters: AccessFilters; invalid: boolean; onChange: (p: Partial<AccessFilters>) => void }) {
  const [draft, setDraft] = useState(filters.q ?? '')
  const inputRef = useRef<HTMLInputElement>(null)
  useEffect(() => setDraft(filters.q ?? ''), [filters.q])
  useEffect(() => {
    const focus = () => inputRef.current?.focus()
    window.addEventListener('relay:focus-search', focus)
    return () => window.removeEventListener('relay:focus-search', focus)
  }, [])

  const chips = ACCESS_TOKEN_KEYS.filter((k) => filters[k])
  const commit = (text: string) => {
    const { tokens, rest } = parseFilterText(text)
    setDraft(rest)
    const patch: Partial<AccessFilters> = { ...tokens }
    if ((filters.q ?? '') !== rest) patch.q = rest || undefined
    if (Object.keys(patch).length > 0) onChange(patch)
  }
  const remove = (k: (typeof ACCESS_TOKEN_KEYS)[number]) => {
    const patch: Partial<AccessFilters> = {}
    patch[k] = undefined
    onChange(patch)
  }

  return (
    <div className="logs-filterbar">
      <div
        className={cx('token-input', invalid && 'invalid')}
        onClick={() => inputRef.current?.focus()}
        title="host:, status:, ip:, method:, kind: become filters — other words search path, client IP, host and user agent"
      >
        <span className="label">filter</span>
        {chips.map((k) => (
          <span key={k} className="token" title={`${k}:${filters[k]}`}>
            {k}:{filters[k]}
            <button
              type="button"
              aria-label={`Remove ${k} filter`}
              onClick={(e) => {
                e.stopPropagation()
                remove(k)
              }}
            >
              ×
            </button>
          </span>
        ))}
        <input
          ref={inputRef}
          value={draft}
          spellCheck={false}
          aria-label="Filter access log"
          placeholder="| path, ip, method, ua…"
          onChange={(e) => {
            const v = e.target.value
            if (/\s$/.test(v) && Object.keys(parseFilterText(v).tokens).length > 0) commit(v)
            else setDraft(v)
          }}
          onKeyDown={(e) => {
            if (e.key === 'Enter') {
              e.preventDefault()
              commit(draft)
            } else if (e.key === 'Backspace' && draft === '' && chips.length > 0) {
              remove(chips[chips.length - 1])
            } else if (e.key === 'Escape') {
              setDraft(filters.q ?? '')
              e.currentTarget.blur()
            }
          }}
          onBlur={() => commit(draft)}
        />
      </div>
      {(chips.length > 0 || filters.q) && (
        <Button
          size="sm"
          variant="ghost"
          onClick={() => onChange({ host: undefined, status: undefined, ip: undefined, method: undefined, kind: undefined, q: undefined })}
        >
          Clear
        </Button>
      )}
      <Select inputSize="sm" aria-label="Time range" value={filters.range} options={ACCESS_RANGES} onChange={(v) => onChange({ range: v })} />
    </div>
  )
}

function HistogramStrip({ data }: { data?: Histogram }) {
  const [hover, setHover] = useState<number | null>(null)
  if (!data) {
    return (
      <div className="logs-hist">
        <div className="hist-bars loading" />
        <div className="logs-axis" />
      </div>
    )
  }
  const n = data.buckets.length
  const max = Math.max(0, ...data.buckets.map((b) => b.total))
  const ticks = n > 1 ? [0, Math.round(n / 4), Math.round(n / 2), Math.round((3 * n) / 4), n - 1].map((i) => tickLabel(data.buckets[i].t, data.stepSeconds)) : []
  const b = hover !== null ? data.buckets[hover] : undefined
  return (
    <div className="logs-hist" onMouseLeave={() => setHover(null)}>
      <div className="hist-bars">
        {data.buckets.map((bk, i) => {
          const rest = bk.total - bk.s4xx - bk.s5xx
          return (
            <div key={bk.t} className={cx('hist-bar', i === n - 1 && 'current')} onMouseEnter={() => setHover(i)}>
              {bk.total > 0 && (
                <div className="hist-stack" style={{ height: `${Math.max(6, (bk.total / max) * 100)}%` }}>
                  {bk.s5xx > 0 && <div className="hist-seg s5" style={{ flexGrow: bk.s5xx }} />}
                  {bk.s4xx > 0 && <div className="hist-seg s4" style={{ flexGrow: bk.s4xx }} />}
                  {rest > 0 && <div className="hist-seg ok" style={{ flexGrow: rest }} />}
                </div>
              )}
            </div>
          )
        })}
      </div>
      {b && (
        <div className="chart-tip" style={{ left: `calc(28px + (100% - 56px) * ${(hover! + 0.5) / n})` }}>
          {tickLabel(b.t, data.stepSeconds)} · {compact(b.total)} req
          {b.s4xx > 0 && <span className="tip-warn"> · {compact(b.s4xx)} 4xx</span>}
          {b.s5xx > 0 && <span className="tip-err"> · {compact(b.s5xx)} 5xx</span>}
        </div>
      )}
      <div className="logs-axis">
        {ticks.map((t, i) => (
          <span key={i}>{t}</span>
        ))}
      </div>
    </div>
  )
}

const AccessRow = memo(function AccessRow({ e, fresh, selected, onOpen }: { e: AccessEntry; fresh: boolean; selected: boolean; onOpen: (id: number) => void }) {
  const up = upstreamInfo(e)
  const req = requestLabel(e)
  return (
    <div
      className={cx('lt-row', e.status >= 500 && 'err', fresh && 'fresh', selected && 'selected')}
      role="button"
      tabIndex={0}
      onClick={() => onOpen(e.id)}
      onKeyDown={(ev) => {
        if (ev.key === 'Enter') onOpen(e.id)
      }}
    >
      <span className="st-muted">{timeCell(e.ts)}</span>
      <span className={`st-${statusTone(e.status)}`}>{e.status || '—'}</span>
      <span title={e.host}>{e.host || '—'}</span>
      <span title={req}>{req}</span>
      <span title={e.clientIp}>{e.clientIp || '—'}</span>
      <span className={up.error ? 'up-err' : undefined}>{up.text}</span>
      <span>{sizeLabel(e)}</span>
    </div>
  )
})
