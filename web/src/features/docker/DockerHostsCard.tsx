// Owner: slice ops. "Docker hosts" card of Settings → Docker discovery.
import { useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { api } from '../../lib/api'
import { keys } from '../../lib/queries'
import type { DockerEndpoint, DockerEndpointType, DockerSettings } from '../../lib/types'
import { Badge, Button, Callout, Card, CodeBlock, ConfirmDialog, CopyButton, IconButton, Menu, cx, useToast } from '../../components/ui'
import DockerHostDialog, { newEndpoint } from './DockerHostDialog'
import { containerCounts, opsKeys, type DockerEndpointStatus, type DockerStatus, type DockerTestResult } from './ops'

export const TYPE_LABEL: Record<DockerEndpointType, string> = { socket: 'Local socket', tcp: 'TCP', tls: 'TCP + TLS', ssh: 'SSH' }

const SOCKET_PROXY_COMPOSE = `services:
  docker-socket-proxy:
    image: tecnativa/docker-socket-proxy:latest
    restart: unless-stopped
    environment:
      CONTAINERS: 1
      EVENTS: 1
      NETWORKS: 1
      INFO: 1
      VERSION: 1
      POST: 0
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    ports:
      - "192.168.1.20:2375:2375"   # bind to the host's LAN IP only`

function statusOf(s: DockerSettings, ep: DockerEndpoint, es: DockerEndpointStatus | undefined): { tone: 'ok' | 'danger' | 'muted'; text: string } {
  if (!s.enabled) return { tone: 'muted', text: 'discovery disabled' }
  if (!ep.enabled) return { tone: 'muted', text: 'disabled' }
  if (!es) return { tone: 'muted', text: 'checking…' }
  if (es.connected) return { tone: 'ok', text: `connected · ${es.version} · ${containerCounts(es.containers, es.running)}` }
  if (es.error) return { tone: 'danger', text: es.error }
  return { tone: 'muted', text: 'connecting…' }
}

export default function DockerHostsCard({ settings, status, readOnly, onSave }: {
  settings: DockerSettings
  status: DockerStatus | undefined
  readOnly: boolean
  /** Persist the full endpoint list (throws ApiError on validation errors). */
  onSave: (endpoints: DockerEndpoint[]) => Promise<void>
}) {
  const qc = useQueryClient()
  const toast = useToast()
  const [dialog, setDialog] = useState<{ ep: DockerEndpoint; index: number; presented?: string } | null>(null)
  const [removing, setRemoving] = useState<DockerEndpoint | null>(null)
  const [busy, setBusy] = useState('')
  const eps = settings.endpoints ?? []
  const esOf = (id: string) => status?.endpoints?.find((e) => e.id === id)

  const openAdd = () => setDialog({ ep: newEndpoint(settings, eps.some((e) => e.type === 'socket')), index: eps.length })

  const saveAt = async (ep: DockerEndpoint, index: number) => {
    const next = [...eps]
    if (index < eps.length) next[index] = ep
    else next.push(ep)
    await onSave(next)
    toast.success(index < eps.length ? `Docker host ${ep.name} saved` : `Docker host ${ep.name} added`, 'Relay reconnects in the background')
  }

  const test = async (ep: DockerEndpoint) => {
    setBusy(ep.id)
    try {
      const r = await api.post<DockerTestResult>('/api/docker/endpoints/test', { endpoint: ep })
      if (r.ok) toast.success(`${ep.name} connected`, `Docker ${r.version} · ${containerCounts(r.containers, r.running)}`)
      else toast.error(new Error(r.error || 'connection failed'), `${ep.name}: connection failed`)
    } catch (err) {
      toast.error(err, `Could not test ${ep.name}`)
    } finally {
      setBusy('')
    }
  }

  const retry = async (ep: DockerEndpoint) => {
    setBusy(ep.id)
    try {
      const next = await api.post<DockerStatus>('/api/docker/retry', { endpointId: ep.id })
      qc.setQueryData(opsKeys.dockerStatus, next)
      qc.invalidateQueries({ queryKey: keys.containers })
      const es = next.endpoints.find((e) => e.id === ep.id)
      if (es?.connected) toast.success(`${ep.name} connected`, `Docker ${es.version} · ${containerCounts(es.containers, es.running)}`)
    } catch (err) {
      toast.error(err, 'Retry failed')
    } finally {
      setBusy('')
    }
  }

  const toggle = async (ep: DockerEndpoint, enabled: boolean) => {
    try {
      await onSave(eps.map((e) => (e.id === ep.id ? { ...e, enabled } : e)))
    } catch (err) {
      toast.error(err, `Could not ${enabled ? 'enable' : 'disable'} ${ep.name}`)
    }
  }

  return (
    <>
      <Card
        title="Docker hosts"
        sub={eps.length > 1 ? `${eps.length} hosts` : undefined}
        actions={!readOnly && <Button variant="ghost" size="sm" icon="plus" onClick={openAdd}>Add Docker host</Button>}
      >
        {eps.length === 0 && (
          <div className="card-body small muted">
            No Docker hosts yet. Add the local Docker socket or a remote host (socket proxy, TLS or SSH) to discover containers.
          </div>
        )}
        {eps.map((ep, i) => {
          const es = esOf(ep.id)
          const line = statusOf(settings, ep, es)
          const failing = settings.enabled && ep.enabled && line.tone === 'danger'
          const mismatch = failing && ep.type === 'ssh' && !!ep.sshKnownHost && !!es?.sshFingerprint && es.sshFingerprint !== ep.sshKnownHost
          return (
            <div key={ep.id || i}>
              <div className={cx('ops-host-row', failing && 'failed', !ep.enabled && 'off')}>
                <span className={`ops-dot ${line.tone === 'muted' ? '' : line.tone}`} />
                <div className="grow" style={{ minWidth: 0 }}>
                  <div className="row gap-8 wrap">
                    <span className="medium">{ep.name}</span>
                    <span className="ops-kind">{TYPE_LABEL[ep.type] ?? ep.type}</span>
                    {ep.autoCreate && <Badge tone="outline">auto-create</Badge>}
                    {ep.autoRemove && <Badge tone="outline">auto-remove</Badge>}
                    {!ep.enabled && <Badge>disabled</Badge>}
                  </div>
                  <div className="ops-host-url" title={ep.url}>
                    {ep.url}
                    {es?.upstreamAddress ? <span> · upstreams via {es.upstreamAddress}</span> : null}
                  </div>
                  <div className={`ops-host-status ${line.tone}`} title={line.text}><span>{line.text}</span></div>
                </div>
                {mismatch ? (
                  <Button size="sm" disabled={readOnly} onClick={() => setDialog({ ep, index: i, presented: es?.sshFingerprint })}>Review host key</Button>
                ) : failing ? (
                  <Button size="sm" variant="ghost" icon="reload" loading={busy === ep.id} onClick={() => retry(ep)}>Retry</Button>
                ) : null}
                <Menu
                  trigger={<IconButton icon="more" bare label={`${ep.name} actions`} />}
                  items={[
                    { header: ep.name },
                    { label: 'Edit…', icon: 'edit', disabled: readOnly, onSelect: () => setDialog({ ep, index: i, presented: es?.sshFingerprint }) },
                    { label: 'Test connection', icon: 'check', disabled: readOnly || busy === ep.id, onSelect: () => test(ep) },
                    { label: ep.enabled ? 'Disable' : 'Enable', icon: 'power', disabled: readOnly, onSelect: () => toggle(ep, !ep.enabled) },
                    'separator',
                    { label: 'Remove…', icon: 'trash', danger: true, disabled: readOnly, onSelect: () => setRemoving(ep) },
                  ]}
                />
              </div>
              {failing && es?.socketMissing && (
                <div className="ops-host-extra col gap-8">
                  <span className="small muted">Mount the socket into the Relay container:</span>
                  <CodeBlock code={'volumes:\n  - /var/run/docker.sock:/var/run/docker.sock:ro'} />
                </div>
              )}
            </div>
          )
        })}
      </Card>

      <details className="ops-help">
        <summary>Connecting a remote Docker host</summary>
        <div className="ops-help-body">
          <div>
            <div className="medium">Socket proxy over TCP (recommended)</div>
            <div className="muted">Run docker-socket-proxy on the remote host, then add <span className="mono">tcp://192.168.1.20:2375</span> as a TCP host. It only allows reading containers, events, networks and version info.</div>
          </div>
          <div style={{ position: 'relative' }}>
            <CodeBlock code={SOCKET_PROXY_COMPOSE} />
            <div style={{ position: 'absolute', top: 8, right: 8 }}><CopyButton text={SOCKET_PROXY_COMPOSE} /></div>
          </div>
          <div>
            <div className="medium">SSH</div>
            <ul>
              <li>Relay logs in with a dedicated private key (no passphrase); put its public key in <span className="mono">~/.ssh/authorized_keys</span>.</li>
              <li>The user must be in the <span className="mono">docker</span> group: <span className="mono">sudo usermod -aG docker &lt;user&gt;</span>.</li>
              <li>The host key is trusted on first connect; Relay refuses to connect if it changes.</li>
            </ul>
          </div>
          <div>
            <div className="medium">Upstream addresses</div>
            <div className="muted">Container IPs of remote hosts aren't reachable, so Relay proxies to <span className="mono">address:published port</span>. Containers without a published port on a remote host can't be proxied.</div>
          </div>
          <Callout tone="warn" title="Security note">
            Docker API access exposes every container's environment and is root-equivalent with write access. Bind the proxy to a private interface and firewall it to Relay's host.
          </Callout>
        </div>
      </details>

      {dialog && (
        <DockerHostDialog
          open
          initial={dialog.ep}
          index={dialog.index}
          presentedFingerprint={dialog.presented}
          onClose={() => setDialog(null)}
          onSave={(ep) => saveAt(ep, dialog.index)}
        />
      )}

      <ConfirmDialog
        open={!!removing}
        onClose={() => setRemoving(null)}
        onConfirm={async () => {
          if (!removing) return
          try {
            await onSave(eps.filter((e) => e.id !== removing.id))
            toast.success(`Docker host ${removing.name} removed`, 'Hosts created from its containers stay and stop syncing')
          } catch (err) {
            toast.error(err, `Could not remove ${removing.name}`)
          }
        }}
        danger
        title={`Remove Docker host ${removing?.name ?? ''}?`}
        message="Relay stops watching its containers. Proxy hosts and backend servers created from them are kept but no longer kept in sync."
        confirmLabel="Remove"
      />
    </>
  )
}
