import { useEffect, useRef, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import {
  Badge, Button, Callout, Checkbox, CodeBlock, Dot, Drawer, Field, Icon, IconButton, Input, RadioCard, Segmented, Select,
  ToggleCard, cx, useToast,
} from '../../components/ui'
import { useContainers, useEntities, useLBStats, useRole, useSaveEntity, useSettings } from '../../lib/queries'
import type { Backend, HealthCheck, Server } from '../../lib/types'
import {
  ALGORITHMS, HEALTH_TYPES, applyNowAction, fieldErrors, findServerStats, frontendUsesBackend, newServer, usePreview, useServerActions,
} from './lbApi'
import { PreviewBlock, PreviewStatus } from './preview'

type Tab = 'servers' | 'balancing' | 'health' | 'frontend' | 'timeouts' | 'config'

const TABS: { id: Tab; label: string }[] = [
  { id: 'servers', label: 'Servers' },
  { id: 'balancing', label: 'Balancing' },
  { id: 'health', label: 'Health checks' },
  { id: 'frontend', label: 'Frontend' },
  { id: 'timeouts', label: 'Timeouts' },
  { id: 'config', label: 'Config' },
]

function tabForField(k: string, isNew: boolean): Tab {
  if (k.startsWith('healthCheck')) return 'health'
  if (k.startsWith('timeouts')) return 'timeouts'
  if (k.startsWith('sticky') || k === 'retries' || k.startsWith('tls')) return 'balancing'
  if (k === 'name') return isNew ? 'servers' : 'balancing'
  return 'servers'
}

export default function BackendDrawer({ initial, onClose, onExpose }: { initial: Backend; onClose: () => void; onExpose: (id: string) => void }) {
  const [draft, setDraft] = useState<Backend>(() => JSON.parse(JSON.stringify(initial)))
  const [tab, setTab] = useState<Tab>('servers')
  const [errors, setErrors] = useState<Record<string, string>>({})
  const { canWrite } = useRole()
  const readOnly = !canWrite
  const isNew = !initial.id
  const navigate = useNavigate()
  const settings = useSettings('haproxy').data
  const stats = useLBStats(5_000).data
  const frontends = useEntities('frontends').data ?? []
  const hosts = useEntities('hosts').data ?? []
  const containers = useContainers(canWrite)
  const save = useSaveEntity('backends')
  const actions = useServerActions()
  const toast = useToast()
  const preview = usePreview('/api/preview/haproxy/backend', readOnly ? null : { backend: draft })

  // drag to reorder
  const [armed, setArmed] = useState<number | null>(null)
  const [dragIndex, setDragIndex] = useState<number | null>(null)
  const [overIndex, setOverIndex] = useState<number | null>(null)

  const bs = stats?.running ? stats.backends.find((x) => x.id === draft.id) : undefined
  const hc = draft.healthCheck
  const http = draft.mode === 'http'
  const set = (patch: Partial<Backend>) => setDraft((d) => ({ ...d, ...patch }))
  const setHC = (patch: Partial<HealthCheck>) => setDraft((d) => ({ ...d, healthCheck: { ...d.healthCheck, ...patch } }))
  const setServer = (i: number, patch: Partial<Server>) =>
    setDraft((d) => ({ ...d, servers: d.servers.map((s, j) => (j === i ? { ...s, ...patch } : s)) }))
  const err = (k: string) => errors[k]

  const setMode = (mode: Backend['mode']) =>
    setDraft((d) => ({
      ...d,
      mode,
      algorithm: mode === 'tcp' && d.algorithm === 'uri' ? 'roundrobin' : d.algorithm,
      sticky: { ...d.sticky, mode: mode === 'tcp' ? 'source' : d.sticky.mode === 'source' ? 'insert' : d.sticky.mode },
      healthCheck: mode === 'tcp' && d.healthCheck.type === 'http' && isNew ? { ...d.healthCheck, type: 'tcp' } : d.healthCheck,
    }))

  const move = (from: number, to: number) =>
    setDraft((d) => {
      const servers = [...d.servers]
      const [s] = servers.splice(from, 1)
      servers.splice(to, 0, s)
      return { ...d, servers }
    })
  const endDrag = () => {
    setArmed(null)
    setDragIndex(null)
    setOverIndex(null)
  }

  const addServer = (address = '', port?: number) =>
    setDraft((d) => {
      // Picking a discovered container fills the first blank row instead of appending.
      const blank = address ? d.servers.findIndex((s) => !s.address.trim()) : -1
      if (blank >= 0) {
        const servers = [...d.servers]
        servers[blank] = { ...servers[blank], address, port: port ?? servers[blank].port }
        return { ...d, servers }
      }
      return { ...d, servers: [...d.servers, newServer(address, port ?? d.servers[d.servers.length - 1]?.port ?? 0)] }
    })

  const onSave = async () => {
    try {
      const saved = await save.mutateAsync(draft)
      toast.show({ kind: 'success', title: 'Backend saved', message: `${saved.name} added to pending changes.`, actions: [applyNowAction] })
      onClose()
    } catch (e) {
      const f = fieldErrors(e)
      const keys = Object.keys(f)
      if (keys.length) {
        setErrors(f)
        setTab(tabForField(keys[0], isNew))
      } else {
        toast.error(e, 'Could not save backend')
      }
    }
  }
  const saveRef = useRef(onSave)
  saveRef.current = onSave
  useEffect(() => {
    const h = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 's') {
        e.preventDefault()
        if (!readOnly) saveRef.current()
      }
    }
    window.addEventListener('keydown', h)
    return () => window.removeEventListener('keydown', h)
  }, [readOnly])

  // discovered containers not yet in this pool
  const multiHost = new Set((containers.data ?? []).map((c) => c.endpointId)).size > 1
  const chips = (containers.data ?? [])
    .filter((c) => c.state === 'running' && !!c.upstreamHost && (!c.backendId || c.backendId === draft.id))
    .map((c) => {
      const port = c.suggestedPort || c.candidatePorts?.[0] || 0
      return { c, address: c.upstreamHost ?? '', port, label: `${multiHost && c.endpointName ? `${c.endpointName} · ` : ''}${c.name}:${port}` }
    })
    .filter(({ address, port }) => port && !draft.servers.some((s) => s.address === address && s.port === port))
    // web-looking containers first in HTTP mode, but any container with a port is offered
    .sort((a, b) => Number(!!b.c.backendId) - Number(!!a.c.backendId) || (http ? Number(b.c.http) - Number(a.c.http) : 0))
    .slice(0, 5)

  // failing server callout
  const failing = draft.servers.map((s) => ({ s, st: findServerStats(bs, s) })).find((x) => x.st?.status === 'DOWN')
  const upOthers = draft.servers.filter((s) => {
    const st = findServerStats(bs, s)
    return s.id !== failing?.s.id && st && (st.status === 'UP' || st.status === 'DRAIN' || st.status === 'NOCHECK')
  }).length
  const failDetail = failing?.st?.checkDetail ? failing.st.checkDetail.split(' · ').filter((_, i, a) => i === 0 || i === a.length - 1).join(', ') : ''

  const users = draft.id ? frontends.filter((f) => frontendUsesBackend(f, draft.id)) : []

  return (
    <Drawer
      open
      onClose={onClose}
      width="wide"
      title={
        isNew ? (
          'New backend'
        ) : (
          <span className="row gap-10" style={{ alignItems: 'baseline' }}>
            {readOnly ? 'Backend' : 'Edit backend'} <span className="mono muted" style={{ fontSize: 14, fontWeight: 500 }}>{initial.name}</span>
          </span>
        )
      }
      subtitle="Saved changes are hot-reloaded into HAProxy without dropping connections"
      tabs={TABS}
      tab={tab}
      onTab={setTab}
      footer={
        <>
          <PreviewStatus state={preview} readOnly={readOnly} />
          <span className="spacer" />
          <Button size="md" onClick={onClose}>{readOnly ? 'Close' : 'Cancel'}</Button>
          {!readOnly && (
            <Button size="md" variant="primary" loading={save.isPending} onClick={onSave}>Save to pending</Button>
          )}
        </>
      }
    >
      {tab === 'servers' && (
        <>
          {isNew && (
            <Field label="Name" hint="Letters, digits, dots and dashes · used as the backend name in haproxy.cfg" error={err('name')}>
              <Input mono autoFocus placeholder="web-app" value={draft.name} invalid={!!err('name')} disabled={readOnly} onChange={(e) => set({ name: e.target.value })} />
            </Field>
          )}
          <div className="grid-2">
            <Field label="Mode" error={err('mode')}>
              <div className="lb-seg-full">
                <Segmented value={draft.mode} onChange={setMode} disabled={readOnly} options={[{ value: 'http', label: 'HTTP (L7)' }, { value: 'tcp', label: 'TCP (L4)' }]} />
              </div>
            </Field>
            <Field label="Algorithm" error={err('algorithm')}>
              <Select
                value={draft.algorithm}
                disabled={readOnly}
                onChange={(v) => set({ algorithm: v as Backend['algorithm'] })}
                options={ALGORITHMS.map((a) => ({ value: a.value, label: `${a.value} · ${a.description}`, disabled: a.value === 'uri' && !http }))}
              />
            </Field>
          </div>

          <div className="col gap-8">
            <div className="lb-section-label">
              <label className="field-label">Servers</label>
              <span className="aside">Drag to reorder · weights 1–256</span>
            </div>
            <div className="lb-table">
              <div className="lb-trow head">
                <span />
                <span>Address</span>
                <span>Port</span>
                <span>Weight</span>
                <span>Role</span>
                <span style={{ textAlign: 'center' }}>Check</span>
                <span />
              </div>
              {draft.servers.map((s, i) => {
                const st = findServerStats(bs, s)
                const tone = st ? (st.status === 'DOWN' ? 'danger' : st.status === 'DRAIN' ? 'warn' : st.status === 'MAINT' ? 'muted' : 'ok') : undefined
                return (
                  <div
                    key={s.id}
                    className={cx(
                      'lb-trow',
                      st?.status === 'DOWN' && 'failing',
                      dragIndex === i && 'dragging',
                      dragIndex !== null && overIndex === i && dragIndex !== i && 'drop-target',
                    )}
                    draggable={!readOnly && armed === i}
                    onDragStart={(e) => {
                      setDragIndex(i)
                      e.dataTransfer.effectAllowed = 'move'
                      e.dataTransfer.setData('text/plain', String(i))
                    }}
                    onDragOver={(e) => {
                      if (dragIndex === null) return
                      e.preventDefault()
                      setOverIndex(i)
                    }}
                    onDrop={(e) => {
                      e.preventDefault()
                      if (dragIndex !== null && dragIndex !== i) move(dragIndex, i)
                      endDrag()
                    }}
                    onDragEnd={endDrag}
                  >
                    <span className="drag-handle" title="Drag to reorder" onMouseDown={() => setArmed(i)} onMouseUp={() => setArmed(null)}>
                      <Icon name="drag" size={14} />
                    </span>
                    <div className={cx('lb-addr', tone && 'has-dot')} title={st ? `${s.name} · ${st.status}${st.checkDetail ? ' · ' + st.checkDetail : ''}` : s.name}>
                      {tone && <Dot tone={tone} />}
                      <Input mono placeholder="10.0.0.x" value={s.address} disabled={readOnly} invalid={!!err(`servers.${i}.address`)} onChange={(e) => setServer(i, { address: e.target.value })} />
                    </div>
                    <Input mono inputMode="numeric" placeholder="8080" value={s.port || ''} disabled={readOnly} invalid={!!err(`servers.${i}.port`)} onChange={(e) => setServer(i, { port: Number(e.target.value.replace(/\D/g, '')) || 0 })} />
                    <Input mono inputMode="numeric" value={s.weight || ''} disabled={readOnly} invalid={!!err(`servers.${i}.weight`)} onChange={(e) => setServer(i, { weight: Number(e.target.value.replace(/\D/g, '')) || 0 })} />
                    <Select value={s.role} disabled={readOnly} options={['active', 'backup']} onChange={(v) => setServer(i, { role: v as Server['role'] })} />
                    <span className="center-cell">
                      <Checkbox checked={s.check && hc.type !== 'none'} disabled={readOnly || hc.type === 'none'} onChange={(v) => setServer(i, { check: v })} />
                    </span>
                    {!readOnly ? (
                      <IconButton icon="close" bare size={14} label="Remove server" onClick={() => set({ servers: draft.servers.filter((_, j) => j !== i) })} />
                    ) : (
                      <span />
                    )}
                  </div>
                )
              })}
              {!readOnly && (
                <div className="lb-tfoot">
                  <button type="button" className="lb-link" onClick={() => addServer()}>+ Add server</button>
                  {chips.length > 0 && (
                    <>
                      <span>or pick from discovered containers:</span>
                      {chips.map(({ c, address, port, label }) => (
                        <button key={`${c.endpointId}/${c.id}`} type="button" className="lb-chip" title={`${c.image} · ${address}:${port}`} onClick={() => addServer(address, port)}>
                          {label}
                        </button>
                      ))}
                    </>
                  )}
                </div>
              )}
            </div>
            {Object.entries(errors).filter(([k]) => k.startsWith('servers')).slice(0, 3).map(([k, v]) => (
              <div key={k} className="field-error">Server {Number(k.split('.')[1]) + 1}: {v}</div>
            ))}
            {failing && (
              <div className="lb-inline-alert">
                <Dot tone="danger" />
                <span>
                  {failing.s.address} is failing health checks{failDetail ? ` (${failDetail})` : ''}.{' '}
                  {upOthers > 0 ? `Requests are being routed to the other ${upOthers === 1 ? 'server' : upOthers}.` : 'No other server is available.'}
                </span>
                {canWrite && failing.s.state === 'ready' && (
                  <button
                    type="button"
                    className="lb-link"
                    style={{ marginLeft: 'auto', color: 'var(--danger-text)' }}
                    onClick={async () => {
                      try {
                        const r = await actions.setState(draft.id, failing.s.id, 'drain')
                        setServer(draft.servers.findIndex((x) => x.id === failing.s.id), { state: 'drain' })
                        toast.show({ kind: r.runtime ? 'success' : 'info', title: `${failing.s.address} is draining`, message: r.note ?? 'Applied to HAProxy immediately.' })
                      } catch (e) {
                        toast.error(e, 'Could not drain server')
                      }
                    }}
                  >
                    Set to drain
                  </button>
                )}
              </div>
            )}
          </div>

          <div className="grid-2">
            <ToggleCard
              title="Sticky sessions"
              description={http ? <>{draft.sticky.mode === 'source' ? 'Source IP' : draft.sticky.mode === 'prefix' ? 'Prefix cookie' : 'Insert cookie'} <span className="mono">{draft.sticky.mode === 'source' ? '' : draft.sticky.cookieName || 'SRVID'}</span></> : 'Same client IP → same server'}
              checked={draft.sticky.enabled}
              disabled={readOnly}
              onChange={(v) => set({ sticky: { ...draft.sticky, enabled: v } })}
            />
            <ToggleCard
              title={http ? 'Forward client IP' : 'Send PROXY protocol'}
              description={http ? 'X-Forwarded-For / PROXY protocol' : 'PROXY v2 header carries the client IP'}
              checked={http ? draft.forwardClientIp : draft.sendProxy}
              disabled={readOnly}
              onChange={(v) => set(http ? { forwardClientIp: v } : { sendProxy: v })}
            />
          </div>
          <PreviewBlock title="Generated haproxy.cfg (this backend)" state={preview} readOnly={readOnly} />
        </>
      )}

      {tab === 'balancing' && (
        <>
          {!isNew && (
            <Field label="Name" hint="Frontends and hosts reference the backend by id, so renaming is safe" error={err('name')}>
              <Input mono value={draft.name} disabled={readOnly} invalid={!!err('name')} onChange={(e) => set({ name: e.target.value })} />
            </Field>
          )}
          <Field label="Algorithm" error={err('algorithm')}>
            <div className="grid-2" style={{ gap: 8 }}>
              {ALGORITHMS.map((a) => (
                <RadioCard
                  key={a.value}
                  selected={draft.algorithm === a.value}
                  disabled={readOnly || (a.value === 'uri' && !http)}
                  onSelect={() => set({ algorithm: a.value })}
                  title={<span className="mono">{a.value}</span>}
                  description={a.description}
                />
              ))}
            </div>
          </Field>
          <div className="col gap-10">
            <ToggleCard title="Sticky sessions" description="Keep a client on the same server" checked={draft.sticky.enabled} disabled={readOnly} onChange={(v) => set({ sticky: { ...draft.sticky, enabled: v } })} />
            {draft.sticky.enabled && (
              <div className="grid-2">
                <Field label="Stick by" error={err('sticky.mode')}>
                  <Select
                    value={http ? draft.sticky.mode : 'source'}
                    disabled={readOnly || !http}
                    onChange={(v) => set({ sticky: { ...draft.sticky, mode: v as Backend['sticky']['mode'] } })}
                    options={[
                      { value: 'insert', label: 'Insert cookie (HAProxy sets it)' },
                      { value: 'prefix', label: "Prefix the app's cookie" },
                      { value: 'source', label: 'Source IP (stick table)' },
                    ]}
                  />
                </Field>
                {http && draft.sticky.mode !== 'source' && (
                  <Field label="Cookie name" error={err('sticky.cookieName')}>
                    <Input mono value={draft.sticky.cookieName} disabled={readOnly} invalid={!!err('sticky.cookieName')} onChange={(e) => set({ sticky: { ...draft.sticky, cookieName: e.target.value } })} />
                  </Field>
                )}
              </div>
            )}
          </div>
          <div className="grid-2">
            <Field label="Retries" hint="Connection retries; option redispatch retries on another server" error={err('retries')}>
              <Input mono inputMode="numeric" value={draft.retries} disabled={readOnly} invalid={!!err('retries')} onChange={(e) => set({ retries: Number(e.target.value.replace(/\D/g, '')) || 0 })} />
            </Field>
          </div>
          <div className="grid-2">
            {http && (
              <ToggleCard title="Forward client IP" description="option forwardfor · X-Forwarded-For" checked={draft.forwardClientIp} disabled={readOnly} onChange={(v) => set({ forwardClientIp: v })} />
            )}
            <ToggleCard title="Send PROXY protocol" description="send-proxy-v2 to every server" checked={draft.sendProxy} disabled={readOnly} onChange={(v) => set({ sendProxy: v })} />
            <ToggleCard title="TLS re-encrypt" description="Connect to servers over TLS" checked={draft.tlsReencrypt} disabled={readOnly} onChange={(v) => set({ tlsReencrypt: v, tlsVerify: v && draft.tlsVerify })} />
            <ToggleCard title="Verify server certificates" description="System CA bundle · off allows self-signed" checked={draft.tlsVerify} disabled={readOnly || !draft.tlsReencrypt} onChange={(v) => set({ tlsVerify: v })} />
          </div>
        </>
      )}

      {tab === 'health' && (
        <>
          <Field label="Check type" error={err('healthCheck.type')}>
            <Select value={hc.type} disabled={readOnly} options={HEALTH_TYPES.map((t) => ({ value: t.value, label: t.label }))} onChange={(v) => setHC({ type: v as HealthCheck['type'] })} />
          </Field>
          {hc.type === 'http' && (
            <div className="grid-2">
              <Field label="Method" error={err('healthCheck.method')}>
                <Select value={hc.method || 'GET'} disabled={readOnly} options={['GET', 'HEAD', 'OPTIONS', 'POST']} onChange={(v) => setHC({ method: v })} />
              </Field>
              <Field label="Path" error={err('healthCheck.path')}>
                <Input mono value={hc.path} placeholder="/health" disabled={readOnly} invalid={!!err('healthCheck.path')} onChange={(e) => setHC({ path: e.target.value })} />
              </Field>
              <Field label="Expected status" hint="200, 2xx, 200-399 or 200,204" error={err('healthCheck.expectStatus')}>
                <Input mono value={hc.expectStatus} placeholder="2xx" disabled={readOnly} invalid={!!err('healthCheck.expectStatus')} onChange={(e) => setHC({ expectStatus: e.target.value })} />
              </Field>
              <Field label="Host header" hint="Optional · for virtual-hosted apps" error={err('healthCheck.host')}>
                <Input mono value={hc.host ?? ''} placeholder="app.home.lan" disabled={readOnly} invalid={!!err('healthCheck.host')} onChange={(e) => setHC({ host: e.target.value })} />
              </Field>
            </div>
          )}
          {hc.type !== 'none' && (
            <>
              <div className="grid-3">
                <Field label="Interval" hint={`Empty = global ${settings?.checkInterval ?? '2s'}`} error={err('healthCheck.interval')}>
                  <Input mono value={hc.interval} placeholder={settings?.checkInterval ?? '2s'} disabled={readOnly} invalid={!!err('healthCheck.interval')} onChange={(e) => setHC({ interval: e.target.value })} />
                </Field>
                <Field label="Rise" hint="Successes to mark UP" error={err('healthCheck.rise')}>
                  <Input mono inputMode="numeric" value={hc.rise || ''} placeholder={String(settings?.rise ?? 2)} disabled={readOnly} onChange={(e) => setHC({ rise: Number(e.target.value.replace(/\D/g, '')) || 0 })} />
                </Field>
                <Field label="Fall" hint="Failures to mark DOWN" error={err('healthCheck.fall')}>
                  <Input mono inputMode="numeric" value={hc.fall || ''} placeholder={String(settings?.fall ?? 3)} disabled={readOnly} onChange={(e) => setHC({ fall: Number(e.target.value.replace(/\D/g, '')) || 0 })} />
                </Field>
              </div>
              <Callout>
                A server goes DOWN after {hc.fall || settings?.fall || 3} failed checks in a row and back UP after {hc.rise || settings?.rise || 2} passing ones.
                {hc.type === 'pgsql' && ' The PostgreSQL check logs in as user relay; allow it in pg_hba.conf.'}
                {draft.servers.some((s) => !s.check) && ' Servers with Check off are always treated as up.'}
              </Callout>
            </>
          )}
          {hc.type === 'none' && <Callout tone="warn">Without health checks HAProxy keeps sending traffic to servers that are down.</Callout>}
        </>
      )}

      {tab === 'frontend' && (
        <>
          <div>
            <div className="section-title">Frontends routing to this backend</div>
            <div className="section-desc">A frontend is a listening port. The Expose wizard adds a localhost frontend and a proxy host so the reverse proxy handles TLS and the domain.</div>
          </div>
          {isNew ? (
            <Callout>Save the backend first, then expose it online or attach it to a frontend.</Callout>
          ) : users.length === 0 ? (
            <Callout tone="warn">Nothing routes to this backend yet, so it receives no traffic.</Callout>
          ) : (
            <div className="col gap-8">
              {users.map((f) => {
                const host = hosts.find((h) => h.id === f.hostId)
                return (
                  <div key={f.id} className="list-row">
                    <Icon name="load-balancer" size={16} />
                    <div className="grow">
                      <div className="mono medium">{f.name}</div>
                      <div className="small muted">
                        bind {f.bind} · {f.mode.toUpperCase()}
                        {host ? ` · via ${host.domains[0]}` : ''} · {f.defaultBackendId === draft.id ? 'default backend' : 'by rule'}
                      </div>
                    </div>
                    {!f.enabled && <Badge>disabled</Badge>}
                    <Button size="sm" onClick={() => navigate(`/load-balancer/frontends?edit=${f.id}`)}>Open</Button>
                  </div>
                )
              })}
            </div>
          )}
          {!isNew && canWrite && (
            <div className="row gap-8">
              {http && <Button icon="expose" onClick={() => onExpose(draft.id)}>Expose online</Button>}
              <Button icon="plus" onClick={() => navigate(`/load-balancer/frontends?new=1&backend=${draft.id}`)}>New frontend</Button>
            </div>
          )}
        </>
      )}

      {tab === 'timeouts' && (
        <>
          <div className="grid-3">
            <Field label="Connect" hint={`Empty = global ${settings?.timeoutConnect ?? '5s'}`} error={err('timeouts.connect')}>
              <Input mono value={draft.timeouts.connect} placeholder={settings?.timeoutConnect ?? '5s'} disabled={readOnly} invalid={!!err('timeouts.connect')} onChange={(e) => set({ timeouts: { ...draft.timeouts, connect: e.target.value } })} />
            </Field>
            <Field label="Server" hint={`Empty = global ${settings?.timeoutServer ?? '50s'}`} error={err('timeouts.server')}>
              <Input mono value={draft.timeouts.server} placeholder={settings?.timeoutServer ?? '50s'} disabled={readOnly} invalid={!!err('timeouts.server')} onChange={(e) => set({ timeouts: { ...draft.timeouts, server: e.target.value } })} />
            </Field>
            <Field label="Queue" hint="Empty = same as connect" error={err('timeouts.queue')}>
              <Input mono value={draft.timeouts.queue} placeholder="—" disabled={readOnly} invalid={!!err('timeouts.queue')} onChange={(e) => set({ timeouts: { ...draft.timeouts, queue: e.target.value } })} />
            </Field>
          </div>
          <Callout>
            Durations use HAProxy units: <span className="mono">500ms</span>, <span className="mono">5s</span>, <span className="mono">2m</span>, <span className="mono">1h</span>. Raise the server timeout for long-polling or streaming apps.
          </Callout>
        </>
      )}

      {tab === 'config' && (
        <>
          <PreviewBlock title="Generated haproxy.cfg (this backend)" state={preview} readOnly={readOnly} />
          {preview.data?.output && (preview.data.checked !== 'haproxy' || !preview.data.valid) && (
            <div className="col gap-6">
              <div className="lb-preview-title">{preview.data.checked === 'haproxy' ? 'haproxy -c output' : 'Validation'}</div>
              <CodeBlock code={preview.data.output} wrap />
            </div>
          )}
        </>
      )}
    </Drawer>
  )
}
