import { useState } from 'react'
import { Badge, Button, Card, ConfirmDialog, Dot, EmptyState, IconButton, Menu, Skeleton, StatCard, cx, useToast, type MenuEntry } from '../../components/ui'
import { useDeleteEntity, useEntities, useLBStats, useRole, useSettings } from '../../lib/queries'
import { compact, pluralize } from '../../lib/format'
import type { Backend, BackendStats, Frontend, LBStats, ProxyHost, Server, ServerStats } from '../../lib/types'
import { algorithmShort, backendDependents, backendRoutes, cloneBackend, findServerStats, healthSummary } from './lbApi'
import { useServerMenu } from './serverActions'
import ConvertHostDialog from './ConvertHostDialog'

export default function BackendsTab({ onNew, onEdit, onExpose, onDuplicate }: {
  onNew: () => void
  onEdit: (id: string) => void
  onExpose: (id: string) => void
  onDuplicate: (b: Backend) => void
}) {
  const backendsQ = useEntities('backends')
  const frontends = useEntities('frontends').data ?? []
  const hosts = useEntities('hosts').data ?? []
  const streams = useEntities('streams').data ?? []
  const settings = useSettings('haproxy').data
  const stats = useLBStats(3_000).data
  const { canWrite } = useRole()
  const [convert, setConvert] = useState(false)
  const [deleting, setDeleting] = useState<Backend | null>(null)
  const del = useDeleteEntity('backends')
  const toast = useToast()

  if (backendsQ.isLoading) {
    return (
      <div className="grid-4">
        {[0, 1, 2, 3].map((i) => <Skeleton key={i} height={96} />)}
      </div>
    )
  }
  const backends = backendsQ.data ?? []

  if (backends.length === 0) {
    return (
      <>
        <Card>
          <EmptyState
            icon="load-balancer"
            title="No load-balancer backends"
            description="Group two or more servers into a pool with health checks. HAProxy starts only once you create the first backend."
            actions={
              canWrite ? (
                <>
                  <Button variant="primary" icon="plus" onClick={onNew}>New backend</Button>
                  <Button onClick={() => setConvert(true)}>Convert a host's upstream into a pool</Button>
                </>
              ) : undefined
            }
          />
        </Card>
        <ConvertHostDialog open={convert} onClose={() => setConvert(false)} onConverted={(id) => onEdit(id)} />
      </>
    )
  }

  const running = !!stats?.running
  const byId = new Map((stats?.backends ?? []).map((b) => [b.id, b]))
  const counts = summarize(backends, stats)

  return (
    <>
      <div className="grid-4">
        <StatCard
          label="Backends"
          value={backends.length}
          meta={!running ? 'haproxy not running' : counts.down ? `${counts.down} down` : counts.degraded ? `${counts.degraded} degraded` : 'all up'}
          metaTone={!running ? 'muted' : counts.down ? 'danger' : counts.degraded ? 'warn' : 'ok'}
        />
        <StatCard
          label="Servers"
          value={counts.servers}
          meta={!running ? '—' : counts.serverParts.length ? counts.serverParts.join(' · ') : 'all healthy'}
          metaTone={!running ? 'muted' : counts.serversDown ? 'warn' : counts.serverParts.length ? 'warn' : 'ok'}
        />
        <StatCard label="Sessions / s" value={running ? compact(stats!.sessRate) : '—'} meta={running ? `peak ${compact(stats!.peakSessRate)}` : undefined} />
        <StatCard
          label="Queue"
          value={running ? compact(stats!.queue) : '—'}
          meta={running ? (stats!.queue ? `${pluralize(stats!.queue, 'waiting request')}` : 'no waiting requests') : undefined}
          metaTone={running && stats!.queue ? 'warn' : undefined}
        />
      </div>

      <div className="lb-grid">
        {backends.map((b) => (
          <BackendCard
            key={b.id}
            backend={b}
            stats={byId.get(b.id)}
            running={running}
            frontends={frontends}
            hosts={hosts}
            globalInterval={settings?.checkInterval}
            canWrite={canWrite}
            onEdit={() => onEdit(b.id)}
            onExpose={() => onExpose(b.id)}
            onDuplicate={() => onDuplicate(cloneBackend(b, backends.map((x) => x.name)))}
            onDelete={() => setDeleting(b)}
          />
        ))}
      </div>

      <ConfirmDialog
        open={!!deleting}
        onClose={() => setDeleting(null)}
        danger
        title={`Delete backend ${deleting?.name ?? ''}?`}
        message={
          deleting && backendDependents(deleting, frontends, hosts, streams).length
            ? `It is still used by ${backendDependents(deleting, frontends, hosts, streams).join(', ')}. Remove those first.`
            : 'The backend and its servers are removed from haproxy.cfg on the next apply.'
        }
        confirmLabel="Delete backend"
        typeToConfirm={deleting?.name}
        onConfirm={async () => {
          if (!deleting) return
          try {
            await del.mutateAsync(deleting.id)
            toast.show({ kind: 'success', title: 'Backend deleted', message: `${deleting.name} is removed on the next apply.` })
          } catch (err) {
            toast.error(err, 'Could not delete backend')
          }
        }}
      />
    </>
  )
}

function summarize(backends: Backend[], stats?: LBStats) {
  let servers = 0
  for (const b of backends) servers += b.servers.length
  let down = 0, degraded = 0, sDown = 0, sDrain = 0, sMaint = 0
  for (const bs of stats?.backends ?? []) {
    if (bs.status === 'DOWN') down++
    if (bs.status === 'DEGRADED') degraded++
    for (const s of bs.servers) {
      if (s.status === 'DOWN') sDown++
      if (s.status === 'DRAIN') sDrain++
      if (s.status === 'MAINT') sMaint++
    }
  }
  const serverParts = [sDown && `${sDown} down`, sDrain && `${sDrain} drain`, sMaint && `${sMaint} maint`].filter(Boolean) as string[]
  return { servers, down, degraded, serversDown: sDown, serverParts }
}

function BackendCard({ backend: b, stats, running, frontends, hosts, globalInterval, canWrite, onEdit, onExpose, onDuplicate, onDelete }: {
  backend: Backend
  stats?: BackendStats
  running: boolean
  frontends: Frontend[]
  hosts: ProxyHost[]
  globalInterval?: string
  canWrite: boolean
  onEdit: () => void
  onExpose: () => void
  onDuplicate: () => void
  onDelete: () => void
}) {
  const serverMenu = useServerMenu(b)
  const active = b.servers.filter((s) => s.role !== 'backup').length
  const backups = b.servers.length - active
  const status = stats?.status
  const tone = !running || !stats ? 'muted' : status === 'DOWN' ? 'danger' : status === 'DEGRADED' ? 'warn' : 'ok'
  const desc = [
    b.mode === 'tcp' ? 'TCP' : 'HTTP',
    algorithmShort(b.algorithm),
    b.sticky.enabled ? (b.sticky.mode === 'source' || b.mode === 'tcp' ? 'sticky source IP' : 'sticky cookie') : null,
    backups ? `${pluralize(active, 'server')} + ${backups} backup` : pluralize(active, 'server'),
    status === 'DEGRADED' ? 'degraded' : status === 'DOWN' ? 'down' : null,
    running && !stats ? 'not applied yet' : null,
  ].filter(Boolean).join(' · ')
  const routes = backendRoutes(b, frontends, hosts)
  const health = healthSummary(b, globalInterval)
  const isTCP = b.mode === 'tcp'

  let secondChip: string | null = null
  if (b.tlsReencrypt) secondChip = `TLS re-encrypt ${b.tlsVerify ? 'verified' : 'on'}`
  else if (b.retries > 0 && b.retries !== 3) secondChip = `retries ${b.retries}`
  else if (!isTCP) secondChip = 'TLS re-encrypt off'
  if (b.sendProxy) secondChip = (secondChip ? secondChip + ' · ' : '') + 'PROXY v2'

  return (
    <div className={cx('card lb-card', running && stats && (status === 'DOWN' || status === 'DEGRADED') && 'alert')}>
      <div className="lb-card-head">
        <Dot tone={tone} large />
        <div className="grow">
          <div className="lb-name clickable" onClick={onEdit}>{b.name}</div>
          <div className="lb-desc">{desc}</div>
        </div>
        {routes.slice(0, 2).map((r) => <Badge key={r}>{r}</Badge>)}
        {routes.length > 2 && <Badge>+{routes.length - 2}</Badge>}
        <Menu
          trigger={<IconButton icon="more" bare label="Backend actions" />}
          items={[
            { header: b.name },
            { label: canWrite ? 'Edit' : 'View', icon: 'edit', shortcut: 'E', onSelect: onEdit },
            ...(canWrite
              ? [
                  { label: 'Expose online', icon: 'expose' as const, disabled: isTCP, onSelect: onExpose },
                  { label: 'Duplicate', icon: 'copy' as const, onSelect: onDuplicate },
                  'separator' as const,
                  { label: 'Delete…', icon: 'trash' as const, danger: true, onSelect: onDelete },
                ]
              : []),
          ]}
        />
      </div>

      <div className="lb-servers">
        {b.servers.length === 0 && <div className="small faint">No servers yet · add one in the drawer</div>}
        {b.servers.map((srv) => (
          <ServerRow
            key={srv.id}
            server={srv}
            stats={findServerStats(stats, srv)}
            running={running && !!stats}
            tcp={isTCP}
            menu={canWrite ? serverMenu.items(srv, findServerStats(stats, srv)) : undefined}
          />
        ))}
      </div>

      <div className="lb-foot">
        <Badge tone={health.enabled ? 'ok' : undefined}>{health.label}</Badge>
        {secondChip && <Badge>{secondChip}</Badge>}
        <span className="right">
          {!running || !stats ? '—' : isTCP ? `${compact(stats.current)} conn` : `${compact(stats.sessRate)} sess/s`}
        </span>
      </div>
      {serverMenu.dialogs}
    </div>
  )
}

function ServerRow({ server: s, stats: st, running, tcp, menu }: {
  server: Server
  stats?: ServerStats
  running: boolean
  tcp: boolean
  menu?: MenuEntry[]
}) {
  const addr = `${s.address}:${s.port}`
  const menuBtn = menu ? <Menu trigger={<IconButton icon="more" bare size={14} label={`Server ${addr} actions`} />} items={menu} /> : <span />

  if (!running || !st) {
    const note = s.state === 'maint' ? 'maintenance · on apply' : s.state === 'drain' ? 'drain · on apply' : s.role === 'backup' ? 'backup' : running ? 'pending apply' : null
    return (
      <div className="lb-srv idle">
        <Dot tone="muted" />
        <span className="truncate" title={s.name}>{addr}</span>
        <span>w {s.weight}</span>
        <span className="span2">{note ?? '—'}</span>
        {menuBtn}
      </div>
    )
  }

  if (st.status === 'DOWN') {
    return (
      <div className="lb-srv down">
        <Dot tone="danger" />
        <span className="truncate" title={s.name}>{addr}</span>
        <span>DOWN</span>
        <span className="span2" title={st.checkDetail}>{st.checkDetail || 'health check failing'}</span>
        {menuBtn}
      </div>
    )
  }
  if (st.status === 'DRAIN') {
    return (
      <div className="lb-srv drain">
        <Dot tone="warn" />
        <span className="truncate" title={s.name}>{addr}</span>
        <span>drain</span>
        <span className="span2">{st.current > 0 ? `maintenance · finishing ${pluralize(st.current, 'session')}` : 'maintenance · no active sessions'}</span>
        {menuBtn}
      </div>
    )
  }
  if (st.status === 'MAINT') {
    return (
      <div className="lb-srv idle">
        <Dot tone="muted" />
        <span className="truncate" title={s.name}>{addr}</span>
        <span>maint</span>
        <span className="span2">{st.checkDetail || 'maintenance'}</span>
        {menuBtn}
      </div>
    )
  }
  if (s.role === 'backup' && st.sessRate === 0 && st.current === 0) {
    return (
      <div className="lb-srv idle">
        <span className="dot hollow" />
        <span className="truncate" title={s.name}>{addr}</span>
        <span>backup</span>
        <span className="span2">standby · not receiving traffic</span>
        {menuBtn}
      </div>
    )
  }
  return (
    <div className="lb-srv">
      <Dot tone={st.status === 'NOLB' ? 'warn' : 'ok'} />
      <span className="truncate" title={s.name}>{addr}</span>
      <span className="faint">w {st.weight || s.weight}</span>
      <span className="lb-share">
        <span className="lb-share-bar"><span style={{ width: `${st.sharePct}%` }} /></span>
        {st.sharePct}%
      </span>
      <span className="faint">{tcp ? `${compact(st.current)} conn` : st.respAvgMs ? `${st.respAvgMs} ms` : '—'}</span>
      {menuBtn}
    </div>
  )
}
