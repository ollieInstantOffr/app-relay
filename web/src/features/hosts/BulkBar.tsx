// Owner: slice hosts. Floating bulk action bar (design 27).
import { useState } from 'react'
import { Button, Callout, ConfirmDialog, IconButton, Menu, useToast, type MenuEntry } from '../../components/ui'
import { api } from '../../lib/api'
import { keys, useEntities } from '../../lib/queries'
import { useQueryClient } from '@tanstack/react-query'
import { pluralize } from '../../lib/format'
import type { ProxyHost } from '../../lib/types'
import { applyNowAction, certCoversAll, certOptionLabel, useInvalidateHosts, type BulkAction, type BulkResult } from './lib'

const VERB: Record<BulkAction, string> = {
  enable: 'enabled',
  disable: 'disabled',
  delete: 'deleted',
  attach_access_list: 'updated',
  set_certificate: 'updated',
}

export function BulkBar({ ids, hosts, onClear, deleteOpen, setDeleteOpen }: {
  ids: string[]
  hosts: ProxyHost[]
  onClear: () => void
  deleteOpen: boolean
  setDeleteOpen: (v: boolean) => void
}) {
  const toast = useToast()
  const qc = useQueryClient()
  const invalidate = useInvalidateHosts()
  const lists = useEntities('access-lists').data ?? []
  const certs = useEntities('certificates').data ?? []
  const [busy, setBusy] = useState<BulkAction | null>(null)
  const selectedHosts = hosts.filter((h) => ids.includes(h.id))
  const systemHosts = selectedHosts.filter((h) => h.system)

  const run = async (action: BulkAction, value = '', label?: string) => {
    setBusy(action)
    try {
      const res = await api.post<BulkResult>('/api/hosts/bulk', { ids, action, value })
      invalidate()
      qc.invalidateQueries({ queryKey: keys.health })
      const verb = VERB[action]
      if (res.failed === 0) {
        toast.show({
          kind: 'success',
          title: `${pluralize(res.ok, 'host')} ${verb}`,
          message: label ? `${label} · added to pending changes.` : 'Added to pending changes.',
          actions: [applyNowAction],
        })
      } else {
        const failures = res.results.filter((r) => !r.ok)
        toast.show({
          kind: 'error',
          title: `${res.failed} of ${res.results.length} hosts couldn't be ${verb}`,
          message:
            failures.slice(0, 3).map((f) => `${f.name || f.id}: ${f.error}`).join(' · ') +
            (failures.length > 3 ? ` · +${failures.length - 3} more` : ''),
          actions: res.ok > 0 ? [applyNowAction] : undefined,
        })
      }
      if (action === 'delete') onClear()
    } catch (err) {
      toast.error(err, 'Bulk action failed')
    } finally {
      setBusy(null)
    }
  }

  const domains = [...new Set(selectedHosts.flatMap((h) => h.domains))]
  const sortedCerts = [...certs].sort(
    (a, b) => Number(certCoversAll(b, domains)) - Number(certCoversAll(a, domains)) || a.name.localeCompare(b.name),
  )
  const listItems: MenuEntry[] = [
    { header: 'Access list' },
    { label: 'None — public', onSelect: () => void run('attach_access_list', '', 'Access list removed') },
    ...lists.map((l) => ({ label: l.name, onSelect: () => void run('attach_access_list', l.id, `Access list ${l.name}`) })),
  ]
  const certItems: MenuEntry[] = [
    { header: 'Certificate' },
    { label: 'None — HTTP only', onSelect: () => void run('set_certificate', '', 'Certificate removed') },
    ...sortedCerts.map((c) => ({
      label: certCoversAll(c, domains) ? certOptionLabel(c) : `${certOptionLabel(c)} · doesn't cover all`,
      onSelect: () => void run('set_certificate', c.id, `Certificate ${c.name}`),
    })),
  ]

  return (
    <>
      <div className="hosts-bulkbar" role="toolbar" aria-label="Bulk actions">
        <span className="count">{ids.length} selected</span>
        <span className="sep" />
        <Menu align="start" trigger={<Button size="sm" variant="on-dark" iconRight="chevron" loading={busy === 'attach_access_list'}>Attach access list</Button>} items={listItems} />
        <Menu align="start" trigger={<Button size="sm" variant="on-dark" iconRight="chevron" loading={busy === 'set_certificate'}>Change certificate</Button>} items={certItems} />
        <Button size="sm" variant="on-dark" loading={busy === 'enable'} onClick={() => void run('enable')}>Enable</Button>
        <Button size="sm" variant="on-dark" loading={busy === 'disable'} onClick={() => void run('disable')}>Disable</Button>
        <Button size="sm" variant="on-dark" className="danger" loading={busy === 'delete'} onClick={() => setDeleteOpen(true)}>Delete…</Button>
        <span className="sep" />
        <IconButton icon="close" label="Clear selection" onClick={onClear} />
      </div>
      <ConfirmDialog
        open={deleteOpen}
        onClose={() => setDeleteOpen(false)}
        danger
        title={`Delete ${pluralize(ids.length, 'host')}?`}
        message="They are removed on next apply. Traffic to these domains gets the default-host response."
        confirmLabel={`Delete ${pluralize(ids.length - systemHosts.length, 'host')}`}
        onConfirm={() => run('delete')}
      >
        <pre className="code" style={{ maxHeight: 160 }}>{selectedHosts.map((h) => h.domains[0]).join('\n')}</pre>
        {systemHosts.length > 0 && (
          <Callout tone="warn">
            {systemHosts.map((h) => h.domains[0]).join(', ')} is Relay's admin UI host and will be skipped.
          </Callout>
        )}
      </ConfirmDialog>
    </>
  )
}
