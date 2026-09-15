// Streams (design 06).
import { useMemo, useState } from 'react'
import { useNavigate, useSearchParams } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { TopBar } from '../../components/shell/TopBar'
import { Button, ConfirmDialog, Dot, EmptyState, IconButton, Menu, Skeleton, Tooltip, useToast } from '../../components/ui'
import { api } from '../../lib/api'
import { useEntities, useHealth, useRole, useSaveEntity, useDeleteEntity } from '../../lib/queries'
import { rate } from '../../lib/format'
import type { Stream, StreamMetrics } from '../../lib/types'
import { pendingToast } from '../certificates/common'
import StreamDrawer from './StreamDrawer'
import '../certificates/certs.css'

export interface PortEntry {
  port: number
  proto: 'tcp' | 'udp'
  address: string
  owner: 'nginx' | 'edge' | 'haproxy' | 'relay' | 'other'
  kind: 'http' | 'https' | 'stream' | 'frontend' | 'admin' | 'stats' | ''
  name: string
  id?: string
  enabled: boolean
  listening: boolean | null
}

const protoLabel = { tcp: 'TCP', udp: 'UDP', both: 'TCP+UDP' } as const

export default function StreamsPage() {
  const toast = useToast()
  const { canWrite } = useRole()
  const [params, setParams] = useSearchParams()
  const navigate = useNavigate()
  const streamsQ = useEntities('streams')
  const streams = streamsQ.data ?? []
  const backends = useEntities('backends').data ?? []
  const health = useHealth().data
  const save = useSaveEntity('streams')
  const del = useDeleteEntity('streams')
  const metrics = useQuery({ queryKey: ['metrics', 'streams'], queryFn: () => api.get<StreamMetrics>('/api/metrics/streams'), refetchInterval: 5_000, retry: false })
  const ports = useQuery({ queryKey: ['ports'], queryFn: () => api.get<PortEntry[]>('/api/ports'), refetchInterval: 15_000 })
  const [deleting, setDeleting] = useState<Stream | undefined>()

  const editId = params.get('edit')
  const drawerOpen = params.get('new') === '1' || !!editId
  const editing = streams.find((s) => s.id === editId)
  const setParam = (key: string, value?: string) => {
    const next = new URLSearchParams(params)
    next.delete('new')
    next.delete('edit')
    if (value) next.set(key, value)
    setParams(next, { replace: true })
  }

  const portChips = useMemo(() => {
    const byPort = new Map<number, { port: number; used: boolean; names: string[] }>()
    for (const p of ports.data ?? []) {
      if (p.owner === 'other') continue
      const chip = byPort.get(p.port) ?? { port: p.port, used: false, names: [] }
      chip.used ||= p.enabled && p.listening !== false
      const label = `${p.name} · ${p.proto}${p.enabled ? '' : ' (disabled)'}${p.listening === false && p.enabled ? ' (not bound yet)' : ''}`
      if (!chip.names.includes(label)) chip.names.push(label)
      byPort.set(p.port, chip)
    }
    return [...byPort.values()].sort((a, b) => a.port - b.port)
  }, [ports.data])
  const otherPorts = (ports.data ?? []).filter((p) => p.owner === 'other')

  const toggle = async (s: Stream) => {
    try {
      const saved = await save.mutateAsync({ ...s, enabled: !s.enabled })
      pendingToast(toast, saved.enabled ? 'Stream enabled' : 'Stream disabled', saved.name)
    } catch (err) {
      toast.error(err, 'Could not update stream')
    }
  }

  return (
    <>
      <TopBar
        title="Streams"
        count={streamsQ.data ? streams.length : undefined}
        actions={canWrite && <Button variant="primary" icon="plus" onClick={() => setParam('new', '1')}>New stream</Button>}
      />
      <div className="page" style={{ gap: 16 }}>
        <div className="muted" style={{ maxWidth: 640, lineHeight: 1.5 }}>
          Raw port forwarding at layer 4 — for anything that isn't HTTP: game servers, databases, SSH, DNS. No TLS termination, no path routing.
        </div>

        <div className="card" style={{ overflow: 'hidden' }}>
          <div className="cs-grid-head cs-streams">
            <span>Listen</span><span>Name</span><span>Proto</span><span>Forward to</span><span>Connections</span><span>Throughput</span><span />
          </div>
          {streamsQ.isLoading && <div style={{ padding: 20 }}><Skeleton height={40} /></div>}
          {streamsQ.data && streams.length === 0 && (
            <EmptyState
              icon="streams"
              title="No streams yet"
              description="Forward a TCP or UDP port — Minecraft, WireGuard, Postgres, SSH — to a machine or container on your network."
              actions={canWrite && <Button variant="primary" icon="plus" onClick={() => setParam('new', '1')}>New stream</Button>}
            />
          )}
          {streams.map((s) => {
            const m = metrics.data?.[s.id]
            const h = health?.[`stream:${s.id}`]
            const backend = backends.find((b) => b.id === s.backendId)
            const specific = s.listenAddress && s.listenAddress !== '0.0.0.0' && s.listenAddress !== '::'
            const forward = s.backendId ? `lb → ${backend?.name ?? 'backend'}` : `${s.forwardHost}:${s.forwardPorts || s.listenPorts}`
            const tone = !s.enabled ? undefined : h?.status === 'healthy' ? 'ok' : h?.status === 'down' ? 'danger' : h?.status === 'degraded' ? 'warn' : 'muted'
            return (
              <div key={s.id} className={`cs-grid-row cs-streams clickable${s.enabled ? '' : ' dim'}`} onClick={() => setParam('edit', s.id)}>
                <span className="col gap-2" style={{ minWidth: 0 }}>
                  <span className="cs-listen truncate">:{s.listenPorts}</span>
                  {specific && <span className="micro faint mono">{s.listenAddress}</span>}
                </span>
                <span className="row gap-10" style={{ minWidth: 0 }}>
                  <Tooltip content={s.enabled ? h?.detail || h?.status || 'No health data yet' : 'Disabled'}>
                    {tone ? <Dot tone={tone} /> : <span className="dot" style={{ background: 'transparent', border: '1.5px solid var(--ink-ghost)' }} />}
                  </Tooltip>
                  <span className="truncate">{s.name}{s.enabled ? '' : ' (disabled)'}</span>
                </span>
                <span className="cs-proto">{protoLabel[s.protocol]}</span>
                <span className="mono truncate" style={{ color: 'var(--ink-muted)' }}>{forward}</span>
                <span className="mono">{!s.enabled || !m ? '—' : m.active === 0 ? '0' : `${m.active} ${s.protocol === 'udp' ? 'peers' : 'active'}`}</span>
                <span className="mono" style={{ color: 'var(--ink-muted)' }}>{!s.enabled || !m ? '—' : rate(m.bytesPerSec)}</span>
                <span onClick={(e) => e.stopPropagation()}>
                  <Menu
                    trigger={<IconButton icon="more" bare label="Stream actions" />}
                    items={[
                      { header: s.name },
                      { label: 'Edit', icon: 'edit', shortcut: 'E', onSelect: () => setParam('edit', s.id) },
                      { label: s.enabled ? 'Disable' : 'Enable', icon: 'power', disabled: !canWrite, onSelect: () => toggle(s) },
                      { label: 'View logs', icon: 'logs', onSelect: () => navigate(`/logs/access?q=${encodeURIComponent('stream:' + s.id)}`) },
                      'separator',
                      { label: 'Delete…', icon: 'trash', danger: true, disabled: !canWrite, onSelect: () => setDeleting(s) },
                    ]}
                  />
                </span>
              </div>
            )
          })}
        </div>

        <div className="grid-2" style={{ gap: 14 }}>
          <div className="card" style={{ padding: '18px 20px', display: 'flex', flexDirection: 'column', gap: 12 }}>
            <div className="card-title">Port usage</div>
            <div className="small muted">Ports bound by Relay on this machine. Grey = free, black = in use.</div>
            {ports.isLoading && <Skeleton height={24} />}
            <div className="row gap-4 wrap">
              {portChips.map((c) => (
                <Tooltip key={c.port} content={c.names.join(' · ')}>
                  <span className={`cs-port${c.used ? ' used' : ''}`}>{c.port}</span>
                </Tooltip>
              ))}
            </div>
            {otherPorts.length > 0 && (
              <div className="col gap-6">
                <div className="micro muted">Other listeners on this machine</div>
                <div className="row gap-4 wrap">
                  {[...new Map(otherPorts.map((p) => [`${p.port}/${p.proto}`, p])).values()].sort((a, b) => a.port - b.port).map((p) => (
                    <Tooltip key={`${p.port}/${p.proto}`} content={`${p.name || 'unknown process'} · ${p.address}:${p.port}/${p.proto}`}>
                      <span className="cs-port other">{p.port}{p.proto === 'udp' ? '/u' : ''}</span>
                    </Tooltip>
                  ))}
                </div>
              </div>
            )}
          </div>
          <div className="card" style={{ padding: '18px 20px', display: 'flex', flexDirection: 'column', gap: 12 }}>
            <div className="card-title">Tip</div>
            <div style={{ lineHeight: 1.5, color: 'var(--ink-muted)' }}>
              Streams bypass access lists and TLS. If you need to expose a database, prefer a VPN stream (WireGuard) and keep the database stream LAN-bound.
            </div>
          </div>
        </div>
      </div>

      <StreamDrawer open={drawerOpen} stream={editing} ports={ports.data} onClose={() => setParam('edit')} />
      <ConfirmDialog
        open={!!deleting}
        onClose={() => setDeleting(undefined)}
        danger
        title={`Delete ${deleting?.name}?`}
        message={`Port :${deleting?.listenPorts} stops forwarding on next apply.`}
        confirmLabel="Delete stream"
        onConfirm={async () => {
          if (!deleting) return
          try {
            await del.mutateAsync(deleting.id)
            pendingToast(toast, 'Stream deleted', deleting.name)
          } catch (err) {
            toast.error(err, 'Could not delete stream')
            throw err
          }
        }}
      />
    </>
  )
}
