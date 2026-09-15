// Re-authentication dialog (design 30b), opened by the `relay:unauthenticated`
// window event that lib/api fires on 401s. Also mounts the account gates.
import { useEffect, useRef, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useQueryClient } from '@tanstack/react-query'
import { Button, Dialog, Field, PasswordInput } from '../../components/ui'
import { api, UNAUTHENTICATED_EVENT } from '../../lib/api'
import { keys } from '../../lib/queries'
import { AccountGate } from './AccountGate'
import { CodeInput } from './CodeInput'
import { describeError, errCode, passkeyLogin, passkeysSupported, useAuthSession, type AuthSession, type AuthUser } from './authApi'
import './auth.css'

function formatTTL(hours: number): string {
  if (hours % 24 === 0) {
    const d = hours / 24
    return `${d} day${d === 1 ? '' : 's'}`
  }
  return `${hours} hour${hours === 1 ? '' : 's'}`
}

interface Context {
  user: AuthUser
  expired: boolean
  ttlHours: number
}

export default function SessionExpiredDialog() {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const { data: session } = useAuthSession()
  const lastUser = useRef<AuthUser | undefined>(undefined)
  const ttl = useRef(168)
  const openRef = useRef(false)
  const [ctx, setCtx] = useState<Context | null>(null)
  const [password, setPassword] = useState('')
  const [needCode, setNeedCode] = useState(false)
  const [code, setCode] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState<'' | 'password' | 'passkey'>('')

  useEffect(() => {
    if (session?.user) lastUser.current = session.user
    if (session?.sessionTtlHours) ttl.current = session.sessionTtlHours
  }, [session])

  useEffect(() => {
    const onUnauthenticated = () => {
      if (openRef.current) return
      const user = lastUser.current
      if (!user) {
        qc.invalidateQueries({ queryKey: keys.session })
        return
      }
      openRef.current = true
      setCtx({ user, expired: new Date(user.sessionExpiresAt).getTime() <= Date.now() + 5_000, ttlHours: ttl.current })
      setPassword('')
      setCode('')
      setNeedCode(false)
      setError('')
    }
    window.addEventListener(UNAUTHENTICATED_EVENT, onUnauthenticated)
    return () => window.removeEventListener(UNAUTHENTICATED_EVENT, onUnauthenticated)
  }, [qc])

  const close = () => {
    openRef.current = false
    setCtx(null)
  }

  const resume = async (s: AuthSession) => {
    if (ctx && s.user?.id !== ctx.user.id) {
      // Someone else signed in (e.g. a different passkey): start fresh.
      qc.clear()
      window.location.assign('/')
      return
    }
    qc.setQueryData(keys.session, s)
    close()
    await qc.invalidateQueries()
  }

  const submit = async (totp?: string) => {
    if (!ctx || busy) return
    if (!password) {
      setError('Enter your password')
      return
    }
    setBusy('password')
    setError('')
    try {
      const s = await api.post<AuthSession>('/api/auth/login', {
        username: ctx.user.username,
        password,
        totp: needCode ? (totp ?? code) : undefined,
        remember: false,
      })
      await resume(s)
    } catch (err) {
      const c = errCode(err)
      if (c === 'totp_required') {
        setNeedCode(true)
      } else {
        if (c === 'invalid_totp') setCode('')
        setError(describeError(err))
      }
    } finally {
      setBusy('')
    }
  }

  const usePasskey = async () => {
    if (busy) return
    setBusy('passkey')
    setError('')
    try {
      await resume(await passkeyLogin(false))
    } catch (err) {
      setError(describeError(err))
    } finally {
      setBusy('')
    }
  }

  const switchAccount = () => {
    close()
    qc.clear()
    navigate('/login')
  }

  const description = ctx
    ? `${ctx.expired ? `You were signed out after ${formatTTL(ctx.ttlHours)} of inactivity.` : 'Your session ended — it was signed out elsewhere or revoked.'} ` +
      'Saved changes are kept in pending changes; anything you hadn\'t saved yet may need to be entered again.'
    : undefined

  return (
    <>
      <Dialog
        open={!!ctx}
        onClose={() => undefined}
        dismissable={false}
        width={440}
        icon="power"
        title="Sign in again"
        description={description}
        footer={
          <>
            <Button variant="ghost" onClick={switchAccount} style={{ marginRight: 'auto' }}>
              Switch account
            </Button>
            <Button variant="primary" loading={busy === 'password'} disabled={busy !== '' || (needCode && code.length !== 6)} onClick={() => submit()}>
              Continue
            </Button>
          </>
        }
      >
        {ctx && (
          <>
            <Field label={`Password for ${ctx.user.username}`} htmlFor="reauth-password">
              <PasswordInput
                id="reauth-password"
                autoFocus
                autoComplete="current-password"
                value={password}
                disabled={needCode}
                onChange={(e) => setPassword(e.target.value)}
                onKeyDown={(e) => e.key === 'Enter' && submit()}
              />
            </Field>
            {needCode && (
              <Field label="2FA code">
                <CodeInput value={code} onChange={setCode} onComplete={(v) => submit(v)} autoFocus invalid={!!error} disabled={busy !== ''} />
              </Field>
            )}
            {error && <div className="auth-error">{error}</div>}
            {passkeysSupported() && (
              <button type="button" className="link-btn" style={{ alignSelf: 'flex-start' }} disabled={busy !== ''} onClick={usePasskey}>
                {busy === 'passkey' ? 'Waiting for passkey…' : 'Or use a passkey'}
              </button>
            )}
          </>
        )}
      </Dialog>
      <AccountGate />
    </>
  )
}
