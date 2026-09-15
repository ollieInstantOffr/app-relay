// Owner: slice hosts. Default host & default TLS certificate (design 20).
import { useEffect, useState } from 'react'
import { Button, Card, EmptyState, Input, RadioCard, Select, Skeleton, useToast } from '../../components/ui'
import { ApiError, errorMessage } from '../../lib/api'
import { useEntities, useRole, useSaveSettings, useSettings } from '../../lib/queries'
import type { DefaultHostSettings } from '../../lib/types'
import { applyNowAction, certDays, isWildcardCert, providerLabel, useUnknownHostHits } from './lib'

const canon = (d: DefaultHostSettings) =>
  JSON.stringify({ action: d.action, redirectTo: d.redirectTo ?? '', hostId: d.hostId ?? '', certificateId: d.certificateId ?? '' })

export function DefaultHostView() {
  const { canWrite } = useRole()
  const readOnly = !canWrite
  const toast = useToast()
  const q = useSettings('default_host')
  const save = useSaveSettings('default_host')
  const hosts = useEntities('hosts').data ?? []
  const certs = useEntities('certificates').data ?? []
  const hits = useUnknownHostHits().data
  const [draft, setDraft] = useState<DefaultHostSettings | null>(null)
  const [errors, setErrors] = useState<Record<string, string>>({})

  useEffect(() => {
    if (q.data && !draft) setDraft(q.data)
  }, [q.data, draft])

  if (q.isError) {
    return (
      <div className="card">
        <EmptyState icon="warning" title="Couldn't load the default host" description={errorMessage(q.error)} actions={<Button onClick={() => q.refetch()}>Retry</Button>} />
      </div>
    )
  }
  if (!draft || !q.data) {
    return (
      <div className="hosts-default-grid">
        <Skeleton height={320} />
        <Skeleton height={220} />
      </div>
    )
  }

  const dirty = canon(draft) !== canon(q.data)
  const set = (patch: Partial<DefaultHostSettings>) => {
    if (readOnly) return
    setDraft((d) => (d ? { ...d, ...patch } : d))
    setErrors({})
  }
  const onSave = async () => {
    try {
      const saved = await save.mutateAsync(draft)
      setDraft(saved)
      setErrors({})
      toast.show({ kind: 'success', title: 'Default host saved', message: 'Added to pending changes.', actions: [applyNowAction] })
    } catch (err) {
      if (err instanceof ApiError && err.fields) setErrors(err.fields)
      else toast.error(err, 'Could not save the default host')
    }
  }

  const hostOptions = hosts
    .filter((h) => h.domains.length > 0)
    .map((h) => ({ value: h.id, label: `${h.domains[0]}${h.enabled ? '' : ' (disabled)'}` }))
    .sort((a, b) => a.label.localeCompare(b.label))
  if (draft.hostId && !hosts.some((h) => h.id === draft.hostId)) hostOptions.push({ value: draft.hostId, label: 'Missing host' })
  const certOptions = certs
    .filter((c) => c.status === 'valid' || c.id === draft.certificateId)
    .sort((a, b) => Number(isWildcardCert(b)) - Number(isWildcardCert(a)) || a.name.localeCompare(b.name))

  return (
    <div className="col gap-16" style={{ maxWidth: 1120 }}>
      <div className="hosts-default-grid">
        <Card title="Default host" pad>
          <div className="small muted">What a request for an unknown domain (or a bare IP) gets. Also answers HTTP-01 challenges.</div>
          <RadioCard selected={draft.action === 'close'} onSelect={() => set({ action: 'close' })} title="Close connection (444)" description="Nothing leaks · recommended when public" />
          <RadioCard selected={draft.action === '404'} onSelect={() => set({ action: '404' })} title="Relay 404 page" description="Neutral, unbranded" />
          <RadioCard selected={draft.action === 'redirect'} onSelect={() => set({ action: 'redirect' })} title="Redirect to">
            <div className="hosts-radio-extra">
              <Input
                mono
                inputSize="sm"
                value={draft.redirectTo ?? ''}
                placeholder="https://home.lan"
                disabled={readOnly}
                invalid={!!errors.redirectTo}
                onChange={(e) => set({ action: 'redirect', redirectTo: e.target.value.trim() })}
                aria-label="Redirect unknown hosts to"
              />
              {errors.redirectTo && <div className="field-error" style={{ marginTop: 4 }}>{errors.redirectTo}</div>}
            </div>
          </RadioCard>
          <RadioCard selected={draft.action === 'host'} onSelect={() => set({ action: 'host' })} title="Serve a proxy host">
            <div className="hosts-radio-extra">
              <Select
                inputSize="sm"
                value={draft.hostId ?? ''}
                placeholder="Pick an existing host"
                options={hostOptions}
                disabled={readOnly}
                invalid={!!errors.hostId}
                onChange={(v) => set({ action: 'host', hostId: v || undefined })}
                aria-label="Proxy host for unknown domains"
              />
              {errors.hostId && <div className="field-error" style={{ marginTop: 4 }}>{errors.hostId}</div>}
            </div>
          </RadioCard>
          {errors.action && <div className="field-error">{errors.action}</div>}
        </Card>

        <Card title="Default TLS certificate" pad>
          <div className="small muted">Presented when SNI matches no host. Self-signed keeps scanners from learning your domains.</div>
          <RadioCard selected={!draft.certificateId} onSelect={() => set({ certificateId: undefined })} title="Self-signed placeholder" description="CN=localhost · regenerated yearly" />
          {certOptions.map((c) => {
            const d = certDays(c)
            return (
              <RadioCard
                key={c.id}
                selected={draft.certificateId === c.id}
                onSelect={() => set({ certificateId: c.id })}
                title={<span className="mono">{c.name}</span>}
                description={[providerLabel(c.provider), d !== undefined ? (d < 0 ? 'expired' : `${d} days`) : c.status].join(' · ')}
              />
            )
          })}
          {certOptions.length === 0 && <div className="small faint">No valid certificates yet — request one on the Certificates page.</div>}
          {errors.certificateId && <div className="field-error">{errors.certificateId}</div>}
        </Card>
      </div>

      <div className="row">
        {typeof hits === 'number' && <span className="hosts-footer-note">Unknown-host hits last 24 h: {hits.toLocaleString()}</span>}
        <div className="spacer" />
        {canWrite && (
          <>
            <Button
              disabled={!dirty || save.isPending}
              onClick={() => {
                setDraft(q.data!)
                setErrors({})
              }}
            >
              Discard
            </Button>
            <Button variant="primary" disabled={!dirty} loading={save.isPending} onClick={onSave}>
              Save to pending
            </Button>
          </>
        )}
      </div>
    </div>
  )
}
