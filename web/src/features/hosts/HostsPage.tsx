// Owner: slice hosts. Proxy hosts page: hosts grid/table, redirects and default host
// (design 02, 03, 18a, 18b, 20, 22a, 27, 30d).
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Navigate, useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { TopBar } from '../../components/shell/TopBar'
import {
  Button, EmptyState, ListPager, Menu, Pagination, SearchInput, Segmented, Select, TableToolbar, matchesSearch, useFitGrid, useFitRows, usePagination,
  type MenuEntry,
} from '../../components/ui'
import { errorMessage } from '../../lib/api'
import { useContainers, useEntities, useHealth, useRole, useSettings } from '../../lib/queries'
import type { ProxyHost, Source } from '../../lib/types'
import DockerSuggestionsDialog from '../docker/DockerSuggestionsDialog'
import HostDrawer from './HostDrawer'
import { useHostActions } from './HostActions'
import { HostsGrid } from './HostsGrid'
import { COLUMNS, HostsTable, type ColumnId, type SortKey, type SortState } from './HostsTable'
import { BulkBar } from './BulkBar'
import { RedirectsView } from './RedirectsTab'
import { DefaultHostView } from './DefaultHostTab'
import { hostHealth, isTyping, overlayOpen, upstreamText, useHostMetrics, useLocalStorage, type ViewCtx } from './lib'
import './hosts.css'

type StatusFilter = '' | 'enabled' | 'disabled' | 'maintenance'
type HealthFilter = '' | 'healthy' | 'errors' | 'unknown'
type TLSFilter = '' | 'https' | 'http'

const SOURCE_LABELS: Record<Source, string> = { manual: 'Manual', docker: 'Docker', expose: 'Expose wizard', import: 'Import', mcp: 'MCP' }

/** Space kept free at the bottom for the floating bulk action bar. */
const BULK_BAR_SPACE = 50
/** Pager under the grid (ListPager) plus the page gap above it. */
const GRID_PAGER = 28 + 16

export default function HostsPage() {
  const { tab } = useParams()
  const [params, setParams] = useSearchParams()
  const { canWrite } = useRole()
  const hostsQ = useEntities('hosts')
  const redirectsQ = useEntities('redirects')
  const [filter, setFilter] = useState('')
  const view = tab === 'redirects' ? 'redirects' : tab === 'default' ? 'default' : 'hosts'

  useEffect(() => {
    const h = () => {
      const input = document.querySelector<HTMLInputElement>('.hosts-page .search-input input')
      input?.focus()
      input?.select()
    }
    window.addEventListener('relay:focus-search', h)
    return () => window.removeEventListener('relay:focus-search', h)
  }, [])
  useEffect(() => setFilter(''), [view])

  const openNewHost = useCallback(
    () =>
      setParams((p) => {
        p.delete('edit')
        p.delete('tab')
        p.set('new', '1')
        return p
      }),
    [setParams],
  )
  const closeDrawer = useCallback(
    () =>
      setParams((p) => {
        p.delete('edit')
        p.delete('new')
        p.delete('tab')
        return p
      }),
    [setParams],
  )

  if (tab && view === 'hosts') return <Navigate to="/hosts" replace />

  const isNew = params.get('new') === '1'
  const editId = params.get('edit')

  return (
    <>
      <TopBar
        title="Proxy hosts"
        tabs={[
          { to: '/hosts', label: 'Hosts', count: hostsQ.data?.length },
          { to: '/hosts/redirects', label: 'Redirects', count: redirectsQ.data?.length },
          { to: '/hosts/default', label: 'Default' },
        ]}
        actions={
          <>
            {canWrite && view === 'hosts' && (
              <Button variant="primary" icon="plus" onClick={openNewHost}>
                New host
              </Button>
            )}
            {canWrite && view === 'redirects' && (
              <Button
                variant="primary"
                icon="plus"
                onClick={() =>
                  setParams((p) => {
                    p.delete('edit')
                    p.set('new', '1')
                    return p
                  })
                }
              >
                New redirect
              </Button>
            )}
          </>
        }
      />
      <div className="page hosts-page">
        {view === 'hosts' && (
          <HostsView
            hosts={hostsQ.data}
            loading={hostsQ.isLoading}
            error={hostsQ.isError ? errorMessage(hostsQ.error) : undefined}
            onRetry={() => void hostsQ.refetch()}
            filter={filter}
            onFilter={setFilter}
            onNew={openNewHost}
          />
        )}
        {view === 'redirects' && (
          <RedirectsView redirects={redirectsQ.data ?? []} loading={redirectsQ.isLoading} filter={filter} onFilter={setFilter} />
        )}
        {view === 'default' && <DefaultHostView />}
      </div>
      {view === 'hosts' && (
        <HostDrawer open={isNew || !!editId} isNew={isNew} hostId={editId ?? undefined} initialTab={params.get('tab')} onClose={closeDrawer} />
      )}
    </>
  )
}

function HostsView({ hosts: all, loading, error, onRetry, filter, onFilter, onNew }: {
  hosts?: ProxyHost[]
  loading: boolean
  error?: string
  onRetry: () => void
  filter: string
  onFilter: (v: string) => void
  onNew: () => void
}) {
  const navigate = useNavigate()
  const { canWrite, isAdmin } = useRole()
  const health = useHealth().data
  const metricsQ = useHostMetrics()
  const certs = useEntities('certificates').data
  const lists = useEntities('access-lists').data
  const backends = useEntities('backends').data
  const tls = useSettings('tls').data
  const containersQ = useContainers()
  const actions = useHostActions()

  const [status, setStatus] = useState<StatusFilter>('')
  const [healthFilter, setHealthFilter] = useState<HealthFilter>('')
  const [tlsFilter, setTlsFilter] = useState<TLSFilter>('')
  const [source, setSource] = useState<Source | ''>('')
  const [mode, setMode] = useLocalStorage<'grid' | 'table'>('relay.hosts.view', 'grid')
  const [sort, setSort] = useLocalStorage<SortState>('relay.hosts.sort', { key: 'updated', dir: 'desc' })
  const [hidden, setHidden] = useLocalStorage<ColumnId[]>('relay.hosts.columns.hidden', [])
  const [selected, setSelected] = useState<Set<string>>(() => new Set())
  const [focusedId, setFocusedId] = useState<string | null>(null)
  const [bulkDeleteOpen, setBulkDeleteOpen] = useState(false)
  const [dockerOpen, setDockerOpen] = useState(false)
  const tableRef = useRef<HTMLDivElement>(null)
  const gridRef = useRef<HTMLDivElement>(null)

  const hosts = useMemo(() => all ?? [], [all])
  const ctx: ViewCtx = useMemo(
    () => ({
      health,
      metrics: metricsQ.data,
      metricsFailed: metricsQ.isError,
      certs: new Map((certs ?? []).map((c) => [c.id, c])),
      lists: new Map((lists ?? []).map((l) => [l.id, l])),
      backends: new Map((backends ?? []).map((b) => [b.id, b.name])),
      tls,
    }),
    [health, metricsQ.data, metricsQ.isError, certs, lists, backends, tls],
  )

  const counts = useMemo(() => {
    const c = { enabled: 0, disabled: 0, maintenance: 0, healthy: 0, errors: 0, unknown: 0, https: 0, http: 0 }
    const sources = new Map<Source, number>()
    for (const h of hosts) {
      const s = hostHealth(h, health).state
      if (h.enabled) c.enabled++
      else c.disabled++
      if (h.maintenance?.enabled) c.maintenance++
      if (s === 'healthy') c.healthy++
      else if (s === 'down' || s === 'degraded') c.errors++
      else if (s === 'unknown') c.unknown++
      if (h.certificateId) c.https++
      else c.http++
      const src = h.source || 'manual'
      sources.set(src, (sources.get(src) ?? 0) + 1)
    }
    return { ...c, sources }
  }, [hosts, health])

  const visible = useMemo(() => {
    const list = hosts.filter((h) => {
      if (status === 'enabled' && !h.enabled) return false
      if (status === 'disabled' && h.enabled) return false
      if (status === 'maintenance' && !h.maintenance?.enabled) return false
      if (healthFilter) {
        const s = hostHealth(h, health).state
        if (healthFilter === 'healthy' && s !== 'healthy') return false
        if (healthFilter === 'errors' && s !== 'down' && s !== 'degraded') return false
        if (healthFilter === 'unknown' && s !== 'unknown') return false
      }
      if (tlsFilter === 'https' && !h.certificateId) return false
      if (tlsFilter === 'http' && h.certificateId) return false
      if (source && (h.source || 'manual') !== source) return false
      if (!filter.trim()) return true
      const listName = h.accessListId ? ctx.lists.get(h.accessListId)?.name : undefined
      const certName = h.certificateId ? ctx.certs.get(h.certificateId)?.name : undefined
      return matchesSearch(filter, ...h.domains, upstreamText(h.upstream, ctx.backends), certName, listName, h.sourceRef)
    })
    const dir = sort.dir === 'asc' ? 1 : -1
    return list.sort((a, b) => {
      let c = 0
      if (sort.key === 'domain') c = (a.domains[0] ?? '').localeCompare(b.domains[0] ?? '')
      else if (sort.key === 'requests') c = (ctx.metrics?.[a.id]?.requests24h ?? 0) - (ctx.metrics?.[b.id]?.requests24h ?? 0)
      else c = (a.updatedAt ?? '').localeCompare(b.updatedAt ?? '')
      return c * dir || (a.domains[0] ?? '').localeCompare(b.domains[0] ?? '')
    })
  }, [hosts, health, status, healthFilter, tlsFilter, source, filter, sort, ctx])

  // Page size: table rows or grid cells that fit the window, leaving room for the bulk bar.
  const bulkSpace = canWrite ? BULK_BAR_SPACE : 0
  const tableRows = useFitRows(tableRef, { reserve: 41 + 24 + bulkSpace })
  const grid = useFitGrid(gridRef, { reserve: GRID_PAGER + 24 + bulkSpace, itemHeight: 153, min: 1 })
  const pageSize = mode === 'grid' ? Math.max(1, grid.pageSize - (canWrite ? 1 : 0)) : tableRows
  const pg = usePagination(visible, pageSize, [filter, status, healthFilter, tlsFilter, source, sort.key, sort.dir, mode])
  const { setPage } = pg

  const filtersOn = !!(filter.trim() || status || healthFilter || tlsFilter || source)
  const clearFilters = () => {
    setStatus('')
    setHealthFilter('')
    setTlsFilter('')
    setSource('')
    onFilter('')
  }

  // Drop selections for hosts that no longer exist.
  useEffect(() => {
    setSelected((prev) => {
      if (prev.size === 0) return prev
      const ids = new Set(hosts.map((h) => h.id))
      const next = new Set([...prev].filter((id) => ids.has(id)))
      return next.size === prev.size ? prev : next
    })
  }, [hosts])

  const toggleSelect = useCallback((id: string) => {
    setSelected((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }, [])

  const onSort = (key: SortKey) =>
    setSort((s) => (s.key === key ? { key, dir: s.dir === 'asc' ? 'desc' : 'asc' } : { key, dir: key === 'domain' ? 'asc' : 'desc' }))

  // Keyboard: J/K move · Enter/E edit · X select · ⌘A select all · ⌫ delete · V grid/table.
  const stateRef = useRef({ visible, focusedId, selected, canWrite, actions })
  stateRef.current = { visible, focusedId, selected, canWrite, actions }
  const lastG = useRef(0)
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (isTyping(e) || overlayOpen()) return
      const { visible, focusedId, selected, canWrite, actions } = stateRef.current
      const key = e.key
      if ((e.metaKey || e.ctrlKey) && key.toLowerCase() === 'a') {
        if (!canWrite || visible.length === 0) return
        e.preventDefault()
        setSelected(new Set(visible.map((h) => h.id)))
        return
      }
      if (e.metaKey || e.ctrlKey || e.altKey) return
      // "G <key>" is global navigation — don't treat the second key as a list shortcut.
      if (key === 'g') {
        lastG.current = Date.now()
        return
      }
      if (Date.now() - lastG.current < 1200) {
        lastG.current = 0
        return
      }
      const idx = visible.findIndex((h) => h.id === focusedId)
      const focused = idx >= 0 ? visible[idx] : undefined
      switch (key) {
        case 'j':
        case 'J':
          e.preventDefault()
          setFocusedId(visible[Math.min(visible.length - 1, idx + 1)]?.id ?? null)
          break
        case 'k':
        case 'K':
          e.preventDefault()
          setFocusedId(visible[Math.max(0, idx - 1)]?.id ?? null)
          break
        case 'Enter':
          if ((e.target as HTMLElement | null)?.closest?.('button, a')) return
          if (focused) {
            e.preventDefault()
            actions.edit(focused)
          }
          break
        case 'e':
        case 'E':
          if (focused) {
            e.preventDefault()
            actions.edit(focused)
          }
          break
        case 'x':
        case 'X':
          if (focused && canWrite) {
            e.preventDefault()
            toggleSelect(focused.id)
          }
          break
        case 'v':
        case 'V':
          e.preventDefault()
          setMode((m) => (m === 'grid' ? 'table' : 'grid'))
          break
        case 'Backspace':
        case 'Delete':
          if (!canWrite) break
          if (selected.size > 0) {
            e.preventDefault()
            setBulkDeleteOpen(true)
          } else if (focused) {
            e.preventDefault()
            actions.requestDelete(focused)
          }
          break
        case 'Escape':
          if (selected.size > 0) setSelected(new Set())
          break
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [setMode, toggleSelect])

  // Moving the keyboard focus onto a host on another page turns to that page.
  useEffect(() => {
    if (!focusedId) return
    const idx = stateRef.current.visible.findIndex((h) => h.id === focusedId)
    if (idx >= 0) setPage(Math.floor(idx / pageSize) + 1)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [focusedId])

  useEffect(() => {
    if (focusedId) document.querySelector(`[data-host-id="${CSS.escape(focusedId)}"]`)?.scrollIntoView({ block: 'nearest' })
  }, [focusedId, mode, pg.page])

  if (error) {
    return (
      <div className="card">
        <EmptyState icon="warning" title="Couldn't load proxy hosts" description={error} actions={<Button onClick={onRetry}>Retry</Button>} />
      </div>
    )
  }

  if (!loading && hosts.length === 0) {
    const dockerCount = containersQ.data?.filter((c) => c.http && !c.hostId && !c.backendId).length ?? 0
    return (
      <>
        <div className="card">
          <EmptyState
            icon="hosts"
            title="No proxy hosts yet"
            description="A host maps a domain to something on your network. Start with one, or pull them in from Docker or an existing Nginx Proxy Manager."
            actions={
              canWrite ? (
                <>
                  <Button variant="primary" icon="plus" onClick={onNew}>New host</Button>
                  {containersQ.isSuccess && (
                    <Button icon="docker" disabled={dockerCount === 0} onClick={() => setDockerOpen(true)}>
                      From Docker · {dockerCount} found
                    </Button>
                  )}
                  {isAdmin && (
                    <Button icon="upload" onClick={() => navigate('/settings/backup?import=npm')}>
                      Import NPM
                    </Button>
                  )}
                </>
              ) : undefined
            }
          />
        </div>
        <DockerSuggestionsDialog open={dockerOpen} onClose={() => setDockerOpen(false)} />
        {actions.dialogs}
      </>
    )
  }

  const columnItems: MenuEntry[] = [
    { header: 'Show columns' },
    ...COLUMNS.map((c) => ({
      label: c.label,
      icon: hidden.includes(c.id) ? undefined : ('check' as const),
      onSelect: () => setHidden((h) => (h.includes(c.id) ? h.filter((x) => x !== c.id) : [...h, c.id])),
    })),
  ]

  const withCount = (label: string, n: number) => `${label} · ${n}`
  const sourceOptions = (Object.keys(SOURCE_LABELS) as Source[])
    .filter((s) => counts.sources.has(s) || s === source)
    .map((s) => ({ value: s, label: withCount(SOURCE_LABELS[s], counts.sources.get(s) ?? 0) }))

  return (
    <>
      <TableToolbar
        actions={
          <>
            {mode === 'table' && <Menu trigger={<Button size="sm" variant="ghost" iconRight="chevron">Columns</Button>} items={columnItems} />}
            <Segmented
              value={mode}
              onChange={(m) => setMode(m)}
              options={[
                { value: 'grid', label: 'Grid' },
                { value: 'table', label: 'Table' },
              ]}
            />
            <Select
              className="hosts-sort"
              inputSize="sm"
              value={sort.key}
              aria-label="Sort hosts"
              onChange={(v) => setSort({ key: v as SortKey, dir: v === 'domain' ? 'asc' : 'desc' })}
              options={[
                { value: 'updated', label: 'Sort: Recently updated' },
                { value: 'domain', label: 'Sort: Domain' },
                { value: 'requests', label: 'Sort: Requests' },
              ]}
            />
          </>
        }
      >
        <SearchInput value={filter} onChange={onFilter} placeholder="Search domain, upstream, certificate" label="Search hosts" width={280} />
        <div className="hosts-filter-select">
          <Select
            inputSize="sm"
            value={status}
            placeholder="All statuses"
            aria-label="Status"
            onChange={(v) => setStatus(v as StatusFilter)}
            options={[
              { value: 'enabled', label: withCount('Enabled', counts.enabled) },
              { value: 'disabled', label: withCount('Disabled', counts.disabled) },
              { value: 'maintenance', label: withCount('Maintenance', counts.maintenance) },
            ]}
          />
        </div>
        <div className="hosts-filter-select">
          <Select
            inputSize="sm"
            value={healthFilter}
            placeholder="All health"
            aria-label="Health"
            onChange={(v) => setHealthFilter(v as HealthFilter)}
            options={[
              { value: 'healthy', label: withCount('Healthy', counts.healthy) },
              { value: 'errors', label: withCount('Down or degraded', counts.errors) },
              { value: 'unknown', label: withCount('Not checked', counts.unknown) },
            ]}
          />
        </div>
        <div className="hosts-filter-select">
          <Select
            inputSize="sm"
            value={tlsFilter}
            placeholder="All TLS"
            aria-label="TLS"
            onChange={(v) => setTlsFilter(v as TLSFilter)}
            options={[
              { value: 'https', label: withCount('HTTPS', counts.https) },
              { value: 'http', label: withCount('HTTP only', counts.http) },
            ]}
          />
        </div>
        {sourceOptions.length > 1 || source ? (
          <div className="hosts-filter-select">
            <Select inputSize="sm" value={source} placeholder="All sources" aria-label="Source" onChange={(v) => setSource(v as Source | '')} options={sourceOptions} />
          </div>
        ) : null}
      </TableToolbar>

      {!loading && visible.length === 0 ? (
        <div className="card">
          <EmptyState
            icon="filter"
            title="No hosts match"
            description={filter.trim() ? `Nothing matches “${filter.trim()}” with the current filters.` : 'No hosts match the current filters.'}
            actions={filtersOn && <Button onClick={clearFilters}>Clear filters</Button>}
          />
        </div>
      ) : mode === 'grid' ? (
        <>
          <HostsGrid
            gridRef={gridRef}
            hosts={pg.rows}
            ctx={ctx}
            selected={selected}
            focusedId={focusedId}
            canWrite={canWrite}
            actions={actions}
            onToggleSelect={toggleSelect}
            onFocus={setFocusedId}
            onNew={onNew}
            loading={loading}
          />
          {!loading && <ListPager page={pg.page} pageSize={pg.pageSize} total={pg.total} onPage={setPage} label="hosts" />}
        </>
      ) : (
        <HostsTable
          cardRef={tableRef}
          hosts={pg.rows}
          allIds={visible.map((h) => h.id)}
          ctx={ctx}
          hidden={hidden}
          sort={sort}
          onSort={onSort}
          selected={selected}
          focusedId={focusedId}
          canWrite={canWrite}
          actions={actions}
          onToggleSelect={toggleSelect}
          onSelectAll={(ids) => setSelected(new Set(ids))}
          onFocus={setFocusedId}
          loading={loading}
          footer={!loading && <Pagination page={pg.page} pageSize={pg.pageSize} total={pg.total} onPage={setPage} label="hosts" />}
        />
      )}

      {canWrite && selected.size > 0 && (
        <BulkBar ids={[...selected]} hosts={hosts} onClear={() => setSelected(new Set())} deleteOpen={bulkDeleteOpen} setDeleteOpen={setBulkDeleteOpen} />
      )}
      {actions.dialogs}
    </>
  )
}
