// Owner: slice ops. Settings → Backup & restore (design 16c).
import { useEffect, useRef, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { useSearchParams } from 'react-router-dom'
import { api, errorMessage } from '../../lib/api'
import { useRole, useSaveSettings, useSettings } from '../../lib/queries'
import type { BackupSettings as BackupSettingsT } from '../../lib/types'
import { bytes, dateTime } from '../../lib/format'
import {
  Button, Callout, Card, Dialog, Field, IconButton, Input, Menu, PasswordInput, SectionHeader, Skeleton, Spinner, Toggle, Tooltip, useToast, ConfirmDialog,
} from '../../components/ui'
import NpmImportWizard from '../docker/NpmImportWizard'
import { applyNowAction, opsKeys, until, useBackups, type BackupRow, type RestoreResult } from '../docker/ops'
import '../docker/ops.css'

const TRIGGER_LABEL: Record<string, string> = {
  scheduled: 'Scheduled',
  manual: 'Manual',
  'before-restore': 'Before restore',
  'before-upgrade': 'Before upgrade',
}

function contents(c: Record<string, number>): string {
  const p = (n: number | undefined, one: string, many: string) => `${n ?? 0} ${n === 1 ? one : many}`
  return `${p(c.hosts, 'host', 'hosts')} · ${p(c.backends, 'backend', 'backends')} · ${p(c.certificates, 'cert', 'certs')}`
}

export default function BackupSettings() {
  const qc = useQueryClient()
  const toast = useToast()
  const { isAdmin } = useRole()
  const settings = useSettings('backup')
  const save = useSaveSettings('backup')
  const backups = useBackups()
  const [params, setParams] = useSearchParams()

  const [scheduleOpen, setScheduleOpen] = useState(false)
  const [schedule, setSchedule] = useState({ time: '03:00', keep: 14 })
  const [passOpen, setPassOpen] = useState(false)
  const [pass, setPass] = useState({ a: '', b: '' })
  const [creating, setCreating] = useState(false)
  const [restore, setRestore] = useState<{ backup?: BackupRow; file?: File } | null>(null)
  const [restorePass, setRestorePass] = useState('')
  const [restoring, setRestoring] = useState(false)
  const [restoreError, setRestoreError] = useState('')
  const [deleting, setDeleting] = useState<BackupRow | null>(null)
  const [drag, setDrag] = useState(false)
  const fileRef = useRef<HTMLInputElement>(null)
  const importOpen = params.get('import') === 'npm'

  useEffect(() => {
    if (restore) {
      setRestorePass('')
      setRestoreError('')
    }
  }, [restore])

  if (!settings.data) {
    return (
      <>
        <SectionHeader title="Backup & restore" description="Hosts, backends, access lists, certificates and users in one encrypted archive." />
        <Skeleton height={260} />
      </>
    )
  }
  const s = settings.data
  const status = backups.data?.status
  const items = backups.data?.items ?? []

  const update = async (patch: Partial<BackupSettingsT>, success?: string) => {
    try {
      await save.mutateAsync({ ...s, ...patch })
      qc.invalidateQueries({ queryKey: opsKeys.backups })
      if (success) toast.success(success)
      return true
    } catch (err) {
      toast.error(err, 'Could not save backup settings')
      return false
    }
  }

  const backupNow = async () => {
    setCreating(true)
    try {
      const row = await api.post<BackupRow>('/api/backups')
      qc.invalidateQueries({ queryKey: opsKeys.backups })
      toast.success('Backup created', `${row.file} · ${bytes(row.size)}`)
    } catch (err) {
      toast.error(err, 'Backup failed')
      qc.invalidateQueries({ queryKey: opsKeys.backups })
    } finally {
      setCreating(false)
    }
  }

  const doRestore = async () => {
    if (!restore) return
    setRestoring(true)
    setRestoreError('')
    try {
      let res: RestoreResult
      if (restore.backup) {
        res = await api.post<RestoreResult>(`/api/backups/${restore.backup.id}/restore`, restorePass ? { passphrase: restorePass } : {})
      } else {
        const form = new FormData()
        form.append('file', restore.file!)
        form.append('passphrase', restorePass)
        res = await api.upload<RestoreResult>('/api/backups/restore', form)
      }
      setRestore(null)
      if (!res.sessionKept) {
        window.location.assign('/')
        return
      }
      qc.invalidateQueries()
      toast.show({
        kind: 'success',
        title: 'Backup restored',
        message: `${contents(res.manifest.counts)} from ${dateTime(res.manifest.created)}. Review the pending changes, then apply. The previous state was saved as a “before restore” backup.`,
        actions: [applyNowAction],
        duration: 0,
      })
    } catch (err) {
      setRestoreError(errorMessage(err))
    } finally {
      setRestoring(false)
    }
  }

  const pickFile = (f: File | undefined) => {
    if (!f) return
    if (!f.name.endsWith('.age')) {
      toast.show({ kind: 'error', title: 'Not a Relay backup', message: 'Choose a .relay.age archive.' })
      return
    }
    setRestore({ file: f })
  }

  const last = status?.lastRun
  const scheduleSub = !s.enabled
    ? 'Scheduled backups are off'
    : [
        status?.nextRunAt ? `Next run ${until(status.nextRunAt)}` : null,
        last
          ? last.status === 'ok'
            ? `last run succeeded (${bytes(last.size)})`
            : last.status === 'failed'
              ? `last run failed: ${last.error}`
              : 'running now'
          : 'no scheduled backup yet',
      ]
        .filter(Boolean)
        .join(' · ')

  const dest = status?.destination
  const encryption = s.passphraseSet
    ? `age · passphrase set · ${s.includePrivateKeys ? 'includes private keys' : 'without private keys'}`
    : 'age · no passphrase set'

  return (
    <>
      <SectionHeader title="Backup & restore" description="Hosts, backends, access lists, certificates and users in one encrypted archive." />

      {status?.warning && (
        <Callout
          tone="warn"
          actions={isAdmin && !s.passphraseSet ? <Button size="sm" onClick={() => { setPass({ a: '', b: '' }); setPassOpen(true) }}>Set passphrase</Button> : undefined}
        >
          {status.warning}
        </Callout>
      )}

      <Card title="Scheduled backups">
        <div className="toggle-row" style={{ gap: 12 }}>
          <div className="grow">
            <div className="toggle-title">Daily at {s.time} · keep {s.keep}</div>
            <div className="toggle-desc" style={{ color: last?.status === 'failed' && s.enabled ? 'var(--danger-text)' : 'var(--ink-faint)' }}>{scheduleSub}</div>
          </div>
          {isAdmin && (
            <Button size="sm" onClick={() => { setSchedule({ time: s.time, keep: s.keep }); setScheduleOpen(true) }}>Edit schedule</Button>
          )}
          <Toggle checked={s.enabled} disabled={!isAdmin} label="Scheduled backups" onChange={(v) => update({ enabled: v })} />
        </div>
        <div className="toggle-row" style={{ gap: 12 }}>
          <div className="grow">
            <div className="toggle-title">Destination</div>
            <div className="toggle-desc mono" style={{ color: 'var(--ink-faint)', marginTop: 2 }}>{dest?.path ?? '/data/backups'} · local disk</div>
          </div>
          {dest &&
            (dest.writable ? (
              <span className="row gap-6 small" style={{ color: 'var(--ok-text)' }}>
                <span className="ops-dot ok" style={{ width: 6, height: 6 }} />
                {dest.freeBytes !== undefined ? `${bytes(dest.freeBytes)} free` : 'writable'}
              </span>
            ) : (
              <span className="row gap-6 small danger-text">
                <span className="ops-dot danger" style={{ width: 6, height: 6 }} />
                not writable
              </span>
            ))}
        </div>
        <div className="toggle-row" style={{ gap: 12 }}>
          <div className="grow">
            <div className="toggle-title">Encryption</div>
            <div className="toggle-desc" style={{ color: s.passphraseSet ? 'var(--ink-faint)' : 'var(--warn-text)' }}>{encryption}</div>
          </div>
          {isAdmin && (
            <Button size="sm" variant="ghost" onClick={() => { setPass({ a: '', b: '' }); setPassOpen(true) }}>
              {s.passphraseSet ? 'Change passphrase' : 'Set passphrase'}
            </Button>
          )}
        </div>
        <div className="toggle-row" style={{ gap: 12 }}>
          <div className="grow">
            <div className="toggle-title">Include private keys</div>
            <div className="toggle-desc" style={{ color: 'var(--ink-faint)' }}>Without them, restored certificates must be issued again</div>
          </div>
          <Toggle checked={s.includePrivateKeys} disabled={!isAdmin} label="Include private keys" onChange={(v) => update({ includePrivateKeys: v })} />
        </div>
      </Card>

      <Card
        title="Snapshots"
        actions={
          isAdmin && (
            s.passphraseSet ? (
              <Button size="sm" variant="primary" loading={creating} onClick={backupNow}>Back up now</Button>
            ) : (
              <Tooltip content="Set an encryption passphrase first">
                <Button size="sm" variant="primary" disabled>Back up now</Button>
              </Tooltip>
            )
          )
        }
      >
        <div className="ops-snap-head">
          <span>Created</span>
          <span>Size</span>
          <span>Contents</span>
          <span>Trigger</span>
          <span />
        </div>
        {backups.isLoading ? (
          <div className="card-body"><Skeleton height={48} /></div>
        ) : items.length === 0 ? (
          <div className="ops-snap-row" style={{ gridTemplateColumns: '1fr' }}>
            <span className="muted">No backups yet{s.passphraseSet ? ' — create one with “Back up now”.' : '. Set a passphrase to start backing up.'}</span>
          </div>
        ) : (
          items.map((b) => (
            <div key={b.id} className="ops-snap-row">
              <span className="mono" title={b.file}>{dateTime(b.createdAt)}</span>
              <span style={{ color: 'var(--ink-muted)' }}>{b.status === 'ok' ? bytes(b.size) : '—'}</span>
              <span style={{ color: 'var(--ink-muted)' }}>
                {b.status === 'running' ? (
                  <span className="row gap-6"><Spinner /> writing…</span>
                ) : b.status === 'failed' ? (
                  <span className="danger-text truncate" title={b.error}>Failed · {b.error}</span>
                ) : (
                  contents(b.contents)
                )}
              </span>
              <span className="faint">{TRIGGER_LABEL[b.trigger] ?? b.trigger}</span>
              <span className="ops-actions">
                {b.status === 'ok' && isAdmin && (
                  <>
                    <Button size="sm" variant="ghost" style={{ color: 'var(--ink)' }} onClick={() => setRestore({ backup: b })}>Restore</Button>
                    <a className="btn btn-ghost btn-sm" href={`/api/backups/${b.id}/download`} download={b.file}>Download</a>
                  </>
                )}
                {isAdmin && b.status !== 'running' && (
                  <Menu
                    trigger={<IconButton icon="more" bare label="Backup actions" />}
                    items={[{ label: 'Delete', icon: 'trash', danger: true, onSelect: () => setDeleting(b) }]}
                  />
                )}
              </span>
            </div>
          ))
        )}
      </Card>

      <div className="ops-two-col">
        <div
          className={`ops-dropzone${drag ? ' drag' : ''}${isAdmin ? '' : ' disabled'}`}
          role="button"
          tabIndex={0}
          onClick={() => isAdmin && fileRef.current?.click()}
          onKeyDown={(e) => e.key === 'Enter' && isAdmin && fileRef.current?.click()}
          onDragOver={(e) => {
            e.preventDefault()
            if (isAdmin) setDrag(true)
          }}
          onDragLeave={() => setDrag(false)}
          onDrop={(e) => {
            e.preventDefault()
            setDrag(false)
            if (isAdmin) pickFile(e.dataTransfer.files[0])
          }}
        >
          <div className="medium">Restore from file</div>
          <div className="small faint">Drop a <span className="mono">.relay.age</span> archive or browse</div>
          <input ref={fileRef} type="file" hidden accept=".age" onChange={(e) => { pickFile(e.target.files?.[0]); e.target.value = '' }} />
        </div>
        <div className="ops-tile">
          <div className="medium">Import from Nginx Proxy Manager</div>
          <div className="desc">Point at an NPM data folder or SQLite DB. Hosts, certs and access lists are converted; nothing is changed until you confirm.</div>
          <div>
            <Button size="sm" variant="link" disabled={!isAdmin} style={{ textDecoration: 'none', marginTop: 4 }} onClick={() => setParams({ import: 'npm' })}>Start import →</Button>
          </div>
        </div>
      </div>

      {/* Edit schedule */}
      <Dialog
        open={scheduleOpen}
        onClose={() => setScheduleOpen(false)}
        title="Backup schedule"
        description={`Runs daily in the ${status?.timezone ?? 'server'} timezone. Manual and before-restore backups are kept separately (newest 10 each).`}
        width={440}
        footer={
          <>
            <Button onClick={() => setScheduleOpen(false)}>Cancel</Button>
            <Button
              variant="primary"
              loading={save.isPending}
              disabled={!/^\d{2}:\d{2}$/.test(schedule.time) || schedule.keep < 1 || schedule.keep > 365}
              onClick={async () => {
                if (await update({ time: schedule.time, keep: schedule.keep, enabled: true }, 'Schedule saved')) setScheduleOpen(false)
              }}
            >
              Save schedule
            </Button>
          </>
        }
      >
        <div className="grid-2">
          <Field label="Time of day">
            <Input type="time" mono value={schedule.time} onChange={(e) => setSchedule({ ...schedule, time: e.target.value })} />
          </Field>
          <Field label="Keep newest" hint="Scheduled backups">
            <Input type="number" mono min={1} max={365} value={schedule.keep} onChange={(e) => setSchedule({ ...schedule, keep: Number(e.target.value) })} />
          </Field>
        </div>
      </Dialog>

      {/* Passphrase */}
      <Dialog
        open={passOpen}
        onClose={() => setPassOpen(false)}
        title={s.passphraseSet ? 'Change backup passphrase' : 'Set backup passphrase'}
        description="Archives are encrypted with age. Without the passphrase a backup cannot be restored — store it in your password manager."
        width={480}
        footer={
          <>
            <Button onClick={() => setPassOpen(false)}>Cancel</Button>
            <Button
              variant="primary"
              loading={save.isPending}
              disabled={pass.a.length < 8 || pass.a !== pass.b}
              onClick={async () => {
                if (await update({ passphrase: pass.a }, 'Passphrase saved')) setPassOpen(false)
              }}
            >
              Save passphrase
            </Button>
          </>
        }
      >
        <div className="col gap-14">
          {s.passphraseSet && <Callout tone="warn">Existing backups keep the passphrase they were created with.</Callout>}
          <Field label="New passphrase" hint="At least 8 characters">
            <PasswordInput autoFocus autoComplete="new-password" value={pass.a} onChange={(e) => setPass({ ...pass, a: e.target.value })} />
          </Field>
          <Field label="Repeat passphrase" error={pass.b && pass.a !== pass.b ? 'Passphrases do not match' : undefined}>
            <PasswordInput autoComplete="new-password" value={pass.b} invalid={!!pass.b && pass.a !== pass.b} onChange={(e) => setPass({ ...pass, b: e.target.value })} />
          </Field>
        </div>
      </Dialog>

      {/* Restore */}
      <Dialog
        open={!!restore}
        onClose={() => !restoring && setRestore(null)}
        icon="rollback"
        iconTone="warn"
        title={restore?.backup ? `Restore backup from ${dateTime(restore.backup.createdAt)}?` : `Restore ${restore?.file?.name ?? 'backup'}?`}
        description="Relay first saves the current state as a “before restore” backup, then replaces hosts, backends, certificates, users and settings. The restored configuration becomes pending until you apply it."
        width={520}
        footer={
          <>
            <Button onClick={() => setRestore(null)} disabled={restoring}>Cancel</Button>
            <Button variant="danger" loading={restoring} disabled={!restorePass && (!!restore?.file || !s.passphraseSet)} onClick={doRestore}>Restore</Button>
          </>
        }
      >
        <div className="col gap-12">
          {restore?.backup && <div className="small muted">{contents(restore.backup.contents)} · {bytes(restore.backup.size)} · {TRIGGER_LABEL[restore.backup.trigger] ?? restore.backup.trigger}</div>}
          <Field
            label="Passphrase"
            hint={restore?.backup && s.passphraseSet ? 'Leave empty to use the current passphrase' : 'The passphrase this archive was encrypted with'}
            error={restoreError || undefined}
          >
            <PasswordInput autoFocus value={restorePass} invalid={!!restoreError} onChange={(e) => setRestorePass(e.target.value)} onKeyDown={(e) => e.key === 'Enter' && doRestore()} />
          </Field>
        </div>
      </Dialog>

      <ConfirmDialog
        open={!!deleting}
        onClose={() => setDeleting(null)}
        danger
        title="Delete backup?"
        message={deleting ? `${deleting.file} will be removed from ${dest?.path ?? 'the backup folder'}. This cannot be undone.` : undefined}
        confirmLabel="Delete backup"
        onConfirm={async () => {
          if (!deleting) return
          try {
            await api.del(`/api/backups/${deleting.id}`)
            qc.invalidateQueries({ queryKey: opsKeys.backups })
            toast.success('Backup deleted')
          } catch (err) {
            toast.error(err, 'Could not delete backup')
          }
        }}
      />

      <NpmImportWizard
        open={importOpen}
        onClose={() => {
          const next = new URLSearchParams(params)
          next.delete('import')
          setParams(next, { replace: true })
        }}
      />
    </>
  )
}
