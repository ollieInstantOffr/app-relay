// Owner: slice hosts. Host drawer · SSL tab (design 30d).
import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useQueryClient } from '@tanstack/react-query'
import { Callout, Field, Segmented, Select, Toggle, ToggleRow } from '../../../components/ui'
import { keys, useEntities, useSettings } from '../../../lib/queries'
import RequestCertificateDialog from '../../certificates/RequestCertificateDialog'
import { ConfigPreviewPanel } from '../ConfigPreview'
import type { HostFormCtx } from '../HostDrawer'
import { capitalize, certCoversAll, certOptionLabel, humanAge, isWildcardCert, rankCerts, type HostDraft } from '../lib'

export function SSLTab({ ctx }: { ctx: HostFormCtx }) {
  const { draft, update, errors, readOnly, preview } = ctx
  const navigate = useNavigate()
  const qc = useQueryClient()
  const certsQ = useEntities('certificates')
  const certs = certsQ.data ?? []
  const tls = useSettings('tls').data
  const general = useSettings('general').data
  const [requestOpen, setRequestOpen] = useState(false)

  const { matching, other } = rankCerts(certs, draft.domains)
  const selected = certs.find((c) => c.id === draft.certificateId)
  const hasCert = !!draft.certificateId
  const wildcard = matching.find((c) => isWildcardCert(c) && c.status === 'valid' && c.id !== draft.certificateId)

  const options = [
    { value: '', label: 'None — HTTP only' },
    ...matching.map((c) => ({ value: c.id, label: certOptionLabel(c) })),
    ...other.map((c) => ({ value: c.id, label: draft.domains.length ? `${certOptionLabel(c)} · doesn't cover all domains` : certOptionLabel(c) })),
  ]
  if (draft.certificateId && !selected) {
    options.push({ value: draft.certificateId, label: certsQ.isLoading ? 'Loading…' : 'Missing certificate' })
  }

  let certHint: string | undefined
  if (!hasCert) certHint = 'No certificate — this host is served over plain HTTP.'
  else if (selected && draft.domains.length && !certCoversAll(selected, draft.domains)) {
    certHint = "This certificate doesn't cover every domain — browsers will warn for the others."
  } else if (selected && selected.status !== 'valid') {
    certHint = selected.status === 'pending' ? 'Issuing… the host uses it as soon as it is valid.' : `Certificate is ${selected.status}.`
  }

  const globalH3 = !!general?.http3
  const effectiveH3 = draft.http3 ?? globalH3
  const globalHsts = tls ? (tls.hsts.enabled ? humanAge(tls.hsts.maxAgeSeconds) : 'off') : '…'
  const hstsDesc =
    draft.hsts === 'inherit'
      ? `Inherits global · ${globalHsts}`
      : draft.hsts === 'on'
        ? `On · max-age ${tls ? humanAge(tls.hsts.maxAgeSeconds) : '6 months'}`
        : 'Off for this host'

  return (
    <>
      <Field label="Certificate" error={errors.certificateId} hint={certHint}>
        <Select value={draft.certificateId ?? ''} options={options} onChange={(v) => update({ certificateId: v || undefined })} aria-label="Certificate" />
        {!readOnly && (
          <div className="row gap-14" style={{ marginTop: 2 }}>
            <button type="button" className="hosts-link" onClick={() => setRequestOpen(true)} disabled={draft.domains.length === 0}>
              Request new
            </button>
            {wildcard && (
              <button type="button" className="hosts-link" onClick={() => update({ certificateId: wildcard.id })}>
                Use {wildcard.name} instead
              </button>
            )}
            <button type="button" className="hosts-link" onClick={() => navigate('/certificates?upload=1')}>
              Upload custom
            </button>
          </div>
        )}
      </Field>

      <div className="hosts-toggle-list">
        <ToggleRow title="Force HTTPS" description="301 from :80">
          <Toggle checked={hasCert && draft.forceHttps} disabled={readOnly || !hasCert} onChange={(forceHttps) => update({ forceHttps })} label="Force HTTPS" />
        </ToggleRow>
        <ToggleRow title="HTTP/2" description="Multiplexed over TLS">
          <Toggle checked={draft.http2} disabled={readOnly} onChange={(http2) => update({ http2 })} label="HTTP/2" />
        </ToggleRow>
        <ToggleRow
          title="HTTP/3 (QUIC)"
          description={`UDP 443 · adds Alt-Svc header · ${draft.http3 === null ? 'inherits' : 'overrides'} global (${globalH3 ? 'on' : 'off'})`}
        >
          <div className="row gap-10">
            {draft.http3 !== null && !readOnly && (
              <button type="button" className="hosts-link" onClick={() => update({ http3: null })}>
                Use global
              </button>
            )}
            <Toggle checked={hasCert && effectiveH3} disabled={readOnly || !hasCert} onChange={(v) => update({ http3: v })} label="HTTP/3" />
          </div>
        </ToggleRow>
        <ToggleRow title="HSTS" description={hstsDesc}>
          <Segmented
            value={draft.hsts}
            disabled={readOnly || !hasCert}
            onChange={(hsts) => update({ hsts })}
            options={[
              { value: 'inherit', label: 'Inherit' },
              { value: 'on', label: 'On' },
              { value: 'off', label: 'Off' },
            ]}
          />
        </ToggleRow>
        {draft.upstream.scheme === 'https' && (
          <ToggleRow title="Upstream TLS" description="Upstream is https:// — verify its certificate?">
            <Segmented
              value={draft.upstreamTlsVerify ? 'verify' : 'skip'}
              disabled={readOnly}
              onChange={(v) => update({ upstreamTlsVerify: v === 'verify' })}
              options={[
                { value: 'verify', label: 'Verify' },
                { value: 'skip', label: 'Skip (self-signed)' },
              ]}
            />
          </ToggleRow>
        )}
      </div>
      {(errors.forceHttps || errors.hsts || errors.http3 || errors.upstreamTlsVerify) && (
        <div className="field-error">{errors.forceHttps || errors.hsts || errors.http3 || errors.upstreamTlsVerify}</div>
      )}

      <Field label="Cipher profile" aside={<span className="field-hint">Global default: {capitalize(tls?.cipherProfile) || '—'}</span>} error={errors.cipherProfile}>
        <Select
          value={draft.cipherProfile}
          onChange={(v) => update({ cipherProfile: v as HostDraft['cipherProfile'] })}
          aria-label="Cipher profile"
          options={[
            { value: '', label: 'Inherit' },
            { value: 'modern', label: 'Modern · TLS 1.3 only' },
            { value: 'intermediate', label: 'Intermediate · TLS 1.2 and 1.3' },
            { value: 'old', label: 'Old · legacy clients' },
          ]}
        />
      </Field>

      <Callout tone="info">
        HTTP/3 needs UDP 443 forwarded on your router in addition to TCP 443. Global toggle in Settings → General; this switch overrides it per host.
      </Callout>

      <ConfigPreviewPanel state={preview} collapsible />

      <RequestCertificateDialog
        open={requestOpen}
        onClose={() => setRequestOpen(false)}
        defaultDomains={draft.domains}
        onRequested={(cert) => {
          qc.invalidateQueries({ queryKey: keys.entities('certificates') })
          update({ certificateId: cert.id })
        }}
      />
    </>
  )
}
