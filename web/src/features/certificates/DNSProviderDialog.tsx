// Add / edit a DNS provider (credentials for DNS-01 challenges).
import { useEffect, useState } from 'react'
import { api, errorMessage } from '../../lib/api'
import { useSaveEntity } from '../../lib/queries'
import { ago } from '../../lib/format'
import type { DNSProvider } from '../../lib/types'
import { Button, Callout, Dialog, Field, Input, PasswordInput, Select, useToast } from '../../components/ui'
import { fieldErrors, toastUnlessFields, useDNSProviderTypes, type DNSTestResult } from './common'
import './certs.css'

export default function DNSProviderDialog({ open, onClose, provider, onSaved }: {
  open: boolean
  onClose: () => void
  provider?: DNSProvider
  onSaved?: (p: DNSProvider) => void
}) {
  const toast = useToast()
  const types = useDNSProviderTypes().data ?? []
  const save = useSaveEntity('dns-providers')
  const [type, setType] = useState('cloudflare')
  const [name, setName] = useState('')
  const [creds, setCreds] = useState<Record<string, string>>({})
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [test, setTest] = useState<DNSTestResult | null>(null)
  const [testing, setTesting] = useState(false)

  useEffect(() => {
    if (!open) return
    setType(provider?.type ?? 'cloudflare')
    setName(provider?.name ?? '')
    setCreds({ ...(provider?.credentials ?? {}) })
    setErrors({})
    setTest(null)
  }, [open, provider])

  const def = types.find((t) => t.type === type)
  const draft = () => ({ id: provider?.id, name: name.trim() || def?.label.toLowerCase().replace(/\s+/g, '') || type, type, credentials: creds })

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

  return (
    <Dialog
      open={open}
      onClose={onClose}
      width={520}
      title={provider ? `Edit ${provider.name}` : 'Add DNS provider'}
      description="Credentials for DNS-01 challenges. Relay creates a temporary _acme-challenge TXT record and removes it afterwards."
      footer={
        <>
          <Button onClick={runTest} loading={testing} icon="check">Test</Button>
          <div className="spacer" />
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" onClick={submit} loading={save.isPending}>{provider ? 'Save' : 'Add provider'}</Button>
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
              setTest(null)
            }}
            options={types.map((t) => ({ value: t.type, label: t.label }))}
          />
        </Field>
        <Field label="Name" error={errors.name} hint="Shown in certificate forms">
          <Input mono value={name} placeholder={def?.label.toLowerCase().replace(/\s+/g, '') ?? type} onChange={(e) => setName(e.target.value)} />
        </Field>
      </div>
      {def?.fields.map((f) => {
        const err = errors[`credentials.${f.key}`]
        const value = creds[f.key] ?? ''
        const set = (v: string) => setCreds((c) => ({ ...c, [f.key]: v }))
        return (
          <Field key={f.key} label={f.label} error={err} hint={f.hint}>
            {f.secret ? (
              <PasswordInput mono invalid={!!err} value={value} placeholder={f.placeholder ?? (f.required ? 'required' : 'optional')} onChange={(e) => set(e.target.value)} autoComplete="off" />
            ) : (
              <Input mono invalid={!!err} value={value} placeholder={f.placeholder} onChange={(e) => set(e.target.value)} autoComplete="off" />
            )}
          </Field>
        )
      })}
      {def && (
        <div className="small muted">
          Need a token? <a href={def.docsUrl} target="_blank" rel="noreferrer" style={{ textDecoration: 'underline' }}>{def.label} setup guide ↗</a>
          {provider?.lastCheckedAt && <span> · last checked {ago(provider.lastCheckedAt)}</span>}
        </div>
      )}
      {test?.status === 'ok' && (
        <Callout tone="ok" title="Credentials work">
          {test.zones.length ? `Zones: ${test.zones.join(', ')}` : 'No zones returned.'}
        </Callout>
      )}
      {test && test.status !== 'ok' && (
        <Callout tone={test.status === 'failed' ? 'danger' : 'info'} title={test.status === 'failed' ? 'Credentials rejected' : 'Not tested'}>
          {test.error}
        </Callout>
      )}
      {save.isError && !Object.keys(errors).length && <Callout tone="danger">{errorMessage(save.error)}</Callout>}
    </Dialog>
  )
}
