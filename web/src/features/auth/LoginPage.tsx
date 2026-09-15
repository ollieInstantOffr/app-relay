// Sign in (design 11): password, TOTP step, passkeys.
import { useEffect, useState, type FormEvent } from 'react'
import { useNavigate, useSearchParams } from 'react-router-dom'
import { useQueryClient } from '@tanstack/react-query'
import { Button, Checkbox, Field, Input, LogoMark, PasswordInput } from '../../components/ui'
import { api } from '../../lib/api'
import { keys } from '../../lib/queries'
import { CodeInput } from './CodeInput'
import { ForgotPasswordDialog } from './ForgotPasswordDialog'
import { describeError, errCode, passkeyLogin, passkeysSupported, safeNext, useAuthSession, type AuthSession } from './authApi'
import './auth.css'

export default function LoginPage() {
  const { data: session, isLoading, isError } = useAuthSession()
  const navigate = useNavigate()
  const [params] = useSearchParams()
  const qc = useQueryClient()
  const next = safeNext(params.get('next'))

  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [remember, setRemember] = useState(true)
  const [step, setStep] = useState<'password' | 'totp'>('password')
  const [code, setCode] = useState('')
  const [error, setError] = useState('')
  const [preferPasskey, setPreferPasskey] = useState(false)
  const [busy, setBusy] = useState<'' | 'password' | 'passkey'>('')
  const [forgot, setForgot] = useState(false)

  useEffect(() => {
    document.title = 'Relay · Sign in'
  }, [])

  useEffect(() => {
    if (!session) return
    if (session.setupRequired) navigate('/setup', { replace: true })
    else if (session.authenticated) navigate(next, { replace: true })
  }, [session, navigate, next])

  const finish = (s: AuthSession) => {
    // Drop anything cached for a previous account before entering the app.
    qc.removeQueries({ predicate: (q) => q.queryKey[0] !== keys.session[0] })
    qc.setQueryData(keys.session, s)
    navigate(next, { replace: true })
  }

  const submit = async (e?: FormEvent, totp?: string) => {
    e?.preventDefault()
    if (busy) return
    if (!username.trim() || !password) {
      setError('Enter your username and password')
      return
    }
    setBusy('password')
    setError('')
    try {
      const s = await api.post<AuthSession>('/api/auth/login', {
        username: username.trim(),
        password,
        totp: step === 'totp' ? (totp ?? code) : undefined,
        remember,
      })
      finish(s)
    } catch (err) {
      switch (errCode(err)) {
        case 'totp_required':
          setStep('totp')
          setCode('')
          break
        case 'invalid_totp':
          setCode('')
          setError(describeError(err))
          break
        case 'passkey_required':
          setPreferPasskey(true)
          setError(describeError(err))
          break
        case 'setup_required':
          navigate('/setup', { replace: true })
          break
        case 'throttled':
          setStep('password')
          setCode('')
          setError(describeError(err))
          break
        default:
          setError(describeError(err))
      }
    } finally {
      setBusy('')
    }
  }

  const signInWithPasskey = async () => {
    if (busy) return
    setBusy('passkey')
    setError('')
    try {
      finish(await passkeyLogin(remember))
    } catch (err) {
      setError(describeError(err))
    } finally {
      setBusy('')
    }
  }

  if (isLoading) {
    return (
      <div className="auth-screen">
        <span className="spinner lg" />
      </div>
    )
  }

  return (
    <div className="auth-screen">
      <div className="auth-wrap">
        <div className="auth-brand">
          <LogoMark size={32} />
          <span>Relay</span>
        </div>
        <form className="auth-card" onSubmit={submit} noValidate>
          <div>
            <div className="auth-title">{step === 'totp' ? 'Two-factor code' : 'Sign in'}</div>
            {step === 'totp' ? (
              <div className="auth-sub">Enter the 6-digit code from your authenticator app for {username.trim()}.</div>
            ) : (
              <div className="auth-sub mono">{window.location.hostname}</div>
            )}
          </div>

          {isError && <div className="auth-error">Relay's API isn't responding. Check that the relay container is running.</div>}

          {step === 'password' ? (
            <>
              <Field label="Username" htmlFor="login-username">
                <Input
                  id="login-username"
                  autoComplete="username webauthn"
                  autoCapitalize="none"
                  spellCheck={false}
                  autoFocus
                  value={username}
                  onChange={(e) => setUsername(e.target.value)}
                />
              </Field>
              <Field
                label="Password"
                htmlFor="login-password"
                aside={
                  <button type="button" className="link-btn" onClick={() => setForgot(true)}>
                    Forgot?
                  </button>
                }
              >
                <PasswordInput id="login-password" autoComplete="current-password" value={password} onChange={(e) => setPassword(e.target.value)} />
              </Field>
              <Checkbox checked={remember} onChange={setRemember} label="Stay signed in on this device" />
            </>
          ) : (
            <>
              <CodeInput value={code} onChange={setCode} onComplete={(v) => submit(undefined, v)} autoFocus disabled={busy !== ''} invalid={!!error} />
              <button
                type="button"
                className="link-btn"
                style={{ alignSelf: 'flex-start' }}
                onClick={() => {
                  setStep('password')
                  setCode('')
                  setError('')
                }}
              >
                ← Back
              </button>
            </>
          )}

          {error && (
            <div className="auth-error" role="alert">
              {error}
            </div>
          )}

          <Button
            type="submit"
            variant={preferPasskey ? 'secondary' : 'primary'}
            size="lg"
            block
            loading={busy === 'password'}
            disabled={busy !== '' || (step === 'totp' && code.length !== 6)}
          >
            {step === 'totp' ? 'Verify' : 'Sign in'}
          </Button>

          {step === 'password' && passkeysSupported() && (
            <>
              <div className="or-divider">or</div>
              <Button
                type="button"
                variant={preferPasskey ? 'primary' : 'secondary'}
                size="lg"
                block
                loading={busy === 'passkey'}
                disabled={busy !== ''}
                onClick={signInWithPasskey}
              >
                Use a passkey
              </Button>
            </>
          )}
        </form>
        <div className="auth-footer">{session?.version ? `Relay ${session.version}` : 'Relay'}</div>
      </div>
      <ForgotPasswordDialog open={forgot} onClose={() => setForgot(false)} username={username} />
    </div>
  )
}
