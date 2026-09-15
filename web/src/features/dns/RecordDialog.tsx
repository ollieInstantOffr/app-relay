// Add / edit a DNS record in a Public DNS zone.
import { useEffect, useState } from 'react'
import type { DNSRecord, DNSRecordInput } from '../../lib/types'
import { Button, Callout, Dialog, Field, Input, Segmented, Select, Textarea, ToggleCard, useToast } from '../../components/ui'
import { errorMessage } from '../../lib/api'
import { fieldErrors } from '../certificates/common'
import { EDITABLE_TYPES, PROXIABLE_TYPES, RECORD_TYPE_INFO, TTL_CHOICES, isProviderError, ttlText, useSaveDNSRecord } from './dnsApi'

export default function RecordDialog({ open, onClose, zone, providerType, record, publicIp }: {
  open: boolean
  onClose: () => void
  zone: string
  providerType: string
  record?: DNSRecord
  publicIp?: string
}) {
  const toast = useToast()
  const save = useSaveDNSRecord(zone)
  const [type, setType] = useState('A')
  const [name, setName] = useState('')
  const [data, setData] = useState('')
  const [ttl, setTtl] = useState(0)
  const [priority, setPriority] = useState(10)
  const [proxied, setProxied] = useState(false)
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [providerError, setProviderError] = useState('')

  useEffect(() => {
    if (!open) return
    setType(record?.type ?? 'A')
    setName(record?.name ?? '')
    setData(record?.data ?? '')
    setTtl(record?.ttl === 1 ? 0 : record?.ttl ?? 0)
    setPriority(record?.priority ?? 10)
    setProxied(!!record?.proxied)
    setErrors({})
    setProviderError('')
  }, [open, record])

  const cloudflare = providerType === 'cloudflare'
  const godaddy = providerType === 'godaddy'
  const info = RECORD_TYPE_INFO[type] ?? { placeholder: '', hint: '' }
  const clear = (k: string) => setErrors((e) => (e[k] ? Object.fromEntries(Object.entries(e).filter(([x]) => x !== k)) : e))
  const trimmedName = name.trim().replace(new RegExp(`\\.?${zone.replace(/\./g, '\\.')}\\.?$`), '') || '@'
  const fqdn = trimmedName === '@' ? zone : `${trimmedName}.${zone}`

  const ttlOptions = [...new Set([...TTL_CHOICES, ttl])]
    .sort((a, b) => a - b)
    .map((v) => ({ value: String(v), label: v === 0 ? 'Auto' : ttlText(v), disabled: godaddy && v > 0 && v < 600, hint: v === 0 ? 'provider default' : undefined }))

  const submit = async () => {
    setErrors({})
    setProviderError('')
    const input: DNSRecordInput = {
      type,
      name: trimmedName,
      data: data.trim(),
      ttl,
      ...(type === 'MX' ? { priority } : {}),
      ...(cloudflare && PROXIABLE_TYPES.has(type) ? { proxied } : {}),
    }
    try {
      await save.mutateAsync({ id: record?.id, input })
      toast.success(record ? 'Record updated' : 'Record created', `${type} ${fqdn} → ${input.data}`)
      onClose()
    } catch (err) {
      const f = fieldErrors(err)
      if (Object.keys(f).length) setErrors(f)
      else if (isProviderError(err)) setProviderError(errorMessage(err))
      else toast.error(err, record ? 'Could not update record' : 'Could not create record')
    }
  }

  return (
    <Dialog
      open={open}
      onClose={onClose}
      width={560}
      title={record ? `Edit ${record.type} record` : 'Add record'}
      description={<>Changes go to your DNS provider right away · <span className="mono">{zone}</span></>}
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" loading={save.isPending} disabled={!data.trim()} onClick={submit}>
            {record ? 'Save record' : 'Add record'}
          </Button>
        </>
      }
    >
      <Field label="Type" error={errors.type}>
        <div>
          <Segmented
            value={type}
            onChange={(v) => {
              setType(v)
              clear('type')
            }}
            options={EDITABLE_TYPES.map((t) => ({ value: t, label: t }))}
          />
        </div>
      </Field>

      <Field label="Name" error={errors.name} hint={<><span className="mono">@</span> for the domain itself · <span className="mono">*.dev</span> for a wildcard</>}>
        <div className="dns-name-input">
          <Input
            mono
            autoFocus={!record}
            value={name}
            invalid={!!errors.name}
            placeholder="www"
            style={{ paddingRight: Math.min(260, zone.length * 7.8 + 30) }}
            onChange={(e) => {
              setName(e.target.value)
              clear('name')
            }}
          />
          <span className="dns-name-suffix">.{zone}</span>
        </div>
      </Field>

      <Field
        label="Value"
        error={errors.data}
        hint={
          type === 'A' && publicIp && data.trim() !== publicIp ? (
            <>
              {info.hint} ·{' '}
              <button type="button" className="btn-link small" onClick={() => setData(publicIp)}>
                use Relay’s public IP {publicIp}
              </button>
            </>
          ) : info.hint
        }
      >
        {type === 'TXT' ? (
          <Textarea mono rows={3} value={data} invalid={!!errors.data} placeholder={info.placeholder} onChange={(e) => { setData(e.target.value); clear('data') }} />
        ) : (
          <Input mono value={data} invalid={!!errors.data} placeholder={info.placeholder} onChange={(e) => { setData(e.target.value); clear('data') }} />
        )}
      </Field>

      <div className="grid-2">
        <Field label="TTL" error={errors.ttl} hint={godaddy ? 'GoDaddy needs at least 10 min' : 'How long resolvers may cache it'}>
          <Select value={String(ttl)} options={ttlOptions} onChange={(v) => { setTtl(Number(v)); clear('ttl') }} />
        </Field>
        {type === 'MX' && (
          <Field label="Priority" error={errors.priority} hint="Lower is tried first">
            <Input mono type="number" min={0} max={65535} value={priority} invalid={!!errors.priority} onChange={(e) => { setPriority(Number(e.target.value)); clear('priority') }} />
          </Field>
        )}
      </div>

      {cloudflare && PROXIABLE_TYPES.has(type) && (
        <ToggleCard
          title="Proxy through Cloudflare"
          description="Hides your IP and sends traffic through Cloudflare first. Turn off if Relay should get certificates with HTTP-01 directly."
          checked={proxied}
          onChange={setProxied}
        />
      )}

      {providerError && <Callout tone="danger" title="Your DNS provider rejected the change">{providerError}</Callout>}
    </Dialog>
  )
}
