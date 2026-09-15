// Add / edit a DNS provider (credentials for DNS-01 challenges). The form is
// rendered from the /api/dns-providers/types schema.
import { useEffect, useMemo, useRef, useState } from 'react'
import { api, errorMessage } from '../../lib/api'
import { useRole, useSaveEntity } from '../../lib/queries'
import { ago } from '../../lib/format'
import type { DNSProvider } from '../../lib/types'
import { Button, Callout, Dialog, Field, IconButton, Input, PasswordInput, Select, useToast } from '../../components/ui'
import { fieldErrors, toastUnlessFields, useDNSProviderTypes, type DNSProviderField, type DNSTestResult } from './common'
import './certs.css'

interface EnvRow { id: number; key: string; value: string }

export default function DNSProviderDialog({ open, onClose, provider, onSaved }: {
  open: boolean
  onClose: () => void
  provider?: DNSProvider
  onSaved?: (p: DNSProvider) => void
}) {
  const toast = useToast()
  const { isAdmin } = useRole()
  const types = useDNSProviderTypes().data ?? []
  const save = useSaveEntity('dns-providers')
  const [type, setType] = useState('cloudflare')
  const [name, setName] = useState('')
  const [creds, setCreds] = useState<Record<string, string>>({})
  const [envRows, setEnvRows] = useState<EnvRow[]>([])
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [test, setTest] = useState<DNSTestResult | null>(null)
  const [testing, setTesting] = useState(false)
  const rowId = useRef(0)

  const def = types.find((t) => t.type === type)

  useEffect(() => {
    if (!open) return
    const t = provider?.type ?? 'cloudflare'
    const d = types.find((x) => x.type === t)
    const all = { ...(provider?.credentials ?? {}) }
    const declared = new Set(d?.fields.map((f) => f.key))
    setType(t)
    setName(provider?.name ?? '')
    setCreds(Object.fromEntries(Object.entries(all).filter(([k]) => declared.has(k))))
    setEnvRows(d?.envPairs ? Object.entries(all).filter(([k]) => !declared.has(k)).map(([key, value]) => ({ id: ++rowId.current, key, value })) : [])
    setErrors({})
    setTest(null)
    // types load once; re-run when they arrive for an edited provider
  }, [open, provider, types.length]) // eslint-disable-line react-hooks/exhaustive-deps

  // the selected lego provider of the "Other" type (env prefix, docs)
  const selectedOption = useMemo(() => {
    const f = def?.fields.find((x) => x.options?.length && x.options.some((o) => o.envPrefix))
    return f?.options?.find((o) => o.value === creds[f.key])
  }, [def, creds])

  const credentials = () => {
    const out: Record<string, string> = { ...creds }
    if (def?.envPairs) for (const r of envRows) if (r.key.trim()) out[r.key.trim().toUpperCase()] = r.value
    return out
  }
  const defaultName = def?.label.toLowerCase().replace(/\s*\(.*\)$/, '').replace(/[^a-z0-9]+/g, '') || type
  const draft = () => ({ id: provider?.id, name: name.trim() || (selectedOption?.value ?? defaultName), type, credentials: credentials() })
  const blocked = !!def?.adminOnly && !isAdmin

  const runTest = async () => {
    setTesting(true)
    setTest(null)
    try {
      setErrors({})
      setTest(await api.post<DNSTestResult>('/api/dns-providers/test', draft()))
    } catch (err) {
      setErrors(fieldErrors(err))
      toastUnlessFields(toast, err, 'Test failed')
    } finally {
      setTesting(false)
    }
  }

  const submit = async () => {
    try {
      setErrors({})
      const saved = await save.mutateAsync(draft())
      if (saved.status === 'failed') {
        toast.show({ kind: 'warning', title: `${saved.name} saved · credentials rejected`, message: saved.lastError })
      } else {
        toast.success(`DNS provider ${saved.name} saved`, saved.status === 'ok' ? `Credentials verified${saved.zones.length ? ' · zones: ' + saved.zones.join(', ') : ''}` : saved.lastError)
      }
      onSaved?.(saved)
      onClose()
    } catch (err) {
      setErrors(fieldErrors(err))
      toastUnlessFields(toast, err, 'Could not save DNS provider')
    }
  }

  const renderField = (f: DNSProviderField) => {
    const err = errors[`credentials.${f.key}`]
    const value = creds[f.key] ?? ''
    const set = (v: string) => setCreds((c) => ({ ...c, [f.key]: v }))
    let control
    if (f.options?.length) {
      control = (
        <Select
          mono={!f.options.some((o) => o.value === '')}
          invalid={!!err}
          value={value}
          placeholder={f.options.some((o) => o.value === '') ? undefined : 'Pick one…'}
          onChange={set}
          options={f.options.map((o) => ({ value: o.value, label: o.label }))}
        />
      )
    } else if (f.secret) {
      control = <PasswordInput mono invalid={!!err} value={value} placeholder={f.placeholder ?? (f.required ? 'required' : 'optional')} onChange={(e) => set(e.target.value)} autoComplete="off" />
    } else {
      control = <Input mono invalid={!!err} value={value} placeholder={f.placeholder} onChange={(e) => set(e.target.value)} autoComplete="off" />
    }
    return <Field key={f.key} label={f.label} error={err} hint={f.hint}>{control}</Field>
  }

  const prefix = selectedOption?.envPrefix ?? ''
  const docsUrl = selectedOption?.docsUrl ?? def?.docsUrl
  const docsLabel = selectedOption ? `${selectedOption.label} (lego)` : def?.label

  return (
    <Dialog
      open={open}
      onClose={onClose}
      width={560}
      title={provider ? `Edit ${provider.name}` : 'Add DNS provider'}
      description="Credentials for DNS-01 challenges. Relay creates a temporary _acme-challenge TXT record and removes it afterwards."
      footer={
        <>
          <Button onClick={runTest} loading={testing} icon="check" disabled={blocked}>Test</Button>
          <div className="spacer" />
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" onClick={submit} loading={save.isPending} disabled={blocked}>{provider ? 'Save' : 'Add provider'}</Button>
        </>
      }
    >
      <div className="grid-2">
        <Field label="Provider" error={errors.type}>
          <Select
            value={type}
            onChange={(v) => {
              setType(v)
              setCreds({})
              setEnvRows([])
              setTest(null)
              setErrors({})
            }}
            options={types.map((t) => ({ value: t.type, label: t.label }))}
          />
        </Field>
        <Field label="Name" error={errors.name} hint="Shown in certificate forms">
          <Input mono value={name} placeholder={selectedOption?.value ?? defaultName} onChange={(e) => setName(e.target.value)} />
        </Field>
      </div>
      {blocked && <Callout tone="warn">Only admins can configure {def?.label} providers.</Callout>}
      {def?.fields.map(renderField)}
      {def?.envPairs && (
        <Field
          label="Environment variables"
          error={errors.credentials}
          hint={prefix ? `lego variables for this provider start with ${prefix} · values are stored as secrets` : 'Pick a provider first'}
        >
          <div className="col gap-8">
            {envRows.map((r) => {
              const err = errors[`credentials.${r.key.trim().toUpperCase()}`]
              const update = (patch: Partial<EnvRow>) => setEnvRows((rows) => rows.map((x) => (x.id === r.id ? { ...x, ...patch } : x)))
              return (
                <div key={r.id}>
                  <div className="row gap-8">
                    <Input mono invalid={!!err} style={{ flex: '1 1 45%' }} value={r.key} placeholder={`${prefix || 'PROVIDER_'}API_KEY`} onChange={(e) => update({ key: e.target.value.toUpperCase() })} autoComplete="off" aria-label="Variable name" />
                    <div style={{ flex: '1 1 55%' }}>
                      <PasswordInput mono invalid={!!err} value={r.value} placeholder="value" onChange={(e) => update({ value: e.target.value })} autoComplete="off" aria-label={`${r.key || 'Variable'} value`} />
                    </div>
                    <IconButton icon="trash" bare label={`Remove ${r.key || 'variable'}`} onClick={() => setEnvRows((rows) => rows.filter((x) => x.id !== r.id))} />
                  </div>
                  {err && <div className="field-error">{err}</div>}
                </div>
              )
            })}
            <div>
              <Button size="sm" variant="ghost" icon="plus" onClick={() => setEnvRows((rows) => [...rows, { id: ++rowId.current, key: prefix, value: '' }])}>Add variable</Button>
            </div>
          </div>
        </Field>
      )}
      {def?.note && <div className="small muted">{def.note}</div>}
      {def && docsUrl && (
        <div className="small muted">
          Need credentials? <a href={docsUrl} target="_blank" rel="noreferrer" style={{ textDecoration: 'underline' }}>{docsLabel} setup guide ↗</a>
          {provider?.lastCheckedAt && <span> · last checked {ago(provider.lastCheckedAt)}</span>}
        </div>
      )}
      {test?.status === 'ok' && (
        <Callout tone="ok" title="Credentials work">
          {test.zones.length ? `Zones: ${test.zones.join(', ')}` : 'No zones returned.'}
        </Callout>
      )}
      {test && test.status !== 'ok' && (
        <Callout tone={test.status === 'failed' ? 'danger' : 'info'} title={test.status === 'failed' ? 'Credentials rejected' : 'Not verified'}>
          {test.error}
          {test.status === 'unknown' && test.zones.length > 0 && <div className="mono small">Domains: {test.zones.join(', ')}</div>}
        </Callout>
      )}
      {save.isError && !Object.keys(errors).length && <Callout tone="danger">{errorMessage(save.error)}</Callout>}
    </Dialog>
  )
}
