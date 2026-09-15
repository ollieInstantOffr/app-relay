// Owner: slice hosts. Add / edit host drawer (design 03, 18a, 18b, 30d).
import { useCallback, useEffect, useRef, useState } from 'react'
import { ApiError } from '../../lib/api'
import { Badge, Button, Callout, ConfirmDialog, Drawer, EmptyState, Spinner, useToast, type TabDef } from '../../components/ui'
import { useEntities, useEntity, useRole, useSaveEntity, useSettings } from '../../lib/queries'
import { pluralize } from '../../lib/format'
import { PreviewStatus } from './ConfigPreview'
import { DetailsTab } from './tabs/DetailsTab'
import { SSLTab } from './tabs/SSLTab'
import { LocationsTab } from './tabs/LocationsTab'
import { AdvancedTab } from './tabs/AdvancedTab'
import {
  applyNowAction, bestCert, certCoversAll, HOST_TABS, hostToDraft, newHost, tabForField, UNRENDERED_FIELDS, useConfigPreview,
  type HostDraft, type HostTab, type PreviewState,
} from './lib'

export interface HostFormCtx {
  draft: HostDraft
  update: (patch: Partial<HostDraft>) => void
  errors: Record<string, string>
  isNew: boolean
  readOnly: boolean
  preview: PreviewState
}

const TAB_LABELS: Record<HostTab, string> = { details: 'Details', ssl: 'SSL', locations: 'Locations', advanced: 'Advanced' }

export default function HostDrawer({ open, hostId, isNew, initialTab, onClose }: {
  open: boolean
  hostId?: string
  isNew: boolean
  initialTab?: string | null
  onClose: () => void
}) {
  const { canWrite } = useRole()
  const readOnly = !canWrite
  const toast = useToast()
  const hostsQ = useEntities('hosts', { enabled: open })
  const cached = hostId ? hostsQ.data?.find((h) => h.id === hostId) : undefined
  const hostQ = useEntity('hosts', open && hostId && !cached && !hostsQ.isLoading ? hostId : undefined)
  const source = cached ?? hostQ.data
  const general = useSettings('general')
  const certs = useEntities('certificates', { enabled: open }).data
  const save = useSaveEntity('hosts')

  const [draft, setDraft] = useState<HostDraft | null>(null)
  const [baseline, setBaseline] = useState('')
  const [loadedKey, setLoadedKey] = useState('')
  const [tab, setTab] = useState<HostTab>('details')
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [certTouched, setCertTouched] = useState(false)
  const [confirmClose, setConfirmClose] = useState(false)
  const confirmRef = useRef(false)
  confirmRef.current = confirmClose

  const key = open ? (isNew ? 'new' : hostId ?? '') : ''

  // Initialise the draft once per opened host; later refetches don't clobber edits.
  useEffect(() => {
    if (!open) {
      setDraft(null)
      setLoadedKey('')
      return
    }
    if (loadedKey === key) return
    let d: HostDraft | null = null
    if (isNew) {
      if (general.isLoading) return
      d = newHost(general.data?.defaults)
    } else if (source) {
      d = hostToDraft(source)
    }
    if (!d) return
    setDraft(d)
    setBaseline(JSON.stringify(d))
    setLoadedKey(key)
    setErrors({})
    setCertTouched(false)
    setTab(HOST_TABS.includes(initialTab as HostTab) ? (initialTab as HostTab) : 'details')
  }, [open, key, loadedKey, isNew, source, general.isLoading, general.data, initialTab])

  // New hosts: pick the best matching certificate until the user chooses one.
  const domainsKey = draft?.domains.join(',')
  useEffect(() => {
    if (!isNew || !draft || certTouched || !certs) return
    const current = certs.find((c) => c.id === draft.certificateId)
    if (current && certCoversAll(current, draft.domains) && current.status === 'valid') return
    const fallback = general.data?.defaults.certificateId
    const fallbackCert = certs.find((c) => c.id === fallback)
    const next =
      bestCert(certs, draft.domains)?.id ??
      (fallbackCert && (draft.domains.length === 0 || certCoversAll(fallbackCert, draft.domains)) ? fallbackCert.id : undefined)
    if (next !== draft.certificateId) {
      setDraft((d) => (d ? { ...d, certificateId: next } : d))
      setBaseline((b) => {
        // Auto-selection alone doesn't make the form dirty.
        try {
          const parsed = JSON.parse(b) as HostDraft
          return JSON.stringify({ ...parsed, certificateId: next })
        } catch {
          return b
        }
      })
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [isNew, domainsKey, certs, certTouched])

  const update = useCallback((patch: Partial<HostDraft>) => {
    setDraft((d) => (d ? { ...d, ...patch } : d))
    if ('certificateId' in patch) setCertTouched(true)
    const changed = Object.keys(patch)
    setErrors((errs) => {
      const next = Object.fromEntries(Object.entries(errs).filter(([k]) => !changed.some((p) => k === p || k.startsWith(p + '.'))))
      return Object.keys(next).length === Object.keys(errs).length ? errs : next
    })
  }, [])

  const preview = useConfigPreview(draft ?? newHost(), open && !!draft)

  const draftRef = useRef(draft)
  draftRef.current = draft
  const tabRef = useRef(tab)
  tabRef.current = tab

  const doSave = async () => {
    const d = draftRef.current
    if (!d || readOnly || save.isPending) return
    try {
      const saved = await save.mutateAsync(d)
      setBaseline(JSON.stringify(d))
      toast.show({
        kind: 'success',
        title: 'Host saved',
        message: `${saved?.domains?.[0] ?? d.domains[0]} added to pending changes.`,
        actions: [applyNowAction],
      })
      onClose()
    } catch (err) {
      if (err instanceof ApiError && err.fields && Object.keys(err.fields).length > 0) {
        const fields = err.fields
        setErrors(fields)
        const withErrors = HOST_TABS.filter((t) => Object.keys(fields).some((f) => tabForField(f) === t))
        if (withErrors.length && !withErrors.includes(tabRef.current)) setTab(withErrors[0])
        const n = Object.keys(fields).length
        toast.show({ kind: 'error', title: 'Host not saved', message: `Fix the ${pluralize(n, 'highlighted field')} and save again.`, duration: 6000 })
      } else {
        toast.error(err, 'Could not save host')
      }
    }
  }
  const saveRef = useRef(doSave)
  saveRef.current = doSave
  const requestSave = useCallback(() => {
    // Commit half-typed chips/inputs (they commit on blur) before reading the draft.
    ;(document.activeElement as HTMLElement | null)?.blur?.()
    window.setTimeout(() => void saveRef.current(), 0)
  }, [])

  useEffect(() => {
    if (!open) return
    const h = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 's') {
        e.preventDefault()
        if (!confirmRef.current) requestSave()
      }
    }
    window.addEventListener('keydown', h)
    return () => window.removeEventListener('keydown', h)
  }, [open, requestSave])

  const dirty = !!draft && JSON.stringify(draft) !== baseline
  const requestClose = () => {
    if (confirmRef.current) return
    if (dirty && !readOnly) setConfirmClose(true)
    else onClose()
  }

  const notFound = open && !isNew && !source && hostQ.isError
  const title = isNew ? 'New proxy host' : source ? `Edit host ${source.domains[0] ?? ''}` : 'Edit host'
  const tabs: TabDef<HostTab>[] = HOST_TABS.map((t) => {
    const n = Object.keys(errors).filter((f) => tabForField(f) === t).length
    return {
      id: t,
      label: (
        <>
          {TAB_LABELS[t]}
          {n > 0 && <span className="dot danger" aria-label={`${n} errors`} />}
        </>
      ),
      ...(t === 'locations' && draft?.locations.length && !n ? { count: draft.locations.length } : {}),
    }
  })
  const unrendered = Object.entries(errors).filter(([k]) => UNRENDERED_FIELDS.test(k))

  const ctx: HostFormCtx | null = draft ? { draft, update, errors, isNew, readOnly, preview } : null

  return (
    <>
      <Drawer
        open={open}
        onClose={requestClose}
        title={title}
        subtitle={readOnly ? "Read-only · viewers can't change hosts" : 'Changes apply on save · config validated first'}
        tabs={ctx ? tabs : undefined}
        tab={tab}
        onTab={setTab}
        headerExtra={
          source || draft ? (
            <div className="row gap-6">
              {source?.system && <Badge tone="dark">system</Badge>}
              {source && source.source !== 'manual' && <Badge>{source.source}</Badge>}
              {draft && !draft.enabled && <Badge>disabled</Badge>}
              {draft?.maintenance?.enabled && <Badge tone="warn">maintenance</Badge>}
            </div>
          ) : undefined
        }
        footer={
          <>
            {ctx && <PreviewStatus state={preview} />}
            <div className="spacer" />
            <Button size="md" onClick={requestClose}>{readOnly ? 'Close' : 'Cancel'}</Button>
            {!readOnly && (
              <Button size="md" variant="primary" loading={save.isPending} disabled={!ctx || notFound} onClick={requestSave}>
                Save to pending
              </Button>
            )}
          </>
        }
      >
        {notFound ? (
          <EmptyState icon="hosts" title="Host not found" description="It may have been deleted. Close the drawer to go back to the list." />
        ) : !ctx ? (
          <div className="row center" style={{ padding: 48 }}>
            <Spinner large />
          </div>
        ) : (
          <fieldset className="hosts-fieldset" disabled={readOnly}>
            {unrendered.length > 0 && (
              <Callout tone="danger">
                {unrendered.map(([k, v]) => (
                  <div key={k}>{v}</div>
                ))}
              </Callout>
            )}
            {tab === 'details' && <DetailsTab ctx={ctx} />}
            {tab === 'ssl' && <SSLTab ctx={ctx} />}
            {tab === 'locations' && <LocationsTab ctx={ctx} />}
            {tab === 'advanced' && <AdvancedTab ctx={ctx} />}
          </fieldset>
        )}
      </Drawer>
      <ConfirmDialog
        open={confirmClose}
        onClose={() => setConfirmClose(false)}
        onConfirm={() => onClose()}
        danger
        title="Discard unsaved changes?"
        message={isNew ? "This host hasn't been saved yet." : "Your edits to this host haven't been saved."}
        confirmLabel="Discard changes"
      />
    </>
  )
}
