// Certificate detail drawer (design 29b).
import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { useQueryClient } from '@tanstack/react-query'
import { api } from '../../lib/api'
import { keys, useDeleteEntity, useEntities, useRole, useSaveEntity } from '../../lib/queries'
import { dateTime, date, ms } from '../../lib/format'
import type { Certificate } from '../../lib/types'
import {
  Badge, Button, Callout, ConfirmDialog, CopyButton, Dialog, Drawer, Field, Menu, Select, Spinner, Status, Toggle, Tooltip, useToast,
} from '../../components/ui'
import {
  certExpiry, challengeLabel, daysText, downloadUrl, isACME, providerName, toastUnlessFields, triggerDownload, usageCount,
  type CertUsage, type CertView,
} from './common'
import './certs.css'

interface TestResult {
  ok: boolean; matches: boolean; address: string; sni: string; servedFingerprint?: string; servedSubject?: string
  servedIssuer?: string; servedNotAfter?: string; hostUsesCert: boolean; error?: string
}

function TestOnHostDialog({ open, onClose, cert, usage }: { open: boolean; onClose: () => void; cert: CertView; usage?: CertUsage }) {
  const hosts = useEntities('hosts').data ?? []
  const [hostId, setHostId] = useState('')
  const [busy, setBusy] = useState(false)
  const [result, setResult] = useState<TestResult | null>(null)
  const toast = useToast()
  useEffect(() => {
    if (!open) return
    setResult(null)
    setHostId(usage?.hosts[0]?.id ?? hosts[0]?.id ?? '')
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open])
  const using = new Set(usage?.hosts.map((h) => h.id))
  const options = [...hosts].sort((a, b) => Number(using.has(b.id)) - Number(using.has(a.id))).map((h) => ({
    value: h.id,
    label: `${h.domains[0] ?? h.id}${using.has(h.id) ? ' · uses this certificate' : ''}`,
  }))
  const run = async () => {
    setBusy(true)
    setResult(null)
    try {
      setResult(await api.post<TestResult>(`/api/certificates/${cert.id}/test`, { hostId }))
    } catch (err) {
      toast.error(err, 'Test failed')
    } finally {
      setBusy(false)
    }
  }
  return (
    <Dialog
      open={open}
      onClose={onClose}
      width={500}
      title="Test on host"
      description="Opens a TLS connection to nginx on this machine with the host's name (SNI) and compares the certificate it serves."
      footer={
        <>
          <Button onClick={onClose}>Close</Button>
          <Button variant="primary" onClick={run} loading={busy} disabled={!hostId}>Run test</Button>
        </>
      }
    >
      <Field label="Host">
        <Select value={hostId} onChange={setHostId} options={options} placeholder={options.length ? undefined : 'No hosts yet'} />
      </Field>
      {result?.ok && result.matches && (
        <Callout tone="ok" title={`nginx serves ${cert.name} for ${result.sni}`}>
          <span className="mono">{result.address} · {result.servedFingerprint?.slice(0, 23)}…</span>
        </Callout>
      )}
      {result?.ok && !result.matches && (
        <Callout tone="warn" title="nginx serves a different certificate">
          {result.servedSubject} · issued by {result.servedIssuer || '—'} · expires {date(result.servedNotAfter)}.{' '}
          {result.hostUsesCert ? 'Apply pending changes so nginx picks up this certificate.' : 'This host is not configured to use this certificate.'}
        </Callout>
      )}
      {result && !result.ok && <Callout tone="danger" title="Handshake failed">{result.error}</Callout>}
    </Dialog>
  )
}

export default function CertificateDrawer({ cert, open, onClose, usage, onReplace }: {
  cert?: CertView
  open: boolean
  onClose: () => void
  usage?: CertUsage
  onReplace: (c: Certificate) => void
}) {
  const toast = useToast()
  const qc = useQueryClient()
  const { canWrite, isAdmin } = useRole()
  const save = useSaveEntity('certificates')
  const del = useDeleteEntity('certificates')
  const providers = useEntities('dns-providers').data
  const [renewing, setRenewing] = useState(false)
  const [confirmRevoke, setConfirmRevoke] = useState(false)
  const [confirmDelete, setConfirmDelete] = useState(false)
  const [testOpen, setTestOpen] = useState(false)

  if (!cert) {
    return (
      <Drawer open={open} onClose={onClose} title="Certificate" width="wide">
        <div className="row muted"><Spinner /> Loading…</div>
      </Drawer>
    )
  }

  const acme = isACME(cert)
  const exp = certExpiry(cert)
  const used = usageCount(usage)
  const dnsProvider = providers?.find((p) => p.id === cert.dnsProviderId)
  const busy = cert.status === 'pending' || cert.renewing
  const via = cert.challenge === 'dns-01' && dnsProvider ? `DNS-01 via ${dnsProvider.name}` : challengeLabel(cert.challenge)

  const statusTone = cert.status === 'valid' ? (exp.tone === 'warn' ? 'warn' : 'ok') : cert.status === 'pending' ? 'pending' : 'danger'
  const statusWord = cert.renewing ? 'Renewing' : cert.status === 'valid' ? 'Valid' : cert.status === 'pending' ? 'Pending' : cert.status === 'failed' ? 'Failed' : 'Expired'
  const subtitle = [
    exp.days !== undefined ? daysText(exp.days) : undefined,
    `${providerName(cert.provider)}${cert.issuer ? ' ' + cert.issuer : ''}`,
    acme ? via : undefined,
  ].filter(Boolean).join(' · ')

  const renew = async () => {
    setRenewing(true)
    try {
      await api.post(`/api/certificates/${cert.id}/renew`)
      qc.invalidateQueries({ queryKey: keys.entities('certificates') })
      toast.show({ kind: 'info', title: `Renewing ${cert.name}`, message: `${providerName(cert.provider)} · ${via}` })
    } catch (err) {
      toast.error(err, 'Renewal could not start')
    } finally {
      setRenewing(false)
    }
  }

  const setAutoRenew = async (v: boolean) => {
    try {
      await save.mutateAsync({ id: cert.id, name: cert.name, autoRenew: v })
      toast.success(v ? `Auto-renew on for ${cert.name}` : `Auto-renew off for ${cert.name}`, v ? undefined : "You'll be notified 14 days before it expires.")
    } catch (err) {
      toastUnlessFields(toast, err, 'Could not change auto-renew')
    }
  }

  const chain = cert.chain ?? []
  const roleOf = (i: number) => (i === 0 ? 'leaf' : i === chain.length - 1 && chain.length > 1 ? 'root' : 'intermediate')
  const history = [...(cert.history ?? [])].reverse()
  const usageChips = [
    ...(usage?.hosts.map((h) => ({ key: 'h' + h.id, label: h.domain, to: `/hosts?edit=${h.id}` })) ?? []),
    ...(usage?.redirects.map((r) => ({ key: 'r' + r.id, label: `↪ ${r.domain}`, to: '/hosts/redirects' })) ?? []),
    ...(usage?.defaultHost ? [{ key: 'default', label: 'default host', to: '/hosts/default' }] : []),
  ]

  return (
    <>
      <Drawer
        open={open}
        onClose={onClose}
        width="wide"
        title={<span className="mono">{cert.name}</span>}
        subtitle={
          <span className="row gap-8" style={{ marginTop: 4 }}>
            <Status tone={statusTone} pulse={busy}>{statusWord}</Status>
            <span className="muted">{subtitle}</span>
          </span>
        }
        footer={
          <>
            <Tooltip content={used ? 'Reassign the hosts using it first' : 'Delete certificate'}>
              <Button variant="ghost" icon="trash" className="danger-text" disabled={!canWrite || used > 0 || busy} onClick={() => setConfirmDelete(true)}>
                Delete
              </Button>
            </Tooltip>
            <div className="spacer" />
            <Button onClick={onClose}>Close</Button>
          </>
        }
      >
        <div className="row gap-8 wrap">
          {acme ? (
            <Button icon="reload" onClick={renew} loading={renewing} disabled={!canWrite || busy}>
              {busy ? 'Renewing…' : cert.status === 'failed' || cert.status === 'expired' ? 'Retry' : 'Force renew'}
            </Button>
          ) : (
            <Button icon="upload" onClick={() => onReplace(cert)} disabled={!canWrite}>Upload replacement</Button>
          )}
          <Menu
            align="start"
            trigger={<Button icon="download" iconRight="chevron" disabled={!cert.notAfter}>Download .pem</Button>}
            items={[
              { label: 'Full chain', onSelect: () => triggerDownload(downloadUrl(cert.id, 'fullchain')) },
              { label: 'Certificate only', onSelect: () => triggerDownload(downloadUrl(cert.id, 'cert')) },
              { label: 'Intermediate chain', onSelect: () => triggerDownload(downloadUrl(cert.id, 'chain')), disabled: chain.length < 2 },
              'separator',
              { label: isAdmin ? 'Private key' : 'Private key · admins only', icon: 'token', disabled: !isAdmin, onSelect: () => triggerDownload(downloadUrl(cert.id, 'key')) },
            ]}
          />
          <Button icon="check" onClick={() => setTestOpen(true)} disabled={!cert.notAfter}>Test on host…</Button>
          <div className="spacer" />
          {acme && cert.notAfter && !cert.lastError?.startsWith('Revoked') && (
            <Button variant="ghost" className="danger-text" onClick={() => setConfirmRevoke(true)} disabled={!canWrite || busy}>Revoke</Button>
          )}
        </div>

        {cert.status === 'pending' && (
          <Callout tone="info" icon="reload" title={cert.notAfter ? 'Retrying renewal' : `Requesting from ${providerName(cert.provider)}`}>
            {cert.challenge === 'dns-01' ? 'Creating the TXT record and waiting for it to propagate — this can take a few minutes.' : 'Serving the challenge on port 80 — usually under a minute.'}
          </Callout>
        )}
        {cert.lastError && cert.status !== 'valid' && cert.status !== 'pending' && (
          <Callout tone={cert.status === 'expired' ? 'danger' : 'warn'} title={cert.notAfter ? 'Renewal failed' : 'Request failed'}>
            <span className="mono">{cert.lastError}</span>
            {cert.challenge === 'dns-01' && dnsProvider && (
              <div style={{ marginTop: 6 }}>
                <Link to="/settings/tls" style={{ textDecoration: 'underline' }}>Check {dnsProvider.name} credentials</Link>
              </div>
            )}
          </Callout>
        )}

        <div className="kv">
          <span className="k">SANs</span>
          <span className="row gap-6 wrap">{cert.domains.map((d) => <span key={d} className="cs-chip">{d}</span>)}</span>
          <span className="k">Key</span>
          <span className="mono">{cert.keyType || '—'}</span>
          <span className="k">Not before → after</span>
          <span className="mono">{cert.notBefore ? `${date(cert.notBefore)} → ${date(cert.notAfter)}` : '—'}</span>
          <span className="k">Fingerprint (SHA-256)</span>
          <span className="row gap-8" style={{ minWidth: 0 }}>
            <span className="mono small truncate" title={cert.fingerprint}>{cert.fingerprint ? `${cert.fingerprint.slice(0, 5)}…${cert.fingerprint.slice(-5)}` : '—'}</span>
            {cert.fingerprint && <CopyButton text={cert.fingerprint} />}
          </span>
          <span className="k">Auto-renew</span>
          <span className="row gap-8">
            {acme ? (
              <>
                <Toggle checked={cert.autoRenew} onChange={setAutoRenew} disabled={!canWrite} label="Auto-renew" />
                <span className="small muted">{cert.autoRenew ? 'Renews automatically before expiry' : 'Off — renew manually'}</span>
              </>
            ) : (
              <span className="small muted">Manual · custom certificates can't be renewed by Relay</span>
            )}
          </span>
        </div>

        {chain.length > 0 && (
          <div className="col gap-10">
            <div className="section-title">Chain</div>
            <div className="cs-chain">
              {chain.map((c, i) => (
                <div key={i} className="cs-chain-row" style={{ paddingLeft: i * 18 }}>
                  {i > 0 && <span className="line" />}
                  <span className="mono">{c}</span>
                  <span className="small muted">{roleOf(i)}{roleOf(i) === 'intermediate' && cert.issuer === c ? ` · ${providerName(cert.provider)}` : ''}</span>
                </div>
              ))}
            </div>
          </div>
        )}

        <div className="col gap-10">
          <div className="row">
            <div className="section-title">Used by {used} {used === 1 ? 'host' : 'hosts'}</div>
            {used > 0 && <span className="small muted">· Deleting requires reassigning them</span>}
          </div>
          {used ? (
            <div className="row gap-6 wrap">
              {usageChips.slice(0, 8).map((c) => <Link key={c.key} to={c.to} className="cs-chip">{c.label}</Link>)}
              {usageChips.length > 8 && <span className="cs-chip">+{usageChips.length - 8}</span>}
            </div>
          ) : (
            <div className="small muted">Not used by any host yet — pick it in a host's SSL tab.</div>
          )}
        </div>

        <div className="col gap-6">
          <div className="section-title">History</div>
          {history.length ? (
            <div className="cs-history">
              {history.map((e, i) => (
                <div key={i} className="cs-history-row">
                  <span className="mono muted">{dateTime(e.at)}</span>
                  <span>{e.message}{e.durationMs > 0 && <span className="muted"> · {ms(e.durationMs)}</span>}</span>
                  <Badge tone={e.result === 'ok' ? 'ok' : e.result === 'retried' ? 'warn' : 'danger'}>{e.result}</Badge>
                </div>
              ))}
            </div>
          ) : (
            <div className="small muted">No events yet.</div>
          )}
        </div>
      </Drawer>

      <TestOnHostDialog open={testOpen} onClose={() => setTestOpen(false)} cert={cert} usage={usage} />
      <ConfirmDialog
        open={confirmRevoke}
        onClose={() => setConfirmRevoke(false)}
        danger
        title={`Revoke ${cert.name}?`}
        message="Let's Encrypt marks the certificate as revoked; browsers checking revocation will reject it. Hosts keep serving it until you request a new one. Only do this if the private key leaked."
        confirmLabel="Revoke"
        typeToConfirm={cert.name}
        onConfirm={async () => {
          try {
            await api.post(`/api/certificates/${cert.id}/revoke`)
            qc.invalidateQueries({ queryKey: keys.entities('certificates') })
            toast.show({ kind: 'warning', title: `${cert.name} revoked`, message: 'Request a new certificate for the hosts using it.' })
          } catch (err) {
            toast.error(err, 'Revocation failed')
            throw err
          }
        }}
      />
      <ConfirmDialog
        open={confirmDelete}
        onClose={() => setConfirmDelete(false)}
        danger
        title={`Delete ${cert.name}?`}
        message="The certificate and its key files are removed from this machine. This can't be undone."
        confirmLabel="Delete certificate"
        onConfirm={async () => {
          try {
            await del.mutateAsync(cert.id)
            toast.success('Certificate deleted', cert.name)
            onClose()
          } catch (err) {
            toast.error(err, 'Could not delete certificate')
            throw err
          }
        }}
      />
    </>
  )
}
