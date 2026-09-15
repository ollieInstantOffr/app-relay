// Owner: slice ops. Settings → Docker discovery (design 15b, 22b) — multiple Docker hosts.
import { useEffect, useMemo, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { api } from '../../lib/api'
import { keys, useContainers, useEntities, useRole, useSaveSettings, useSettings } from '../../lib/queries'
import type { Backend, Container, DockerEndpoint, DockerSettings as DockerSettingsT } from '../../lib/types'
import { Badge, Button, Card, Checkbox, Field, Input, Menu, SectionHeader, Select, Skeleton, Toggle, ToggleCard, cx, useToast } from '../../components/ui'
import DockerSuggestionsDialog from '../docker/DockerSuggestionsDialog'
import DockerHostsCard from '../docker/DockerHostsCard'
import { applyNowAction, useDockerStatus } from '../docker/ops'
import '../docker/ops.css'

const LABEL_REFERENCE = [
  ['relay.host=grafana.home.lan', ''],
  ['relay.port=3000', ''],
  ['relay.tls=auto', '# letsencrypt | auto | off'],
  ['relay.access=lan-only', ''],
  ['relay.backend=web-app', '# join an HAProxy backend instead'],
]

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

  const [dialog, setDialog] = useState<{ open: boolean; preselect?: string[] }>({ open: false })
  const [showStopped, setShowStopped] = useState(true)
  const [pattern, setPattern] = useState('')

  useEffect(() => {
    if (settings.data) setPattern(settings.data.domainPattern)
  }, [settings.data])

  const refreshDocker = () => {
    qc.invalidateQueries({ queryKey: ['docker'] })
    qc.invalidateQueries({ queryKey: keys.containers })
  }

  const update = async (patch: Partial<DockerSettingsT>) => {
    if (!settings.data) return
    try {
      await save.mutateAsync({ ...settings.data, ...patch })
      refreshDocker()
    } catch (err) {
      toast.error(err, 'Could not save Docker settings')
    }
  }

  /** Save the endpoint list; errors propagate so dialogs can show field errors. */
  const saveEndpoints = async (endpoints: DockerEndpoint[]) => {
    if (!settings.data) return
    await save.mutateAsync({ ...settings.data, endpoints })
    refreshDocker()
    // Reconnect happens in the background; poll status shortly after.
    setTimeout(refreshDocker, 1500)
  }

  const addToBackend = async (c: Container, b: Backend) => {
    try {
      await api.post(`/api/docker/backends/${b.id}/servers`, { endpointId: c.endpointId, containerId: c.id, port: c.suggestedPort })
      qc.invalidateQueries({ queryKey: keys.entities('backends') })
      qc.invalidateQueries({ queryKey: keys.pending })
      qc.invalidateQueries({ queryKey: ['docker'] })
      toast.show({ kind: 'success', title: `${c.name} added to backend ${b.name}`, message: `${c.upstreamHost}:${c.suggestedPort} · added to pending changes`, actions: [applyNowAction] })
    } catch (err) {
      toast.error(err, `Could not add ${c.name} to ${b.name}`)
    }
  }

  const endpoints = settings.data?.endpoints ?? []
  const multiHost = endpoints.filter((e) => e.enabled).length > 1

  const all = useMemo(() => {
    const list = containers.data ?? []
    return list.filter((c) => !c.hostId && !c.backendId && (c.http || matchBackend(c, backends.data ?? [])))
  }, [containers.data, backends.data])
  const stoppedCount = all.filter((c) => c.state !== 'running').length
  const discovered = showStopped ? all : all.filter((c) => c.state === 'running')

  const groups = useMemo(() => {
    const order = new Map(endpoints.map((e, i) => [e.id, i]))
    const byEp = new Map<string, Container[]>()
    for (const c of discovered) byEp.set(c.endpointId, [...(byEp.get(c.endpointId) ?? []), c])
    return [...byEp.entries()]
      .sort(([a], [b]) => (order.get(a) ?? 99) - (order.get(b) ?? 99))
      .map(([id, list]) => ({
        id,
        name: list[0]?.endpointName || id,
        address: st?.endpoints?.find((e) => e.id === id)?.upstreamAddress ?? '',
        list: [...list].sort((a, b) => Number(b.state === 'running') - Number(a.state === 'running') || a.name.localeCompare(b.name)),
      }))
  }, [discovered, endpoints, st])

  if (settings.isLoading || !settings.data) {
    return (
      <>
        <SectionHeader title="Docker discovery" description="Watch Docker hosts and suggest containers as upstreams. Optionally auto-create hosts from labels." />
        <Skeleton height={220} />
      </>
    )
  }
  const s = settings.data
  const disabled = !isAdmin

  return (
    <>
      <SectionHeader
        title="Docker discovery"
        description="Watch Docker hosts and suggest containers as upstreams. Optionally auto-create hosts from labels."
        actions={
          <label className="ops-header-toggle">
            Enabled
            <Toggle checked={s.enabled} disabled={disabled} onChange={(v) => update({ enabled: v })} label="Docker discovery enabled" />
          </label>
        }
      />

      <DockerHostsCard settings={s} status={st} readOnly={disabled} onSave={saveEndpoints} />

      <Card title="Defaults for new hosts">
        <div className="card-body col gap-14">
          <ToggleCard
            title="Keep upstreams in sync"
            description="Update the upstream address and port when a container is recreated"
            checked={s.keepInSync}
            disabled={disabled}
            onChange={(v) => update({ keepInSync: v })}
          />
          <Field label="Domain pattern" hint={<>Used for suggestions, e.g. <span className="mono">{(pattern || '{name}.home.lan').replaceAll('{name}', 'grafana')}</span></>} error={pattern && !pattern.includes('{name}') ? 'must contain {name}' : undefined}>
            <Input
              mono
              value={pattern}
              disabled={disabled}
              invalid={!!pattern && !pattern.includes('{name}')}
              onChange={(e) => setPattern(e.target.value)}
              onBlur={() => pattern.trim() !== s.domainPattern && pattern.includes('{name}') && update({ domainPattern: pattern.trim() })}
              onKeyDown={(e) => e.key === 'Enter' && (e.target as HTMLInputElement).blur()}
            />
          </Field>
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
            <div className="row gap-12">
              {stoppedCount > 0 && <Checkbox checked={showStopped} onChange={setShowStopped} label={<span className="small" style={{ fontWeight: 400 }}>Show stopped</span>} />}
              <span className="small muted" style={{ fontWeight: 400 }}>{discovered.length}</span>
              {canWrite && discovered.some((c) => c.upstreamHost || c.linkOnStart) && (
                <Button size="sm" onClick={() => setDialog({ open: true })}>Create hosts…</Button>
              )}
            </div>
          }
        >
          {containers.isLoading ? (
            <div className="card-body"><Skeleton height={60} /></div>
          ) : discovered.length === 0 ? (
            <div className="ops-list-row"><span className="muted small">Every {showStopped ? '' : 'running '}web container is already proxied.</span></div>
          ) : (
            groups.map((g) => (
              <div key={g.id}>
                {multiHost && (
                  <div className="ops-group-head">
                    {g.name}
                    {g.address && <span className="mono">· {g.address}</span>}
                    <span style={{ marginLeft: 'auto', fontWeight: 400 }}>{g.list.length}</span>
                  </div>
                )}
                {g.list.map((c) => {
                  const matched = matchBackend(c, backends.data ?? [])
                  const running = c.state === 'running'
                  const reachable = !!c.upstreamHost
                  const where = reachable
                    ? `${c.upstreamHost}:${c.suggestedPort || 'port?'}`
                    : c.linkOnStart ? `${c.suggestedPort ? `:${c.suggestedPort} · ` : ''}linked when it starts` : c.reason || 'not reachable'
                  return (
                    <div key={`${c.endpointId}/${c.id}`} className={cx('ops-list-row', !running && 'stopped')}>
                      <span className={`ops-dot ${running ? 'ok' : ''}`} title={c.state} />
                      <span className="ops-name" title={`${c.image}${c.reason ? ` · ${c.reason}` : ''}`}>
                        {c.name} <span className="faint">· {where}</span>
                      </span>
                      {!running && <Badge>stopped</Badge>}
                      {canWrite && (reachable || c.linkOnStart) && (
                        <div className="row gap-4">
                          {c.http && !matched ? (
                            <Button size="sm" variant="ghost" style={{ color: 'var(--ink)' }} onClick={() => setDialog({ open: true, preselect: [c.id] })}>Create host</Button>
                          ) : null}
                          {(backends.data?.length ?? 0) > 0 && reachable && c.suggestedPort > 0 && (matched || !c.http) && (
                            <Menu
                              trigger={<Button size="sm" variant="ghost" style={{ color: 'var(--ink)' }}>Add to backend ▾</Button>}
                              items={[
                                { header: `${c.name} · ${where}` },
                                ...(backends.data ?? []).map((b) => ({ label: b.name + (b.id === matched?.id ? ' · suggested' : ''), onSelect: () => addToBackend(c, b) })),
                                ...(c.http ? (['separator', { label: 'Create host instead', onSelect: () => setDialog({ open: true, preselect: [c.id] }) }] as const) : []),
                              ]}
                            />
                          )}
                        </div>
                      )}
                    </div>
                  )
                })}
              </div>
            ))
          )}
        </Card>
      )}

      <DockerSuggestionsDialog open={dialog.open} preselect={dialog.preselect} onClose={() => setDialog({ open: false })} />
    </>
  )
}
