// Tunnels: gateways on public servers that publish hosts and TCP streams
// without port forwarding.
import { useRef, useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { TopBar } from '../../components/shell/TopBar'
import {
  Badge, Button, Card, ConfirmDialog, Dot, EmptyState, IconButton, ListPager, Menu, NoMatches, SearchInput, Select, Skeleton, StatCard, Status, TableToolbar,
  cx, matchesSearch, useFitGrid, usePagination, useToast,
} from '../../components/ui'
import { api } from '../../lib/api'
import { useEngine, useRole } from '../../lib/queries'
import { ago, bytes, pluralize } from '../../lib/format'
import type { Gateway } from '../../lib/types'
import ConnectGatewayWizard from './ConnectGatewayWizard'
import GatewayDrawer from './GatewayDrawer'
import { gatewayState, shortFingerprint, transportLabel, tunnelsDocs, useInvalidateTunnels, useTunnels, type GatewayView } from './api'
import './tunnels.css'

const STATES = [
  { value: 'ok', label: 'Connected' },
  { value: 'danger', label: 'Down' },
  { value: 'warn', label: 'Needs attention' },
  { value: 'muted', label: 'Idle or disabled' },
]

export default function TunnelsPage() {
  const [params, setParams] = useSearchParams()
  const { canWrite } = useRole()
  const toast = useToast()
  const q = useTunnels()
  const engine = useEngine('tunnel')
  const invalidate = useInvalidateTunnels()
  const [search, setSearch] = useState('')
  const [stateFilter, setStateFilter] = useState('')
  const [deleting, setDeleting] = useState<GatewayView | null>(null)
  const gridRef = useRef<HTMLDivElement>(null)
  const { pageSize } = useFitGrid(gridRef, { reserve: 20 + 28 + 24, min: 1, itemHeight: 190 })

  const connectOpen = params.get('connect') ?? ''
  const editId = params.get('edit') ?? ''
  const update = (mut: (p: URLSearchParams) => void) => {
    const next = new URLSearchParams(params)
    mut(next)
    setParams(next, { replace: true })
  }

  const gateways = q.data?.gateways ?? []
  const engineRunning = !!q.data?.engine
  const states = new Map(gateways.map((g) => [g.id, gatewayState(g, g.status, engineRunning)]))
  const filtered = gateways.filter(
    (g) => (!stateFilter || states.get(g.id)?.tone === stateFilter) && matchesSearch(search, g.name, g.address, ...g.published.hosts, ...g.published.streams),
  )
  const pg = usePagination(filtered, pageSize, [search, stateFilter])
  const editing = gateways.find((g) => g.id === editId)

  const connected = gateways.filter((g) => states.get(g.id)?.tone === 'ok').length
  const down = gateways.filter((g) => states.get(g.id)?.tone === 'danger').length
  const hostCount = gateways.reduce((n, g) => n + g.published.hosts.length, 0)
  const streamCount = gateways.reduce((n, g) => n + g.published.streams.length, 0)
  const totals = gateways.reduce((t, g) => ({ in: t.in + (g.status?.bytesIn ?? 0), out: t.out + (g.status?.bytesOut ?? 0), open: t.open + (g.status?.activeStreams ?? 0) }), { in: 0, out: 0, open: 0 })

  const setEnabled = async (g: GatewayView, enabled: boolean) => {
    try {
      const { status: _s, published: _p, ...entity } = g
      await api.put<Gateway>(`/api/gateways/${g.id}`, { ...entity, enabled })
      invalidate()
      toast.success(enabled ? 'Gateway enabled' : 'Gateway disabled', enabled ? `${g.name} connects again.` : `${g.name} is disconnected.`)
    } catch (err) {
      toast.error(err, 'Could not update gateway')
    }
  }

  const meta = engine ? (
    <Status tone={!engine.reachable ? 'muted' : engine.running ? 'ok' : 'muted'}>
      {!engine.reachable
        ? engine.container === 'missing'
          ? 'Tunnel engine · add the relay-tunnel service'
          : engine.container === 'stopped' || engine.standby
            ? 'Tunnel engine · standby'
            : 'Tunnel engine · agent unreachable'
        : engine.running
          ? `Tunnel engine · running${q.data?.engine?.startedAt ? ` · started ${ago(q.data.engine.startedAt)}` : ''}`
          : 'Tunnel engine · stopped'}
    </Status>
  ) : null

  return (
    <>
      <TopBar
        title="Tunnels"
        meta={meta}
        actions={
          <>
            <Link to={tunnelsDocs}><Button>How tunnels work</Button></Link>
            {canWrite && <Button variant="primary" icon="plus" onClick={() => update((p) => p.set('connect', 'new'))}>Connect gateway</Button>}
          </>
        }
      />
      <div className="page">
        {q.isLoading ? (
          <div className="grid-4">{[0, 1, 2, 3].map((i) => <Skeleton key={i} height={96} />)}</div>
        ) : gateways.length === 0 ? (
          <Card>
            <EmptyState
              icon="tunnel"
              title="No tunnel gateways"
              description="Publish hosts and TCP streams through a small gateway on a public server, without opening ports on your router. Relay dials out to it and HTTPS stays encrypted until it reaches this Relay."
              actions={canWrite ? <Button variant="primary" icon="plus" onClick={() => update((p) => p.set('connect', 'new'))}>Connect gateway</Button> : undefined}
            />
          </Card>
        ) : (
          <>
            <div className="grid-4">
              <StatCard label="Gateways" value={gateways.length} meta={down ? `${down} down` : `${connected} connected`} metaTone={down ? 'danger' : connected ? 'ok' : 'muted'} />
              <StatCard label="Published hosts" value={hostCount} meta={hostCount ? 'through tunnels' : 'none yet'} />
              <StatCard label="Published streams" value={streamCount} meta="TCP only" />
              <StatCard label="Traffic" value={bytes(totals.in + totals.out)} meta={engineRunning ? `${pluralize(totals.open, 'open connection')}` : 'engine not running'} />
            </div>

            <TableToolbar>
              <SearchInput value={search} onChange={setSearch} placeholder="Search gateways or published hosts" label="Search gateways" />
              <div className="tun-filter">
                <Select inputSize="sm" value={stateFilter} placeholder="All states" options={STATES} onChange={setStateFilter} aria-label="State" />
              </div>
            </TableToolbar>

            {filtered.length === 0 ? (
              <Card><NoMatches what="gateways" onClear={() => { setSearch(''); setStateFilter('') }} /></Card>
            ) : (
              <div className="tun-grid" ref={gridRef}>
                {pg.rows.map((g) => (
                  <GatewayCard
                    key={g.id}
                    view={g}
                    engineRunning={engineRunning}
                    canWrite={canWrite}
                    onEdit={() => update((p) => p.set('edit', g.id))}
                    onPair={() => update((p) => p.set('connect', g.id))}
                    onEnable={(v) => setEnabled(g, v)}
                    onDelete={() => setDeleting(g)}
                  />
                ))}
              </div>
            )}
            <ListPager page={pg.page} pageSize={pg.pageSize} total={pg.total} onPage={pg.setPage} label="gateways" />
          </>
        )}
      </div>

      {connectOpen && canWrite && (
        <ConnectGatewayWizard key={connectOpen} gatewayId={connectOpen === 'new' ? undefined : connectOpen} onClose={() => update((p) => p.delete('connect'))} />
      )}
      {editing && (
        <GatewayDrawer
          key={editing.id}
          view={editing}
          engineRunning={engineRunning}
          onClose={() => update((p) => p.delete('edit'))}
          onPair={(id) => update((p) => { p.delete('edit'); p.set('connect', id) })}
        />
      )}
      <ConfirmDialog
        open={!!deleting}
        onClose={() => setDeleting(null)}
        danger
        title={`Delete gateway ${deleting?.name ?? ''}?`}
        message={
          deleting && deleting.published.hosts.length + deleting.published.streams.length > 0
            ? `It still publishes ${[...deleting.published.hosts, ...deleting.published.streams].join(', ')}. Stop publishing them through this gateway and apply first.`
            : "Relay forgets the gateway and its key. The server keeps running until you stop it; run `relay gateway reset` there to pair it again."
        }
        confirmLabel="Delete gateway"
        typeToConfirm={deleting?.name}
        onConfirm={async () => {
          if (!deleting) return
          try {
            await api.del(`/api/gateways/${deleting.id}`)
            invalidate()
            toast.show({ kind: 'success', title: 'Gateway deleted', message: `${deleting.name} was removed.` })
          } catch (err) {
            toast.error(err, 'Could not delete gateway')
          }
        }}
      />
    </>
  )
}

function GatewayCard({ view: g, engineRunning, canWrite, onEdit, onPair, onEnable, onDelete }: {
  view: GatewayView
  engineRunning: boolean
  canWrite: boolean
  onEdit: () => void
  onPair: () => void
  onEnable: (enabled: boolean) => void
  onDelete: () => void
}) {
  const st = g.status
  const state = gatewayState(g, st, engineRunning)
  const connected = st?.state === 'connected'
  const desc = [
    g.address,
    connected ? (st?.transport ?? '').toUpperCase() : transportLabel[g.transport],
    connected && st?.rttMs ? `${Math.round(st.rttMs)} ms` : null,
    connected && st?.connectedAt ? `up ${ago(st.connectedAt).replace(' ago', '')}` : null,
  ].filter(Boolean).join(' · ')
  const published = g.published.hosts.length + g.published.streams.length

  return (
    <div className={cx('card tun-card', state.tone === 'danger' && 'alert')}>
      <div className="tun-card-head">
        <Dot tone={state.tone} large pulse={state.tone === 'warn' && g.pairState === 'paired'} />
        <div className="grow">
          <div className="tun-name clickable" onClick={onEdit}>{g.name}</div>
          <div className="tun-desc">{desc}</div>
        </div>
        <Badge tone={state.tone === 'ok' ? 'ok' : state.tone === 'danger' ? 'danger' : state.tone === 'warn' ? 'warn' : undefined}>{state.label}</Badge>
        <Menu
          trigger={<IconButton icon="more" bare label="Gateway actions" />}
          items={[
            { header: g.name },
            { label: canWrite ? 'Edit' : 'View', icon: 'edit', shortcut: 'E', onSelect: onEdit },
            ...(canWrite
              ? [
                  { label: g.pairState === 'paired' ? 'Pair again…' : 'Show pairing command', icon: 'token' as const, onSelect: onPair },
                  { label: g.enabled ? 'Disable' : 'Enable', icon: 'power' as const, onSelect: () => onEnable(!g.enabled) },
                  'separator' as const,
                  { label: 'Delete…', icon: 'trash' as const, danger: true, onSelect: onDelete },
                ]
              : []),
          ]}
        />
      </div>

      {state.detail && state.tone !== 'ok' && state.tone !== 'muted' && <div className="tun-note">{state.detail}</div>}

      <div className="tun-routes">
        {g.pairState !== 'paired' ? (
          <div className="tun-route idle">
            <Dot tone="warn" />
            <span className="grow">Start the gateway on the server with the pairing command</span>
            {canWrite && <Button size="sm" onClick={onPair}>Show command</Button>}
          </div>
        ) : published === 0 ? (
          <div className="small faint">Nothing published · choose <span className="medium">Publish through tunnel</span> in a host or TCP stream</div>
        ) : (
          <>
            {g.published.hosts.slice(0, 4).map((h) => (
              <div key={h} className="tun-route">
                <Dot tone={connected ? 'ok' : 'muted'} />
                <span className="mono truncate grow">{h}</span>
                <span className="faint">host</span>
              </div>
            ))}
            {g.published.streams.slice(0, 2).map((s) => (
              <div key={s} className="tun-route">
                <Dot tone={connected ? 'ok' : 'muted'} />
                <span className="mono truncate grow">{s}</span>
                <span className="faint">TCP stream</span>
              </div>
            ))}
            {published > 6 && <div className="small faint">+{published - Math.min(4, g.published.hosts.length) - Math.min(2, g.published.streams.length)} more</div>}
          </>
        )}
        {st?.portErrors?.map((pe) => (
          <div key={pe.port} className="tun-route down">
            <Dot tone="danger" />
            <span className="mono">:{pe.port}</span>
            <span className="grow truncate" title={pe.error}>{pe.error}</span>
          </div>
        ))}
      </div>

      <div className="tun-foot">
        <Badge>{g.version ? `gateway ${g.version}` : 'version unknown'}</Badge>
        <Badge title={g.gatewayPin}>key {shortFingerprint(g.gatewayPin)}</Badge>
        <span className="right">{st ? `${bytes(st.bytesIn)} in · ${bytes(st.bytesOut)} out` : '—'}</span>
      </div>
    </div>
  )
}
