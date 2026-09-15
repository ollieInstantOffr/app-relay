// Request certificate dialog (design 23). Shared: hosts & lb slices import it.
import { useEffect, useMemo, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../../lib/api'
import { keys, useEntities, useRole, useSettings } from '../../lib/queries'
import type { Certificate, Challenge } from '../../lib/types'
import { Button, Callout, Checkbox, ChipsInput, Dialog, Field, RadioCard, Select, Spinner, useToast } from '../../components/ui'
import DNSProviderDialog from './DNSProviderDialog'
import { challengeLabel, directoryHost, fieldErrors, isWildcard, providerName, toastUnlessFields } from './common'
import './certs.css'

const domainRe = /^(\*\.)?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]*[a-z0-9]$/i

export default function RequestCertificateDialog(props: {
  open: boolean
  onClose: () => void
  defaultDomains?: string[]
  onRequested?: (cert: Certificate) => void
}) {
  const { open, onClose, defaultDomains, onRequested } = props
  const toast = useToast()
  const qc = useQueryClient()
  const { canWrite } = useRole()
  const tls = useSettings('tls').data
  const providers = useEntities('dns-providers').data ?? []
  const [domains, setDomains] = useState<string[]>([])
  const [challenge, setChallenge] = useState<Challenge>('dns-01')
  const [dnsProviderId, setDnsProviderId] = useState('')
  const [staging, setStaging] = useState(false)
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [busy, setBusy] = useState(false)
  const [addProvider, setAddProvider] = useState(false)

  useEffect(() => {
    if (!open) return
    const initial = (defaultDomains ?? []).map((d) => d.trim().toLowerCase()).filter(Boolean)
    setDomains(initial)
    setErrors({})
    setStaging(false)
    const wildcard = initial.some(isWildcard)
    setChallenge(wildcard ? 'dns-01' : tls?.preferredChallenge === 'http-01' ? 'http-01' : providers.length ? 'dns-01' : 'http-01')
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open])

  useEffect(() => {
    if (!dnsProviderId || !providers.some((p) => p.id === dnsProviderId)) {
      const best = providers.find((p) => p.status === 'ok') ?? providers[0]
      setDnsProviderId(best?.id ?? '')
    }
  }, [providers, dnsProviderId])

  const wildcard = domains.find(isWildcard)
  useEffect(() => {
    if (wildcard && challenge === 'http-01') setChallenge('dns-01')
  }, [wildcard, challenge])

  const primary = domains[0]
  const rate = useQuery({
    queryKey: ['certificates', 'rate-limit', primary],
    queryFn: () => api.get<{ limit: number; used: number; registeredDomain: string }>(`/api/certificates/rate-limit?domain=${encodeURIComponent(primary)}`),
    enabled: open && !!primary,
    staleTime: 30_000,
  })

  const invalidDomain = useMemo(() => domains.find((d) => !domainRe.test(d)), [domains])
  const selectedProvider = providers.find((p) => p.id === dnsProviderId)
  const custom = tls?.acmeProvider === 'custom'
  const production = !custom && tls?.acmeProvider !== 'letsencrypt-staging'
  const caName = custom ? `the ACME server${directoryHost(tls?.acmeDirectoryUrl) ? ` (${directoryHost(tls?.acmeDirectoryUrl)})` : ''}` : 'Let’s Encrypt'

  const submit = async () => {
    setBusy(true)
    setErrors({})
    try {
      const cert = await api.post<Certificate>('/api/certificates/request', {
        domains,
        challenge,
        dnsProviderId: challenge === 'dns-01' ? dnsProviderId : undefined,
        staging: staging && production,
        autoRenew: true,
      })
      qc.invalidateQueries({ queryKey: keys.entities('certificates') })
      toast.show({
        kind: 'info',
        title: `Requesting certificate for ${cert.name}`,
        message: `${providerName(cert.provider)} · ${challengeLabel(cert.challenge)}${challenge === 'dns-01' ? ' · DNS propagation can take a couple of minutes' : ' · usually under a minute'}`,
      })
      onRequested?.(cert)
      onClose()
    } catch (err) {
      const f = fieldErrors(err)
      setErrors(f)
      toastUnlessFields(toast, err, 'Certificate request failed')
    } finally {
      setBusy(false)
    }
  }

  const domainError = errors.domains ?? Object.entries(errors).find(([k]) => k.startsWith('domains.'))?.[1] ?? (invalidDomain ? `${invalidDomain} is not a valid domain name` : undefined)

  return (
    <>
      <Dialog
        open={open && !addProvider}
        onClose={onClose}
        width={520}
        icon="certificates"
        title="Request certificate"
        description={custom
          ? `Custom ACME server · ${directoryHost(tls?.acmeDirectoryUrl) || 'not configured'} — change in Settings → Default TLS`
          : production ? "Let's Encrypt · production" : "Let's Encrypt · staging (browser-untrusted) — change in Settings → Default TLS"}
        footer={
          <>
            <span className="mono small muted" style={{ marginRight: 'auto' }}>
              {custom ? '' : rate.data ? `LE limit: ${rate.data.limit} certs / domain / week · ${rate.data.used} used` : primary ? 'LE limit: 50 certs / domain / week' : ''}
            </span>
            <Button onClick={onClose}>Cancel</Button>
            <Button
              variant="primary"
              onClick={submit}
              loading={busy}
              disabled={!canWrite || !domains.length || !!invalidDomain || (challenge === 'dns-01' && !dnsProviderId)}
            >
              Request
            </Button>
          </>
        }
      >
        <Field label="Domains" error={domainError} hint="Enter adds a name · wildcards like *.home.lan need DNS-01">
          <ChipsInput values={domains} onChange={setDomains} placeholder={domains.length ? 'add another…' : 'app.example.com'} invalid={!!domainError} />
        </Field>
        <Field label="Challenge" error={errors.challenge ?? errors.dnsProviderId}>
          <div className="col gap-8">
            <RadioCard
              selected={challenge === 'dns-01'}
              onSelect={() => setChallenge('dns-01')}
              title={
                <span className="row gap-8">
                  DNS-01
                  {providers.length > 0 && (
                    <span onClick={(e) => e.stopPropagation()} style={{ minWidth: 180 }}>
                      <Select
                        inputSize="sm"
                        value={dnsProviderId}
                        onChange={(v) => {
                          setDnsProviderId(v)
                          setChallenge('dns-01')
                        }}
                        options={providers.map((p) => ({ value: p.id, label: `${p.name}${p.status === 'failed' ? ' · auth failed' : ''}` }))}
                      />
                    </span>
                  )}
                </span>
              }
              description={providers.length ? 'Required for wildcards · works behind CGNAT' : 'Required for wildcards · add a DNS provider first'}
            >
              {providers.length === 0 && (
                <Button size="sm" icon="plus" style={{ marginTop: 8 }} onClick={(e) => { e.stopPropagation(); setAddProvider(true) }}>
                  Add provider
                </Button>
              )}
              {selectedProvider?.status === 'failed' && challenge === 'dns-01' && (
                <div className="small warn-text" style={{ marginTop: 6 }}>Last credential test failed: {selectedProvider.lastError}</div>
              )}
            </RadioCard>
            <RadioCard
              selected={challenge === 'http-01'}
              onSelect={() => setChallenge('http-01')}
              disabled={!!wildcard}
              title="HTTP-01"
              description={wildcard ? 'Unavailable — wildcard domains need DNS-01' : `${caName.charAt(0).toUpperCase()}${caName.slice(1)} fetches a token from port 80 · the domain must point at this machine`}
            />
          </div>
        </Field>
        {!custom && (
          <Checkbox
            checked={staging && production}
            disabled={!production}
            onChange={setStaging}
            label="Use staging first (no rate limits, browser-untrusted)"
          />
        )}
        {!canWrite && <Callout tone="info">Viewers can't request certificates.</Callout>}
        {busy && <div className="row small muted"><Spinner /> Creating order…</div>}
      </Dialog>
      <DNSProviderDialog
        open={addProvider}
        onClose={() => setAddProvider(false)}
        onSaved={(p) => {
          setDnsProviderId(p.id)
          setChallenge('dns-01')
        }}
      />
    </>
  )
}
