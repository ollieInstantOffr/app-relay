// Owner: slice ops. Settings → Docker discovery (design 15b, 22b).
import { useEffect, useMemo, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { api, errorMessage } from '../../lib/api'
import { keys, useContainers, useEntities, useRole, useSaveSettings, useSettings } from '../../lib/queries'
import type { Backend, Container, DockerSettings as DockerSettingsT } from '../../lib/types'
import {
  Button, Callout, Card, CodeBlock, Dialog, Field, Icon, Input, Menu, SectionHeader, Select, Skeleton, Toggle, ToggleCard, useToast,
} from '../../components/ui'
import DockerSuggestionsDialog from '../docker/DockerSuggestionsDialog'
import { applyNowAction, useDockerStatus, type DockerStatus } from '../docker/ops'
import '../docker/ops.css'

const LABEL_REFERENCE = [
  ['relay.host=grafana.home.lan', ''],
  ['relay.port=3000', ''],
  ['relay.tls=auto', '# letsencrypt | auto | off'],
  ['relay.access=lan-only', ''],
  ['relay.backend=web-app', '# join an HAProxy backend instead'],
]

function statusLine(st: DockerStatus | undefined): { tone: 'ok' | 'danger' | 'muted'; text: string } {
  if (!st) return { tone: 'muted', text: 'checking…' }
  if (!st.enabled) return { tone: 'muted', text: 'discovery disabled' }
  if (st.connected) return { tone: 'ok', text: `connected · ${st.version} · ${st.containers} ${st.containers === 1 ? 'container' : 'containers'}` }
  if (!st.checkedAt) return { tone: 'muted', text: 'connecting…' }
  return { tone: 'danger', text: st.error || 'not connected' }
}

function matchBackend(c: Container, backends: Backend[]): Backend | undefined {
  const name = c.name.toLowerCase()
  return backends.find((b) => {
    const bn = b.name.toLowerCase()
    return name === bn || name.startsWith(bn + '-') || name.startsWith(bn + '_') || c.labels['relay.backend'] === b.name
  })
}

export default function DockerSettings() {
  const qc = useQueryClient()
  const toast = useToast()
  const { isAdmin, canWrite } = useRole()
  const settings = useSettings('docker')
  const save = useSaveSettings('docker')
  const status = useDockerStatus()
  const st = status.data
  const containers = useContainers(!!st?.connected)
  const backends = useEntities('backends')
  const certs = useEntities('certificates')
  const accessLists = useEntities('access-lists')

  const [endpoint, setEndpoint] = useState('')
  const [dialog, setDialog] = useState<{ open: boolean; preselect?: string[] }>({ open: false })
  const [remoteOpen, setRemoteOpen] = useState(false)
  const [remote, setRemote] = useState('tcp://')
  const [retrying, setRetrying] = useState(false)

  useEffect(() => {
    if (settings.data) setEndpoint(settings.data.endpoint)
  }, [settings.data])

  const update = async (patch: Partial<DockerSettingsT>) => {
    if (!settings.data) return
    try {
      await save.mutateAsync({ ...settings.data, ...patch })
      qc.invalidateQueries({ queryKey: ['docker'] })
    } catch (err) {
      toast.error(err, 'Could not save Docker settings')
    }
  }

  const retry = async () => {
    setRetrying(true)
    try {
      const next = await api.post<DockerStatus>('/api/docker/retry')
      qc.setQueryData(['docker', 'status'], next)
      qc.invalidateQueries({ queryKey: keys.containers })
      if (next.connected) toast.success('Docker connected', `${next.version} · ${next.containers} containers`)
    } catch (err) {
      toast.error(err, 'Retry failed')
    } finally {
      setRetrying(false)
    }
  }

  const addToBackend = async (c: Container, b: Backend) => {
    try {
      await api.post(`/api/docker/backends/${b.id}/servers`, { containerId: c.id, port: c.suggestedPort })
      qc.invalidateQueries({ queryKey: keys.entities('backends') })
      qc.invalidateQueries({ queryKey: keys.pending })
      qc.invalidateQueries({ queryKey: ['docker'] })
      toast.show({ kind: 'success', title: `${c.name} added to backend ${b.name}`, message: `${c.ip}:${c.suggestedPort} · added to pending changes`, actions: [applyNowAction] })
    } catch (err) {
      toast.error(err, `Could not add ${c.name} to ${b.name}`)
    }
  }

  const discovered = useMemo(() => {
    const list = containers.data ?? []
    return list.filter((c) => !c.hostId && !c.backendId && c.ip && (c.http || matchBackend(c, backends.data ?? [])))
  }, [containers.data, backends.data])

  if (settings.isLoading || !settings.data) {
    return (
      <>
        <SectionHeader title="Docker discovery" description="Watch the Docker socket and suggest containers as upstreams. Optionally auto-create hosts from labels." />
        <Skeleton height={220} />
      </>
    )
  }
  const s = settings.data
  const line = statusLine(st)
  const failing = s.enabled && st && !st.connected && !!st.checkedAt
  const disabled = !isAdmin
  const endpointDirty = endpoint.trim() !== s.endpoint

  return (
    <>
      <SectionHeader
        title="Docker discovery"
        description="Watch the Docker socket and suggest containers as upstreams. Optionally auto-create hosts from labels."
        actions={
          <label className="ops-header-toggle">
            Enabled
            <Toggle checked={s.enabled} disabled={disabled} onChange={(v) => update({ enabled: v })} label="Docker discovery enabled" />
          </label>
        }
      />

      {failing && (
        <Card>
          <div className="card-body col gap-14">
            <div className="row-top gap-12">
              <div className="empty-icon" style={{ margin: 0, width: 40, height: 40 }}><Icon name="docker" size={20} /></div>
              <div className="grow">
                <div className="semibold" style={{ fontSize: 15 }}>{st?.socketMissing ? 'Docker socket not found' : "Can't reach Docker"}</div>
                <div className="muted" style={{ marginTop: 4, lineHeight: 1.55 }}>
                  {st?.socketMissing ? (
                    <>Looked for <span className="mono">{s.endpoint.replace('unix://', '')}</span>. Mount it into the Relay container, or point at a remote Docker host over TCP.</>
                  ) : (
                    <><span className="mono">{s.endpoint}</span> · {st?.error}</>
                  )}
                </div>
              </div>
            </div>
            <CodeBlock code={'volumes:\n  - /var/run/docker.sock:/var/run/docker.sock:ro'} />
            <Callout tone="warn" title="Security note">
              Access to the Docker socket is equivalent to root on the host. Mount it read-only, or use a socket proxy that only allows GET /containers.
            </Callout>
            <div className="row gap-8">
              <Button icon="reload" loading={retrying} onClick={retry}>Retry detection</Button>
              <Button disabled={disabled} onClick={() => { setRemote(s.endpoint.startsWith('tcp://') ? s.endpoint : 'tcp://'); setRemoteOpen(true) }}>Connect remote host…</Button>
            </div>
          </div>
        </Card>
      )}

      <Card title="Connection">
        <div className="card-body col gap-14">
          <Field label="Socket / host" error={save.error && endpointDirty ? errorMessage(save.error) : undefined}>
            <div className="row gap-8">
              <div className="ops-input-status grow">
                <Input
                  mono
                  value={endpoint}
                  disabled={disabled}
                  onChange={(e) => setEndpoint(e.target.value)}
                  onKeyDown={(e) => e.key === 'Enter' && endpointDirty && update({ endpoint: endpoint.trim() })}
                />
                {!endpointDirty && (
                  <span className={`ops-inline-status ${line.tone}`} title={line.text}>
                    <span className={`ops-dot ${line.tone === 'muted' ? '' : line.tone}`} style={{ width: 6, height: 6 }} />
                    {line.text}
                  </span>
                )}
              </div>
              {endpointDirty && (
                <>
                  <Button onClick={() => setEndpoint(s.endpoint)}>Reset</Button>
                  <Button variant="primary" loading={save.isPending} onClick={() => update({ endpoint: endpoint.trim() })}>Save</Button>
                </>
              )}
            </div>
          </Field>
          <ToggleCard
            title="Auto-create hosts from labels"
            description={<>Containers with <span className="mono">relay.host=…</span> get a proxy host automatically</>}
            checked={s.autoCreate}
            disabled={disabled}
            onChange={(v) => update({ autoCreate: v })}
          />
          <ToggleCard
            title="Remove hosts when container is removed"
            description="Only for hosts that were auto-created"
            checked={s.autoRemove}
            disabled={disabled}
            onChange={(v) => update({ autoRemove: v })}
          />
          <ToggleCard
            title="Keep upstreams in sync"
            description="Update the upstream IP and port when a container is recreated"
            checked={s.keepInSync}
            disabled={disabled}
            onChange={(v) => update({ keepInSync: v })}
          />
          <div className="grid-2">
            <Field label="Certificate for new hosts" hint="Auto picks a valid certificate that covers the domain">
              <Select
                value={s.defaultCertificateId ?? ''}
                disabled={disabled}
                placeholder="Auto (matching wildcard)"
                options={(certs.data ?? []).map((c) => ({ value: c.id, label: `${c.name}${c.status !== 'valid' ? ` · ${c.status}` : ''}` }))}
                onChange={(v) => update({ defaultCertificateId: v || undefined })}
              />
            </Field>
            <Field label="Access list for new hosts" hint="Labels can override with relay.access">
              <Select
                value={s.defaultAccessListId ?? ''}
                disabled={disabled}
                placeholder="Host default"
                options={(accessLists.data ?? []).map((l) => ({ value: l.id, label: l.name }))}
                onChange={(v) => update({ defaultAccessListId: v || undefined })}
              />
            </Field>
          </div>
        </div>
      </Card>

      <div className="ops-label-ref">
        <div className="ops-label-title">Label reference</div>
        <pre>
          {LABEL_REFERENCE.map(([l, c]) => (
            <div key={l}>
              {l.padEnd(c ? 24 : 0)}
              {c && <span className="cm">{c}</span>}
            </div>
          ))}
        </pre>
      </div>

      {s.enabled && st?.connected && (
        <Card
          title="Discovered, not yet proxied"
          actions={
            <div className="row gap-10">
              <span className="small muted" style={{ fontWeight: 400 }}>{discovered.length}</span>
              {canWrite && discovered.some((c) => c.http) && (
                <Button size="sm" onClick={() => setDialog({ open: true })}>Create hosts…</Button>
              )}
            </div>
          }
        >
          {containers.isLoading ? (
            <div className="card-body"><Skeleton height={60} /></div>
          ) : discovered.length === 0 ? (
            <div className="ops-list-row"><span className="muted small">Every running web container is already proxied.</span></div>
          ) : (
            discovered.map((c) => {
              const matched = matchBackend(c, backends.data ?? [])
              return (
                <div key={c.id} className="ops-list-row">
                  <span className="ops-dot ok" title={c.state} />
                  <span className="ops-name" title={c.image}>
                    {c.name} <span className="faint">· {c.ip}{c.suggestedPort ? `:${c.suggestedPort}` : ''}</span>
                  </span>
                  {canWrite && (
                    <div className="row gap-4">
                      {(!matched || !c.http) && c.http ? (
                        <Button size="sm" variant="ghost" style={{ color: 'var(--ink)' }} onClick={() => setDialog({ open: true, preselect: [c.id] })}>Create host</Button>
                      ) : null}
                      {(backends.data?.length ?? 0) > 0 && (matched || !c.http) && (
                        <Menu
                          trigger={<Button size="sm" variant="ghost" style={{ color: 'var(--ink)' }}>Add to backend ▾</Button>}
                          items={[
                            { header: `${c.name} · port ${c.suggestedPort || '—'}` },
                            ...(backends.data ?? []).map((b) => ({ label: b.name + (b.id === matched?.id ? ' · suggested' : ''), onSelect: () => addToBackend(c, b) })),
                            ...(c.http ? (['separator', { label: 'Create host instead', onSelect: () => setDialog({ open: true, preselect: [c.id] }) }] as const) : []),
                          ]}
                        />
                      )}
                    </div>
                  )}
                </div>
              )
            })
          )}
        </Card>
      )}

      <DockerSuggestionsDialog open={dialog.open} preselect={dialog.preselect} onClose={() => setDialog({ open: false })} />

      <Dialog
        open={remoteOpen}
        onClose={() => setRemoteOpen(false)}
        title="Connect remote Docker host"
        description="Relay talks to the Docker API over TCP. Expose it only on a trusted network, ideally through a read-only socket proxy."
        footer={
          <>
            <Button onClick={() => setRemoteOpen(false)}>Cancel</Button>
            <Button
              variant="primary"
              loading={save.isPending}
              disabled={!/^(tcp|https?):\/\/.+/.test(remote.trim())}
              onClick={async () => {
                await update({ endpoint: remote.trim(), enabled: true })
                setRemoteOpen(false)
                retry()
              }}
            >
              Connect
            </Button>
          </>
        }
      >
        <Field label="Docker host" hint="e.g. tcp://192.168.1.20:2375 or tcp://docker-socket-proxy:2375">
          <Input mono autoFocus value={remote} onChange={(e) => setRemote(e.target.value)} />
        </Field>
      </Dialog>
    </>
  )
}
