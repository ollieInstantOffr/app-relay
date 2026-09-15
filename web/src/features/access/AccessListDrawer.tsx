// Edit access list drawer (design 28a): IP rules | Basic auth | Hosts.
import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { useQueryClient } from '@tanstack/react-query'
import { api } from '../../lib/api'
import { useRole, useSaveEntity } from '../../lib/queries'
import { ago, rid } from '../../lib/format'
import type { AccessList, BasicAuthUser } from '../../lib/types'
import {
  Button, Callout, CopyButton, Dialog, Drawer, Field, Icon, IconButton, Input, Textarea, ToggleCard, ToggleRow, Tooltip, useToast,
} from '../../components/ui'
import { fieldErrors, pendingToast, randomPassword, toastUnlessFields, triggerDownload } from '../certificates/common'
import RuleEditor from './RuleEditor'
import { type AccessUsage } from './access'
import '../certificates/certs.css'

export type AccessTab = 'rules' | 'auth' | 'hosts'

type DraftUser = BasicAuthUser & { key: string; existing: boolean; reset: boolean }

const blank = (): AccessList => ({
  id: '', createdAt: '', updatedAt: '', name: '', description: '', rules: [],
  basicAuth: { enabled: false, realm: '', users: [] }, satisfyAny: false,
})

function toDraftUsers(l: AccessList): DraftUser[] {
  return l.basicAuth.users.map((u) => ({ ...u, password: u.password ?? '', key: rid(), existing: !u.password, reset: !!u.password }))
}

export default function AccessListDrawer({ open, onClose, list, initialTab = 'rules', usage, onSaved }: {
  open: boolean
  onClose: () => void
  list?: AccessList
  initialTab?: AccessTab
  usage?: AccessUsage
  onSaved?: (l: AccessList) => void
}) {
  const toast = useToast()
  const qc = useQueryClient()
  const { canWrite, isAdmin } = useRole()
  const save = useSaveEntity('access-lists')
  const [tab, setTab] = useState<AccessTab>(initialTab)
  const [draft, setDraft] = useState<AccessList>(blank())
  const [users, setUsers] = useState<DraftUser[]>([])
  const [newUser, setNewUser] = useState({ username: '', password: '' })
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [importOpen, setImportOpen] = useState(false)
  const [importText, setImportText] = useState('')
  const [importBusy, setImportBusy] = useState(false)

  useEffect(() => {
    if (!open) return
    const base = list ? structuredClone(list) : blank()
    setDraft(base)
    setUsers(toDraftUsers(base))
    setNewUser({ username: '', password: '' })
    setErrors({})
    setTab(initialTab)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, list?.id])

  const editing = !!draft.id
  const set = (patch: Partial<AccessList>) => setDraft((d) => ({ ...d, ...patch }))
  const setAuth = (patch: Partial<AccessList['basicAuth']>) => setDraft((d) => ({ ...d, basicAuth: { ...d.basicAuth, ...patch } }))

  const addUser = () => {
    const username = newUser.username.trim()
    if (!username) return
    if (users.some((u) => u.username === username)) {
      setErrors((e) => ({ ...e, newUser: `${username} already exists` }))
      return
    }
    setUsers((us) => [...us, { username, password: newUser.password, key: rid(), existing: false, reset: true }])
    setNewUser({ username: '', password: '' })
    setErrors((e) => ({ ...e, newUser: '' }))
    if (!draft.basicAuth.enabled) setAuth({ enabled: true })
  }

  const payload = (): Partial<AccessList> => {
    const pending = newUser.username.trim() ? [{ username: newUser.username.trim(), password: newUser.password }] : []
    return {
      ...draft,
      id: draft.id || undefined,
      basicAuth: {
        ...draft.basicAuth,
        users: [
          ...users.map((u) => ({ username: u.username, ...(u.reset && u.password ? { password: u.password } : {}) })),
          ...pending,
        ],
      },
    }
  }

  const submit = async () => {
    setErrors({})
    try {
      const saved = await save.mutateAsync(payload())
      qc.invalidateQueries({ queryKey: ['entities', 'access-lists'] })
      pendingToast(toast, editing ? 'Access list saved' : 'Access list created', saved.name)
      onSaved?.(saved)
      onClose()
    } catch (err) {
      const f = fieldErrors(err)
      setErrors(f)
      if (Object.keys(f).some((k) => k.startsWith('basicAuth') || k === 'satisfyAny')) setTab('auth')
      else if (Object.keys(f).some((k) => k.startsWith('rules') || k === 'name')) setTab('rules')
      toastUnlessFields(toast, err, 'Could not save access list')
    }
  }

  useEffect(() => {
    if (!open) return
    const h = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 's') {
        e.preventDefault()
        if (canWrite) submit()
      }
    }
    window.addEventListener('keydown', h)
    return () => window.removeEventListener('keydown', h)
  })

  const runImport = async () => {
    setImportBusy(true)
    try {
      const res = await api.post<{ list: AccessList; added: number; updated: number }>(`/api/access-lists/${draft.id}/htpasswd`, { content: importText })
      qc.invalidateQueries({ queryKey: ['entities', 'access-lists'] })
      // merge imported users into the draft; their hashes now live on the server
      const imported = res.list.basicAuth.users
      setUsers((us) => {
        const byName = new Map(us.map((u) => [u.username, u]))
        imported.forEach((u) => byName.set(u.username, { ...u, password: '', key: rid(), existing: true, reset: false }))
        return [...byName.values()]
      })
      setAuth({ enabled: true, realm: draft.basicAuth.realm || res.list.basicAuth.realm })
      pendingToast(toast, `Imported ${res.added + res.updated} users`, `${res.added} added · ${res.updated} updated in ${res.list.name},`)
      setImportOpen(false)
      setImportText('')
    } catch (err) {
      toast.error(err, 'Import failed')
    } finally {
      setImportBusy(false)
    }
  }

  const hostCount = usage ? new Set(usage.hosts.map((h) => h.id)).size : 0
  const tabs = [
    { id: 'rules' as const, label: 'IP rules' },
    { id: 'auth' as const, label: 'Basic auth' },
    ...(editing ? [{ id: 'hosts' as const, label: 'Hosts', count: hostCount }] : []),
  ]

  return (
    <>
      <Drawer
        open={open}
        onClose={onClose}
        title={editing ? `Edit access list "${list?.name}"` : 'New access list'}
        subtitle="IP rules run first; basic auth prompts after they pass"
        tabs={tabs}
        tab={tab}
        onTab={setTab}
        footer={
          <>
            <div className="spacer" />
            <Button onClick={onClose}>Cancel</Button>
            <Button variant="primary" onClick={submit} loading={save.isPending} disabled={!canWrite}>Save to pending</Button>
          </>
        }
      >
        {tab === 'rules' && (
          <>
            <div className="grid-2">
              <Field label="Name" error={errors.name}>
                <Input mono value={draft.name} invalid={!!errors.name} placeholder="lan-only" onChange={(e) => set({ name: e.target.value })} autoFocus={!editing} />
              </Field>
              <Field label="Description" error={errors.description}>
                <Input value={draft.description} placeholder="Only local network clients" onChange={(e) => set({ description: e.target.value })} />
              </Field>
            </div>
            <div className="card" style={{ overflow: 'hidden' }}>
              <div className="card-header">IP rules<span className="sub">Evaluated top to bottom · first match wins</span></div>
              <RuleEditor rules={draft.rules} onChange={(rules) => set({ rules })} errors={errors} readOnly={!canWrite} />
            </div>
          </>
        )}

        {tab === 'auth' && (
          <>
            <ToggleCard
              title="Require username & password"
              description="Browser prompt · applied after IP rules pass"
              checked={draft.basicAuth.enabled}
              onChange={(v) => setAuth({ enabled: v })}
              disabled={!canWrite}
            />
            {errors['basicAuth.users'] && <Callout tone="danger">{errors['basicAuth.users']}</Callout>}
            <div className="card" style={{ overflow: 'hidden' }}>
              <table className="table compact cs-users">
                <thead>
                  <tr><th>Username</th><th>Password</th><th>Last used</th><th /></tr>
                </thead>
                <tbody>
                  {users.map((u, i) => {
                    const pwErr = errors[`basicAuth.users.${i}.password`]
                    const nameErr = errors[`basicAuth.users.${i}.username`]
                    return (
                      <tr key={u.key}>
                        <td>
                          <span className="mono">{u.username}</span>
                          {nameErr && <div className="field-error">{nameErr}</div>}
                        </td>
                        <td>
                          {u.reset ? (
                            <div className="row gap-6">
                              <Input mono inputSize="sm" invalid={!!pwErr} value={u.password ?? ''} placeholder="new password" onChange={(e) => setUsers((us) => us.map((x) => (x.key === u.key ? { ...x, password: e.target.value } : x)))} style={{ width: 170 }} />
                              <Button size="sm" onClick={() => setUsers((us) => us.map((x) => (x.key === u.key ? { ...x, password: randomPassword() } : x)))}>Generate</Button>
                              {u.password && <CopyButton text={u.password} label="" />}
                              {u.existing && <Button size="sm" variant="ghost" onClick={() => setUsers((us) => us.map((x) => (x.key === u.key ? { ...x, reset: false, password: '' } : x)))}>Keep</Button>}
                            </div>
                          ) : (
                            <span className="row gap-10">
                              <span className="mono muted">••••••••</span>
                              {canWrite && <Button variant="link" size="sm" onClick={() => setUsers((us) => us.map((x) => (x.key === u.key ? { ...x, reset: true, password: '' } : x)))}>Reset</Button>}
                            </span>
                          )}
                          {pwErr && <div className="field-error">{pwErr}</div>}
                        </td>
                        <td className="small muted">{u.lastUsedAt ? ago(u.lastUsedAt) : u.existing ? 'never' : 'new'}</td>
                        <td style={{ width: 36 }}>
                          {canWrite && <IconButton icon="close" bare size={14} label={`Remove ${u.username}`} onClick={() => setUsers((us) => us.filter((x) => x.key !== u.key))} />}
                        </td>
                      </tr>
                    )
                  })}
                  {canWrite && (
                    <tr>
                      <td>
                        <Input mono inputSize="sm" value={newUser.username} placeholder="username" onChange={(e) => setNewUser((n) => ({ ...n, username: e.target.value }))} onKeyDown={(e) => e.key === 'Enter' && addUser()} style={{ width: 130 }} />
                        {errors.newUser && <div className="field-error">{errors.newUser}</div>}
                      </td>
                      <td>
                        <div className="row gap-6">
                          <Input mono inputSize="sm" value={newUser.password} placeholder="password" onChange={(e) => setNewUser((n) => ({ ...n, password: e.target.value }))} onKeyDown={(e) => e.key === 'Enter' && addUser()} style={{ width: 170 }} />
                          <Button size="sm" onClick={() => setNewUser((n) => ({ ...n, password: randomPassword() }))}>Generate</Button>
                        </div>
                      </td>
                      <td colSpan={2}>
                        <Button size="sm" variant="primary" onClick={addUser} disabled={!newUser.username.trim() || newUser.password.length < 8}>Add</Button>
                      </td>
                    </tr>
                  )}
                </tbody>
              </table>
              {!users.length && !canWrite && <div className="small muted" style={{ padding: 14 }}>No users.</div>}
            </div>
            <div className="row gap-8">
              <Tooltip content={editing ? 'Import bcrypt entries from an htpasswd file' : 'Save the list first'}>
                <Button size="sm" icon="upload" disabled={!canWrite || !editing} onClick={() => setImportOpen(true)}>Import htpasswd</Button>
              </Tooltip>
              <Tooltip content={isAdmin ? 'Download user:hash lines' : 'Admins only'}>
                <Button size="sm" icon="download" disabled={!isAdmin || !editing || !list?.basicAuth.users.length} onClick={() => triggerDownload(`/api/access-lists/${draft.id}/htpasswd`)}>Export</Button>
              </Tooltip>
            </div>
            <Field label="Realm" error={errors['basicAuth.realm']} hint="Shown in the browser's password prompt">
              <Input value={draft.basicAuth.realm} placeholder="Restricted" onChange={(e) => setAuth({ realm: e.target.value })} disabled={!canWrite} />
            </Field>
            <div className="card">
              <ToggleRow title="Satisfy any" description="IP match OR password — allowed IPs skip the prompt, everyone else can sign in" checked={draft.satisfyAny} onChange={(v) => set({ satisfyAny: v })} disabled={!canWrite || !draft.basicAuth.enabled} />
            </div>
            {errors.satisfyAny && <div className="field-error">{errors.satisfyAny}</div>}
            <div className="row gap-6 small muted"><Icon name="token" size={14} /> Passwords stored bcrypt-hashed</div>
          </>
        )}

        {tab === 'hosts' && (
          <div className="col gap-12">
            {usage?.hosts.length ? (
              <div className="col" style={{ gap: 6 }}>
                {usage.hosts.map((h) => (
                  <Link key={h.id + h.via} to={`/hosts?edit=${h.id}`} className="list-row">
                    <span className="mono grow">{h.domain}</span>
                    <span className="small muted">{h.via === 'host' ? 'whole host' : h.via}</span>
                    <Icon name="chevron" size={14} />
                  </Link>
                ))}
              </div>
            ) : (
              <div className="small muted">No hosts use this list. Attach it from a host's Details tab.</div>
            )}
            {!!usage?.other.length && <Callout tone="info" title="Also used by">{usage.other.join(' · ')}</Callout>}
          </div>
        )}
      </Drawer>

      <Dialog
        open={importOpen}
        onClose={() => setImportOpen(false)}
        width={520}
        title="Import htpasswd"
        description="Paste user:hash lines. Only bcrypt hashes ($2y$, created with htpasswd -B) are accepted; existing users with the same name get the imported password. Saves to pending immediately."
        footer={
          <>
            <Button onClick={() => setImportOpen(false)}>Cancel</Button>
            <Button variant="primary" loading={importBusy} disabled={!importText.trim()} onClick={runImport}>Import</Button>
          </>
        }
      >
        <Textarea mono value={importText} onChange={(e) => setImportText(e.target.value)} placeholder={'jonas:$2y$05$…\nmira:$2y$05$…'} style={{ minHeight: 160, fontSize: 12 }} spellCheck={false} />
        <div className="row gap-8">
          <input type="file" accept=".htpasswd,.txt,text/plain" onChange={async (e) => { const f = e.target.files?.[0]; if (f) setImportText(await f.text()) }} />
        </div>
      </Dialog>
    </>
  )
}
