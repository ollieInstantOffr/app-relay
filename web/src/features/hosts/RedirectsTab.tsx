// Owner: slice hosts. Redirects tab + drawer (design 20).
import { useCallback, useEffect, useRef, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import {
  Badge, Button, ConfirmDialog, Drawer, EmptyState, Field, IconButton, Input, Menu, Segmented, Select, Skeleton, Spinner, ToggleCard, cx, useToast,
  type MenuEntry,
} from '../../components/ui'
import { ApiError } from '../../lib/api'
import { useDeleteEntity, useEntities, useEntity, useRole, useSaveEntity } from '../../lib/queries'
import type { Redirect } from '../../lib/types'
import { DomainsInput } from './DomainsInput'
import { applyNowAction, certOptionLabel, newRedirect, rankCerts, urlError, useDebounced, useDomainChecks, type RedirectDraft } from './lib'

const CODE_HINTS: Record<number, string> = {
  301: 'Permanent · browsers cache it',
  302: 'Temporary · not cached',
  307: 'Temporary · keeps method and body',
  308: 'Permanent · keeps method and body',
}

const redirectName = (r: { domains: string[]; fromPath: string }) => `${r.domains[0] ?? ''}${r.fromPath}`

export function RedirectsView({ redirects, loading, filter }: { redirects: Redirect[]; loading: boolean; filter: string }) {
  const { canWrite } = useRole()
  const [params, setParams] = useSearchParams()
  const toast = useToast()
  const save = useSaveEntity('redirects')
  const del = useDeleteEntity('redirects')
  const [deleting, setDeleting] = useState<Redirect | null>(null)
  const editId = params.get('edit')
  const isNew = params.get('new') === '1'

  const open = (r?: Redirect) =>
    setParams((p) => {
      if (r) {
        p.set('edit', r.id)
        p.delete('new')
      } else {
        p.set('new', '1')
        p.delete('edit')
      }
      return p
    })
  const close = useCallback(
    () =>
      setParams((p) => {
        p.delete('edit')
        p.delete('new')
        return p
      }),
    [setParams],
  )

  const toggle = async (r: Redirect) => {
    try {
      await save.mutateAsync({ ...r, enabled: !r.enabled })
      toast.show({
        kind: 'success',
        title: r.enabled ? 'Redirect disabled' : 'Redirect enabled',
        message: `${redirectName(r)} added to pending changes.`,
        actions: [applyNowAction],
      })
    } catch (err) {
      toast.error(err, 'Could not update redirect')
    }
  }

  const q = filter.trim().toLowerCase()
  const rows = redirects.filter(
    (r) => !q || r.domains.some((d) => d.includes(q)) || r.to.toLowerCase().includes(q) || r.fromPath.toLowerCase().includes(q),
  )

  let body: React.ReactNode
  if (loading) {
    body = (
      <div className="card card-pad col gap-10">
        <Skeleton height={16} />
        <Skeleton height={16} />
        <Skeleton height={16} width="60%" />
      </div>
    )
  } else if (redirects.length === 0) {
    body = (
      <div className="card">
        <EmptyState
          icon="redirect"
          title="No redirects yet"
          description="Send a domain — or one path on it — somewhere else: www to apex, old names to new ones."
          actions={canWrite ? <Button variant="primary" icon="plus" onClick={() => open()}>New redirect</Button> : undefined}
        />
      </div>
    )
  } else {
    body = (
      <div className="card">
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>From</th>
                <th>To</th>
                <th>Code</th>
                <th>Options</th>
                <th className="col-menu" style={{ width: 44 }} aria-label="Actions" />
              </tr>
            </thead>
            <tbody>
              {rows.map((r) => {
                const items: MenuEntry[] = [{ header: redirectName(r) }, { label: canWrite ? 'Edit' : 'View', icon: 'edit', onSelect: () => open(r) }]
                if (canWrite) {
                  items.push('separator', { label: r.enabled ? 'Disable' : 'Enable', icon: 'power', onSelect: () => void toggle(r) }, {
                    label: 'Delete…', icon: 'trash', danger: true, onSelect: () => setDeleting(r),
                  })
                }
                return (
                  <tr key={r.id} className={cx('clickable', !r.enabled && 'dim')} onClick={() => open(r)}>
                    <td>
                      <div className="row gap-8 nowrap">
                        <span className="medium">
                          {r.domains[0]}
                          {r.fromPath && <span className="mono muted">{r.fromPath}</span>}
                        </span>
                        {r.domains.length > 1 && (
                          <span className="faint mono small" title={r.domains.slice(1).join(', ')}>+{r.domains.length - 1}</span>
                        )}
                        {!r.enabled && <Badge>disabled</Badge>}
                      </div>
                    </td>
                    <td>
                      <span className="hosts-redirect-to">
                        {r.to}
                        {r.keepPath && <span className="var">$request_uri</span>}
                      </span>
                    </td>
                    <td className="mono">{r.code}</td>
                    <td className="muted small nowrap">
                      {[r.keepPath ? 'keep path' : 'drop path', r.certificateId ? 'TLS' : null, r.forceHttps ? 'force https' : null].filter(Boolean).join(' · ')}
                    </td>
                    <td className="col-menu" onClick={(e) => e.stopPropagation()}>
                      <Menu trigger={<IconButton bare icon="more" label={`Actions for ${redirectName(r)}`} />} items={items} />
                    </td>
                  </tr>
                )
              })}
              {rows.length === 0 && (
                <tr>
                  <td colSpan={5} className="muted" style={{ textAlign: 'center', padding: 28 }}>
                    No redirects match “{filter}”
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
      </div>
    )
  }

  return (
    <>
      {body}
      <RedirectDrawer open={isNew || !!editId} isNew={isNew} redirectId={editId ?? undefined} redirects={redirects} onClose={close} />
      <ConfirmDialog
        open={!!deleting}
        onClose={() => setDeleting(null)}
        danger
        title={deleting ? `Delete redirect ${redirectName(deleting)}?` : ''}
        message="It stops redirecting on next apply. Requests then get the default-host response, or the proxy host on that domain."
        confirmLabel="Delete redirect"
        onConfirm={async () => {
          if (!deleting) return
          try {
            await del.mutateAsync(deleting.id)
            toast.show({ kind: 'success', title: 'Redirect deleted', message: `${redirectName(deleting)} removed on next apply.`, actions: [applyNowAction] })
          } catch (err) {
            toast.error(err, 'Could not delete redirect')
          }
        }}
      />
    </>
  )
}

function RedirectDrawer({ open, isNew, redirectId, redirects, onClose }: {
  open: boolean
  isNew: boolean
  redirectId?: string
  redirects: Redirect[]
  onClose: () => void
}) {
  const { canWrite } = useRole()
  const readOnly = !canWrite
  const toast = useToast()
  const cached = redirectId ? redirects.find((r) => r.id === redirectId) : undefined
  const one = useEntity('redirects', open && redirectId && !cached ? redirectId : undefined)
  const current = cached ?? one.data
  const certs = useEntities('certificates', { enabled: open }).data ?? []
  const save = useSaveEntity('redirects')
  const [draft, setDraft] = useState<RedirectDraft | null>(null)
  const [loadedKey, setLoadedKey] = useState('')
  const [errors, setErrors] = useState<Record<string, string>>({})

  const key = open ? (isNew ? 'new' : redirectId ?? '') : ''
  useEffect(() => {
    if (!open) {
      setDraft(null)
      setLoadedKey('')
      return
    }
    if (loadedKey === key) return
    const d = isNew ? newRedirect() : current ? { ...newRedirect(), ...current } : null
    if (!d) return
    setDraft(d)
    setLoadedKey(key)
    setErrors({})
  }, [open, key, loadedKey, isNew, current])

  const update = (patch: Partial<RedirectDraft>) => {
    setDraft((d) => (d ? { ...d, ...patch } : d))
    const changed = Object.keys(patch)
    setErrors((errs) => Object.fromEntries(Object.entries(errs).filter(([k]) => !changed.some((p) => k === p || k.startsWith(p + '.')))))
  }

  const fromPath = useDebounced(draft?.fromPath ?? '', 400)
  const conflicts = useDomainChecks(draft?.domains ?? [], { kind: 'redirect', excludeId: draft?.id, fromPath, enabled: open && !readOnly })

  const draftRef = useRef(draft)
  draftRef.current = draft
  const doSave = async () => {
    const d = draftRef.current
    if (!d || readOnly || save.isPending) return
    try {
      const saved = await save.mutateAsync(d)
      toast.show({ kind: 'success', title: 'Redirect saved', message: `${redirectName(saved ?? d)} added to pending changes.`, actions: [applyNowAction] })
      onClose()
    } catch (err) {
      if (err instanceof ApiError && err.fields && Object.keys(err.fields).length) setErrors(err.fields)
      else toast.error(err, 'Could not save redirect')
    }
  }
  const saveRef = useRef(doSave)
  saveRef.current = doSave
  const requestSave = useCallback(() => {
    ;(document.activeElement as HTMLElement | null)?.blur?.()
    window.setTimeout(() => void saveRef.current(), 0)
  }, [])
  useEffect(() => {
    if (!open) return
    const h = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 's') {
        e.preventDefault()
        requestSave()
      }
    }
    window.addEventListener('keydown', h)
    return () => window.removeEventListener('keydown', h)
  }, [open, requestSave])

  const notFound = open && !isNew && !current && one.isError
  const { matching, other } = rankCerts(certs, draft?.domains ?? [])
  const certOptions = [
    { value: '', label: 'None — HTTP only' },
    ...matching.map((c) => ({ value: c.id, label: certOptionLabel(c) })),
    ...other.map((c) => ({ value: c.id, label: `${certOptionLabel(c)}${draft?.domains.length ? " · doesn't cover all domains" : ''}` })),
  ]
  const domainErrors = draft
    ? Object.entries(errors).flatMap(([k, v]) => {
        if (k === 'domains') return [v]
        const m = /^domains\.(\d+)$/.exec(k)
        return m ? [`${draft.domains[Number(m[1])] ?? ''} — ${v}`] : []
      })
    : []
  const toErr = errors.to || (draft?.to ? urlError(draft.to) : '')

  return (
    <Drawer
      open={open}
      onClose={onClose}
      title={isNew ? 'New redirect' : `Edit redirect ${current ? redirectName(current) : ''}`}
      subtitle={readOnly ? "Read-only · viewers can't change redirects" : 'Changes apply on save · config validated first'}
      footer={
        <>
          <div className="spacer" />
          <Button size="md" onClick={onClose}>{readOnly ? 'Close' : 'Cancel'}</Button>
          {!readOnly && (
            <Button size="md" variant="primary" loading={save.isPending} disabled={!draft} onClick={requestSave}>
              Save to pending
            </Button>
          )}
        </>
      }
    >
      {notFound ? (
        <EmptyState icon="redirect" title="Redirect not found" description="It may have been deleted." />
      ) : !draft ? (
        <div className="row center" style={{ padding: 48 }}>
          <Spinner large />
        </div>
      ) : (
        <fieldset className="hosts-fieldset" disabled={readOnly}>
          <Field label="Domain names">
            <DomainsInput
              values={draft.domains}
              onChange={(domains) => update({ domains })}
              conflicts={conflicts}
              serverErrors={domainErrors}
              disabled={readOnly}
              autoFocus={isNew}
              emptyPlaceholder="www.example.com"
              hint="Wildcards allowed · press Enter to add"
            />
          </Field>
          <Field label="From path" error={errors.fromPath} hint="Leave empty to redirect every path on these domains">
            <Input mono value={draft.fromPath} placeholder="/ (whole domain)" invalid={!!errors.fromPath} onChange={(e) => update({ fromPath: e.target.value.trim() })} />
          </Field>
          <Field label="Redirect to" error={toErr} hint="Full URL · nginx variables like $host are allowed">
            <Input mono value={draft.to} placeholder="https://example.com" invalid={!!toErr} onChange={(e) => update({ to: e.target.value.trim() })} />
          </Field>
          <Field label="Status code" error={errors.code} hint={CODE_HINTS[draft.code]}>
            <Segmented
              value={String(draft.code)}
              disabled={readOnly}
              onChange={(v) => update({ code: Number(v) as Redirect['code'] })}
              options={['301', '302', '307', '308']}
            />
          </Field>
          <ToggleCard
            title="Keep path"
            description={draft.keepPath ? 'Appends the original path and query ($request_uri)' : 'Always sends visitors to the exact target URL'}
            checked={draft.keepPath}
            disabled={readOnly}
            onChange={(keepPath) => update({ keepPath })}
          />
          <Field label="Certificate" error={errors.certificateId} hint="Needed to answer https:// requests for these domains before redirecting">
            <Select
              value={draft.certificateId ?? ''}
              options={certOptions}
              onChange={(v) => update(v ? { certificateId: v } : { certificateId: undefined, forceHttps: false })}
              aria-label="Certificate"
            />
          </Field>
          <div className="grid-2">
            <ToggleCard
              title="Force HTTPS"
              description="301 from :80 first"
              checked={draft.forceHttps && !!draft.certificateId}
              disabled={readOnly || !draft.certificateId}
              onChange={(forceHttps) => update({ forceHttps })}
            />
            <ToggleCard title="Enabled" description="Disabled redirects aren't served" checked={draft.enabled} disabled={readOnly} onChange={(enabled) => update({ enabled })} />
          </div>
          {errors.forceHttps && <div className="field-error">{errors.forceHttps}</div>}
        </fieldset>
      )}
    </Drawer>
  )
}
