// Add user dialog (design 28c).
import { useEffect, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Button, Callout, Checkbox, CopyButton, Dialog, Field, Input, RadioCard, useToast } from '../../components/ui'
import { api } from '../../lib/api'
import type { Role } from '../../lib/types'
import { authKeys, describeError, fieldErrors, generatePassword, type UserRow } from './authApi'
import './auth.css'

const ROLES: { role: Role; title: string; description: string }[] = [
  { role: 'admin', title: 'Admin', description: 'Everything, incl. users and settings' },
  { role: 'editor', title: 'Editor', description: 'Hosts, certs, backends · can apply' },
  { role: 'viewer', title: 'Viewer', description: 'Read-only dashboards and logs' },
]

export function AddUserDialog({ open, onClose, require2fa }: { open: boolean; onClose: () => void; require2fa: boolean }) {
  const qc = useQueryClient()
  const toast = useToast()
  const [username, setUsername] = useState('')
  const [email, setEmail] = useState('')
  const [role, setRole] = useState<Role>('editor')
  const [password, setPassword] = useState('')
  const [mustChange, setMustChange] = useState(true)
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    if (!open) return
    setUsername('')
    setEmail('')
    setRole('editor')
    setPassword(generatePassword())
    setMustChange(true)
    setErrors({})
    setError('')
  }, [open])

  const create = async () => {
    setBusy(true)
    setErrors({})
    setError('')
    try {
      const u = await api.post<UserRow>('/api/users', { username: username.trim(), email: email.trim(), role, password, mustChangePassword: mustChange })
      qc.invalidateQueries({ queryKey: authKeys.users })
      toast.success('User created', `Share the initial password with ${u.username} securely.`)
      onClose()
    } catch (err) {
      const f = fieldErrors(err)
      if (Object.keys(f).length) setErrors(f)
      else setError(describeError(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog
      open={open}
      onClose={onClose}
      width={500}
      title="Add user"
      description={`Signs in with username + password${require2fa ? '; 2FA required for admins' : ''}.`}
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" loading={busy} disabled={!username.trim() || !password} onClick={create}>
            Create user
          </Button>
        </>
      }
    >
      <div className="grid-2">
        <Field label="Username" htmlFor="add-user-name" error={errors.username}>
          <Input
            id="add-user-name"
            autoFocus
            autoCapitalize="none"
            spellCheck={false}
            placeholder="alex"
            value={username}
            invalid={!!errors.username}
            onChange={(e) => setUsername(e.target.value)}
          />
        </Field>
        <Field label="Email (optional)" htmlFor="add-user-email" error={errors.email} hint="For your records">
          <Input id="add-user-email" type="email" value={email} invalid={!!errors.email} onChange={(e) => setEmail(e.target.value)} />
        </Field>
      </div>
      <div className="field">
        <label className="field-label">Role</label>
        <div className="col gap-8">
          {ROLES.map((r) => (
            <RadioCard key={r.role} selected={role === r.role} onSelect={() => setRole(r.role)} title={r.title} description={r.description} />
          ))}
        </div>
        {errors.role && <div className="field-error">{errors.role}</div>}
      </div>
      <Field label="Initial password" htmlFor="add-user-password" error={errors.password}>
        <div className="row gap-8">
          <Input id="add-user-password" mono value={password} invalid={!!errors.password} onChange={(e) => setPassword(e.target.value)} />
          <Button onClick={() => setPassword(generatePassword())}>Regenerate</Button>
          <CopyButton text={password} size="default" />
        </div>
      </Field>
      <Checkbox checked={mustChange} onChange={setMustChange} label="Must change on first sign-in" />
      {error && <Callout tone="danger">{error}</Callout>}
    </Dialog>
  )
}
