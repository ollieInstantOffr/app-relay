import { Fragment, useEffect, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import {
  Badge, Button, Callout, Card, Checkbox, ChipsInput, Dialog, Field, Icon, IconButton, Input, RadioCard, Select, Spinner, Toggle,
  ToggleCard, cx, useToast,
} from '../../components/ui'
import { api, errorMessage } from '../../lib/api'
import { keys, useEntities, usePending, useSettings } from '../../lib/queries'
import { daysUntil, pluralize } from '../../lib/format'
import type { Certificate, ForwardAuth, RateLimit } from '../../lib/types'
import {
  DOMAIN_RE, algorithmShort, applyNowAction, certCovers, fieldErrors, nextFreePort,
  type ExposePreview, type ExposeRequest, type ExposeResult,
} from './lbApi'

const STEPS = ['Backend', 'Domain & TLS', 'Access', 'Review']

const CHALLENGES = [
  { value: 'http-01', label: 'HTTP-01' },
  { value: 'dns-01', label: 'DNS-01' },
  { value: 'tls-alpn-01', label: 'TLS-ALPN-01' },
]

const PROVIDERS = [
  { value: 'authelia', label: 'Authelia' },
  { value: 'authentik', label: 'authentik' },
  { value: 'oauth2-proxy', label: 'oauth2-proxy' },
  { value: 'custom', label: 'Custom' },
]

function stepForField(k: string) {
  if (k === 'backendId') return 0
  if (k === 'domain' || k.startsWith('certificate')) return 1
  return 2
}

function certLabel(c: Certificate) {
  const d = daysUntil(c.notAfter)
  if (c.status === 'pending') return `${c.name} · issuing`
  return d === undefined ? c.name : `${c.name} · ${d} days left`
}

export default function ExposeWizard({ backendId, onClose }: { backendId: string; onClose: () => void }) {
  const backends = useEntities('backends').data
  const frontends = useEntities('frontends').data ?? []
  const certs = useEntities('certificates').data ?? []
  const lists = useEntities('access-lists').data ?? []
  const dnsProviders = useEntities('dns-providers').data ?? []
  const general = useSettings('general').data
  const tls = useSettings('tls').data
  const haproxy = useSettings('haproxy').data
  const pending = usePending().data
  const qc = useQueryClient()
  const toast = useToast()

  const [step, setStep] = useState(backendId ? 1 : 0)
  const [req, setReq] = useState<ExposeRequest>({
    backendId,
    domain: '',
    certificate: { mode: 'request' },
    forceHttps: true,
    websockets: true,
    access: { mode: 'public' },
    blockExploits: true,
    noIndex: false,
    applyNow: false,
  })
  const [seeded, setSeeded] = useState(false)
  const [certTouched, setCertTouched] = useState(false)
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [preview, setPreview] = useState<ExposePreview | null>(null)
  const [previewErr, setPreviewErr] = useState('')
  const [submitting, setSubmitting] = useState<'pending' | 'apply' | null>(null)
  const [openAfter, setOpenAfter] = useState(true)

  // Defaults for new hosts (Settings → General).
  useEffect(() => {
    if (seeded || !general) return
    setSeeded(true)
    setReq((r) => ({
      ...r,
      forceHttps: general.defaults.forceHttps,
      websockets: general.defaults.websockets,
      blockExploits: general.defaults.blockExploits,
      access: general.defaults.accessListId ? { mode: 'list', accessListId: general.defaults.accessListId } : r.access,
    }))
  }, [general, seeded])

  const patch = (p: Partial<ExposeRequest>) => {
    setReq((r) => ({ ...r, ...p }))
    setErrors({})
  }

  const backend = backends?.find((b) => b.id === req.backendId)
  const httpBackends = (backends ?? []).filter((b) => b.mode === 'http')
  const domain = req.domain.trim().toLowerCase().replace(/\.$/, '')
  const domainOk = DOMAIN_RE.test(domain) && domain.includes('.')
  const covering = domainOk ? certs.filter((c) => certCovers(c.domains, domain) && c.status !== 'failed' && c.status !== 'expired') : []
  const mode = req.certificate.mode
  const challenge = req.certificate.challenge || tls?.preferredChallenge || 'http-01'
  const tlsOn = mode !== 'none'
  const scheme = tlsOn ? 'https' : 'http'
  const port = preview?.port ?? nextFreePort(frontends, haproxy?.exposePortStart ?? 10080)
  const list = lists.find((l) => l.id === req.access.accessListId)
  const fa = req.forwardAuth
  const rl = req.rateLimit
  const geo = req.geoBlock

  // Prefer an existing certificate that already covers the domain.
  useEffect(() => {
    if (certTouched) return
    const best = covering.find((c) => c.status === 'valid')
    setReq((r) => {
      if (best && r.certificate.mode !== 'existing') return { ...r, certificate: { mode: 'existing', certificateId: best.id } }
      if (!best && r.certificate.mode === 'existing') return { ...r, certificate: { mode: 'request' } }
      return r
    })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [domain, certs.length, certTouched])

  const setCert = (c: ExposeRequest['certificate']) => {
    setCertTouched(true)
    patch({ certificate: c })
  }

  const body = (applyNow: boolean): ExposeRequest => ({
    ...req,
    domain,
    certificate: mode === 'request' ? { mode, challenge, dnsProviderId: challenge === 'dns-01' ? req.certificate.dnsProviderId : undefined } : req.certificate,
    forceHttps: tlsOn && req.forceHttps,
    access: req.access.mode === 'list' ? req.access : { mode: 'public' },
    forwardAuth: fa?.enabled ? fa : undefined,
    rateLimit: rl?.enabled ? rl : undefined,
    geoBlock: geo?.enabled ? geo : undefined,
    applyNow,
  })

  const canContinue = (() => {
    switch (step) {
      case 0:
        return !!backend
      case 1:
        if (!domainOk) return false
        if (mode === 'existing' && !req.certificate.certificateId) return false
        if (mode === 'request' && challenge === 'dns-01' && !req.certificate.dnsProviderId) return false
        return true
      case 2:
        if (req.access.mode === 'list' && !req.access.accessListId) return false
        if (fa?.enabled && !fa.verifyUrl.trim()) return false
        if (rl?.enabled && !(rl.requestsPerSecond > 0)) return false
        if (geo?.enabled && geo.allowCountries.length === 0) return false
        return true
    }
    return true
  })()

  // Review: render both configs server-side.
  useEffect(() => {
    if (step !== 3) return
    let live = true
    setPreview(null)
    setPreviewErr('')
    api
      .post<ExposePreview>('/api/lb/expose/preview', body(false))
      .then((p) => live && setPreview(p))
      .catch((e) => {
        if (!live) return
        const f = fieldErrors(e)
        const k = Object.keys(f)
        if (k.length) {
          setErrors(f)
          setStep(stepForField(k[0]))
        } else setPreviewErr(errorMessage(e))
      })
    return () => {
      live = false
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [step])

  const submit = async (applyNow: boolean) => {
    setSubmitting(applyNow ? 'apply' : 'pending')
    try {
      const res = await api.post<ExposeResult>('/api/lb/expose', body(applyNow))
      qc.invalidateQueries({ queryKey: ['entities'] })
      qc.invalidateQueries({ queryKey: keys.pending })
      qc.invalidateQueries({ queryKey: keys.versions })
      const url = `${scheme}://${domain}`
      if (applyNow) {
        if (res.applyError) {
          toast.show({ kind: 'error', title: 'Saved, but the apply failed', message: `${res.applyError} · the host and frontend stay in pending changes.` })
        } else {
          toast.show({
            kind: 'success',
            title: `${domain} is live`,
            message: res.version ? `Applied as config version v${res.version.id}.` : 'Applied.',
            actions: [{ label: 'Open', onClick: () => window.open(url, '_blank', 'noopener') }],
          })
          if (openAfter) window.open(url, '_blank', 'noopener')
        }
      } else {
        toast.show({
          kind: 'success',
          title: 'Expose added to pending changes',
          message: `${domain} → ${res.frontend.bind} → ${backend?.name ?? ''}`,
          actions: [applyNowAction],
        })
      }
      onClose()
    } catch (e) {
      const f = fieldErrors(e)
      const k = Object.keys(f)
      if (k.length) {
        setErrors(f)
        setStep(stepForField(k[0]))
      } else {
        toast.error(e, 'Could not expose backend')
      }
    } finally {
      setSubmitting(null)
    }
  }

  const protection = [fa?.enabled && 'sign-in', rl?.enabled && 'rate-limit', req.blockExploits && 'exploits', geo?.enabled && 'geo', req.noIndex && 'noindex']
    .filter(Boolean)
    .join(' · ')
  const tlsText = mode === 'request' ? `LE · ${challenge.toUpperCase()}` : mode === 'existing' ? certs.find((c) => c.id === req.certificate.certificateId)?.name ?? '—' : 'none'

  const diagram = (
    <div className="lb-path">
      <div className="lb-preview-title" style={{ marginBottom: 8 }}>What gets created</div>
      <div className="lb-path-node">
        <div className="k">Internet</div>
        <div className="v">{domainOk ? `${scheme}://${domain}` : `${scheme}://…`}</div>
      </div>
      <div className="lb-path-arrow">↓</div>
      <div className="lb-path-node new">
        <div className="k">Reverse proxy <span className="lb-tag-new">new host</span></div>
        <div className="v">nginx · {tlsOn ? 'TLS' : 'HTTP'} · {req.access.mode === 'list' ? list?.name ?? 'access list' : 'public'}</div>
      </div>
      <div className="lb-path-arrow">↓</div>
      <div className="lb-path-node new">
        <div className="k">Load balancer <span className="lb-tag-new">new frontend</span></div>
        <div className="v">haproxy · 127.0.0.1:{port}</div>
      </div>
      <div className="lb-path-arrow">↓</div>
      <div className="lb-path-node">
        <div className="k">Backend · {backend?.name ?? '—'}</div>
        <div className="v">{backend ? `${pluralize(backend.servers.length, 'server')} · ${algorithmShort(backend.algorithm)}` : 'pick a backend'}</div>
      </div>
      <div className="faint" style={{ fontSize: 11, marginTop: 8, lineHeight: 1.5 }}>
        The LB frontend binds to localhost only; the proxy is the sole public entry.
      </div>
    </div>
  )

  const summary = (
    <div className="lb-path">
      <div className="lb-preview-title" style={{ marginBottom: 8 }}>Summary so far</div>
      <div className="lb-summary">
        <span className="k">Backend</span><span className="v">{backend?.name ?? '—'}</span>
        <span className="k">Domain</span><span className="v">{domain || '—'}</span>
        <span className="k">TLS</span><span className="v">{tlsText}</span>
        <span className="k">Access</span><span className="v">{req.access.mode === 'list' ? list?.name ?? '—' : 'public'}</span>
        <span className="k">Protection</span><span className="v">{protection || 'none'}</span>
      </div>
      <div className="faint" style={{ fontSize: 11, marginTop: 10, lineHeight: 1.5 }}>You can change any of this later from the host's drawer.</div>
    </div>
  )

  return (
    <Dialog
      open
      onClose={onClose}
      dismissable={false}
      className="wizard lb-wizard"
      footer={
        <>
          <span className="small faint">Step {step + 1} of 4</span>
          <span className="spacer" />
          {step > 0 && <Button size="md" disabled={!!submitting} onClick={() => setStep(step - 1)}>← Back</Button>}
          {step < 3 ? (
            <Button size="md" variant="primary" disabled={!canContinue} onClick={() => setStep(step + 1)}>
              {step === 2 ? 'Review →' : 'Continue →'}
            </Button>
          ) : (
            <>
              <Button size="md" loading={submitting === 'pending'} disabled={!!submitting || !!previewErr} onClick={() => submit(false)}>Add to pending</Button>
              <Button size="md" variant="primary" loading={submitting === 'apply'} disabled={!!submitting || !preview} onClick={() => submit(true)}>Apply now</Button>
            </>
          )}
        </>
      }
    >
      <div className="lb-wiz-head">
        <div className="grow">
          <div className="lb-wiz-title">
            {backend ? <>Expose <span className="mono">{backend.name}</span> online</> : 'Expose a backend online'}
          </div>
          <div className="lb-wiz-sub">Creates a proxy host on the reverse proxy that points at this load balancer. One save, both configs.</div>
        </div>
        <IconButton icon="close" label="Close" onClick={onClose} />
      </div>
      <div className="lb-wiz-steps">
        {STEPS.map((s, i) => (
          <Fragment key={s}>
            {i > 0 && <span className={cx('lb-wiz-line', i <= step && 'done')} />}
            <span className={cx('lb-wiz-step', i === step && 'active', i < step && 'done')}>
              <span className="n">{i < step ? <Icon name="check" size={10} /> : i + 1}</span>
              {s}
            </span>
          </Fragment>
        ))}
      </div>

      {step === 0 && (
        <div className="lb-wiz-body">
          <div className="col gap-16">
            <Field label="Backend" hint="Only HTTP backends can sit behind the reverse proxy" error={errors.backendId}>
              <Select
                placeholder="Pick a backend…"
                value={req.backendId}
                onChange={(v) => patch({ backendId: v })}
                options={httpBackends.map((b) => ({ value: b.id, label: `${b.name} · ${pluralize(b.servers.length, 'server')} · ${algorithmShort(b.algorithm)}` }))}
              />
            </Field>
            {backends && httpBackends.length === 0 && <Callout tone="warn">No HTTP backends yet. Create one on the Backends tab first.</Callout>}
            {backend && (
              <Card pad>
                <div className="col gap-6">
                  <div className="lb-name">{backend.name}</div>
                  {backend.servers.map((s) => (
                    <div key={s.id} className="mono small muted">
                      {s.address}:{s.port} · w {s.weight}{s.role === 'backup' ? ' · backup' : ''}
                    </div>
                  ))}
                  {backend.servers.length === 0 && <div className="small warn-text">This backend has no servers yet.</div>}
                </div>
              </Card>
            )}
          </div>
          {diagram}
        </div>
      )}

      {step === 1 && (
        <div className="lb-wiz-body">
          <div className="col gap-18">
            <Field
              label="Public domain"
              error={errors.domain ?? (req.domain && !domainOk ? 'Not a valid domain name' : undefined)}
              hint={general?.publicIp ? `Point DNS for this domain at ${general.publicIp} (this server)` : 'Point DNS for this domain at this server'}
            >
              <Input mono autoFocus placeholder="app.example.com" value={req.domain} invalid={!!errors.domain || (!!req.domain && !domainOk)} onChange={(e) => patch({ domain: e.target.value })} />
            </Field>
            <Field label="Certificate" error={errors['certificate.certificateId'] ?? errors['certificate.dnsProviderId'] ?? errors['certificate.challenge'] ?? errors['certificate.mode']}>
              <div className="col gap-6">
                <RadioCard
                  selected={mode === 'request'}
                  onSelect={() => setCert({ mode: 'request', challenge })}
                  title={`Request new · ${tls?.acmeProvider === 'custom' ? 'ACME server' : `Let's Encrypt${tls?.acmeProvider === 'letsencrypt-staging' ? ' (staging)' : ''}`}`}
                  description={`${challenge.toUpperCase()} · auto-renews${challenge === 'http-01' ? ' · ready in ~30s' : ''}`}
                >
                  {mode === 'request' && (
                    <div className="row gap-8" style={{ marginTop: 8 }} onClick={(e) => e.stopPropagation()}>
                      <div style={{ width: 140 }}>
                        <Select inputSize="sm" value={challenge} options={CHALLENGES} onChange={(v) => setCert({ ...req.certificate, mode: 'request', challenge: v })} />
                      </div>
                      {challenge === 'dns-01' && (
                        <div className="grow">
                          <Select
                            inputSize="sm"
                            placeholder={dnsProviders.length ? 'DNS provider…' : 'No DNS providers configured'}
                            value={req.certificate.dnsProviderId ?? ''}
                            options={dnsProviders.map((p) => ({ value: p.id, label: `${p.name} · ${p.type}` }))}
                            onChange={(v) => setCert({ ...req.certificate, mode: 'request', challenge, dnsProviderId: v })}
                          />
                        </div>
                      )}
                    </div>
                  )}
                </RadioCard>
                <RadioCard
                  selected={mode === 'existing'}
                  disabled={covering.length === 0}
                  onSelect={() => setCert({ mode: 'existing', certificateId: covering[0]?.id })}
                  title="Use existing"
                  description={
                    covering.length
                      ? certLabel(covering.find((c) => c.id === req.certificate.certificateId) ?? covering[0])
                      : domainOk
                        ? `No certificate covers ${domain}`
                        : 'Enter a domain to find matching certificates'
                  }
                >
                  {mode === 'existing' && covering.length > 1 && (
                    <div style={{ marginTop: 8 }} onClick={(e) => e.stopPropagation()}>
                      <Select inputSize="sm" value={req.certificate.certificateId ?? ''} options={covering.map((c) => ({ value: c.id, label: certLabel(c) }))} onChange={(v) => setCert({ mode: 'existing', certificateId: v })} />
                    </div>
                  )}
                </RadioCard>
                <RadioCard selected={mode === 'none'} onSelect={() => setCert({ mode: 'none' })} title="None (HTTP only)" description="Not recommended for anything public" />
              </div>
            </Field>
            <div className="grid-2" style={{ gap: 10 }}>
              <div className="lb-toggle-sm">
                <span className="grow">Force HTTPS</span>
                <Toggle checked={tlsOn && req.forceHttps} disabled={!tlsOn} onChange={(v) => patch({ forceHttps: v })} />
              </div>
              <div className="lb-toggle-sm">
                <span className="grow">Websockets</span>
                <Toggle checked={req.websockets} onChange={(v) => patch({ websockets: v })} />
              </div>
            </div>
          </div>
          {diagram}
        </div>
      )}

      {step === 2 && (
        <div className="lb-wiz-body">
          <div className="col gap-16">
            <div className="muted">Who may reach <span className="mono">{domain}</span>, and how hard it is to abuse.</div>
            <Field label="Who can reach it" error={errors['access.accessListId']}>
              <div className="col gap-6">
                <RadioCard
                  selected={req.access.mode === 'public'}
                  onSelect={() => patch({ access: { mode: 'public' } })}
                  title="Anyone on the internet"
                  description="It's a public app — protect it with the options below"
                />
                <RadioCard
                  selected={req.access.mode === 'list'}
                  disabled={lists.length === 0}
                  onSelect={() => patch({ access: { mode: 'list', accessListId: req.access.accessListId ?? lists[0]?.id } })}
                  title="Access list"
                  description={lists.length ? `e.g. ${lists.slice(0, 3).map((l) => l.name).join(' · ')}` : 'No access lists yet'}
                >
                  {req.access.mode === 'list' && (
                    <div style={{ marginTop: 8, maxWidth: 260 }} onClick={(e) => e.stopPropagation()}>
                      <Select inputSize="sm" value={req.access.accessListId ?? ''} placeholder="choose…" options={lists.map((l) => ({ value: l.id, label: l.name }))} onChange={(v) => patch({ access: { mode: 'list', accessListId: v } })} />
                    </div>
                  )}
                </RadioCard>
              </div>
            </Field>
            <div className="col gap-8">
              <ToggleCard
                title="Sign-in required (forward-auth)"
                description={fa?.enabled && fa.verifyUrl ? `${PROVIDERS.find((p) => p.value === fa.provider)?.label ?? fa.provider} at ${hostOf(fa.verifyUrl)}` : 'Authelia, authentik or oauth2-proxy in front of the app'}
                checked={!!fa?.enabled}
                onChange={(v) => patch({ forwardAuth: { ...defaultForwardAuth(), ...fa, enabled: v } })}
              />
              {fa?.enabled && (
                <div className="grid-2">
                  <Field label="Provider">
                    <Select value={fa.provider} options={PROVIDERS} onChange={(v) => patch({ forwardAuth: { ...fa, provider: v } })} />
                  </Field>
                  <Field label="Verify URL" error={errors['forwardAuth.verifyUrl']}>
                    <Input mono placeholder="http://10.0.0.5:9091/api/verify" value={fa.verifyUrl} onChange={(e) => patch({ forwardAuth: { ...fa, verifyUrl: e.target.value } })} />
                  </Field>
                </div>
              )}
              <ToggleCard
                title="Rate limit"
                description={rl?.enabled ? `${rl.requestsPerSecond} req/s per IP, burst ${rl.burst}` : 'Per client IP · 429 when exceeded'}
                checked={!!rl?.enabled}
                onChange={(v) => patch({ rateLimit: { ...defaultRateLimit(), ...rl, enabled: v } })}
              />
              {rl?.enabled && (
                <div className="grid-2">
                  <Field label="Requests / s">
                    <Input mono inputMode="numeric" value={rl.requestsPerSecond || ''} onChange={(e) => patch({ rateLimit: { ...rl, requestsPerSecond: Number(e.target.value.replace(/\D/g, '')) || 0 } })} />
                  </Field>
                  <Field label="Burst">
                    <Input mono inputMode="numeric" value={rl.burst || ''} onChange={(e) => patch({ rateLimit: { ...rl, burst: Number(e.target.value.replace(/\D/g, '')) || 0 } })} />
                  </Field>
                </div>
              )}
              <ToggleCard title="Block exploits" description="Common scanner paths" checked={req.blockExploits} onChange={(v) => patch({ blockExploits: v })} />
              <ToggleCard
                title="Geo-block"
                description="Allow only selected countries"
                checked={!!geo?.enabled}
                onChange={(v) => patch({ geoBlock: { allowCountries: [], ...geo, enabled: v } })}
              />
              {geo?.enabled && (
                <Field label="Allowed countries" hint="ISO 3166 codes · Enter to add">
                  <ChipsInput
                    values={geo.allowCountries}
                    placeholder="DE"
                    validate={(v) => /^[A-Za-z]{2}$/.test(v)}
                    onChange={(vals) => patch({ geoBlock: { ...geo, allowCountries: [...new Set(vals.map((x) => x.toUpperCase()))] } })}
                  />
                </Field>
              )}
              <ToggleCard title="Hide from search engines" description="X-Robots-Tag: noindex" checked={req.noIndex} onChange={(v) => patch({ noIndex: v })} />
            </div>
            {req.access.mode === 'public' && !fa?.enabled && (
              <Callout tone="warn">
                Public with no sign-in. Make sure the app itself has authentication — Relay can't add it for you unless you pick forward-auth.
              </Callout>
            )}
          </div>
          {summary}
        </div>
      )}

      {step === 3 && (
        <div className="lb-wiz-body single">
          <div className="col gap-16">
            <div>
              <div className="semibold">Review &amp; apply</div>
              <div className="small muted">Two configs change. Both are validated before anything reloads.</div>
            </div>
            {previewErr && <Callout tone="danger">{previewErr}</Callout>}
            <div className="lb-review">
              <ReviewCard title="Reverse proxy" tag="+1 host" label="nginx -t" valid={preview?.nginxValid} output={preview?.nginxOutput} code={preview?.nginx} loading={!preview && !previewErr} />
              <ReviewCard title="Load balancer" tag="+1 frontend" label="haproxy -c" valid={preview?.haproxyValid} output={preview?.haproxyOutput} code={preview?.haproxy} loading={!preview && !previewErr} />
            </div>
            <div>
              <div className="section-title" style={{ marginBottom: 10 }}>What happens when you apply</div>
              <ol className="lb-steps-list">
                {[
                  mode === 'request' && `Request certificate (${challenge.toUpperCase()}${challenge === 'http-01' ? ', ~30 s' : ''})`,
                  'Add HAProxy frontend, hot-reload (0 dropped connections)',
                  'Add nginx host, reload, health-check for 10 s — auto-rollback on failure',
                  `Saved as config version v${(pending?.liveVersion ?? 0) + 1}`,
                ]
                  .filter(Boolean)
                  .map((t, i) => (
                    <li key={i}><span className="n">{i + 1}</span>{t}</li>
                  ))}
              </ol>
            </div>
            <Checkbox checked={openAfter} onChange={setOpenAfter} label={`Open ${scheme}://${domain} in a new tab when done`} />
          </div>
        </div>
      )}
    </Dialog>
  )
}

function ReviewCard({ title, tag, label, valid, output, code, loading }: {
  title: string
  tag: string
  label: string
  valid?: boolean | null
  output?: string
  code?: string
  loading: boolean
}) {
  return (
    <Card>
      <div className="card-header">
        {title}
        <Badge tone="ok">{tag}</Badge>
        <div className="spacer" />
        {loading ? (
          <Spinner />
        ) : valid === true ? (
          <span className="mono small ok-text" title={output}>{label} ✓</span>
        ) : valid === false ? (
          <span className="mono small danger-text" title={output}>{label} failed</span>
        ) : (
          <span className="mono small faint" title={output}>not validated</span>
        )}
      </div>
      <pre>{loading ? 'Rendering…' : code || '# nothing rendered'}</pre>
      {valid === false && output && (
        <div className="card-row small danger-text" style={{ whiteSpace: 'pre-wrap', borderTop: '1px solid var(--hairline-soft)' }}>{output}</div>
      )}
    </Card>
  )
}

function defaultForwardAuth(): ForwardAuth {
  return { enabled: true, provider: 'authelia', verifyUrl: '', passRemoteUser: true, passRemoteGroups: false, skipWellKnown: true }
}

function defaultRateLimit(): RateLimit {
  return { enabled: true, requestsPerSecond: 30, burst: 60 }
}

function hostOf(url: string) {
  try {
    return new URL(url).host
  } catch {
    return url
  }
}
