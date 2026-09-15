// Settings → Default TLS (design 15c).
import { useEffect, useMemo, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { api } from '../../lib/api'
import { keys, useDeleteEntity, useEntities, useRole, useSaveSettings, useSettings } from '../../lib/queries'
import { ago } from '../../lib/format'
import type { DNSProvider, TLSSettings as TLS } from '../../lib/types'
import {
  Button, Callout, Card, Checkbox, ConfirmDialog, Dot, Field, IconButton, Input, Menu, PasswordInput, SectionHeader, Segmented, Select, Skeleton, Textarea, Toggle, Tooltip, cx, useToast,
} from '../../components/ui'
import DNSProviderDialog from '../certificates/DNSProviderDialog'
import { dnsSummary, dnsTypeLabel, fieldErrors, pendingToast, toastUnlessFields, useDNSProviderTypes } from '../certificates/common'
import '../certificates/certs.css'

const cipherInfo = {
  modern: 'Mozilla "Modern" · TLS 1.3 only · recent clients',
  intermediate: 'Mozilla "Intermediate" · TLS 1.2 + 1.3',
  old: 'Mozilla "Old" · TLS 1.0+ · only for legacy clients',
} as const

const maxAges = [
  { value: '86400', label: '1 day' },
  { value: '604800', label: '1 week' },
  { value: '2592000', label: '1 month' },
  { value: '15768000', label: '6 months' },
  { value: '31536000', label: '1 year' },
  { value: '63072000', label: '2 years' },
]

function maxAgeText(sec: number) {
  return maxAges.find((m) => Number(m.value) === sec)?.label ?? `${Math.round(sec / 86400)} days`
}

export default function TLSSettings() {
  const toast = useToast()
  const qc = useQueryClient()
  const { isAdmin, canWrite } = useRole()
  const settings = useSettings('tls')
  const save = useSaveSettings('tls')
  const providers = useEntities('dns-providers').data ?? []
  const certs = useEntities('certificates').data ?? []
  const types = useDNSProviderTypes().data
  const delProvider = useDeleteEntity('dns-providers')
  const [draft, setDraft] = useState<TLS | undefined>()
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [dialog, setDialog] = useState<{ open: boolean; provider?: DNSProvider }>({ open: false })
  const [testing, setTesting] = useState<string | undefined>()
  const [deleting, setDeleting] = useState<DNSProvider | undefined>()

  useEffect(() => {
    if (settings.data) setDraft(structuredClone(settings.data))
  }, [settings.data])

  const dirty = useMemo(() => !!draft && JSON.stringify(draft) !== JSON.stringify(settings.data), [draft, settings.data])
  const readOnly = !isAdmin

  if (!draft) {
    return (
      <>
        <SectionHeader title="Default TLS" description="ACME account, challenge preferences and the cipher policy applied to every host." />
        <Skeleton height={180} />
        <Skeleton height={140} />
      </>
    )
  }

  const set = (patch: Partial<TLS>) => setDraft((d) => (d ? { ...d, ...patch } : d))
  const setHSTS = (patch: Partial<TLS['hsts']>) => setDraft((d) => (d ? { ...d, hsts: { ...d.hsts, ...patch } } : d))

  const submit = async () => {
    setErrors({})
    try {
      await save.mutateAsync(draft)
      pendingToast(toast, 'TLS settings saved', 'Default TLS')
    } catch (err) {
      setErrors(fieldErrors(err))
      toastUnlessFields(toast, err, 'Could not save TLS settings')
    }
  }

  const testProvider = async (p: DNSProvider) => {
    setTesting(p.id)
    try {
      const res = await api.post<DNSProvider>(`/api/dns-providers/${p.id}/test`)
      qc.invalidateQueries({ queryKey: keys.entities('dns-providers') })
      if (res.status === 'ok') toast.success(`${res.name} credentials work`, res.zones.length ? `Zones: ${res.zones.join(', ')}` : undefined)
      else if (res.status === 'unknown') toast.show({ kind: 'info', title: `${res.name} not verified`, message: res.lastError })
      else toast.show({ kind: 'error', title: `${res.name} credentials rejected`, message: res.lastError, actions: [{ label: 'Fix credentials', primary: true, onClick: () => setDialog({ open: true, provider: res }) }] })
    } catch (err) {
      toast.error(err, 'Test failed')
    } finally {
      setTesting(undefined)
    }
  }

  const certsUsing = (p: DNSProvider) => certs.filter((c) => c.dnsProviderId === p.id)

  return (
    <>
      <SectionHeader title="Default TLS" description="ACME account, challenge preferences and the cipher policy applied to every host." />
      {readOnly && <Callout tone="info">Only admins can change these settings.</Callout>}

      <Card title="ACME account">
        <div className="card-body grid-2" style={{ gap: 14 }}>
          <Field label="Provider" error={errors.acmeProvider}>
            <Select
              value={draft.acmeProvider}
              disabled={readOnly}
              onChange={(v) => set({ acmeProvider: v as TLS['acmeProvider'] })}
              options={[
                { value: 'letsencrypt', label: "Let's Encrypt (production)" },
                { value: 'letsencrypt-staging', label: "Let's Encrypt (staging · untrusted)" },
                { value: 'custom', label: 'Custom ACME server' },
              ]}
            />
          </Field>
          <Field label="Contact email" error={errors.email} hint="Optional · used for the ACME account">
            <Input type="email" value={draft.email} disabled={readOnly} invalid={!!errors.email} placeholder="you@example.com" onChange={(e) => set({ email: e.target.value })} />
          </Field>
          {draft.acmeProvider === 'custom' && (
            <>
              <div style={{ gridColumn: '1 / -1' }}>
                <Field label="ACME directory URL" error={errors.acmeDirectoryUrl} hint="step-ca, ZeroSSL, Google Trust Services or an internal CA · must be https://">
                  <Input mono value={draft.acmeDirectoryUrl ?? ''} disabled={readOnly} invalid={!!errors.acmeDirectoryUrl} placeholder="https://ca.internal:9000/acme/acme/directory" onChange={(e) => set({ acmeDirectoryUrl: e.target.value })} />
                </Field>
              </div>
              <div style={{ gridColumn: '1 / -1' }}>
                <Field label="CA bundle (optional)" error={errors.acmeCaBundle} hint="PEM root(s) that sign the directory's TLS certificate, if it isn't publicly trusted">
                  <Textarea mono rows={4} value={draft.acmeCaBundle ?? ''} disabled={readOnly} invalid={!!errors.acmeCaBundle} placeholder={'-----BEGIN CERTIFICATE-----\n…\n-----END CERTIFICATE-----'} onChange={(e) => set({ acmeCaBundle: e.target.value })} />
                </Field>
              </div>
              <Field label="EAB key ID (optional)" error={errors.eabKid} hint="External Account Binding · ZeroSSL, Google, some step-ca setups">
                <Input mono value={draft.eabKid ?? ''} disabled={readOnly} invalid={!!errors.eabKid} autoComplete="off" onChange={(e) => set({ eabKid: e.target.value })} />
              </Field>
              <Field label="EAB HMAC key" error={errors.eabHmacKey} hint="base64url · stored as a secret">
                <PasswordInput mono value={draft.eabHmacKey ?? ''} disabled={readOnly} invalid={!!errors.eabHmacKey} autoComplete="off" placeholder={draft.eabKid ? 'required with a key ID' : 'optional'} onChange={(e) => set({ eabHmacKey: e.target.value })} />
              </Field>
            </>
          )}
          <Field label="Preferred challenge" error={errors.preferredChallenge}>
            <div className="segmented" role="tablist" style={{ display: 'flex', height: 38 }}>
              {(['dns-01', 'http-01'] as const).map((c) => (
                <button key={c} type="button" style={{ flex: 1 }} disabled={readOnly} className={cx(draft.preferredChallenge === c && 'active')} onClick={() => set({ preferredChallenge: c })}>
                  {c.toUpperCase()}
                </button>
              ))}
              <Tooltip content="Unavailable while the reverse proxy owns port 443">
                <button type="button" disabled style={{ flex: 1, width: '100%' }}>TLS-ALPN</button>
              </Tooltip>
            </div>
          </Field>
          <Field label="Renew when" error={errors.renewDaysBefore}>
            <Select
              value={String(draft.renewDaysBefore)}
              disabled={readOnly}
              onChange={(v) => set({ renewDaysBefore: Number(v) })}
              options={[7, 14, 21, 30, 45, 60].map((d) => ({ value: String(d), label: `${d} days before expiry` }))
                .concat([7, 14, 21, 30, 45, 60].includes(draft.renewDaysBefore) ? [] : [{ value: String(draft.renewDaysBefore), label: `${draft.renewDaysBefore} days before expiry` }])}
            />
          </Field>
        </div>
      </Card>

      <Card title="DNS providers" actions={canWrite && <Button variant="ghost" size="sm" icon="plus" onClick={() => setDialog({ open: true })}>Add provider</Button>}>
        {providers.length === 0 && (
          <div className="card-body small muted">No DNS providers yet. Add one to issue wildcard certificates with DNS-01 — it also works when port 80 isn't reachable from the internet.</div>
        )}
        {providers.map((p) => {
          const failed = p.status === 'failed'
          const used = certsUsing(p)
          return (
            <div key={p.id} className={cx('cs-provider-row', failed && 'failed')}>
              <Dot tone={p.status === 'ok' ? 'ok' : failed ? 'danger' : 'muted'} />
              <div className="grow" style={{ minWidth: 0 }}>
                <div className="medium">{p.name}<span className="small muted" style={{ fontWeight: 400 }}>{p.name.toLowerCase() !== dnsTypeLabel(types, p.type).toLowerCase() ? ` · ${dnsTypeLabel(types, p.type)}` : ''}</span></div>
                <div className="mono small truncate" style={{ marginTop: 2, color: failed ? 'var(--danger-text)' : 'var(--ink-faint)' }}>
                  {failed
                    ? `${p.lastError ?? 'credentials rejected'}${p.lastCheckedAt ? ' ' + ago(p.lastCheckedAt) : ''}`
                    : dnsSummary(p, types) || (p.status === 'unknown' ? p.lastError || 'Not tested yet' : '—')}
                </div>
              </div>
              {failed ? (
                <Button size="sm" variant="ghost" className="medium" disabled={!canWrite} onClick={() => setDialog({ open: true, provider: p })}>Fix credentials</Button>
              ) : (
                <Button size="sm" variant="ghost" loading={testing === p.id} onClick={() => testProvider(p)} disabled={!canWrite}>Test</Button>
              )}
              <Menu
                trigger={<IconButton icon="more" bare label={`${p.name} actions`} />}
                items={[
                  { header: p.name },
                  { label: 'Edit credentials', icon: 'edit', disabled: !canWrite, onSelect: () => setDialog({ open: true, provider: p }) },
                  { label: 'Test now', icon: 'check', disabled: !canWrite, onSelect: () => testProvider(p) },
                  'separator',
                  { label: used.length ? `Delete… · used by ${used.length} ${used.length === 1 ? 'certificate' : 'certificates'}` : 'Delete…', icon: 'trash', danger: true, disabled: !canWrite || used.length > 0, onSelect: () => setDeleting(p) },
                ]}
              />
            </div>
          )
        })}
      </Card>

      <Card title="Protocol policy">
        <div className="toggle-row">
          <div className="grow">
            <div className="toggle-title">Cipher profile</div>
            <div className="toggle-desc">{cipherInfo[draft.cipherProfile]}</div>
            {errors.cipherProfile && <div className="field-error">{errors.cipherProfile}</div>}
          </div>
          <Segmented
            value={draft.cipherProfile}
            disabled={readOnly}
            onChange={(v) => set({ cipherProfile: v })}
            options={[{ value: 'modern', label: 'Modern' }, { value: 'intermediate', label: 'Intermediate' }, { value: 'old', label: 'Old' }]}
          />
        </div>
        <div className="toggle-row" style={{ flexWrap: 'wrap' }}>
          <div className="grow">
            <div className="toggle-title">HSTS on all TLS hosts</div>
            <div className="toggle-desc">
              {draft.hsts.enabled ? `max-age ${maxAgeText(draft.hsts.maxAgeSeconds)}${draft.hsts.includeSubdomains ? ' · includeSubDomains' : ''}${draft.hsts.preload ? ' · preload' : ''}` : 'Off · hosts can still turn it on individually'}
            </div>
          </div>
          <Toggle checked={draft.hsts.enabled} disabled={readOnly} onChange={(v) => setHSTS({ enabled: v })} label="HSTS" />
          {draft.hsts.enabled && (
            <div className="row gap-16 wrap" style={{ flexBasis: '100%', marginTop: 12 }}>
              <Field label="max-age" error={errors['hsts.maxAgeSeconds']}>
                <Select
                  inputSize="sm"
                  value={String(draft.hsts.maxAgeSeconds)}
                  disabled={readOnly}
                  onChange={(v) => setHSTS({ maxAgeSeconds: Number(v) })}
                  options={maxAges.some((m) => Number(m.value) === draft.hsts.maxAgeSeconds) ? maxAges : [...maxAges, { value: String(draft.hsts.maxAgeSeconds), label: maxAgeText(draft.hsts.maxAgeSeconds) }]}
                />
              </Field>
              <Checkbox checked={draft.hsts.includeSubdomains} disabled={readOnly} onChange={(v) => setHSTS({ includeSubdomains: v })} label="includeSubDomains" />
              <Checkbox checked={draft.hsts.preload} disabled={readOnly} onChange={(v) => setHSTS({ preload: v })} label="preload" />
              {errors['hsts.preload'] && <div className="field-error">{errors['hsts.preload']}</div>}
            </div>
          )}
        </div>
        <div className="toggle-row">
          <div className="grow">
            <div className="toggle-title">OCSP stapling</div>
            <div className="toggle-desc">Staples revocation status for certificates that include an OCSP URL</div>
          </div>
          <Toggle checked={draft.ocspStapling} disabled={readOnly} onChange={(v) => set({ ocspStapling: v })} label="OCSP stapling" />
        </div>
      </Card>

      {!readOnly && (
        <div className="row gap-8">
          <Button variant="primary" onClick={submit} loading={save.isPending} disabled={!dirty}>Save changes</Button>
          {dirty && <Button variant="ghost" onClick={() => { setDraft(structuredClone(settings.data!)); setErrors({}) }}>Discard</Button>}
        </div>
      )}

      <DNSProviderDialog open={dialog.open} provider={dialog.provider} onClose={() => setDialog({ open: false })} />
      <ConfirmDialog
        open={!!deleting}
        onClose={() => setDeleting(undefined)}
        danger
        title={`Delete ${deleting?.name}?`}
        message="Its credentials are removed from Relay. Certificates can no longer use it for DNS-01."
        confirmLabel="Delete provider"
        onConfirm={async () => {
          if (!deleting) return
          try {
            await delProvider.mutateAsync(deleting.id)
            toast.success('DNS provider deleted', deleting.name)
          } catch (err) {
            toast.error(err, 'Could not delete DNS provider')
            throw err
          }
        }}
      />
    </>
  )
}
