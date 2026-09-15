// First-run setup (design 12, 26a, 26b): Admin account · Network · Done.
import { useEffect, useRef, useState, type FormEvent } from 'react'
import { useNavigate } from 'react-router-dom'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Badge, Button, Callout, Field, Icon, Input, LogoMark, PasswordInput, Skeleton, ToggleCard, cx, type IconName } from '../../components/ui'
import { api } from '../../lib/api'
import { keys, useEngines } from '../../lib/queries'
import DockerSuggestionsDialog from '../docker/DockerSuggestionsDialog'
import { ConfirmPasswordInput, StrengthMeter } from './PasswordFields'
import { TotpEnrollment } from './TotpEnrollment'
import {
  MIN_PASSWORD, authKeys, describeError, errCode, fieldErrors, useAuthSession,
  type AuthSession, type FinishResult, type NetworkReport,
} from './authApi'
import './auth.css'

type Step = 0 | 1 | 2
const STEPS = ['Admin account', 'Network', 'Done']

export default function SetupWizard() {
  const { data: session, isError } = useAuthSession()
  const navigate = useNavigate()
  const [step, setStep] = useState<Step | null>(null)
  const [result, setResult] = useState<FinishResult | null>(null)

  useEffect(() => {
    document.title = 'Relay · Setup'
  }, [])

  useEffect(() => {
    if (!session || step === 2) return
    if (session.setupRequired) {
      if (step === null) setStep(0)
      return
    }
    if (!session.authenticated) {
      navigate('/login?next=/setup', { replace: true })
      return
    }
    if (session.user?.role !== 'admin' || session.setupDone) {
      if (step === null) navigate('/', { replace: true })
      return
    }
    if (step === null) setStep(1)
  }, [session, step, navigate])

  return (
    <div className="auth-screen">
      <div className="auth-wrap">
        <div className="auth-brand">
          <LogoMark size={32} />
          <span>Relay</span>
        </div>
        {step === null ? (
          isError ? <div className="auth-error">Relay's API isn't responding. Check that the relay container is running.</div> : <span className="spinner lg" />
        ) : (
          <div className="auth-card wide" style={step > 0 ? { width: 540 } : undefined}>
            <div className="wizard-steps">
              {STEPS.map((label, i) => (
                <div key={label} className={cx('wizard-step', i === step && 'active', i < step && 'done')}>
                  <span className="num">{i < step ? <Icon name="check" size={11} /> : i + 1}</span>
                  {label}
                </div>
              ))}
            </div>
            {step === 0 && <AdminStep session={session} onNext={() => setStep(1)} />}
            {step === 1 && (
              <NetworkStep
                onBack={() => setStep(0)}
                onFinished={(r) => {
                  setResult(r)
                  setStep(2)
                }}
              />
            )}
            {step === 2 && <DoneStep result={result} />}
          </div>
        )}
        {step === 0 && !session?.authenticated && (
          <div className="auth-note">
            Step 2 detects listening ports (80/443), your LAN CIDR for the default <span className="mono">lan-only</span> access list, and Docker if present.
          </div>
        )}
      </div>
    </div>
  )
}

// ---------------------------------------------------------------- step 1
function AdminStep({ session, onNext }: { session?: AuthSession; onNext: () => void }) {
  const qc = useQueryClient()
  const user = session?.authenticated ? session.user : undefined
  const [phase, setPhase] = useState<'form' | 'totp' | 'summary'>(user ? 'summary' : 'form')
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [enroll, setEnroll] = useState(true)
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const create = async (e?: FormEvent) => {
    e?.preventDefault()
    const errs: Record<string, string> = {}
    if (!username.trim()) errs.username = 'Username is required'
    if ([...password].length < MIN_PASSWORD) errs.password = `At least ${MIN_PASSWORD} characters`
    if (confirm !== password) errs.confirm = "Passwords don't match"
    setErrors(errs)
    if (Object.keys(errs).length) return
    setBusy(true)
    setError('')
    try {
      const s = await api.post<AuthSession>('/api/setup/admin', { username: username.trim(), password, enroll2fa: enroll })
      qc.setQueryData(keys.session, s)
      if (enroll) setPhase('totp')
      else onNext()
    } catch (err) {
      const f = fieldErrors(err)
      if (Object.keys(f).length) setErrors(f)
      else setError(describeError(err))
      if (errCode(err) === 'setup_done') qc.invalidateQueries({ queryKey: keys.session })
    } finally {
      setBusy(false)
    }
  }

  if (phase === 'totp') {
    return (
      <>
        <div className="wizard-body">
          <div>
            <div className="auth-title">Set up two-factor authentication</div>
            <div className="auth-sub">Adds a second step when {user?.username ?? username} signs in. You can add a passkey later in Settings → Users &amp; access.</div>
          </div>
          <TotpEnrollment
            onDone={() => {
              qc.invalidateQueries({ queryKey: keys.session })
              onNext()
            }}
            onCancel={onNext}
            cancelLabel="Skip for now"
            doneLabel="Verify & continue →"
          />
        </div>
        <div className="wizard-foot">
          <span className="small faint">Step 1 of 3</span>
        </div>
      </>
    )
  }

  if (phase === 'summary' && user) {
    const twoFactor = user.totpEnabled || user.passkeyCount > 0
    return (
      <>
        <div className="wizard-body">
          <div>
            <div className="auth-title">Admin account</div>
            <div className="auth-sub">
              Signed in as <span className="medium">{user.username}</span>. You can invite others later in Settings.
            </div>
          </div>
          <div className="list-row">
            <div className="grow">
              <div className="medium">Two-factor authentication</div>
              <div className="small faint">Authenticator app or passkey</div>
            </div>
            {twoFactor ? (
              <Badge tone="ok">on</Badge>
            ) : (
              <Button size="sm" onClick={() => setPhase('totp')}>
                Set up now
              </Button>
            )}
          </div>
        </div>
        <div className="wizard-foot">
          <span className="small faint">Step 1 of 3</span>
          <div className="spacer" />
          <Button variant="primary" size="md" onClick={onNext}>
            Continue →
          </Button>
        </div>
      </>
    )
  }

  return (
    <form onSubmit={create} noValidate>
      <div className="wizard-body">
        <div>
          <div className="auth-title">Create the first admin</div>
          <div className="auth-sub">No accounts exist yet. This user gets full control; you can invite others later in Settings.</div>
        </div>
        <Field label="Username" htmlFor="setup-username" error={errors.username}>
          <Input
            id="setup-username"
            autoFocus
            autoComplete="username"
            autoCapitalize="none"
            spellCheck={false}
            value={username}
            invalid={!!errors.username}
            onChange={(e) => setUsername(e.target.value)}
          />
        </Field>
        <Field label="Password" htmlFor="setup-password" error={errors.password}>
          <PasswordInput id="setup-password" autoComplete="new-password" value={password} invalid={!!errors.password} onChange={(e) => setPassword(e.target.value)} />
          <StrengthMeter password={password} />
        </Field>
        <Field label="Confirm password" htmlFor="setup-confirm" error={errors.confirm}>
          <ConfirmPasswordInput id="setup-confirm" value={confirm} password={password} invalid={!!errors.confirm} onChange={setConfirm} />
        </Field>
        <ToggleCard title="Set up 2FA now" description="Recommended · scan a QR code on the next step" checked={enroll} onChange={setEnroll} />
        {error && <div className="auth-error">{error}</div>}
      </div>
      <div className="wizard-foot">
        <span className="small faint">Step 1 of 3</span>
        <div className="spacer" />
        <Button type="submit" variant="primary" size="md" loading={busy}>
          Create account →
        </Button>
      </div>
    </form>
  )
}

// ---------------------------------------------------------------- step 2
function CheckIcon({ status }: { status: string }) {
  return (
    <span className={cx('check-icon', status)} aria-label={status}>
      {status === 'ok' ? <Icon name="check" size={12} /> : status === 'unknown' ? '?' : <Icon name="warning" size={12} />}
    </span>
  )
}

function NetworkStep({ onBack, onFinished }: { onBack: () => void; onFinished: (r: FinishResult) => void }) {
  const qc = useQueryClient()
  const report = useQuery({
    queryKey: authKeys.network,
    queryFn: () => api.get<NetworkReport>('/api/setup/network'),
    staleTime: Infinity,
    retry: false,
  })
  const data = report.data
  const [lan, setLan] = useState('')
  const [editLan, setEditLan] = useState(false)
  const [domain, setDomain] = useState('')
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [error, setError] = useState('')
  const [busy, setBusy] = useState<'' | 'saving' | 'applying'>('')
  const initialized = useRef(false)

  useEffect(() => {
    if (!data || initialized.current) return
    initialized.current = true
    setLan(data.lanCidr)
    setDomain(data.adminDomain)
    if (!data.lanDetected) setEditLan(true)
  }, [data])

  const next = async () => {
    setBusy('saving')
    setError('')
    setErrors({})
    try {
      await api.post('/api/setup/network', { lanCidr: lan.trim(), adminDomain: domain.trim() })
      if (data) qc.setQueryData<NetworkReport>(authKeys.network, { ...data, lanCidr: lan.trim(), adminDomain: domain.trim().toLowerCase() })
      setBusy('applying')
      const r = await api.post<FinishResult>('/api/setup/finish')
      onFinished(r)
      qc.invalidateQueries({ queryKey: keys.session })
      qc.invalidateQueries({ queryKey: keys.pending })
    } catch (err) {
      const f = fieldErrors(err)
      if (Object.keys(f).length) {
        setErrors(f)
        if (f.lanCidr) setEditLan(true)
      } else {
        setError(describeError(err))
      }
    } finally {
      setBusy('')
    }
  }

  return (
    <>
      <div className="wizard-body">
        <div>
          <div className="auth-title">Network check</div>
          <div className="auth-sub">Relay probed your environment. Fix anything red before exposing hosts publicly.</div>
        </div>

        {report.isLoading ? (
          <div className="col gap-12">
            <div className="small faint">Checking ports, Docker, your LAN and public address…</div>
            {Array.from({ length: 5 }, (_, i) => (
              <Skeleton key={i} height={34} />
            ))}
          </div>
        ) : report.isError ? (
          <Callout tone="danger" title="Network check failed" actions={<Button size="sm" onClick={() => report.refetch()}>Retry</Button>}>
            {describeError(report.error)}
          </Callout>
        ) : data ? (
          <div className="checklist">
            {data.checks.map((c) => (
              <div key={c.id} className="check-row">
                <CheckIcon status={c.status} />
                <div className="grow">
                  <div className="check-title">{c.title}</div>
                  <div className="check-detail">{c.detail}</div>
                  {c.id === 'lan' && editLan && (
                    <div style={{ marginTop: 8, maxWidth: 240 }}>
                      <Field error={errors.lanCidr}>
                        <Input mono inputSize="sm" value={lan} invalid={!!errors.lanCidr} placeholder="192.168.1.0/24" aria-label="LAN CIDR" onChange={(e) => setLan(e.target.value)} />
                      </Field>
                    </div>
                  )}
                </div>
                {c.id === 'lan' && !editLan && (
                  <Button size="sm" variant="ghost" onClick={() => setEditLan(true)}>
                    Edit
                  </Button>
                )}
                {c.id === 'public' && (
                  <Button size="sm" variant="ghost" icon="reload" loading={report.isFetching} onClick={() => report.refetch()}>
                    Recheck
                  </Button>
                )}
              </div>
            ))}
          </div>
        ) : null}

        <Field
          label="Admin UI domain (optional)"
          htmlFor="setup-domain"
          error={errors.adminDomain}
          hint="Relay creates a proxy host for itself with a LAN-only access list."
        >
          <Input
            id="setup-domain"
            mono
            placeholder="proxy.home.lan"
            value={domain}
            invalid={!!errors.adminDomain}
            autoCapitalize="none"
            spellCheck={false}
            onChange={(e) => setDomain(e.target.value)}
          />
        </Field>
        {busy === 'applying' && <div className="small muted">Saving and applying the initial configuration…</div>}
        {error && <div className="auth-error">{error}</div>}
      </div>
      <div className="wizard-foot">
        <span className="small faint">Step 2 of 3</span>
        <div className="spacer" />
        <Button onClick={onBack} disabled={busy !== ''}>
          ← Back
        </Button>
        <Button variant="primary" loading={busy !== ''} disabled={!data || !lan.trim()} onClick={next}>
          Continue →
        </Button>
      </div>
    </>
  )
}

// ---------------------------------------------------------------- step 3
function TaskCard({ icon, title, sub, onClick, disabled }: { icon: IconName; title: string; sub: string; onClick: () => void; disabled?: boolean }) {
  return (
    <button type="button" className="task-card" onClick={onClick} disabled={disabled}>
      <span className="task-icon">
        <Icon name={icon} size={16} />
      </span>
      <span className="grow">
        <span className="medium" style={{ display: 'block' }}>
          {title}
        </span>
        <span className="small faint">{sub}</span>
      </span>
      <span className="faint">→</span>
    </button>
  )
}

function DoneStep({ result }: { result: FinishResult | null }) {
  const navigate = useNavigate()
  const qc = useQueryClient()
  const user = useAuthSession().data?.user
  const engines = useEngines().data
  const [dockerOpen, setDockerOpen] = useState(false)

  const facts: string[] = []
  if (engines) {
    facts.push(!engines.nginx.reachable ? 'nginx agent not reachable' : engines.nginx.running ? 'nginx running' : 'nginx not running')
  }
  if (user) {
    facts.push(`admin ${user.username}`)
    facts.push(user.totpEnabled || user.passkeyCount > 0 ? '2FA enrolled' : '2FA not set up')
  }
  const containers = result?.containers
  const httpApps = result?.httpContainers
  const dockerOK = typeof containers === 'number'
  const adminDomain = qc.getQueryData<NetworkReport>(authKeys.network)?.adminDomain ?? ''
  const base = adminDomain.split('.').length > 2 ? adminDomain.split('.').slice(1).join('.') : adminDomain

  return (
    <>
      <div className="wizard-body">
        <div>
          <div className="auth-title">Relay is ready</div>
          {facts.length > 0 && <div className="auth-sub mono small">{facts.join(' · ')}</div>}
        </div>
        {result?.error && (
          <Callout tone="warn" title="The initial apply didn't complete">
            {/[.!?]$/.test(result.error.trim()) ? result.error.trim() : `${result.error.trim()}.`} Your settings are saved — apply again from the pending changes bar once the engines are running.
          </Callout>
        )}
        <div className="col gap-8">
          <div className="field-label">Pick a first task</div>
          <TaskCard
            icon="docker"
            title="Create hosts from Docker"
            sub={dockerOK ? `${containers} containers found · ${httpApps ?? 0} look like web apps` : 'Docker socket not found'}
            disabled={!dockerOK}
            onClick={() => setDockerOpen(true)}
          />
          <TaskCard icon="hosts" title="Add a proxy host manually" sub="Domain → IP:port, TLS in one screen" onClick={() => navigate('/hosts?new=1')} />
          <TaskCard icon="upload" title="Import from Nginx Proxy Manager" sub="Hosts, certs and access lists are converted" onClick={() => navigate('/settings/backup?import=npm')} />
          <TaskCard
            icon="certificates"
            title="Set up a wildcard certificate"
            sub={base ? `Connect a DNS provider and request *.${base}` : 'Connect a DNS provider and request a wildcard'}
            onClick={() => navigate('/certificates?request=1')}
          />
        </div>
      </div>
      <div className="wizard-foot">
        <Button variant="ghost" onClick={() => navigate('/')}>
          Skip — go to the dashboard
        </Button>
        <div className="spacer" />
        {dockerOK ? (
          <Button variant="primary" size="md" onClick={() => setDockerOpen(true)}>
            Create hosts from Docker →
          </Button>
        ) : (
          <Button variant="primary" size="md" onClick={() => navigate('/hosts?new=1')}>
            Add a proxy host →
          </Button>
        )}
      </div>
      <DockerSuggestionsDialog open={dockerOpen} onClose={() => setDockerOpen(false)} />
    </>
  )
}
