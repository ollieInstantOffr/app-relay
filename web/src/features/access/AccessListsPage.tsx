// Access lists (design 05).
import { useEffect, useMemo, useRef, useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { TopBar } from '../../components/shell/TopBar'
import {
  Badge, Button, Callout, ConfirmDialog, Dialog, EmptyState, Field, Input, NoMatches, Pagination, SearchInput, Select, Skeleton, Spinner, TableToolbar,
  Toggle, Tooltip, matchesSearch, useFitGrid, usePagination, useToast,
} from '../../components/ui'
import { api } from '../../lib/api'
import { Topics, useBusEvent } from '../../lib/events'
import { useDeleteEntity, useEntities, useRole, useSaveEntity, useSettings } from '../../lib/queries'
import { ago, clock, rid } from '../../lib/format'
import type { AccessList } from '../../lib/types'
import { fieldErrors, pendingToast, toastUnlessFields } from '../certificates/common'
import AccessListDrawer, { type AccessTab } from './AccessListDrawer'
import RuleEditor from './RuleEditor'
import { hostCount, listSummary, useAccessUsage, type AccessDecision, type Denial } from './access'
import '../certificates/certs.css'

function TestIPDialog({ open, onClose, list, draft }: { open: boolean; onClose: () => void; list: AccessList; draft?: AccessList }) {
  const [ip, setIp] = useState('')
  const [busy, setBusy] = useState(false)
  const [result, setResult] = useState<AccessDecision | null>(null)
  const [error, setError] = useState('')
  useEffect(() => {
    if (open) {
      setResult(null)
      setError('')
    }
  }, [open])
  const run = async () => {
    setBusy(true)
    setError('')
    setResult(null)
    try {
      setResult(await api.post<AccessDecision>(`/api/access-lists/${list.id}/test`, { ip: ip.trim(), list: draft }))
    } catch (err) {
      setError(fieldErrors(err).ip ?? String((err as Error).message))
    } finally {
      setBusy(false)
    }
  }
  return (
    <Dialog
      open={open}
      onClose={onClose}
      width={460}
      title={`Test an IP against ${list.name}`}
      description={draft ? 'Uses your unsaved edits.' : 'Evaluates the rules the way the reverse proxy does: first matching rule wins, then basic auth.'}
      footer={
        <>
          <Button onClick={onClose}>Close</Button>
          <Button variant="primary" onClick={run} loading={busy} disabled={!ip.trim()}>Test</Button>
        </>
      }
    >
      <Field label="Client IP" error={error}>
        <Input mono autoFocus value={ip} invalid={!!error} placeholder="203.0.113.7" onChange={(e) => setIp(e.target.value)} onKeyDown={(e) => e.key === 'Enter' && ip.trim() && run()} />
      </Field>
      {result && (
        <Callout
          tone={result.allowed ? 'ok' : result.requiresAuth ? 'info' : 'danger'}
          title={result.allowed ? 'Allowed' : result.requiresAuth ? 'Password prompt' : 'Denied · 403'}
        >
          {result.explanation}
          {result.matchedRule?.note && <span> · {result.matchedRule.note}</span>}
        </Callout>
      )}
    </Dialog>
  )
}

function Denials({ listId }: { listId: string }) {
  const q = useQuery({
    queryKey: ['access-denials', listId],
    queryFn: () => api.get<Denial[]>(`/api/access-lists/${listId}/denials?since=24h&limit=50`),
    refetchInterval: 30_000,
  })
  return (
    <div className="card" style={{ padding: '14px 18px', display: 'flex', flexDirection: 'column', gap: 10 }}>
      <div className="row">
        <div className="card-title">Recent denials · 24h</div>
        {q.data && q.data.length > 0 && <span className="small muted" style={{ marginLeft: 'auto' }}>{q.data.length}{q.data.length === 50 ? '+' : ''}</span>}
      </div>
      {q.isLoading && <Skeleton height={48} />}
      {q.isError && <div className="small muted">Couldn't load denials.</div>}
      {q.data && q.data.length === 0 && <div className="small muted">No denied requests in the last 24 hours.</div>}
      {q.data && q.data.length > 0 && (
        <div className="col" style={{ gap: 2, maxHeight: 260, overflow: 'auto' }}>
          {q.data.map((d) => (
            <div key={d.id} className="cs-denial">
              <span className="faint">{clock(d.ts)}</span>
              <Link to={`/logs/access?ip=${encodeURIComponent(d.clientIp)}`} className="truncate">{d.clientIp}</Link>
              <span className="truncate muted">{d.method} {d.path} → {d.host}</span>
              <span className="danger-text">{d.status}</span>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

export default function AccessListsPage() {
  const toast = useToast()
  const qc = useQueryClient()
  const { canWrite } = useRole()
  const [params, setParams] = useSearchParams()
  const listsQ = useEntities('access-lists')
  const lists = listsQ.data ?? []
  const usageQ = useAccessUsage()
  const usage = usageQ.data ?? {}
  const general = useSettings('general').data
  const save = useSaveEntity('access-lists')
  const del = useDeleteEntity('access-lists')

  const selectedId = params.get('list') ?? lists[0]?.id
  const selected = lists.find((l) => l.id === selectedId)
  const [draft, setDraft] = useState<AccessList | undefined>()
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [drawer, setDrawer] = useState<{ open: boolean; list?: AccessList; tab?: AccessTab }>({ open: false })
  const [testOpen, setTestOpen] = useState(false)
  const [confirmDelete, setConfirmDelete] = useState(false)
  const [search, setSearch] = useState('')
  const [kind, setKind] = useState('')
  const listRef = useRef<HTMLDivElement>(null)

  const filtered = useMemo(
    () =>
      lists.filter((l) => {
        if (kind === 'ip' && l.rules.length === 0) return false
        if (kind === 'auth' && !l.basicAuth.enabled) return false
        if (kind === 'open' && (l.rules.length > 0 || l.basicAuth.enabled)) return false
        return matchesSearch(search, l.name, l.description, ...l.rules.map((r) => r.cidr), ...l.rules.map((r) => r.note))
      }),
    [lists, search, kind],
  )
  // Items that fit the sidebar: below them sit the pager and the list padding.
  const { pageSize } = useFitGrid(listRef, { reserve: 41 + 12, itemHeight: 63, min: 3 })
  const pg = usePagination(filtered, pageSize, [search, kind])
  const filtersOn = !!(search.trim() || kind)

  useBusEvent(Topics.ConfigChanged, () => qc.invalidateQueries({ queryKey: ['entities', 'access-lists', '__usage'] }))

  useEffect(() => {
    setDraft(selected ? structuredClone(selected) : undefined)
    setErrors({})
  }, [selected])

  useEffect(() => {
    if (params.get('new') === '1' && canWrite) {
      setDrawer({ open: true })
      const next = new URLSearchParams(params)
      next.delete('new')
      setParams(next, { replace: true })
    }
  }, [params, setParams, canWrite])

  const dirty = useMemo(() => !!selected && !!draft && JSON.stringify(selected) !== JSON.stringify(draft), [selected, draft])
  const select = (id: string) => {
    const next = new URLSearchParams(params)
    next.set('list', id)
    setParams(next, { replace: true })
  }

  const saveDraft = async () => {
    if (!draft) return
    setErrors({})
    try {
      const saved = await save.mutateAsync({ ...draft, basicAuth: { ...draft.basicAuth, users: draft.basicAuth.users.map((u) => ({ username: u.username })) } })
      pendingToast(toast, 'Access list saved', saved.name)
    } catch (err) {
      setErrors(fieldErrors(err))
      toastUnlessFields(toast, err, 'Could not save access list')
    }
  }

  const createLanOnly = async () => {
    try {
      const lan = general?.lanCidr || '192.168.0.0/16'
      const saved = await save.mutateAsync({
        name: 'lan-only',
        description: 'Only local network clients. Everyone else gets a 403.',
        rules: [
          { id: rid(), action: 'allow', cidr: lan, note: 'Home LAN' },
          { id: rid(), action: 'deny', cidr: 'all', note: 'Default' },
        ],
        basicAuth: { enabled: false, realm: '', users: [] },
        satisfyAny: false,
      })
      pendingToast(toast, 'Access list created', saved.name)
      select(saved.id)
    } catch (err) {
      toast.error(err, 'Could not create access list')
    }
  }

  const u = selected ? usage[selected.id] : undefined
  const inUse = !!u && (u.hosts.length > 0 || u.other.length > 0)
  const attached = u ? [...new Map(u.hosts.map((h) => [h.id, h])).values()] : []

  return (
    <>
      <TopBar
        title="Access lists"
        count={listsQ.data ? lists.length : undefined}
        actions={canWrite && <Button variant="primary" icon="plus" onClick={() => setDrawer({ open: true })}>New list</Button>}
      />
      {listsQ.isLoading ? (
        <div className="page"><Skeleton height={200} /></div>
      ) : lists.length === 0 ? (
        <div className="page">
          <EmptyState
            icon="access"
            title="No access lists yet"
            description="An access list restricts hosts to your LAN or VPN, adds a password prompt, or both. Attach it to hosts, locations, the admin UI or the MCP server."
            actions={canWrite && (
              <>
                <Button variant="primary" icon="plus" onClick={() => setDrawer({ open: true })}>New list</Button>
                <Button onClick={createLanOnly}>Create lan-only ({general?.lanCidr || '192.168.0.0/16'})</Button>
              </>
            )}
          />
        </div>
      ) : (
        <div className="cs-split">
          <div className="cs-list">
            <div className="cs-list-tools">
              <TableToolbar>
                <SearchInput value={search} onChange={setSearch} placeholder="Search name or description" label="Search access lists" width={320} />
                <div className="cs-list-filter">
                  <Select
                    inputSize="sm"
                    value={kind}
                    placeholder="All lists"
                    aria-label="Restriction"
                    onChange={setKind}
                    options={[
                      { value: 'ip', label: 'With IP rules' },
                      { value: 'auth', label: 'With basic auth' },
                      { value: 'open', label: 'No restrictions' },
                    ]}
                  />
                </div>
              </TableToolbar>
            </div>
            <div className="cs-list-items" ref={listRef}>
            {filtered.length === 0 && (
              <NoMatches
                what="lists"
                onClear={
                  filtersOn
                    ? () => {
                        setSearch('')
                        setKind('')
                      }
                    : undefined
                }
              />
            )}
            {pg.rows.map((l) => {
              const n = hostCount(usage[l.id])
              return (
                <button key={l.id} type="button" className={`cs-list-item${l.id === selectedId ? ' active' : ''}`} onClick={() => select(l.id)}>
                  <div className="row gap-8">
                    <span className="mono medium truncate">{l.name}</span>
                    <span className="micro muted" style={{ marginLeft: 'auto', whiteSpace: 'nowrap' }}>{usageQ.data ? `${n} ${n === 1 ? 'host' : 'hosts'}` : ''}</span>
                  </div>
                  <div className="small muted truncate">{listSummary(l)}</div>
                </button>
              )
            })}
            </div>
            <Pagination page={pg.page} pageSize={pg.pageSize} total={pg.total} onPage={pg.setPage} label="lists" />
          </div>

          {selected && draft && (
            <div className="cs-detail">
              <div className="row-top gap-16">
                <div className="grow">
                  <div className="cs-detail-title">{selected.name}</div>
                  <div className="muted" style={{ marginTop: 4 }}>{selected.description || 'No description'}</div>
                </div>
                <Button onClick={() => setTestOpen(true)}>Test an IP</Button>
                {canWrite && <Button icon="edit" onClick={() => setDrawer({ open: true, list: selected })}>Edit</Button>}
                {canWrite && (
                  <Tooltip content={inUse ? `Used by ${[...attached.map((h) => h.domain), ...(u?.other ?? [])].slice(0, 4).join(', ')}` : 'Delete list'}>
                    <Button className="danger-text" disabled={inUse} onClick={() => setConfirmDelete(true)}>Delete</Button>
                  </Tooltip>
                )}
              </div>

              <div className="grid-2" style={{ gap: 14, alignItems: 'start' }}>
                <div className="card" style={{ overflow: 'hidden' }}>
                  <div className="card-header">IP rules<div className="spacer" /><span className="small muted" style={{ fontWeight: 400 }}>Evaluated top to bottom</span></div>
                  <RuleEditor rules={draft.rules} onChange={(rules) => setDraft({ ...draft, rules })} errors={errors} readOnly={!canWrite} />
                </div>
                <div className="card" style={{ overflow: 'hidden' }}>
                  <div className="card-header" style={{ gap: 12 }}>
                    Basic authentication
                    <div className="spacer" />
                    <Toggle
                      checked={draft.basicAuth.enabled}
                      disabled={!canWrite}
                      label="Basic authentication"
                      onChange={(v) => {
                        if (v && draft.basicAuth.users.length === 0) {
                          setDrawer({ open: true, list: { ...draft, basicAuth: { ...draft.basicAuth, enabled: true } }, tab: 'auth' })
                          return
                        }
                        setDraft({ ...draft, basicAuth: { ...draft.basicAuth, enabled: v }, satisfyAny: v ? draft.satisfyAny : false })
                      }}
                    />
                  </div>
                  {draft.basicAuth.enabled ? (
                    <div className="col gap-10" style={{ padding: 18 }}>
                      <div className="small muted">
                        {draft.satisfyAny ? 'Satisfy any — allowed IPs skip the prompt, others can sign in.' : 'Clients passing the IP rules must also enter a password.'}
                        {draft.basicAuth.realm && <> · realm “{draft.basicAuth.realm}”</>}
                      </div>
                      <div className="col" style={{ gap: 4 }}>
                        {draft.basicAuth.users.map((usr) => (
                          <div key={usr.username} className="row small">
                            <span className="mono grow">{usr.username}</span>
                            <span className="muted">{usr.lastUsedAt ? `used ${ago(usr.lastUsedAt)}` : 'never used'}</span>
                          </div>
                        ))}
                      </div>
                      {canWrite && <Button size="sm" onClick={() => setDrawer({ open: true, list: selected, tab: 'auth' })}>Manage users</Button>}
                      {errors['basicAuth.users'] && <div className="field-error">{errors['basicAuth.users']}</div>}
                    </div>
                  ) : (
                    <div className="col gap-12" style={{ padding: 18, color: 'var(--ink-faint)', fontSize: 13 }}>
                      <div>Off — clients matching an allow rule pass through without a password.</div>
                      <div className="cs-dashed">Turn on to add users</div>
                    </div>
                  )}
                </div>
              </div>

              <div className="card" style={{ padding: '14px 18px', display: 'flex', flexDirection: 'column', gap: 10 }}>
                <div className="row">
                  <div className="card-title">Attached hosts</div>
                  <span className="small muted" style={{ marginLeft: 'auto' }}>{usageQ.data ? attached.length : <Spinner />}</span>
                </div>
                {attached.length ? (
                  <div className="row gap-6 wrap">
                    {attached.map((h) => (
                      <Link key={h.id} to={`/hosts?edit=${h.id}`} className="cs-chip" title={h.via === 'host' ? undefined : h.via}>{h.domain}</Link>
                    ))}
                  </div>
                ) : (
                  <div className="small muted">No hosts use this list yet — pick it in a host's Details tab.</div>
                )}
                {!!u?.other.length && (
                  <div className="row gap-6 wrap">
                    {u.other.map((o) => <Badge key={o} tone="outline">{o}</Badge>)}
                  </div>
                )}
              </div>

              <Denials listId={selected.id} />

              {dirty && canWrite && (
                <div className="cs-sticky-footer">
                  <span className="small">Unsaved changes to <span className="mono">{selected.name}</span></span>
                  <div className="spacer" />
                  <Button onClick={() => { setDraft(structuredClone(selected)); setErrors({}) }}>Discard</Button>
                  <Button variant="primary" onClick={saveDraft} loading={save.isPending}>Save to pending</Button>
                </div>
              )}
            </div>
          )}
        </div>
      )}

      <AccessListDrawer
        open={drawer.open}
        list={drawer.list}
        initialTab={drawer.tab}
        usage={drawer.list?.id ? usage[drawer.list.id] : undefined}
        onClose={() => setDrawer({ open: false })}
        onSaved={(l) => select(l.id)}
      />
      {selected && (
        <TestIPDialog open={testOpen} onClose={() => setTestOpen(false)} list={selected} draft={dirty ? draft : undefined} />
      )}
      <ConfirmDialog
        open={confirmDelete}
        onClose={() => setConfirmDelete(false)}
        danger
        title={`Delete ${selected?.name}?`}
        message="The list is removed on next apply. It isn't attached to anything."
        confirmLabel="Delete list"
        onConfirm={async () => {
          if (!selected) return
          try {
            await del.mutateAsync(selected.id)
            pendingToast(toast, 'Access list deleted', selected.name)
            const next = new URLSearchParams(params)
            next.delete('list')
            setParams(next, { replace: true })
          } catch (err) {
            toast.error(err, 'Could not delete access list')
            throw err
          }
        }}
      />
    </>
  )
}
