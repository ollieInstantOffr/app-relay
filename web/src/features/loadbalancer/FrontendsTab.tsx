import { useRef, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import {
  Badge, Button, Callout, Card, ConfirmDialog, EmptyState, IconButton, ListPager, Menu, NoMatches, SearchInput, Select, Skeleton, TableToolbar, cx,
  matchesSearch, useElementHeight, useFitGrid, usePagination, useToast,
} from '../../components/ui'
import { useDeleteEntity, useEntities, useLBStats, useRole, useSaveEntity, useSettings } from '../../lib/queries'
import { compact } from '../../lib/format'
import type { Backend, Frontend, LBStats, ProxyHost } from '../../lib/types'
import { applyNowAction, bindKind, cloneFrontend, conditionText, nextFreePort, splitBind } from './lbApi'

export default function FrontendsTab({ onNew, onEdit, onDuplicate }: {
  onNew: () => void
  onEdit: (id: string) => void
  onDuplicate: (f: Frontend) => void
}) {
  const q = useEntities('frontends')
  const backends = useEntities('backends').data ?? []
  const hosts = useEntities('hosts').data ?? []
  const settings = useSettings('haproxy').data
  const stats = useLBStats(5_000).data
  const { canWrite } = useRole()
  const navigate = useNavigate()
  const save = useSaveEntity('frontends')
  const del = useDeleteEntity('frontends')
  const toast = useToast()
  const [deleting, setDeleting] = useState<Frontend | null>(null)
  const [search, setSearch] = useState('')
  const [mode, setMode] = useState('')
  const [status, setStatus] = useState('')
  const listRef = useRef<HTMLDivElement>(null)
  const calloutRef = useRef<HTMLDivElement>(null)
  const calloutHeight = useElementHeight(calloutRef, 64)
  // Below the list: gap (20) + ListPager (28) + gap (20) + info callout + page bottom padding (24).
  const { pageSize } = useFitGrid(listRef, { reserve: 20 + 28 + 20 + calloutHeight + 24, min: 1, itemHeight: 120 })

  const frontends = q.data ?? []
  const backendName = (id?: string) => backends.find((b) => b.id === id)?.name
  const filtered = frontends.filter(
    (f) =>
      (!mode || f.mode === mode) &&
      (!status || (status === 'enabled') === f.enabled) &&
      matchesSearch(
        search,
        f.name,
        f.bind,
        frontendHost(f, hosts)?.domains.join(' '),
        backendName(f.defaultBackendId),
        ...f.rules.map((r) => backendName(r.backendId)),
      ),
  )
  const pg = usePagination(filtered, pageSize, [search, mode, status])
  const clearFilters = () => {
    setSearch('')
    setMode('')
    setStatus('')
  }

  if (q.isLoading) return <Skeleton height={180} />

  const toggle = async (f: Frontend) => {
    try {
      await save.mutateAsync({ ...f, enabled: !f.enabled })
      toast.show({ kind: 'success', title: `Frontend ${f.enabled ? 'disabled' : 'enabled'}`, message: `${f.name} added to pending changes.`, actions: [applyNowAction] })
    } catch (err) {
      toast.error(err, 'Could not update frontend')
    }
  }

  return (
    <>
      <div className="muted" style={{ maxWidth: 820 }}>
        A frontend is a listening port plus rules that pick a backend. Frontends created by the Expose wizard bind to localhost and are reached only through the reverse proxy.
      </div>

      {frontends.length > 0 && (
        <TableToolbar>
          <SearchInput value={search} onChange={setSearch} placeholder="Search frontends" label="Search frontends" />
          <div className="lb-filter-select">
            <Select inputSize="sm" value={mode} placeholder="All modes" options={MODES} onChange={setMode} aria-label="Mode" />
          </div>
          <div className="lb-filter-select">
            <Select inputSize="sm" value={status} placeholder="All statuses" options={STATUSES} onChange={setStatus} aria-label="Status" />
          </div>
        </TableToolbar>
      )}

      {frontends.length === 0 ? (
        <Card>
          <EmptyState
            icon="load-balancer"
            title="No frontends yet"
            description={
              backends.length
                ? 'Backends receive traffic once a frontend listens for them. Expose an HTTP backend through the reverse proxy, or bind a TCP port directly.'
                : 'Create a backend first, then expose it or give it a listening port.'
            }
            actions={
              canWrite ? (
                <>
                  <Button variant="primary" icon="plus" onClick={onNew}>New frontend</Button>
                  {backends.some((b) => b.mode === 'http') && (
                    <Button icon="expose" onClick={() => navigate('/load-balancer/frontends?expose=new')}>Expose a backend online</Button>
                  )}
                </>
              ) : undefined
            }
          />
        </Card>
      ) : filtered.length === 0 ? (
        <Card>
          <NoMatches what="frontends" onClear={clearFilters} />
        </Card>
      ) : (
        <div className="col gap-14" ref={listRef}>
          {pg.rows.map((f) => (
            <FrontendCard
              key={f.id}
              frontend={f}
              backends={backends}
              hosts={hosts}
              stats={stats}
              canWrite={canWrite}
              onEdit={() => onEdit(f.id)}
              onToggle={() => toggle(f)}
              onDuplicate={() => onDuplicate(cloneFrontend(f, frontends.map((x) => x.name), nextFreePort(frontends, settings?.exposePortStart ?? 10080)))}
              onDelete={() => setDeleting(f)}
            />
          ))}
        </div>
      )}
      <ListPager page={pg.page} pageSize={pg.pageSize} total={pg.total} onPage={pg.setPage} label="frontends" />

      <div ref={calloutRef}>
        <Callout tone="info" icon="info">
          The reverse proxy owns public TLS and domains. The load balancer owns pools and health. Public HTTP traffic always enters through the reverse proxy; only raw TCP/UDP frontends bind to a LAN or public IP directly.
        </Callout>
      </div>

      <ConfirmDialog
        open={!!deleting}
        onClose={() => setDeleting(null)}
        danger
        title={`Delete frontend ${deleting?.name ?? ''}?`}
        message={
          deleting?.hostId && hosts.some((h) => h.id === deleting.hostId)
            ? `Proxy host ${hosts.find((h) => h.id === deleting.hostId)?.domains[0]} sends its traffic here. Delete or repoint that host first.`
            : `Port ${splitBind(deleting?.bind ?? '').port ?? ''} stops listening on the next apply.`
        }
        confirmLabel="Delete frontend"
        onConfirm={async () => {
          if (!deleting) return
          try {
            await del.mutateAsync(deleting.id)
            toast.show({ kind: 'success', title: 'Frontend deleted', message: `${deleting.name} is removed on the next apply.` })
          } catch (err) {
            toast.error(err, 'Could not delete frontend')
          }
        }}
      />
    </>
  )
}

const MODES = [
  { value: 'http', label: 'HTTP' },
  { value: 'tcp', label: 'TCP' },
]

const STATUSES = [
  { value: 'enabled', label: 'Enabled' },
  { value: 'disabled', label: 'Disabled' },
]

/** Proxy host that sends traffic to a frontend (linked, or a loopback upstream on its port). */
function frontendHost(f: Frontend, hosts: ProxyHost[]): ProxyHost | undefined {
  const { port } = splitBind(f.bind)
  return hosts.find((h) => h.id === f.hostId) ?? hosts.find((h) => h.upstream.port === port && /^127\.|^localhost$/.test(h.upstream.host))
}

function FrontendCard({ frontend: f, backends, hosts, stats, canWrite, onEdit, onToggle, onDuplicate, onDelete }: {
  frontend: Frontend
  backends: Backend[]
  hosts: ProxyHost[]
  stats?: LBStats
  canWrite: boolean
  onEdit: () => void
  onToggle: () => void
  onDuplicate: () => void
  onDelete: () => void
}) {
  const { addr } = splitBind(f.bind)
  const kind = bindKind(addr)
  const host = frontendHost(f, hosts)
  const fs = stats?.running ? stats.frontends.find((x) => x.id === f.id) : undefined
  const name = (id?: string) => backends.find((b) => b.id === id)?.name
  const sni = f.rules.some((r) => r.conditions.some((c) => c.type === 'sni'))
  const tcp = f.mode === 'tcp'

  const bindBadge =
    kind === 'loopback' ? (
      <Badge tone="info">{host ? 'via reverse proxy' : 'localhost only'}</Badge>
    ) : kind === 'lan' ? (
      <Badge>LAN bind · not proxied</Badge>
    ) : kind === 'public' ? (
      <Badge tone="warn">public bind</Badge>
    ) : null

  return (
    <div className={cx('card', !f.enabled && 'dim')}>
      <div className="card-header">
        <span className="mono" style={{ fontSize: 14 }}>{f.name}</span>
        <span className="sub mono">bind {f.bind} · {tcp ? 'TCP' : 'HTTP'}</span>
        {bindBadge}
        {host && <Badge>{host.domains[0]}</Badge>}
        {sni && <Badge>SNI routing</Badge>}
        {f.acceptProxy && <Badge>PROXY protocol</Badge>}
        {f.compression && <Badge>gzip</Badge>}
        {!f.enabled && <Badge>disabled</Badge>}
        <div className="spacer" />
        <span className="mono small faint">{fs ? (tcp ? `${compact(fs.current)} conn` : `${compact(fs.sessRate)} sess/s`) : '—'}</span>
        <Menu
          trigger={<IconButton icon="more" bare label="Frontend actions" />}
          items={[
            { header: f.name },
            { label: canWrite ? 'Edit' : 'View', icon: 'edit', onSelect: onEdit },
            ...(canWrite
              ? [
                  { label: f.enabled ? 'Disable' : 'Enable', icon: 'power' as const, onSelect: onToggle },
                  { label: 'Duplicate', icon: 'copy' as const, onSelect: onDuplicate },
                  'separator' as const,
                  { label: 'Delete…', icon: 'trash' as const, danger: true, onSelect: onDelete },
                ]
              : []),
          ]}
        />
      </div>
      {f.rules.length === 0 ? (
        <div className="card-row mono small">
          {tcp ? 'TCP' : 'HTTP'} → {name(f.defaultBackendId) ?? <span className="danger-text">no backend</span>}
          {host && <span className="faint">· {host.certificateId ? 'https' : 'http'}://{host.domains[0]}</span>}
        </div>
      ) : (
        <table className="lb-fe-rules">
          <thead>
            <tr>
              <th className="idx">#</th>
              <th>If</th>
              <th>Then use backend</th>
              <th style={{ width: 60 }} />
            </tr>
          </thead>
          <tbody>
            {f.rules.map((r, i) => (
              <tr key={r.id}>
                <td className="idx">{i + 1}</td>
                <td>{r.conditions.map(conditionText).join(' and ')}</td>
                <td>{name(r.backendId) ?? <span className="danger-text">missing backend</span>}</td>
                <td>{canWrite && <button type="button" className="lb-link" onClick={onEdit}>Edit</button>}</td>
              </tr>
            ))}
            <tr>
              <td className="idx">{f.rules.length + 1}</td>
              <td className="faint">otherwise</td>
              <td>{name(f.defaultBackendId) ?? <span className="faint">— (503)</span>}</td>
              <td />
            </tr>
          </tbody>
        </table>
      )}
      {canWrite && !tcp && (
        <div className="card-row" style={{ borderTop: '1px solid var(--hairline-soft)' }}>
          <button type="button" className="lb-link" onClick={onEdit}>+ Add rule</button>
        </div>
      )}
    </div>
  )
}
