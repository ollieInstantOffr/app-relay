// New / edit stream drawer (design 28b).
import { useEffect, useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api } from '../../lib/api'
import { useContainers, useEntities, useRole, useSaveEntity, useSelectedProxyEngine } from '../../lib/queries'
import { useDockerStatus } from '../docker/ops'
import type { ConfigPreview, Stream } from '../../lib/types'
import {
  Button, Callout, CodeBlock, Field, Input, Segmented, Select, Spinner, Status, ToggleCard, useToast, Drawer,
} from '../../components/ui'
import { fieldErrors, pendingToast, toastUnlessFields } from '../certificates/common'
import type { PortEntry } from './StreamsPage'
import { previewCheck, previewTitle } from '../hosts/ConfigPreview'
import '../certificates/certs.css'

const ownerLabel: Record<PortEntry['owner'], string> = { nginx: 'nginx', edge: 'Relay Edge', haproxy: 'HAProxy', relay: 'Relay', other: 'another process' }

interface PortCheck { free: boolean; conflicts: PortEntry[]; messages: string[]; ports: number; listenersKnown: boolean }

const blank = (): Stream => ({
  id: '', createdAt: '', updatedAt: '', name: '', protocol: 'tcp', listenAddress: '0.0.0.0', listenPorts: '',
  forwardHost: '', forwardPorts: '', proxyProtocol: false, idleTimeout: '10m', enabled: true,
})

function useDebounced<T>(value: T, ms: number): T {
  const [v, setV] = useState(value)
  useEffect(() => {
    const t = setTimeout(() => setV(value), ms)
    return () => clearTimeout(t)
  }, [value, ms])
  return v
}

export default function StreamDrawer({ open, onClose, stream, ports }: {
  open: boolean
  onClose: () => void
  stream?: Stream
  ports?: PortEntry[]
}) {
  const edge = useSelectedProxyEngine() === 'edge'
  const toast = useToast()
  const { canWrite } = useRole()
  const save = useSaveEntity('streams')
  const backends = useEntities('backends').data ?? []
  const containers = useContainers(open).data ?? []
  const dockerStatus = useDockerStatus(open)
  const [draft, setDraft] = useState<Stream>(blank())
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [customAddr, setCustomAddr] = useState(false)

  useEffect(() => {
    if (!open) return
    const base = stream ? { ...stream } : blank()
    setDraft(base)
    setErrors({})
    setCustomAddr(false)
  }, [open, stream])

  const set = (patch: Partial<Stream>) => setDraft((d) => ({ ...d, ...patch }))
  const editing = !!draft.id

  // listen address options: wildcards + addresses seen on this machine
  const addrOptions = useMemo(() => {
    const opts = [
      { value: '0.0.0.0', label: '0.0.0.0 · all interfaces' },
      { value: '127.0.0.1', label: '127.0.0.1 · this machine only' },
      { value: '::', label: ':: · all IPv6 interfaces' },
    ]
    const seen = new Set(opts.map((o) => o.value))
    for (const p of ports ?? []) {
      if (!seen.has(p.address)) {
        seen.add(p.address)
        opts.push({ value: p.address, label: `${p.address} · in use by ${p.owner === 'other' ? p.name || 'a process' : ownerLabel[p.owner]}` })
      }
    }
    if (draft.listenAddress && !seen.has(draft.listenAddress)) opts.push({ value: draft.listenAddress, label: draft.listenAddress })
    opts.push({ value: '__custom', label: 'Other address…' })
    return opts
  }, [ports, draft.listenAddress])

  const checkKey = useDebounced({ listenAddress: draft.listenAddress, listenPorts: draft.listenPorts.trim(), protocol: draft.protocol, excludeId: draft.id }, 350)
  const portCheck = useQuery({
    queryKey: ['streams', 'check-ports', checkKey],
    queryFn: () => api.post<PortCheck>('/api/streams/check-ports', checkKey),
    enabled: open && !!checkKey.listenPorts,
    retry: false,
  })
  const portError = errors.listenPorts ?? (portCheck.error ? fieldErrors(portCheck.error).listenPorts ?? fieldErrors(portCheck.error).listenAddress : undefined)

  const previewKey = useDebounced(draft, 600)
  const preview = useQuery({
    queryKey: ['preview', 'proxy', 'stream', previewKey],
    queryFn: () => api.post<ConfigPreview>('/api/preview/proxy/stream', { stream: { ...previewKey, id: previewKey.id || 'new' } }),
    enabled: open && !!previewKey.listenPorts && (!!previewKey.forwardHost || !!previewKey.backendId) && !!previewKey.name,
    retry: false,
  })

  const [lo] = draft.listenPorts.split('-').map((p) => parseInt(p, 10))
  const suggestions = useMemo(() => {
    const out: { label: string; host: string; port: number }[] = []
    const multiHost = new Set(containers.map((c) => c.endpointId)).size > 1
    // Address of each Docker host for published ports ("" = local: container IPs are routable).
    const remote = new Map((dockerStatus.data?.endpoints ?? []).map((e) => [e.id, e.upstreamAddress]))
    for (const c of containers) {
      if (c.state !== 'running') continue
      const hostAddr = remote.get(c.endpointId) ?? ''
      for (const p of c.ports) {
        const protoOK = draft.protocol === 'both' || p.proto === draft.protocol
        if (!protoOK || !lo || (p.private !== lo && p.public !== lo)) continue
        // local: container IP + container port (or loopback for host networking);
        // remote: host address + published port (host networking: container port).
        const target = !hostAddr
          ? { host: c.ip || '127.0.0.1', port: p.private }
          : p.public ? { host: hostAddr, port: p.public } : !c.ip ? { host: hostAddr, port: p.private } : null
        if (!target) continue
        out.push({ label: `${multiHost && c.endpointName ? `${c.endpointName} · ` : ''}${c.name}:${target.port}/${p.proto}`, ...target })
      }
    }
    return out.slice(0, 3)
  }, [containers, dockerStatus.data, draft.protocol, lo])

  const submit = async () => {
    setErrors({})
    try {
      const saved = await save.mutateAsync({ ...draft, id: draft.id || undefined, forwardHost: draft.backendId ? '' : draft.forwardHost })
      pendingToast(toast, editing ? 'Stream saved' : 'Stream created', `${saved.name} (:${saved.listenPorts})`)
      onClose()
    } catch (err) {
      setErrors(fieldErrors(err))
      toastUnlessFields(toast, err, 'Could not save stream')
    }
  }

  useEffect(() => {
    if (!open) return
    const h = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 's') {
        e.preventDefault()
        if (canWrite) submit()
      }
    }
    window.addEventListener('keydown', h)
    return () => window.removeEventListener('keydown', h)
  })

  const tcpBackends = backends.filter((b) => b.mode === 'tcp')
  const wildcard = draft.listenAddress === '0.0.0.0' || draft.listenAddress === '::'

  return (
    <Drawer
      open={open}
      onClose={onClose}
      title={editing ? `Edit stream ${stream?.name}` : 'New stream'}
      subtitle="Raw TCP/UDP port forward · no TLS, no access lists"
      footer={
        <>
          {preview.data ? (
            preview.data.valid
              ? <Status tone="ok">Config valid <span className="faint mono">· {previewCheck(preview.data.engine)}</span></Status>
              : <Status tone="danger">Config invalid <span className="faint mono">· {previewCheck(preview.data.engine)}</span></Status>
          ) : preview.isFetching ? (
            <span className="row small muted"><Spinner /> Validating…</span>
          ) : null}
          <div className="spacer" />
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" onClick={submit} loading={save.isPending} disabled={!canWrite}>Save to pending</Button>
        </>
      }
    >
      <Field label="Name" error={errors.name}>
        <Input value={draft.name} invalid={!!errors.name} placeholder="Valheim" onChange={(e) => set({ name: e.target.value })} autoFocus={!editing} />
      </Field>
      <Field label="Protocol" error={errors.protocol}>
        <Segmented
          value={draft.protocol}
          onChange={(v) => set({ protocol: v })}
          options={[{ value: 'tcp', label: 'TCP' }, { value: 'udp', label: 'UDP' }, { value: 'both', label: 'Both' }]}
        />
      </Field>
      <div className="row-top gap-12">
        <Field label="Listen" error={errors.listenAddress} className="grow">
          {customAddr ? (
            <div className="row gap-6">
              <Input mono value={draft.listenAddress} invalid={!!errors.listenAddress} placeholder="10.0.0.1" onChange={(e) => set({ listenAddress: e.target.value.trim() })} />
              <Button size="sm" variant="ghost" onClick={() => { setCustomAddr(false); set({ listenAddress: '0.0.0.0' }) }}>List</Button>
            </div>
          ) : (
            <Select mono value={draft.listenAddress} options={addrOptions} onChange={(v) => (v === '__custom' ? (setCustomAddr(true), set({ listenAddress: '' })) : set({ listenAddress: v }))} />
          )}
        </Field>
        <Field
          label="Ports"
          className="grow"
          error={portError ?? (portCheck.data && !portCheck.data.free ? portCheck.data.messages[0] + (portCheck.data.messages.length > 1 ? ` (+${portCheck.data.messages.length - 1} more)` : '') : undefined)}
          hint={portCheck.data?.free ? <span className="ok-text">Ports free · ranges allowed{portCheck.data.listenersKnown ? '' : ' · host listeners unknown'}</span> : 'e.g. 25565 or 2456-2458 · ranges allowed'}
        >
          <Input mono value={draft.listenPorts} invalid={!!portError || portCheck.data?.free === false} placeholder="2456-2458" onChange={(e) => set({ listenPorts: e.target.value.replace(/\s/g, '') })} />
        </Field>
      </div>

      <div className="col gap-8">
        <div className="row-top gap-12">
          <Field label="Forward to" error={errors.forwardHost} className="grow">
            <Input mono value={draft.backendId ? '' : draft.forwardHost} disabled={!!draft.backendId} invalid={!!errors.forwardHost} placeholder={draft.backendId ? 'via load balancer' : '10.0.0.61'} onChange={(e) => set({ forwardHost: e.target.value.trim() })} list="stream-container-hosts" />
            <datalist id="stream-container-hosts">
              {containers.filter((c) => c.state === 'running' && c.upstreamHost).map((c) => <option key={`${c.endpointId}/${c.id}`} value={c.upstreamHost}>{c.endpointName ? `${c.endpointName} · ${c.name}` : c.name}</option>)}
            </datalist>
          </Field>
          <Field label="Port" error={errors.forwardPorts} hint="same = listen port">
            <Input mono value={draft.forwardPorts} disabled={!!draft.backendId} placeholder="same" onChange={(e) => set({ forwardPorts: e.target.value.replace(/\s/g, '') })} style={{ width: 130 }} />
          </Field>
        </div>
        {suggestions.length > 0 && !draft.backendId && (
          <div className="row gap-6 wrap">
            <span className="micro muted">Suggestions</span>
            {suggestions.map((s) => (
              <button key={s.label} type="button" className="cs-chip" onClick={() => set({ forwardHost: s.host, forwardPorts: s.port === lo ? '' : String(s.port) })}>{s.label}</button>
            ))}
          </div>
        )}
        <Field label="or a backend" error={errors.backendId} hint={draft.backendId ? 'Proxies to the backend’s TCP frontend on 127.0.0.1' : undefined}>
          <Select
            value={draft.backendId ?? ''}
            placeholder="— none —"
            options={tcpBackends.map((b) => ({ value: b.id, label: `${b.name} · ${b.servers.length} servers` }))}
            onChange={(v) => set({ backendId: v || undefined, protocol: v ? 'tcp' : draft.protocol })}
            disabled={!tcpBackends.length}
          />
        </Field>
      </div>

      <div className="grid-2">
        <ToggleCard title="PROXY protocol" description={edge && draft.protocol !== 'tcp' ? 'Pass client IP to upstream · Relay Edge sends it on TCP only' : 'Pass client IP to upstream'} checked={draft.proxyProtocol} onChange={(v) => set({ proxyProtocol: v })} />
        <Field label="Idle timeout" error={errors.idleTimeout}>
          <Input mono value={draft.idleTimeout} placeholder="10m" onChange={(e) => set({ idleTimeout: e.target.value.trim() })} />
        </Field>
      </div>
      {editing && <ToggleCard title="Enabled" description="Disabled streams keep their config but don't bind the port" checked={draft.enabled} onChange={(v) => set({ enabled: v })} />}

      {wildcard && (
        <Callout tone="warn">Binding to all interfaces exposes this port to the internet if your router forwards it. Streams bypass access lists.</Callout>
      )}

      {preview.data && (
        <div className="col gap-6">
          <div className="micro muted">{previewTitle(preview.data.engine)}</div>
          <CodeBlock code={preview.data.config} maxHeight={200} />
        </div>
      )}
      {preview.data && !preview.data.valid && preview.data.output && <Callout tone="danger"><span className="mono">{preview.data.output}</span></Callout>}
    </Drawer>
  )
}
