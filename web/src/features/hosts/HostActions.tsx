// Owner: slice hosts. Host menu actions (design 23 host menu) and the delete dialog with undo.
import { useCallback, useEffect, useState } from 'react'
import { useNavigate, useSearchParams } from 'react-router-dom'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Button, Callout, Checkbox, Dialog, Input, useToast, type MenuEntry } from '../../components/ui'
import { api, errorMessage } from '../../lib/api'
import { keys, useEntities, useRole, useSaveEntity } from '../../lib/queries'
import { pluralize, upstreamUrl } from '../../lib/format'
import type { HealthStatus, ProxyHost } from '../../lib/types'
import { applyNowAction, hostToDraft, openableDomain, probeMessage, useInvalidateHosts, type HostTab, type HostUsage } from './lib'
import { checkDNS, deleteDNSRecord, dnsKeys, usePublicDNS } from '../dns/dnsApi'
import type { DNSRecord } from '../../lib/types'

export interface HostActions {
  edit: (h: ProxyHost, tab?: HostTab) => void
  duplicate: (h: ProxyHost) => Promise<void>
  toggle: (h: ProxyHost) => Promise<void>
  toggleMaintenance: (h: ProxyHost) => Promise<void>
  probe: (h: ProxyHost) => Promise<void>
  requestDelete: (h: ProxyHost) => void
  menuItems: (h: ProxyHost) => MenuEntry[]
  dialogs: React.ReactNode
}

export function useHostActions(): HostActions {
  const navigate = useNavigate()
  const [, setParams] = useSearchParams()
  const toast = useToast()
  const qc = useQueryClient()
  const invalidate = useInvalidateHosts()
  const { canWrite } = useRole()
  const { mutateAsync: saveHost } = useSaveEntity('hosts')
  const [deleting, setDeleting] = useState<ProxyHost | null>(null)

  const edit = useCallback(
    (h: ProxyHost, tab?: HostTab) =>
      setParams((p) => {
        p.delete('new')
        p.set('edit', h.id)
        if (tab && tab !== 'details') p.set('tab', tab)
        else p.delete('tab')
        return p
      }),
    [setParams],
  )

  const duplicate = useCallback(
    async (h: ProxyHost) => {
      try {
        const cp = await api.post<ProxyHost>(`/api/hosts/${h.id}/duplicate`)
        invalidate()
        toast.show({ kind: 'success', title: 'Host duplicated', message: `${cp.domains[0]} was created disabled — set its domains, then enable it.` })
        edit(cp)
      } catch (err) {
        toast.error(err, 'Could not duplicate host')
      }
    },
    [invalidate, toast, edit],
  )

  const toggle = useCallback(
    async (h: ProxyHost) => {
      try {
        await api.post<ProxyHost>(`/api/hosts/${h.id}/toggle`, { enabled: !h.enabled })
        invalidate()
        toast.show({
          kind: 'success',
          title: h.enabled ? 'Host disabled' : 'Host enabled',
          message: `${h.domains[0]} added to pending changes.`,
          actions: [applyNowAction],
        })
      } catch (err) {
        toast.error(err, h.enabled ? 'Could not disable host' : 'Could not enable host')
      }
    },
    [invalidate, toast],
  )

  const toggleMaintenance = useCallback(
    async (h: ProxyHost) => {
      const start = !h.maintenance?.enabled
      const d = hostToDraft(h)
      try {
        await saveHost({ ...d, maintenance: { ...d.maintenance, enabled: start } })
        invalidate()
        toast.show({
          kind: 'success',
          title: start ? 'Maintenance started' : 'Maintenance ended',
          message: `${h.domains[0]} added to pending changes.`,
          actions: [applyNowAction],
        })
      } catch (err) {
        toast.error(err, start ? 'Could not start maintenance' : 'Could not end maintenance')
      }
    },
    [saveHost, invalidate, toast],
  )

  const probe = useCallback(
    async (h: ProxyHost) => {
      const target = upstreamUrl(h.upstream)
      const id = toast.show({ kind: 'progress', title: 'Testing upstream…', message: target })
      try {
        const s = await api.post<HealthStatus>('/api/health/probe', { upstream: h.upstream })
        const m = probeMessage(s)
        toast.update(id, {
          kind: m.tone === 'ok' ? 'success' : m.tone === 'warn' ? 'warning' : 'error',
          title: m.text,
          message: `${h.domains[0]} → ${target}`,
          duration: m.tone === 'danger' ? 0 : 6000,
        })
        qc.invalidateQueries({ queryKey: keys.health })
      } catch (err) {
        toast.update(id, { kind: 'error', title: 'Upstream test failed', message: errorMessage(err), duration: 0 })
      }
    },
    [toast, qc],
  )

  const menuItems = useCallback(
    (h: ProxyHost): MenuEntry[] => {
      const domain = openableDomain(h)
      const items: MenuEntry[] = [
        { header: h.domains[0] },
        { label: canWrite ? 'Edit' : 'View', icon: 'edit', shortcut: 'E', onSelect: () => edit(h) },
        {
          label: 'Open in new tab',
          icon: 'external',
          shortcut: '↗',
          disabled: !domain,
          onSelect: () => domain && window.open(`${h.certificateId ? 'https' : 'http'}://${domain}`, '_blank', 'noopener'),
        },
      ]
      if (canWrite) items.push({ label: 'Test upstream now', icon: 'reload', disabled: !!h.upstream.backendId && !h.upstream.host, onSelect: () => void probe(h) })
      items.push({ label: 'View logs for host', icon: 'logs', onSelect: () => navigate(`/logs/access?host=${encodeURIComponent(h.domains[0] ?? '')}`) })
      items.push({ label: 'View traffic flow', icon: 'topology', onSelect: () => navigate(`/topology/hosts/${h.id}`) })
      if (canWrite) {
        items.push(
          'separator',
          { label: 'Duplicate', icon: 'copy', onSelect: () => void duplicate(h) },
          { label: h.enabled ? 'Disable' : 'Enable', icon: 'power', disabled: h.system && h.enabled, onSelect: () => void toggle(h) },
          {
            label: h.maintenance?.enabled ? 'End maintenance' : 'Start maintenance',
            icon: 'drain',
            disabled: h.system && !h.maintenance?.enabled,
            onSelect: () => void toggleMaintenance(h),
          },
          { label: 'Delete…', icon: 'trash', shortcut: '⌫', danger: true, disabled: h.system, onSelect: () => setDeleting(h) },
        )
      }
      return items
    },
    [canWrite, edit, probe, navigate, duplicate, toggle, toggleMaintenance],
  )

  return {
    edit,
    duplicate,
    toggle,
    toggleMaintenance,
    probe,
    requestDelete: (h) => {
      if (canWrite && !h.system) setDeleting(h)
    },
    menuItems,
    dialogs: <DeleteHostDialog host={deleting} onClose={() => setDeleting(null)} />,
  }
}

function defaultResponse(u: HostUsage): string {
  switch (u.defaultHostAction) {
    case 'close': return '444'
    case '404': return 'Relay 404 page'
    case 'redirect': return u.defaultHostTarget ? `redirect to ${u.defaultHostTarget}` : 'redirect'
    case 'host': return u.defaultHostTarget ? `served by ${u.defaultHostTarget}` : 'default host'
  }
  return u.defaultHostAction
}

export function DeleteHostDialog({ host, onClose }: { host: ProxyHost | null; onClose: () => void }) {
  const open = !!host
  const toast = useToast()
  const qc = useQueryClient()
  const invalidate = useInvalidateHosts()
  const certs = useEntities('certificates', { enabled: open }).data
  const usageQ = useQuery({
    queryKey: ['hosts', 'usage', host?.id ?? ''],
    queryFn: () => api.get<HostUsage>(`/api/hosts/${host!.id}/usage`),
    enabled: open,
    staleTime: 0,
    retry: false,
  })
  const [deleteCert, setDeleteCert] = useState(false)
  const [deleteDNS, setDeleteDNS] = useState(false)
  const [typed, setTyped] = useState('')
  const [busy, setBusy] = useState(false)
  const publicDNS = usePublicDNS().enabled

  useEffect(() => {
    setDeleteCert(false)
    setDeleteDNS(false)
    setTyped('')
    setBusy(false)
  }, [host?.id])

  if (!host) return null
  const domain = host.domains[0] ?? ''
  const u = usageQ.data
  const locs = host.locations?.length ?? 0
  const certName = u?.certificateName || certs?.find((c) => c.id === host.certificateId)?.name || ''
  const certUnused = !!host.certificateId && !!u && u.certificateSharedWith.length === 0 && !!certName
  const needTyping = locs > 0 || !!host.certificateId
  const blocked = !!u?.isDefaultHost
  const ok = !blocked && (!needTyping || typed.trim().toLowerCase() === domain)

  const submit = async () => {
    setBusy(true)
    const withCert = deleteCert && certUnused
    try {
      await api.del(`/api/hosts/${host.id}${withCert ? '?deleteCertificate=1' : ''}`)
    } catch (err) {
      setBusy(false)
      toast.error(err, 'Could not delete host')
      return
    }
    invalidate()
    if (withCert) qc.invalidateQueries({ queryKey: keys.entities('certificates') })
    onClose()
    const snapshot = host
    if (deleteDNS && publicDNS) void removeDNSRecords(snapshot)
    toast.show({
      kind: 'undo',
      title: 'Host deleted',
      message: withCert ? `${domain} and certificate ${certName} removed on next apply.` : `${domain} removed on next apply.`,
      duration: 5000,
      actions: [
        {
          label: 'Undo',
          onClick: async () => {
            const { id: _id, createdAt: _c, updatedAt: _u, ...rest } = snapshot
            void _id
            void _c
            void _u
            const body = withCert ? { ...rest, certificateId: '', forceHttps: false } : rest
            try {
              await api.post<ProxyHost>('/api/hosts', body)
              invalidate()
              toast.show({
                kind: 'success',
                title: 'Host restored',
                message: withCert ? `${domain} is back, without its deleted certificate (HTTP only).` : `${domain} is back in pending changes.`,
              })
            } catch (err) {
              toast.error(err, 'Could not restore host')
            }
          },
        },
      ],
    })
  }

  /** Deletes records that point to Relay for the host's domains. Never conflicts, shared wildcards or records other hosts use. */
  const removeDNSRecords = async (h: ProxyHost) => {
    const domains = h.domains.filter(Boolean)
    if (!domains.length) return
    try {
      const results = await checkDNS(domains)
      const seen = new Set<string>()
      const targets: { zone: string; rec: DNSRecord }[] = []
      for (const r of results) {
        if (!r.zone || r.status === 'conflict') continue
        for (const rec of r.relayRecords ?? []) {
          const key = `${r.zone}/${rec.id}`
          if (seen.has(key)) continue
          if (rec.fqdn.startsWith('*.') && rec.fqdn !== r.domain) continue
          if (rec.hosts?.some((id) => id !== h.id)) continue
          seen.add(key)
          targets.push({ zone: r.zone, rec })
        }
      }
      if (!targets.length) {
        toast.show({ kind: 'info', title: 'No DNS records to delete', message: 'None of its records pointed to Relay.' })
        return
      }
      const done: string[] = []
      const failed: string[] = []
      for (const t of targets) {
        try {
          await deleteDNSRecord(t.zone, t.rec.id)
          done.push(`${t.rec.type} ${t.rec.fqdn}`)
        } catch (err) {
          failed.push(`${t.rec.fqdn}: ${errorMessage(err)}`)
        }
      }
      qc.invalidateQueries({ queryKey: dnsKeys.all })
      if (failed.length) {
        toast.show({ kind: 'error', title: done.length ? `Deleted ${pluralize(done.length, 'DNS record')}, ${failed.length} failed` : 'Could not delete DNS records', message: failed.join(' · ') })
      } else {
        toast.success(`Deleted ${pluralize(done.length, 'DNS record')}`, done.join(' · '))
      }
    } catch (err) {
      toast.error(err, 'Could not delete DNS records')
    }
  }

  return (
    <Dialog
      open={open}
      onClose={onClose}
      icon="trash"
      iconTone="danger"
      title={`Delete ${domain}?`}
      description={
        u ? (
          <>
            The host{locs > 0 ? ` and its ${pluralize(locs, 'location')} are` : ' is'} removed on next apply. Traffic to this domain will get the
            default-host response ({defaultResponse(u)}).
          </>
        ) : usageQ.isError ? (
          <>The host{locs > 0 ? ` and its ${pluralize(locs, 'location')} are` : ' is'} removed on next apply.</>
        ) : (
          'Checking what depends on this host…'
        )
      }
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="danger" disabled={!ok || (!u && !usageQ.isError)} loading={busy} onClick={submit}>
            Delete host
          </Button>
        </>
      }
    >
      {blocked && (
        <Callout tone="danger">
          This host is the default host for unknown domains. Pick another default in Proxy hosts → Default before deleting it.
        </Callout>
      )}
      {certUnused && (
        <Checkbox
          checked={deleteCert}
          onChange={setDeleteCert}
          label={
            <span>
              Its certificate <span className="mono">{certName}</span> is used by nothing else. Delete it too
            </span>
          }
        />
      )}
      {publicDNS && (
        <Checkbox
          checked={deleteDNS}
          onChange={setDeleteDNS}
          label={
            <span>
              Also delete its DNS records <span className="muted">· only records that point to Relay, removed right away</span>
            </span>
          }
        />
      )}
      {host.certificateId && u && u.certificateSharedWith.length > 0 && (
        <div className="small muted">
          Its certificate <span className="mono">{certName || host.certificateId}</span> stays — also used by {u.certificateSharedWith.slice(0, 3).join(', ')}
          {u.certificateSharedWith.length > 3 ? ` +${u.certificateSharedWith.length - 3}` : ''}.
        </div>
      )}
      {needTyping && !blocked && (
        <div className="field">
          <label className="field-label" htmlFor="hosts-delete-confirm">Type the domain to confirm</label>
          <Input
            id="hosts-delete-confirm"
            mono
            autoFocus
            value={typed}
            placeholder={domain}
            onChange={(e) => setTyped(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && ok && !busy) void submit()
            }}
          />
        </div>
      )}
    </Dialog>
  )
}
