// "My account" card: password, authenticator app, passkeys, active sessions.
import { useEffect, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useQueryClient } from '@tanstack/react-query'
import { Badge, Button, Card, ConfirmDialog, Dialog, Field, Icon, Input, PasswordInput, Segmented, Skeleton, useToast } from '../../components/ui'
import { api } from '../../lib/api'
import { keys } from '../../lib/queries'
import { ago, date, dateTime } from '../../lib/format'
import { ConfirmPasswordInput, StrengthMeter } from './PasswordFields'
import { TotpEnrollment } from './TotpEnrollment'
import {
  MIN_PASSWORD, authKeys, describeError, deviceLabel, errCode, fieldErrors, passkeysSupported, registerPasskey, roleBadge,
  useAuthSession, useMySessions, usePasskeys, type AuthSession, type Passkey, type SessionRow,
} from './authApi'
import './auth.css'

export function MyAccount() {
  const { data: session } = useAuthSession()
  const user = session?.user
  const qc = useQueryClient()
  const toast = useToast()
  const navigate = useNavigate()
  const passkeys = usePasskeys()
  const [allSessions, setAllSessions] = useState(false)
  const isAdmin = user?.role === 'admin'
  const sessions = useMySessions(allSessions && isAdmin)
  const [pwOpen, setPwOpen] = useState(false)
  const [totpOpen, setTotpOpen] = useState(false)
  const [totpOffOpen, setTotpOffOpen] = useState(false)
  const [addPkOpen, setAddPkOpen] = useState(false)
  const [removePk, setRemovePk] = useState<Passkey | null>(null)
  const [revoke, setRevoke] = useState<SessionRow | null>(null)
  const supported = passkeysSupported()

  if (!user) return null

  const refreshAccount = () => {
    qc.invalidateQueries({ queryKey: keys.session })
    qc.invalidateQueries({ queryKey: authKeys.passkeys })
    qc.invalidateQueries({ queryKey: authKeys.users })
  }

  const doRevoke = async (s: SessionRow) => {
    try {
      await api.del(`/api/sessions/${s.id}`)
      if (s.current) {
        qc.clear()
        navigate('/login')
        return
      }
      qc.invalidateQueries({ queryKey: ['auth', 'sessions'] })
      toast.success('Session revoked', `${deviceLabel(s.userAgent)} · ${s.ip || 'unknown address'}`)
    } catch (err) {
      toast.error(err, "Couldn't revoke the session")
    }
  }

  const doRemovePasskey = async (pk: Passkey) => {
    try {
      await api.del(`/api/auth/passkeys/${encodeURIComponent(pk.id)}`)
      refreshAccount()
      toast.success('Passkey removed', pk.name)
    } catch (err) {
      toast.error(new Error(describeError(err)), "Couldn't remove the passkey")
    }
  }

  return (
    <>
      <Card title="My account" sub={`${user.username} · ${roleBadge[user.role] ?? user.role}`}>
        <div className="account-section">
          <div className="mini-row">
            <div className="grow">
              <div className="toggle-title">Password</div>
              <div className="toggle-desc" style={{ fontSize: 12 }}>Changing it signs out your other sessions</div>
            </div>
            <Button size="sm" onClick={() => setPwOpen(true)}>
              Change password
            </Button>
          </div>
        </div>

        <div className="account-section">
          <div className="mini-row">
            <div className="grow">
              <div className="toggle-title">Authenticator app</div>
              <div className="toggle-desc" style={{ fontSize: 12 }}>6-digit codes at sign-in (TOTP)</div>
            </div>
            {user.totpEnabled ? (
              <>
                <span className="twofa on">
                  <span className="d" />
                  on
                </span>
                <Button size="sm" onClick={() => setTotpOffOpen(true)}>
                  Turn off
                </Button>
              </>
            ) : (
              <Button size="sm" onClick={() => setTotpOpen(true)}>
                Set up
              </Button>
            )}
          </div>
        </div>

        <div className="account-section">
          <div className="mini-row">
            <div className="grow">
              <div className="toggle-title">Passkeys</div>
              <div className="toggle-desc" style={{ fontSize: 12 }}>Touch ID, Windows Hello or a security key · sign in without a password, counts as 2FA</div>
            </div>
            <Button size="sm" icon="plus" disabled={!supported} onClick={() => setAddPkOpen(true)}>
              Add passkey
            </Button>
          </div>
          {!supported && <div className="small faint">Passkeys need HTTPS (or localhost) and a browser that supports them.</div>}
          {passkeys.isLoading && <Skeleton height={40} />}
          {(passkeys.data ?? []).map((pk) => (
            <div key={pk.id} className="list-row">
              <Icon name="token" size={16} />
              <div className="grow">
                <div className="medium truncate">{pk.name}</div>
                <div className="small faint">
                  added {date(pk.createdAt)} · {pk.lastUsedAt ? `used ${ago(pk.lastUsedAt)}` : 'never used'}
                </div>
              </div>
              <Button size="sm" variant="ghost" icon="trash" onClick={() => setRemovePk(pk)}>
                Remove
              </Button>
            </div>
          ))}
        </div>

        <div className="account-section">
          <div className="mini-row">
            <div className="grow">
              <div className="toggle-title">Active sessions</div>
              <div className="toggle-desc" style={{ fontSize: 12 }}>Browsers signed in {allSessions ? 'to Relay' : 'as you'}</div>
            </div>
            {isAdmin && (
              <Segmented
                value={allSessions ? 'all' : 'mine'}
                onChange={(v) => setAllSessions(v === 'all')}
                options={[
                  { value: 'mine', label: 'Mine' },
                  { value: 'all', label: 'All users' },
                ]}
              />
            )}
          </div>
          {sessions.isLoading && <Skeleton height={40} />}
          {sessions.isError && <div className="small danger-text">{describeError(sessions.error)}</div>}
          {(sessions.data ?? []).map((s) => (
            <div key={s.id} className="list-row">
              <div className="grow" style={{ minWidth: 0 }}>
                <div className="row gap-8">
                  <span className="medium truncate">{deviceLabel(s.userAgent)}</span>
                  {s.current && <Badge tone="ok">this device</Badge>}
                  {s.remember && !s.current && <Badge>remembered</Badge>}
                </div>
                <div className="small faint mono truncate">
                  {allSessions ? `${s.username} · ` : ''}
                  {s.ip || '—'} · active {ago(s.lastSeenAt)} · expires {dateTime(s.expiresAt)}
                </div>
              </div>
              <Button size="sm" variant="ghost" onClick={() => setRevoke(s)}>
                {s.current ? 'Sign out' : 'Revoke'}
              </Button>
            </div>
          ))}
        </div>
      </Card>

      <ChangePasswordDialog open={pwOpen} onClose={() => setPwOpen(false)} username={user.username} />

      <Dialog open={totpOpen} onClose={() => setTotpOpen(false)} width={500} title="Set up authenticator app" description="Scan the code, then confirm with the 6 digits your app shows.">
        {totpOpen && (
          <TotpEnrollment
            onCancel={() => setTotpOpen(false)}
            onDone={() => {
              setTotpOpen(false)
              refreshAccount()
              toast.success('Two-factor authentication on', 'Relay will ask for a code when you sign in.')
            }}
          />
        )}
      </Dialog>

      <TurnOffTotpDialog open={totpOffOpen} onClose={() => setTotpOffOpen(false)} onDone={refreshAccount} />
      <AddPasskeyDialog open={addPkOpen} onClose={() => setAddPkOpen(false)} onDone={refreshAccount} />

      <ConfirmDialog
        open={!!removePk}
        onClose={() => setRemovePk(null)}
        danger
        title={`Remove passkey “${removePk?.name ?? ''}”?`}
        message="You won't be able to sign in with it anymore. Remove it from your device's password manager too."
        confirmLabel="Remove passkey"
        onConfirm={() => (removePk ? doRemovePasskey(removePk) : undefined)}
      />
      <ConfirmDialog
        open={!!revoke}
        onClose={() => setRevoke(null)}
        danger={!revoke?.current}
        title={revoke?.current ? 'Sign out of this browser?' : `Revoke ${revoke ? deviceLabel(revoke.userAgent) : ''} session?`}
        message={revoke?.current ? 'You will return to the sign-in page.' : 'That browser is signed out immediately.'}
        confirmLabel={revoke?.current ? 'Sign out' : 'Revoke session'}
        onConfirm={() => (revoke ? doRevoke(revoke) : undefined)}
      />
    </>
  )
}

function ChangePasswordDialog({ open, onClose, username }: { open: boolean; onClose: () => void; username: string }) {
  const qc = useQueryClient()
  const toast = useToast()
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    if (!open) return
    setCurrent('')
    setNext('')
    setConfirm('')
    setErrors({})
  }, [open])

  const submit = async () => {
    const errs: Record<string, string> = {}
    if (!current) errs.current = 'Enter your current password'
    if ([...next].length < MIN_PASSWORD) errs.new = `At least ${MIN_PASSWORD} characters`
    if (confirm !== next) errs.confirm = "Passwords don't match"
    setErrors(errs)
    if (Object.keys(errs).length) return
    setBusy(true)
    try {
      const s = await api.post<AuthSession>('/api/auth/password', { current, new: next })
      qc.setQueryData(keys.session, s)
      qc.invalidateQueries({ queryKey: ['auth', 'sessions'] })
      toast.success('Password changed', 'Your other sessions were signed out.')
      onClose()
    } catch (err) {
      const f = fieldErrors(err)
      if (Object.keys(f).length) setErrors(f)
      else setErrors({ form: describeError(err) })
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog
      open={open}
      onClose={onClose}
      width={440}
      title="Change password"
      description={`For ${username}. Other browsers signed in as you are signed out.`}
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" loading={busy} onClick={submit}>
            Change password
          </Button>
        </>
      }
    >
      <Field label="Current password" htmlFor="pw-current" error={errors.current}>
        <PasswordInput id="pw-current" autoFocus autoComplete="current-password" value={current} invalid={!!errors.current} onChange={(e) => setCurrent(e.target.value)} />
      </Field>
      <Field label="New password" htmlFor="pw-new" error={errors.new}>
        <PasswordInput id="pw-new" autoComplete="new-password" value={next} invalid={!!errors.new} onChange={(e) => setNext(e.target.value)} />
        <StrengthMeter password={next} />
      </Field>
      <Field label="Confirm new password" htmlFor="pw-confirm" error={errors.confirm}>
        <ConfirmPasswordInput id="pw-confirm" value={confirm} password={next} invalid={!!errors.confirm} onChange={setConfirm} />
      </Field>
      {errors.form && <div className="auth-error">{errors.form}</div>}
    </Dialog>
  )
}

function TurnOffTotpDialog({ open, onClose, onDone }: { open: boolean; onClose: () => void; onDone: () => void }) {
  const qc = useQueryClient()
  const toast = useToast()
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    if (open) {
      setPassword('')
      setError('')
    }
  }, [open])

  const submit = async () => {
    setBusy(true)
    setError('')
    try {
      const s = await api.post<AuthSession>('/api/auth/totp/disable', { password })
      qc.setQueryData(keys.session, s)
      onDone()
      toast.success('Authenticator app turned off')
      onClose()
    } catch (err) {
      setError(errCode(err) === '2fa_required' || !Object.keys(fieldErrors(err)).length ? describeError(err) : fieldErrors(err).password ?? describeError(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog
      open={open}
      onClose={onClose}
      width={440}
      icon="warning"
      iconTone="warn"
      title="Turn off authenticator app?"
      description="Sign-in will no longer ask for a 6-digit code. Confirm with your password."
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="danger" loading={busy} disabled={!password} onClick={submit}>
            Turn off
          </Button>
        </>
      }
    >
      <Field label="Password" htmlFor="totp-off-password" error={error}>
        <PasswordInput
          id="totp-off-password"
          autoFocus
          autoComplete="current-password"
          value={password}
          invalid={!!error}
          onChange={(e) => setPassword(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && password && submit()}
        />
      </Field>
    </Dialog>
  )
}

function AddPasskeyDialog({ open, onClose, onDone }: { open: boolean; onClose: () => void; onDone: () => void }) {
  const toast = useToast()
  const [name, setName] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    if (open) {
      setName(deviceLabel())
      setError('')
    }
  }, [open])

  const submit = async () => {
    setBusy(true)
    setError('')
    try {
      const pk = await registerPasskey(name.trim() || 'Passkey')
      onDone()
      toast.success('Passkey added', pk.name)
      onClose()
    } catch (err) {
      setError(describeError(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog
      open={open}
      onClose={onClose}
      width={440}
      title="Add a passkey"
      description="Your browser will ask you to confirm with Touch ID, Windows Hello, your phone or a security key."
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" loading={busy} onClick={submit}>
            Continue
          </Button>
        </>
      }
    >
      <Field label="Name" htmlFor="passkey-name" hint="So you can tell your passkeys apart">
        <Input id="passkey-name" autoFocus value={name} maxLength={64} onChange={(e) => setName(e.target.value)} onKeyDown={(e) => e.key === 'Enter' && submit()} />
      </Field>
      {error && <div className="auth-error">{error}</div>}
    </Dialog>
  )
}
