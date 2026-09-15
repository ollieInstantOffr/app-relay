// Full-screen gates inside the app: forced password change (after an admin
// reset) and forced 2FA enrolment (Require 2FA for admins). Also sends admins
// back to the setup wizard until first-run setup is finished.
import { useEffect, useState, type FormEvent } from 'react'
import { useNavigate } from 'react-router-dom'
import { useQueryClient } from '@tanstack/react-query'
import { Button, Field, LogoMark, PasswordInput, Portal } from '../../components/ui'
import { api } from '../../lib/api'
import { keys } from '../../lib/queries'
import { ConfirmPasswordInput, StrengthMeter } from './PasswordFields'
import { TotpEnrollment } from './TotpEnrollment'
import {
  MIN_PASSWORD, describeError, deviceLabel, fieldErrors, passkeysSupported, registerPasskey, useAuthSession,
  type AuthSession, type AuthUser,
} from './authApi'
import './auth.css'

export function AccountGate() {
  const { data: session } = useAuthSession()
  const navigate = useNavigate()
  const user = session?.user

  useEffect(() => {
    if (session?.authenticated && user?.role === 'admin' && session.setupDone === false && !user.mustChangePassword) {
      navigate('/setup', { replace: true })
    }
  }, [session?.authenticated, session?.setupDone, user?.role, user?.mustChangePassword, navigate])

  if (!user) return null
  if (user.mustChangePassword) {
    return (
      <Portal>
        <div className="gate-screen">
          <PasswordChangeCard user={user} />
        </div>
      </Portal>
    )
  }
  if (user.mustEnroll2fa) {
    return (
      <Portal>
        <div className="gate-screen">
          <EnrollCard user={user} />
        </div>
      </Portal>
    )
  }
  return null
}

function useSignOut() {
  const qc = useQueryClient()
  const navigate = useNavigate()
  return async () => {
    await api.post('/api/auth/logout').catch(() => undefined)
    qc.clear()
    navigate('/login')
  }
}

function Brand() {
  return (
    <div className="auth-brand">
      <LogoMark size={32} />
      <span>Relay</span>
    </div>
  )
}

function PasswordChangeCard({ user }: { user: AuthUser }) {
  const qc = useQueryClient()
  const signOut = useSignOut()
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    const errs: Record<string, string> = {}
    if (!current) errs.current = 'Enter the temporary password'
    if ([...next].length < MIN_PASSWORD) errs.new = `At least ${MIN_PASSWORD} characters`
    if (confirm !== next) errs.confirm = "Passwords don't match"
    setErrors(errs)
    if (Object.keys(errs).length) return
    setBusy(true)
    setError('')
    try {
      const s = await api.post<AuthSession>('/api/auth/password', { current, new: next })
      qc.setQueryData(keys.session, s)
      await qc.invalidateQueries()
    } catch (err) {
      const f = fieldErrors(err)
      if (Object.keys(f).length) setErrors(f)
      else setError(describeError(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="auth-wrap">
      <Brand />
      <form className="auth-card" onSubmit={submit} noValidate>
        <div>
          <div className="auth-title">Choose a new password</div>
          <div className="auth-sub">
            Your password was reset. Pick a new one to keep using Relay as <span className="medium">{user.username}</span>.
          </div>
        </div>
        <Field label="Temporary password" htmlFor="gate-current" error={errors.current}>
          <PasswordInput id="gate-current" autoFocus autoComplete="current-password" value={current} invalid={!!errors.current} onChange={(e) => setCurrent(e.target.value)} />
        </Field>
        <Field label="New password" htmlFor="gate-new" error={errors.new}>
          <PasswordInput id="gate-new" autoComplete="new-password" value={next} invalid={!!errors.new} onChange={(e) => setNext(e.target.value)} />
          <StrengthMeter password={next} />
        </Field>
        <Field label="Confirm new password" htmlFor="gate-confirm" error={errors.confirm}>
          <ConfirmPasswordInput id="gate-confirm" value={confirm} password={next} invalid={!!errors.confirm} onChange={setConfirm} />
        </Field>
        {error && <div className="auth-error">{error}</div>}
        <Button type="submit" variant="primary" size="lg" block loading={busy}>
          Save password
        </Button>
        <button type="button" className="link-btn" style={{ alignSelf: 'center' }} onClick={signOut}>
          Sign out
        </button>
      </form>
    </div>
  )
}

function EnrollCard({ user }: { user: AuthUser }) {
  const qc = useQueryClient()
  const signOut = useSignOut()
  const [mode, setMode] = useState<'choose' | 'totp'>('choose')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const supported = passkeysSupported()

  const done = () => {
    qc.invalidateQueries()
  }

  const addPasskey = async () => {
    setBusy(true)
    setError('')
    try {
      await registerPasskey(deviceLabel())
      done()
    } catch (err) {
      setError(describeError(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="auth-wrap">
      <Brand />
      <div className="auth-card" style={{ width: 460 }}>
        <div>
          <div className="auth-title">Set up two-factor authentication</div>
          <div className="auth-sub">
            This Relay requires 2FA for admins. Add an authenticator app or a passkey to keep using it as <span className="medium">{user.username}</span>.
          </div>
        </div>
        {mode === 'totp' ? (
          <TotpEnrollment onDone={done} onCancel={() => setMode('choose')} cancelLabel="Back" />
        ) : (
          <>
            <Button variant="primary" size="lg" block onClick={() => setMode('totp')}>
              Use an authenticator app
            </Button>
            <Button size="lg" block loading={busy} disabled={!supported} onClick={addPasskey}>
              Add a passkey
            </Button>
            {!supported && <div className="small faint">Passkeys need HTTPS (or localhost) and a browser that supports them.</div>}
            {error && <div className="auth-error">{error}</div>}
          </>
        )}
        <button type="button" className="link-btn" style={{ alignSelf: 'center' }} onClick={signOut}>
          Sign out
        </button>
      </div>
    </div>
  )
}
