// SSL certificates (design 04).
import { useMemo, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { useQueryClient } from '@tanstack/react-query'
import { TopBar } from '../../components/shell/TopBar'
import {
  Button, ConfirmDialog, Dot, EmptyState, IconButton, Menu, Skeleton, Spinner, Toggle, useToast,
} from '../../components/ui'
import { api } from '../../lib/api'
import { Topics, useBusEvent } from '../../lib/events'
import { keys, useDeleteEntity, useEntities, useRole, useSaveEntity } from '../../lib/queries'
import type { Certificate, DNSProvider } from '../../lib/types'
import CertificateDrawer from './CertificateDrawer'
import DNSProviderDialog from './DNSProviderDialog'
import RequestCertificateDialog from './RequestCertificateDialog'
import UploadCertificateDialog from './UploadCertificateDialog'
import {
  certExpiry, certSubtitle, daysText, downloadUrl, isACME, providerText, triggerDownload, usageCount, usageText, useCertUsage,
  type CertView,
} from './common'
import './certs.css'

function SummaryCard({ value, label, tone }: { value: number | string; label: string; tone: 'ok' | 'warn' | 'muted' }) {
  return (
    <div className="card cs-summary">
      <div className={`cs-summary-icon ${tone}`}>
        <span className={`cs-summary-dot${tone === 'muted' ? ' ring' : ''}`} />
      </div>
      <div>
        <div className="cs-summary-value">{value}</div>
        <div className="cs-summary-label">{label}</div>
      </div>
    </div>
  )
}

export default function CertificatesPage() {
  const toast = useToast()
  const qc = useQueryClient()
  const { canWrite } = useRole()
  const [params, setParams] = useSearchParams()
  const certsQ = useEntities('certificates')
  const certs = (certsQ.data ?? []) as CertView[]
  const providers = useEntities('dns-providers').data ?? []
  const usage = useCertUsage()
  const save = useSaveEntity('certificates')
  const del = useDeleteEntity('certificates')
  const [providerDialog, setProviderDialog] = useState<{ open: boolean; provider?: DNSProvider }>({ open: false })
  const [replace, setReplace] = useState<Certificate | undefined>()
  const [deleting, setDeleting] = useState<CertView | undefined>()
  const [retrying, setRetrying] = useState<string | undefined>()

  const selectedId = params.get('cert') ?? undefined
  const selected = certs.find((c) => c.id === selectedId)
  const setParam = (key: string, value?: string) => {
    const next = new URLSearchParams(params)
    if (value) next.set(key, value)
    else next.delete(key)
    setParams(next, { replace: true })
  }

  // Toast when an order finishes while the page is open.
  useBusEvent<{ id: string; status: string }>(Topics.CertChanged, async (ev) => {
    const cached = qc.getQueryData<CertView[]>(keys.entities('certificates'))
    const prev = cached?.find((c) => c.id === ev.data.id)
    if (!prev || !(prev.status === 'pending' || prev.renewing) || ev.data.status === 'pending') return
    try {
      const c = await api.get<Certificate>(`/api/certificates/${ev.data.id}`)
      if (c.status === 'valid' && (prev.status === 'pending' || prev.renewing)) {
        toast.success(prev.notAfter ? `Certificate renewed · ${c.name}` : `Certificate issued · ${c.name}`, `${providerText(c)}${c.issuer ? ' · ' + c.issuer : ''} · valid ${daysText(certExpiry(c).days ?? 0)}`)
      } else if (c.status === 'failed') {
        toast.show({
          kind: 'error',
          title: prev.notAfter ? 'Certificate renewal failed' : 'Certificate request failed',
          message: c.lastError,
          actions: [
            { label: 'Retry', primary: true, onClick: () => retry(c) },
            { label: c.challenge === 'dns-01' ? 'Open provider' : 'Details', onClick: () => (c.challenge === 'dns-01' ? setProviderDialog({ open: true, provider: providers.find((p) => p.id === c.dnsProviderId) }) : setParam('cert', c.id)) },
          ],
        })
      }
    } catch {
      /* deleted meanwhile */
    }
  })

  const retry = async (c: Certificate) => {
    setRetrying(c.id)
    try {
      await api.post(`/api/certificates/${c.id}/renew`)
      qc.invalidateQueries({ queryKey: keys.entities('certificates') })
    } catch (err) {
      toast.error(err, 'Retry could not start')
    } finally {
      setRetrying(undefined)
    }
  }

  const stats = useMemo(() => {
    let valid = 0, expiring = 0, custom = 0
    for (const c of certs) {
      const days = certExpiry(c).days
      if (c.provider === 'custom') custom++
      if (c.status === 'valid' && c.autoRenew && isACME(c)) valid++
      if (days !== undefined && days < 14 && c.status !== 'pending') expiring++
    }
    return { valid, expiring, custom }
  }, [certs])

  const toggleAutoRenew = async (c: CertView, v: boolean) => {
    try {
      await save.mutateAsync({ id: c.id, name: c.name, autoRenew: v })
      toast.success(v ? `Auto-renew on for ${c.name}` : `Auto-renew off for ${c.name}`)
    } catch (err) {
      toast.error(err, 'Could not change auto-renew')
    }
  }

  const actions = canWrite ? (
    <>
      <Button onClick={() => setParam('upload', '1')}>Upload custom</Button>
      <Button variant="primary" icon="plus" onClick={() => setParam('request', '1')}>Request certificate</Button>
    </>
  ) : undefined

  return (
    <>
      <TopBar title="Certificates" count={certsQ.data ? certs.length : undefined} actions={actions} />
      <div className="page" style={{ gap: 16 }}>
        <div className="grid-3">
          <SummaryCard value={certsQ.data ? stats.valid : '—'} label="Valid & auto-renewing" tone="ok" />
          <SummaryCard value={certsQ.data ? stats.expiring : '—'} label="Expiring within 14 days" tone="warn" />
          <SummaryCard value={certsQ.data ? stats.custom : '—'} label="Custom (manual)" tone="muted" />
        </div>

        <div className="card" style={{ overflow: 'hidden' }}>
          <div className="cs-grid-head cs-certs">
            <span>Certificate</span><span>Provider</span><span>Expiry</span><span>Used by</span><span>Auto-renew</span><span />
          </div>
          {certsQ.isLoading && (
            <div className="col gap-12" style={{ padding: 20 }}>
              <Skeleton height={40} /><Skeleton height={40} /><Skeleton height={40} />
            </div>
          )}
          {certsQ.isError && <EmptyState icon="warning" title="Couldn't load certificates" description={String(certsQ.error)} />}
          {certsQ.data && certs.length === 0 && (
            <EmptyState
              icon="certificates"
              title="No certificates yet"
              description="Request a free Let's Encrypt certificate — use DNS-01 for wildcards like *.home.lan — or upload one from your own CA."
              actions={canWrite && (
                <>
                  <Button variant="primary" icon="plus" onClick={() => setParam('request', '1')}>Request certificate</Button>
                  <Button onClick={() => setParam('upload', '1')}>Upload custom</Button>
                </>
              )}
            />
          )}
          {certs.map((c) => {
            const exp = certExpiry(c)
            const sub = certSubtitle(c)
            const u = usage.get(c.id)
            const busy = c.status === 'pending' || c.renewing
            const failed = c.status === 'failed' || c.status === 'expired'
            return (
              <div key={c.id} className={`cs-grid-row cs-certs clickable${failed ? ' warn' : ''}`} onClick={() => setParam('cert', c.id)}>
                <span className="col gap-2" style={{ minWidth: 0 }}>
                  <span className="mono medium truncate">{c.name}</span>
                  <span className="micro truncate" style={{ color: sub.tone === 'danger' ? 'var(--danger-text)' : sub.tone === 'warn' ? 'var(--warn-text)' : 'var(--ink-faint)' }}>{sub.text}</span>
                </span>
                <span className="small" style={{ color: 'var(--ink-muted)' }}>{providerText(c)}</span>
                <span className="col gap-6">
                  {busy && !c.notAfter ? (
                    <span className="row gap-6 small muted"><Spinner /> {c.status === 'pending' ? 'Requesting…' : 'Renewing…'}</span>
                  ) : exp.days === undefined ? (
                    <span className="mono small muted">—</span>
                  ) : (
                    <>
                      <span className="mono small row gap-6" style={{ color: exp.tone === 'danger' ? 'var(--danger-text)' : exp.tone === 'warn' ? 'var(--warn-text)' : undefined }}>
                        {busy && <Spinner />}
                        {daysText(exp.days)}
                      </span>
                      <span className={`cs-expiry-bar ${exp.tone}`}><span style={{ width: `${exp.tone === 'muted' ? 100 : exp.pct}%` }} /></span>
                    </>
                  )}
                </span>
                <span className="small" style={{ color: usageCount(u) ? 'var(--ink-muted)' : 'var(--ink-faint)' }}>{usageText(u)}</span>
                <span className="row gap-8" onClick={(e) => e.stopPropagation()}>
                  {isACME(c) ? (
                    <Toggle checked={c.autoRenew} onChange={(v) => toggleAutoRenew(c, v)} disabled={!canWrite} label={`Auto-renew ${c.name}`} />
                  ) : (
                    <span className="small muted">Manual</span>
                  )}
                  {failed && isACME(c) && canWrite && (
                    <Button variant="link" size="sm" onClick={() => retry(c)} disabled={retrying === c.id || busy}>
                      {retrying === c.id ? 'Retrying…' : 'Retry'}
                    </Button>
                  )}
                </span>
                <span onClick={(e) => e.stopPropagation()}>
                  <Menu
                    trigger={<IconButton icon="more" bare label="Certificate actions" />}
                    items={[
                      { header: c.name },
                      { label: 'Details', icon: 'info', onSelect: () => setParam('cert', c.id) },
                      ...(isACME(c)
                        ? [{ label: failed ? 'Retry' : 'Force renew', icon: 'reload' as const, disabled: !canWrite || busy, onSelect: () => retry(c) }]
                        : [{ label: 'Upload replacement', icon: 'upload' as const, disabled: !canWrite, onSelect: () => setReplace(c) }]),
                      { label: 'Download full chain', icon: 'download', disabled: !c.notAfter, onSelect: () => triggerDownload(downloadUrl(c.id, 'fullchain')) },
                      'separator',
                      { label: usageCount(u) ? `Delete… · used by ${usageText(u)}` : 'Delete…', icon: 'trash', danger: true, disabled: !canWrite || usageCount(u) > 0 || !!busy, onSelect: () => setDeleting(c) },
                    ]}
                  />
                </span>
              </div>
            )
          })}
        </div>

        <div className="card" style={{ padding: '16px 20px', display: 'flex', alignItems: 'center', gap: 16, flexWrap: 'wrap' }}>
          <div className="grow">
            <div className="card-title">DNS providers</div>
            <div className="small muted" style={{ marginTop: 2 }}>Credentials for DNS-01 wildcard challenges</div>
          </div>
          {providers.map((p) => (
            <button key={p.id} type="button" className={`cs-chip${p.status === 'failed' ? ' danger' : ''}`} onClick={() => setProviderDialog({ open: true, provider: p })} title={p.lastError || p.zones.join(', ')} disabled={!canWrite}>
              <Dot tone={p.status === 'ok' ? 'ok' : p.status === 'failed' ? 'danger' : 'muted'} />
              {p.name}{p.status === 'failed' ? ' · auth failed' : ''}
            </button>
          ))}
          {canWrite && <Button variant="ghost" size="sm" icon="plus" onClick={() => setProviderDialog({ open: true })}>Add provider</Button>}
        </div>
      </div>

      <CertificateDrawer
        open={!!selectedId}
        cert={selected}
        usage={selected ? usage.get(selected.id) : undefined}
        onClose={() => setParam('cert')}
        onReplace={(c) => setReplace(c)}
      />
      <RequestCertificateDialog
        open={params.get('request') === '1'}
        onClose={() => setParam('request')}
        defaultDomains={params.get('domains')?.split(',').filter(Boolean)}
      />
      <UploadCertificateDialog
        open={params.get('upload') === '1' || !!replace}
        replace={replace}
        onClose={() => {
          setReplace(undefined)
          if (params.get('upload')) setParam('upload')
        }}
      />
      <DNSProviderDialog open={providerDialog.open} provider={providerDialog.provider} onClose={() => setProviderDialog({ open: false })} />
      <ConfirmDialog
        open={!!deleting}
        onClose={() => setDeleting(undefined)}
        danger
        title={`Delete ${deleting?.name}?`}
        message="The certificate and its key files are removed from this machine. This can't be undone."
        confirmLabel="Delete certificate"
        onConfirm={async () => {
          if (!deleting) return
          try {
            await del.mutateAsync(deleting.id)
            toast.success('Certificate deleted', deleting.name)
          } catch (err) {
            toast.error(err, 'Could not delete certificate')
            throw err
          }
        }}
      />
    </>
  )
}
