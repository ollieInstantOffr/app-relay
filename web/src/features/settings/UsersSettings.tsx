// Settings → Users & access (design 08, 28c).
import { useEffect, useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useQueryClient } from '@tanstack/react-query'
import {
  Badge, Button, Callout, Card, ConfirmDialog, CopyButton, Dialog, Field, IconButton, Menu, SectionHeader, Select, Skeleton,
  ToggleRow, cx, useToast, type MenuEntry,
} from '../../components/ui'
import { api } from '../../lib/api'
import { useEntities, useRole, useSaveSettings, useSettings } from '../../lib/queries'
import { ago } from '../../lib/format'
import type { ApiToken, Role, SecuritySettings } from '../../lib/types'
import NotAllowed from '../auth/NotAllowed'
import CreateTokenDialog from '../auth/CreateTokenDialog'
import { AddUserDialog } from '../auth/AddUserDialog'
import { MyAccount } from '../auth/MyAccount'
import { ROLES, authKeys, describeError, roleBadge, useAuthSession, useTokens, useUsers, type UserRow } from '../auth/authApi'
import '../auth/auth.css'

export default function UsersSettings() {
  const { isAdmin } = useRole()
  const { data: session } = useAuthSession()
  return (
    <>
      <SectionHeader title="Users &amp; access" description="Who can sign in to Relay and what they can change." />
      {session?.user && !isAdmin && <NotAllowed what="users and access" />}
      {isAdmin && (
        <>
          <UsersCard />
          <RestTokensCard />
          <SignInCard />
          <ResetAdminCard />
        </>
      )}
      <MyAccount />
    </>
  )
}

// ---------------------------------------------------------------- temporary password result
interface TempPassword {
  username: string
  password: string
  signedOut: number
  self: boolean
}

function TempPasswordDialog({ value, onClose }: { value: TempPassword | null; onClose: () => void }) {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const done = () => {
    const self = value?.self
    onClose()
    if (self) {
      qc.clear()
      navigate('/login')
    }
  }
  return (
    <Dialog
      open={!!value}
      onClose={done}
      dismissable={false}
      width={480}
      icon="check"
      iconTone="ok"
      title={`New temporary password for ${value?.username ?? ''}`}
      description={
        value
          ? `Share it securely — it's shown only once. ${value.self ? 'You' : 'They'} must choose a new password at next sign-in · ${value.signedOut} session${value.signedOut === 1 ? '' : 's'} signed out.`
          : undefined
      }
      footer={
        <Button variant="primary" onClick={done}>
          {value?.self ? 'Done — sign in again' : 'Done'}
        </Button>
      }
    >
      {value && (
        <div className="token-reveal">
          <code className="temp-password">{value.password}</code>
          <CopyButton text={value.password} />
        </div>
      )}
    </Dialog>
  )
}

async function resetPassword(u: { id: string; username: string }): Promise<TempPassword> {
  const r = await api.post<{ password: string; signedOut: number; self: boolean }>(`/api/users/${u.id}/reset-password`)
  return { username: u.username, ...r }
}

// ---------------------------------------------------------------- users
type Pending = { kind: 'reset' | 'reset2fa' | 'disable' | 'delete'; user: UserRow }

function UsersCard() {
  const users = useUsers()
  const qc = useQueryClient()
  const toast = useToast()
  const security = useSettings('security').data
  const [inviteOpen, setInviteOpen] = useState(false)
  const [pending, setPending] = useState<Pending | null>(null)
  const [temp, setTemp] = useState<TempPassword | null>(null)

  const refresh = () => qc.invalidateQueries({ queryKey: authKeys.users })

  const update = async (u: UserRow, patch: { role?: Role; disabled?: boolean }, message: string) => {
    try {
      await api.put(`/api/users/${u.id}`, patch)
      refresh()
      toast.success(message, u.username)
    } catch (err) {
      toast.error(new Error(describeError(err)), `Couldn't update ${u.username}`)
    }
  }

  const confirmPending = async () => {
    if (!pending) return
    const u = pending.user
    try {
      switch (pending.kind) {
        case 'reset':
          setTemp(await resetPassword(u))
          break
        case 'reset2fa':
          await api.post(`/api/users/${u.id}/reset-2fa`)
          toast.success('2FA reset', `${u.username} can sign in with their password and set up 2FA again.`)
          break
        case 'disable':
          await api.put(`/api/users/${u.id}`, { disabled: true })
          toast.success('User disabled', `${u.username} was signed out everywhere.`)
          break
        case 'delete':
          await api.del(`/api/users/${u.id}`)
          toast.success('User deleted', u.username)
          break
      }
      refresh()
      qc.invalidateQueries({ queryKey: ['tokens'] })
    } catch (err) {
      toast.error(new Error(describeError(err)), 'Action failed')
    }
  }

  const menuFor = (u: UserRow): MenuEntry[] => {
    const items: MenuEntry[] = [{ header: u.username }]
    for (const { role: r } of ROLES) {
      if (r !== u.role) items.push({ label: `Change role to ${roleBadge[r]}`, icon: 'users', onSelect: () => update(u, { role: r }, `Role changed to ${roleBadge[r]}`) })
    }
    items.push('separator')
    items.push({ label: 'Reset password…', icon: 'reload', onSelect: () => setPending({ kind: 'reset', user: u }) })
    if (u.twoFactor) items.push({ label: 'Reset 2FA…', icon: 'token', onSelect: () => setPending({ kind: 'reset2fa', user: u }) })
    items.push(
      u.disabled
        ? { label: 'Enable', icon: 'power', onSelect: () => update(u, { disabled: false }, 'User enabled') }
        : { label: 'Disable…', icon: 'power', disabled: u.self, onSelect: () => setPending({ kind: 'disable', user: u }) },
    )
    items.push('separator')
    items.push({ label: 'Delete…', icon: 'trash', danger: true, disabled: u.self, onSelect: () => setPending({ kind: 'delete', user: u }) })
    return items
  }

  const confirmCopy: Record<Pending['kind'], { title: string; message: string; label: string; danger: boolean }> = {
    reset: {
      title: `Reset password for ${pending?.user.username}?`,
      message: 'Relay generates a temporary password and signs out all of their sessions. They must choose a new password at next sign-in.',
      label: 'Reset password',
      danger: false,
    },
    reset2fa: {
      title: `Reset 2FA for ${pending?.user.username}?`,
      message: 'Removes their authenticator app and all passkeys and signs them out. Use this when they lose their device.',
      label: 'Reset 2FA',
      danger: true,
    },
    disable: {
      title: `Disable ${pending?.user.username}?`,
      message: "They're signed out everywhere and can't sign in until re-enabled. API tokens they created stop working meanwhile.",
      label: 'Disable user',
      danger: true,
    },
    delete: {
      title: `Delete ${pending?.user.username}?`,
      message: 'Their sessions end and the API tokens they created are revoked. This can’t be undone.',
      label: 'Delete user',
      danger: true,
    },
  }
  const copy = pending ? confirmCopy[pending.kind] : null

  return (
    <>
      <Card
        title="Users"
        actions={
          <Button size="sm" variant="ghost" icon="plus" onClick={() => setInviteOpen(true)}>
            Invite
          </Button>
        }
      >
        {users.isLoading && (
          <div className="card-body col gap-8">
            <Skeleton height={32} />
            <Skeleton height={32} />
          </div>
        )}
        {users.isError && <div className="card-body small danger-text">{describeError(users.error)}</div>}
        {(users.data ?? []).map((u) => (
          <div key={u.id} className={cx('user-row', u.disabled && 'dim')}>
            <div className="user-avatar">{u.username.slice(0, 1).toUpperCase()}</div>
            <div className="grow" style={{ minWidth: 0 }}>
              <div className="user-name truncate">
                {u.username}
                {u.self && ' (you)'}
              </div>
              <div className="user-sub truncate">
                {[u.email, u.self ? 'last active now' : u.lastActiveAt ? `last active ${ago(u.lastActiveAt)}` : 'never signed in'].filter(Boolean).join(' · ')}
              </div>
            </div>
            {u.disabled && <Badge>disabled</Badge>}
            {u.mustChangePassword && <Badge tone="pending">temporary password</Badge>}
            <Badge tone={u.role === 'admin' ? 'dark' : u.role === 'member' ? 'outline' : undefined} title={ROLES.find((r) => r.role === u.role)?.description}>
              {roleBadge[u.role] ?? u.role}
            </Badge>
            <span className={cx('twofa', u.twoFactor ? 'on' : 'off')} title={u.twoFactor ? [u.totpEnabled && 'authenticator app', u.passkeys > 0 && `${u.passkeys} passkey${u.passkeys === 1 ? '' : 's'}`].filter(Boolean).join(' + ') : undefined}>
              <span className="d" />
              2FA {u.twoFactor ? 'on' : 'off'}
            </span>
            <Menu trigger={<IconButton icon="more" bare label={`Actions for ${u.username}`} />} items={menuFor(u)} />
          </div>
        ))}
      </Card>
      <AddUserDialog open={inviteOpen} onClose={() => setInviteOpen(false)} require2fa={!!security?.require2faForAdmins} />
      <ConfirmDialog
        open={!!pending}
        onClose={() => setPending(null)}
        onConfirm={confirmPending}
        danger={copy?.danger}
        title={copy?.title ?? ''}
        message={copy?.message}
        confirmLabel={copy?.label}
        typeToConfirm={pending?.kind === 'delete' ? pending.user.username : undefined}
      />
      <TempPasswordDialog value={temp} onClose={() => setTemp(null)} />
    </>
  )
}

// ---------------------------------------------------------------- REST tokens
function tokenStatus(t: ApiToken): { label: string; active: boolean } {
  if (t.revokedAt) return { label: 'revoked', active: false }
  if (t.expiresAt && new Date(t.expiresAt).getTime() <= Date.now()) return { label: 'expired', active: false }
  return { label: t.lastUsedAt ? `used ${ago(t.lastUsedAt)}` : 'never used', active: true }
}

function RestTokensCard() {
  const tokens = useTokens('rest')
  const qc = useQueryClient()
  const toast = useToast()
  const [open, setOpen] = useState(false)
  const [revoking, setRevoking] = useState<ApiToken | null>(null)

  const remove = async (t: ApiToken) => {
    const { active } = tokenStatus(t)
    try {
      await api.del(`/api/tokens/${t.id}`)
      qc.invalidateQueries({ queryKey: ['tokens'] })
      toast.success(active ? 'Token revoked' : 'Token deleted', t.name)
    } catch (err) {
      toast.error(err, "Couldn't remove the token")
    }
  }

  return (
    <>
      <Card
        title="REST API tokens"
        actions={
          <Button size="sm" variant="ghost" icon="plus" onClick={() => setOpen(true)}>
            Generate token
          </Button>
        }
      >
        {tokens.isLoading && (
          <div className="card-body">
            <Skeleton height={32} />
          </div>
        )}
        {tokens.data && tokens.data.length === 0 && (
          <div className="card-body small muted">
            No REST API tokens. Scripts and integrations send them as <span className="mono">Authorization: Bearer rl_api_…</span>
          </div>
        )}
        {(tokens.data ?? []).map((t) => {
          const st = tokenStatus(t)
          return (
            <div key={t.id} className={cx('user-row', !st.active && 'dim')}>
              <div className="user-avatar api">API</div>
              <div className="grow" style={{ minWidth: 0 }}>
                <div className="user-name truncate">{t.name}</div>
                <div className="user-sub truncate">
                  <span className="mono">
                    {t.prefix}••••{t.last4}
                  </span>{' '}
                  · {t.scope === 'read' ? 'read-only' : 'read + write'} · {st.label}
                  {t.createdBy ? ` · by ${t.createdBy}` : ''}
                </div>
              </div>
              <Badge>{t.scope === 'write' ? 'editor' : 'viewer'}</Badge>
              <Button size="sm" variant="ghost" onClick={() => (st.active ? setRevoking(t) : remove(t))}>
                {st.active ? 'Revoke' : 'Delete'}
              </Button>
            </div>
          )
        })}
      </Card>
      <CreateTokenDialog open={open} onClose={() => setOpen(false)} surface="rest" />
      <ConfirmDialog
        open={!!revoking}
        onClose={() => setRevoking(null)}
        danger
        title={`Revoke ${revoking?.name ?? ''}?`}
        message="Requests using this token are rejected immediately. This can't be undone."
        confirmLabel="Revoke token"
        onConfirm={() => (revoking ? remove(revoking) : undefined)}
      />
    </>
  )
}

// ---------------------------------------------------------------- sign-in settings
const SESSION_LENGTHS = [
  { value: '24', label: '1 day' },
  { value: '168', label: '7 days' },
  { value: '720', label: '30 days' },
  { value: '2160', label: '90 days' },
]

function SignInCard() {
  const security = useSettings('security')
  const save = useSaveSettings('security')
  const lists = useEntities('access-lists')
  const toast = useToast()
  const sec = security.data

  const options = useMemo(() => {
    const opts = [...SESSION_LENGTHS]
    if (sec && !opts.some((o) => o.value === String(sec.sessionTtlHours))) {
      const h = sec.sessionTtlHours
      opts.push({ value: String(h), label: h % 24 === 0 ? `${h / 24} days` : `${h} hours` })
    }
    return opts
  }, [sec])

  const commit = async (patch: Partial<SecuritySettings>, message: string) => {
    if (!sec) return
    try {
      await save.mutateAsync({ ...sec, ...patch })
      toast.success(message)
    } catch (err) {
      toast.error(new Error(describeError(err)), 'Not saved')
    }
  }

  const allLists = lists.data ?? []
  const candidate = allLists.find((l) => l.name === 'lan-only') ?? allLists[0]
  const current = allLists.find((l) => l.id === sec?.adminAccessListId)

  return (
    <Card title="Sign-in">
      {!sec ? (
        <div className="card-body col gap-8">
          <Skeleton height={36} />
          <Skeleton height={36} />
          <Skeleton height={36} />
        </div>
      ) : (
        <>
          <ToggleRow
            title="Require 2FA for admins"
            description="TOTP or passkey"
            checked={sec.require2faForAdmins}
            disabled={save.isPending}
            onChange={(v) => commit({ require2faForAdmins: v }, v ? '2FA is now required for admins' : '2FA is no longer required for admins')}
          />
          <ToggleRow
            title="Restrict admin UI to LAN"
            description={
              sec.adminAccessListId ? (
                <span className="row gap-6" style={{ display: 'inline-flex' }}>
                  Applies access list
                  <Select
                    inputSize="sm"
                    mono
                    style={{ width: 'auto', height: 26 }}
                    value={sec.adminAccessListId}
                    options={[
                      ...(current ? [] : [{ value: sec.adminAccessListId, label: '(deleted list)' }]),
                      ...allLists.map((l) => ({ value: l.id, label: l.name })),
                    ]}
                    onChange={(v) => commit({ adminAccessListId: v }, 'Admin UI access list changed')}
                  />
                  to this dashboard
                </span>
              ) : candidate ? (
                <>
                  Applies access list <span className="mono">{candidate.name}</span> to this dashboard
                </>
              ) : (
                'Create an access list first (Access lists)'
              )
            }
            checked={!!sec.adminAccessListId}
            disabled={save.isPending || (!sec.adminAccessListId && !candidate)}
            onChange={(v) =>
              commit({ adminAccessListId: v ? candidate?.id ?? '' : '' }, v ? `Admin UI restricted to ${candidate?.name}` : 'Admin UI no longer restricted')
            }
          />
          <ToggleRow title="Session length" description="Sign out inactive sessions">
            <Select
              inputSize="sm"
              style={{ width: 130 }}
              value={String(sec.sessionTtlHours)}
              options={options}
              disabled={save.isPending}
              onChange={(v) => commit({ sessionTtlHours: Number(v) }, 'Session length saved')}
            />
          </ToggleRow>
        </>
      )}
    </Card>
  )
}

// ---------------------------------------------------------------- reset admin password
function ResetAdminCard() {
  const users = useUsers()
  const toast = useToast()
  const qc = useQueryClient()
  const [open, setOpen] = useState(false)
  const [target, setTarget] = useState('')
  const [busy, setBusy] = useState(false)
  const [temp, setTemp] = useState<TempPassword | null>(null)
  const admins = (users.data ?? []).filter((u) => u.role === 'admin' && !u.disabled)
  const selected = admins.find((u) => u.id === target)

  useEffect(() => {
    if (open && !target) setTarget(admins.find((u) => u.self)?.id ?? admins[0]?.id ?? '')
  }, [open, target, admins])

  const reset = async () => {
    if (!selected) return
    setBusy(true)
    try {
      const r = await resetPassword(selected)
      setOpen(false)
      setTemp(r)
      qc.invalidateQueries({ queryKey: authKeys.users })
    } catch (err) {
      toast.error(new Error(describeError(err)), "Couldn't reset the password")
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
      <div className="danger-card">
        <div className="grow">
          <div className="toggle-title">Reset admin password</div>
          <div className="toggle-desc" style={{ fontSize: 12 }}>
            Signs out all sessions
          </div>
        </div>
        <Button className="danger-outline" onClick={() => setOpen(true)}>
          Reset
        </Button>
      </div>
      <Dialog
        open={open}
        onClose={() => setOpen(false)}
        width={460}
        icon="warning"
        iconTone="danger"
        title="Reset admin password"
        description="Generates a temporary password and signs out every session of that admin. They must choose a new password at next sign-in."
        footer={
          <>
            <Button onClick={() => setOpen(false)}>Cancel</Button>
            <Button variant="danger" loading={busy} disabled={!selected} onClick={reset}>
              Reset password
            </Button>
          </>
        }
      >
        <Field label="Admin">
          <Select value={target} onChange={setTarget} options={admins.map((u) => ({ value: u.id, label: `${u.username}${u.self ? ' (you)' : ''}` }))} />
        </Field>
        {selected?.self && <Callout tone="warn">You'll be signed out too — copy the temporary password before closing the next dialog.</Callout>}
      </Dialog>
      <TempPasswordDialog value={temp} onClose={() => setTemp(null)} />
    </>
  )
}
