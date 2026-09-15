// Generate API token dialog + one-time reveal (design 28d). Used by the MCP
// settings (surface "mcp") and Users & access (surface "rest").
import { useEffect, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Button, Callout, Checkbox, ChipsInput, CopyButton, Dialog, Field, Input, RadioCard, Select } from '../../components/ui'
import { api } from '../../lib/api'
import { date } from '../../lib/format'
import type { ApiToken } from '../../lib/types'
import { describeError, fieldErrors } from './authApi'
import './auth.css'

const EXPIRY = [
  { value: '30', label: '30 days' },
  { value: '90', label: '90 days' },
  { value: '365', label: '1 year' },
  { value: '0', label: 'Never' },
]

export default function CreateTokenDialog(props: {
  open: boolean
  onClose: () => void
  surface: 'mcp' | 'rest'
  onCreated?: (token: ApiToken) => void
}) {
  const { open, onClose, surface, onCreated } = props
  const qc = useQueryClient()
  const [name, setName] = useState('')
  const [scope, setScope] = useState<'read' | 'write'>('read')
  const [expires, setExpires] = useState('90')
  const [limit, setLimit] = useState<string[]>([])
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [created, setCreated] = useState<ApiToken | null>(null)
  const [stored, setStored] = useState(false)

  useEffect(() => {
    if (!open) return
    setName('')
    setScope('read')
    setExpires('90')
    setLimit([])
    setErrors({})
    setError('')
    setCreated(null)
    setStored(false)
  }, [open])

  const surfaceLabel = surface === 'mcp' ? 'MCP' : 'REST API'

  const create = async () => {
    if (!name.trim() || busy) return
    setBusy(true)
    setErrors({})
    setError('')
    try {
      const t = await api.post<ApiToken>('/api/tokens', {
        name: name.trim(),
        scope,
        surfaces: [surface],
        expiresInDays: Number(expires),
        limitTo: surface === 'mcp' ? limit : [],
      })
      setCreated(t)
      onCreated?.(t)
      qc.invalidateQueries({ queryKey: ['tokens'] })
    } catch (err) {
      const f = fieldErrors(err)
      if (Object.keys(f).length) setErrors(f)
      else setError(describeError(err))
    } finally {
      setBusy(false)
    }
  }

  if (created) {
    const days = EXPIRY.find((e) => e.value === expires)?.label
    return (
      <Dialog
        open={open}
        onClose={onClose}
        dismissable={false}
        width={500}
        icon="check"
        iconTone="ok"
        title="Token created"
        description="Copy it now — Relay stores only a hash and can't show it again."
        footer={
          <>
            <Checkbox checked={stored} onChange={setStored} label="I've stored it somewhere safe" />
            <div className="spacer" />
            <Button variant="primary" disabled={!stored} onClick={onClose}>
              Done
            </Button>
          </>
        }
      >
        <div className="token-reveal">
          <code>{created.token}</code>
          <CopyButton text={created.token ?? ''} />
        </div>
        <div className="kv">
          <span className="k">Name</span>
          <span>{created.name}</span>
          <span className="k">Expires</span>
          <span>{created.expiresAt ? `${days} · ${date(created.expiresAt)}` : 'Never'}</span>
          <span className="k">Scope</span>
          <span>
            {created.scope === 'read' ? 'read-only' : 'read + write'} · {created.surfaces.map((s) => (s === 'mcp' ? 'MCP' : 'REST')).join(' + ')}
          </span>
          {created.limitTo.length > 0 && (
            <>
              <span className="k">Limited to</span>
              <span className="mono small">{created.limitTo.join(' · ')}</span>
            </>
          )}
        </div>
      </Dialog>
    )
  }

  return (
    <Dialog
      open={open}
      onClose={onClose}
      width={500}
      title={`Generate ${surfaceLabel} token`}
      description={
        surface === 'mcp'
          ? 'For an AI assistant connecting to /mcp. Tool permissions still apply to every call.'
          : 'For scripts and integrations calling the REST API with an Authorization: Bearer header.'
      }
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" loading={busy} disabled={!name.trim()} onClick={create}>
            Generate token
          </Button>
        </>
      }
    >
      <Field label="Name" htmlFor="token-name" error={errors.name} hint={surface === 'mcp' ? 'The client using it, e.g. Claude Desktop' : 'Where it’s used, e.g. Home Assistant'}>
        <Input id="token-name" autoFocus value={name} invalid={!!errors.name} onChange={(e) => setName(e.target.value)} onKeyDown={(e) => e.key === 'Enter' && create()} />
      </Field>
      <div className="field">
        <label className="field-label">Scope</label>
        <div className="grid-2">
          <RadioCard
            selected={scope === 'read'}
            onSelect={() => setScope('read')}
            title="Read-only"
            description={surface === 'mcp' ? 'List hosts, query logs, check status' : 'GET requests only · acts as a viewer'}
          />
          <RadioCard
            selected={scope === 'write'}
            onSelect={() => setScope('write')}
            title="Read + write"
            description={surface === 'mcp' ? 'Write tools follow their permission (confirm, allow…)' : 'Change configuration · acts as an editor'}
          />
        </div>
        {errors.scope && <div className="field-error">{errors.scope}</div>}
      </div>
      <Field label="Expires" error={errors.expiresInDays}>
        <Select value={expires} onChange={setExpires} options={EXPIRY} />
      </Field>
      {surface === 'mcp' && (
        <Field label="Limit to (optional)" hint="Domain globs like *.home.lan or backend names · empty = everything" error={errors.limitTo}>
          <ChipsInput values={limit} onChange={setLimit} placeholder={limit.length ? 'add another…' : '*.home.lan, web-app…'} />
        </Field>
      )}
      {error && <Callout tone="danger">{error}</Callout>}
    </Dialog>
  )
}
