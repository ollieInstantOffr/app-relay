// Owner: slice ops. Add / edit one Docker host (endpoint) — Settings → Docker discovery.
import { useEffect, useState } from 'react'
import { api, ApiError, errorMessage } from '../../lib/api'
import type { DockerEndpoint, DockerEndpointType } from '../../lib/types'
import { Button, Callout, Dialog, Field, Input, Segmented, Textarea, ToggleCard } from '../../components/ui'
import { containerCounts, type DockerTestResult } from './ops'

const TYPE_OPTIONS: { value: DockerEndpointType; label: string }[] = [
  { value: 'socket', label: 'Local socket' },
  { value: 'tcp', label: 'TCP' },
  { value: 'tls', label: 'TCP + TLS' },
  { value: 'ssh', label: 'SSH' },
]

const DEFAULT_URL: Record<DockerEndpointType, string> = {
  socket: 'unix:///var/run/docker.sock',
  tcp: 'tcp://',
  tls: 'tcp://',
  ssh: '',
}

function urlHost(url: string): string {
  try {
    const u = new URL(url.replace(/^tcp:/, 'http:').replace(/^ssh:/, 'http:'))
    return u.hostname.replace(/^\[|\]$/g, '')
  } catch {
    return ''
  }
}

function isLoopback(host: string): boolean {
  return host === 'localhost' || host === '::1' || /^127\./.test(host)
}

/** ssh://user@host:22 ↔ user@host:22 */
function sshTarget(url: string): string {
  return url.replace(/^ssh:\/\//, '').replace(/\/+$/, '')
}
function sshURL(target: string): string {
  const t = target.trim()
  return t ? `ssh://${t.replace(/^ssh:\/\//, '')}` : ''
}

/** Suggested address for upstreams: the URL host, except for local endpoints. */
function autoUpstream(type: DockerEndpointType, url: string): string {
  if (type === 'socket') return ''
  const h = urlHost(url)
  return isLoopback(h) ? '' : h
}

function slug(s: string): string {
  return s.toLowerCase().replace(/[^a-z0-9._-]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 40)
}

export function newEndpoint(defaults: { autoCreate: boolean; autoRemove: boolean }, hasLocal: boolean): DockerEndpoint {
  return {
    id: '', name: hasLocal ? '' : 'local', type: hasLocal ? 'tcp' : 'socket', url: hasLocal ? 'tcp://' : DEFAULT_URL.socket, upstreamAddress: '',
    enabled: true, autoCreate: defaults.autoCreate, autoRemove: defaults.autoRemove,
  }
}

/** Stored secret (write-only) with a Replace action, or a textarea. */
function SecretField({ label, hint, stored, value, onChange, placeholder, error }: {
  label: string; hint?: string; stored: boolean; value: string; onChange: (v: string) => void; placeholder: string; error?: string
}) {
  const [replacing, setReplacing] = useState(false)
  if (stored && !replacing && !value) {
    return (
      <Field label={label} error={error}>
        <div className="ops-boxed-row">
          <span className="ops-dot ok" />
          <span className="grow small">Stored <span className="faint">· not shown again</span></span>
          <Button size="sm" variant="ghost" onClick={() => setReplacing(true)}>Replace</Button>
        </div>
      </Field>
    )
  }
  return (
    <Field label={label} hint={hint} error={error}>
      <Textarea mono rows={4} value={value} invalid={!!error} placeholder={placeholder} spellCheck={false} onChange={(e) => onChange(e.target.value)} />
    </Field>
  )
}

export default function DockerHostDialog({ open, onClose, initial, index, onSave, presentedFingerprint }: {
  open: boolean
  onClose: () => void
  /** SSH host key the server presented on the last (failed) connect. */
  presentedFingerprint?: string
  /** Endpoint to edit (id set) or a new draft from newEndpoint(). */
  initial: DockerEndpoint
  /** Position in the endpoints list (for mapping 422 field errors). */
  index: number
  /** Persist the endpoint; throws ApiError with fields on validation errors. */
  onSave: (ep: DockerEndpoint) => Promise<void>
}) {
  const [draft, setDraft] = useState<DockerEndpoint>(initial)
  const [upstreamAuto, setUpstreamAuto] = useState(true)
  const [nameAuto, setNameAuto] = useState(true)
  const [test, setTest] = useState<DockerTestResult | null>(null)
  const [testing, setTesting] = useState(false)
  const [saving, setSaving] = useState(false)
  const [errors, setErrors] = useState<Record<string, string>>({})
  const editing = !!initial.id

  useEffect(() => {
    if (!open) return
    setDraft(initial)
    setUpstreamAuto(!initial.id || initial.upstreamAddress === '' || initial.upstreamAddress === autoUpstream(initial.type, initial.url))
    setNameAuto(!initial.id && !initial.name)
    setTest(null)
    setErrors({})
  }, [open, initial])

  const set = (patch: Partial<DockerEndpoint>) => {
    setDraft((d) => {
      const next = { ...d, ...patch }
      if ((patch.url !== undefined || patch.type !== undefined) && upstreamAuto) next.upstreamAddress = autoUpstream(next.type, next.url)
      if (patch.url !== undefined && nameAuto) {
        const h = urlHost(next.url)
        next.name = next.type === 'socket' ? 'local' : h && !isLoopback(h) ? slug(h) : next.name
      }
      return next
    })
    setTest(null)
  }

  const changeType = (type: DockerEndpointType) => {
    const url = type === 'socket' ? DEFAULT_URL.socket : draft.type === 'socket' || (type === 'ssh') !== (draft.type === 'ssh') ? DEFAULT_URL[type] : draft.url
    set({ type, url })
  }

  const payload = (): DockerEndpoint => ({ ...draft, name: draft.name.trim().toLowerCase(), url: draft.url.trim(), upstreamAddress: draft.upstreamAddress.trim() })

  const runTest = async () => {
    setTesting(true)
    setErrors({})
    try {
      setTest(await api.post<DockerTestResult>('/api/docker/endpoints/test', { endpoint: payload() }))
    } catch (err) {
      if (err instanceof ApiError && err.fields) setErrors(err.fields)
      setTest({ ok: false, containers: 0, running: 0, error: errorMessage(err) })
    } finally {
      setTesting(false)
    }
  }

  const save = async () => {
    setSaving(true)
    setErrors({})
    const ep = payload()
    // Trust the fingerprint the user just saw in the test result.
    if (ep.type === 'ssh' && !ep.sshKnownHost && test?.ok && test.sshFingerprint) ep.sshKnownHost = test.sshFingerprint
    try {
      await onSave(ep)
      onClose()
    } catch (err) {
      if (err instanceof ApiError && err.fields) {
        const prefix = `endpoints.${index}.`
        const mine: Record<string, string> = {}
        for (const [k, v] of Object.entries(err.fields)) if (k.startsWith(prefix)) mine[k.slice(prefix.length)] = v
        setErrors(Object.keys(mine).length ? mine : { _: errorMessage(err) })
      } else {
        setErrors({ _: errorMessage(err) })
      }
    } finally {
      setSaving(false)
    }
  }

  const host = urlHost(draft.url)
  const presented = test ? test.sshFingerprint : presentedFingerprint
  const fingerprintChanged = draft.type === 'ssh' && !!draft.sshKnownHost && !!presented && presented !== draft.sshKnownHost
  const canSave = !!draft.name.trim() && /^[a-z]+:\/\/./.test(draft.url.trim())

  return (
    <Dialog
      open={open}
      onClose={onClose}
      width={640}
      icon="docker"
      title={editing ? `Edit Docker host ${initial.name}` : 'Add Docker host'}
      description="Relay watches containers on this host and proxies to their published ports."
      footer={
        <>
          <Button icon="check" loading={testing} disabled={!canSave} onClick={runTest} style={{ marginRight: 'auto' }}>Test connection</Button>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" loading={saving} disabled={!canSave} onClick={save}>{editing ? 'Save' : 'Add Docker host'}</Button>
        </>
      }
    >
      <div className="col gap-16">
        <div className="grid-2">
          <Field label="Name" error={errors.name} hint="Shown next to containers, e.g. nas · grafana">
            <Input mono value={draft.name} invalid={!!errors.name} placeholder="nas" onChange={(e) => { setNameAuto(false); setDraft({ ...draft, name: e.target.value.toLowerCase() }) }} />
          </Field>
          <Field label="Connection" error={errors.type}>
            <Segmented value={draft.type} onChange={changeType} options={TYPE_OPTIONS} />
          </Field>
        </div>

        {draft.type === 'ssh' ? (
          <Field label="SSH target" error={errors.url} hint="user@host:port — the user must be in the docker group on that host">
            <Input mono value={sshTarget(draft.url)} invalid={!!errors.url} placeholder="docker@192.168.1.20:22" onChange={(e) => set({ url: sshURL(e.target.value) })} />
          </Field>
        ) : (
          <Field
            label={draft.type === 'socket' ? 'Socket' : 'Docker API URL'}
            error={errors.url}
            hint={
              draft.type === 'socket' ? 'Mount the socket into the Relay container (read-only)'
                : draft.type === 'tcp' ? 'Docker API or docker-socket-proxy on port 2375 — unencrypted, use only on a trusted network'
                  : 'Docker daemon with --tlsverify, usually port 2376'
            }
          >
            <Input mono value={draft.url} invalid={!!errors.url} placeholder={draft.type === 'socket' ? DEFAULT_URL.socket : draft.type === 'tls' ? 'tcp://192.168.1.20:2376' : 'tcp://192.168.1.20:2375'} onChange={(e) => set({ url: e.target.value })} />
          </Field>
        )}

        {draft.type === 'tls' && (
          <>
            <Field label="CA certificate" error={errors.tlsCa} hint="PEM · leave empty to use the system trust store">
              <Textarea mono rows={3} value={draft.tlsCa ?? ''} invalid={!!errors.tlsCa} placeholder="-----BEGIN CERTIFICATE-----" spellCheck={false} onChange={(e) => set({ tlsCa: e.target.value })} />
            </Field>
            <div className="grid-2">
              <Field label="Client certificate" error={errors.tlsCert}>
                <Textarea mono rows={4} value={draft.tlsCert ?? ''} invalid={!!errors.tlsCert} placeholder="-----BEGIN CERTIFICATE-----" spellCheck={false} onChange={(e) => set({ tlsCert: e.target.value })} />
              </Field>
              <SecretField label="Client key" stored={!!draft.tlsKeySet} value={draft.tlsKey ?? ''} error={errors.tlsKey} placeholder="-----BEGIN PRIVATE KEY-----" onChange={(v) => set({ tlsKey: v })} />
            </div>
          </>
        )}

        {draft.type === 'ssh' && (
          <>
            <SecretField
              label="Private key"
              hint="Dedicated key without passphrase; add its public key to ~/.ssh/authorized_keys of that user"
              stored={!!draft.sshKeySet}
              value={draft.sshKey ?? ''}
              error={errors.sshKey}
              placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"
              onChange={(v) => set({ sshKey: v })}
            />
            <Field label="Host key">
              <div className="ops-boxed-row">
                <span className={`ops-dot ${draft.sshKnownHost ? 'ok' : ''}`} />
                <span className="grow small">
                  {draft.sshKnownHost ? <span className="mono">{draft.sshKnownHost}</span> : <span className="muted">Not trusted yet — the key presented on first connect is trusted (test to see it)</span>}
                </span>
                {draft.sshKnownHost && <Button size="sm" variant="ghost" onClick={() => set({ sshKnownHost: '' })}>Reset</Button>}
              </div>
            </Field>
          </>
        )}

        {draft.type !== 'socket' && (
          <Field
            label="Address for upstreams"
            error={errors.upstreamAddress}
            hint={
              draft.upstreamAddress.trim()
                ? <>Relay proxies to <span className="mono">{draft.upstreamAddress.trim()}:&lt;published port&gt;</span></>
                : isLoopback(host) ? 'Empty: this daemon runs next to Relay, containers are reached by their IP' : 'IP or hostname Relay uses to reach published container ports on this host'
            }
          >
            <Input mono value={draft.upstreamAddress} invalid={!!errors.upstreamAddress} placeholder={host && !isLoopback(host) ? host : '192.168.1.20'} onChange={(e) => { setUpstreamAuto(false); setDraft({ ...draft, upstreamAddress: e.target.value }) }} />
          </Field>
        )}

        {test && (test.ok ? (
          <Callout tone="ok" title={`Connected · Docker ${test.version} · ${containerCounts(test.containers, test.running)}`}>
            {test.sshFingerprint && !fingerprintChanged && (
              <>Host key <span className="mono">{test.sshFingerprint}</span>{draft.sshKnownHost ? ' matches the trusted key.' : ' — confirm it matches ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub on the host; it is trusted when you save.'}</>
            )}
          </Callout>
        ) : (
          <Callout tone="danger" title="Connection failed">{test.error}</Callout>
        ))}
        {fingerprintChanged && (
          <Callout
            tone="warn"
            title="SSH host key changed"
            actions={<Button size="sm" onClick={() => set({ sshKnownHost: presented })}>Trust new key</Button>}
          >
            Trusted <span className="mono">{draft.sshKnownHost}</span>, server presented <span className="mono">{presented}</span>. Only trust it if the host was reinstalled.
          </Callout>
        )}

        <div className="grid-2">
          <ToggleCard title="Auto-create hosts from labels" description={<><span className="mono">relay.host=…</span> on this host</>} checked={draft.autoCreate} onChange={(v) => setDraft({ ...draft, autoCreate: v })} />
          <ToggleCard title="Remove with container" description="Only auto-created hosts" checked={draft.autoRemove} onChange={(v) => setDraft({ ...draft, autoRemove: v })} />
        </div>
        {errors._ && <div className="field-error">{errors._}</div>}
      </div>
    </Dialog>
  )
}
