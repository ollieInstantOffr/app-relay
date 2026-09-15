// Public DNS: records in the domains Relay manages at the user's DNS provider.
import { useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { TopBar } from '../../components/shell/TopBar'
import {
  Badge, Button, Callout, ConfirmDialog, EmptyState, Icon, IconButton, Input, Select, Skeleton, Tooltip, useToast,
} from '../../components/ui'
import { errorMessage } from '../../lib/api'
import { useEntities, useRole } from '../../lib/queries'
import type { DNSRecord } from '../../lib/types'
import { dnsTypeLabel, useDNSProviderTypes } from '../certificates/common'
import RecordDialog from './RecordDialog'
import {
  EDITABLE_TYPES, readStoredZone, storeZone, ttlText, useDNSRecords, useDNSStatus, useDeleteDNSRecord, useInvalidateDNS, usePublicDNS,
} from './dnsApi'
import './dns.css'

const EDITABLE = new Set<string>(EDITABLE_TYPES)

export default function DNSPage() {
  const toast = useToast()
  const { isAdmin, canWrite } = useRole()
  const { enabled, isLoading } = usePublicDNS()
  const status = useDNSStatus(enabled)
  const typeDefs = useDNSProviderTypes().data
  const hosts = useEntities('hosts', { enabled }).data ?? []
  const invalidate = useInvalidateDNS()
  const [stored, setStored] = useState(readStoredZone)
  const [filter, setFilter] = useState('')
  const [typeFilter, setTypeFilter] = useState('')
  const [dialog, setDialog] = useState<{ open: boolean; record?: DNSRecord }>({ open: false })
  const [deleting, setDeleting] = useState<DNSRecord | undefined>()

  const zones = useMemo(() => [...(status.data?.zones ?? [])].sort((a, b) => a.name.localeCompare(b.name)), [status.data])
  const zone = zones.find((z) => z.name === stored) ?? zones[0]
  const zoneName = zone?.name ?? ''
  const recordsQ = useDNSRecords(enabled ? zone?.name : undefined)
  const del = useDeleteDNSRecord(zoneName)
  const providerType = recordsQ.data?.providerType ?? zone?.providerType ?? ''

  useEffect(() => setTypeFilter(''), [zoneName])

  const hostDomain = useMemo(() => new Map(hosts.map((h) => [h.id, h.domains[0] ?? h.id])), [hosts])

  const records = useMemo(() => {
    const list = recordsQ.data?.records ?? []
    const needle = filter.trim().toLowerCase()
    return list
      .filter((r) => !typeFilter || r.type === typeFilter)
      .filter((r) => !needle || r.fqdn.toLowerCase().includes(needle) || r.name.toLowerCase().includes(needle) || r.data.toLowerCase().includes(needle))
      .sort((a, b) => (a.name === '@' ? -1 : b.name === '@' ? 1 : a.name.localeCompare(b.name)) || a.type.localeCompare(b.type))
  }, [recordsQ.data, filter, typeFilter])
  const types = useMemo(() => [...new Set((recordsQ.data?.records ?? []).map((r) => r.type))].sort(), [recordsQ.data])
  const total = recordsQ.data?.records.length

  const pickZone = (name: string) => {
    setStored(name)
    storeZone(name)
  }

  const providerErrors = (status.data?.providers ?? []).filter((p) => p.error)

  const topbar = (
    <TopBar
      title="Public DNS"
      count={enabled && total !== undefined ? total : undefined}
      actions={
        enabled && canWrite && zone ? (
          <Button variant="primary" icon="plus" onClick={() => setDialog({ open: true })}>Add record</Button>
        ) : undefined
      }
    />
  )

  if (isLoading) {
    return (
      <>
        {topbar}
        <div className="page"><Skeleton height={40} /><Skeleton height={240} /></div>
      </>
    )
  }

  if (!enabled) {
    return (
      <>
        {topbar}
        <div className="page">
          <div className="card">
            <EmptyState
              icon="expose"
              title="Public DNS is off"
              description="Connect GoDaddy or Cloudflare to see and edit your domains’ records here, and let Relay create records for new proxy hosts."
              actions={
                <>
                  {isAdmin ? (
                    <Link to="/settings/public-dns" className="btn btn-primary">Set up Public DNS</Link>
                  ) : (
                    <span className="small muted">Ask an admin to turn it on.</span>
                  )}
                  <Link to="/docs/public-dns" className="btn">How it works</Link>
                </>
              }
            />
          </div>
        </div>
      </>
    )
  }

  return (
    <>
      {topbar}
      <div className="page" style={{ gap: 16 }}>
        {status.isError && <Callout tone="danger" title="Couldn’t load your domains">{errorMessage(status.error)}</Callout>}
        {providerErrors.map((p) => (
          <Callout
            key={p.id}
            tone="warn"
            title={`${p.name} · ${dnsTypeLabel(typeDefs, p.type)}`}
            actions={isAdmin ? <Link to="/settings/public-dns" className="btn btn-sm">Settings</Link> : undefined}
          >
            {p.error}
          </Callout>
        ))}

        {status.isLoading ? (
          <Skeleton height={240} />
        ) : zones.length === 0 ? (
          <div className="card">
            <EmptyState
              icon="expose"
              title="No domains found"
              description="None of the connected DNS providers returned a domain. Check the providers in Settings → Public DNS."
              actions={isAdmin && <Link to="/settings/public-dns" className="btn">Open settings</Link>}
            />
          </div>
        ) : (
          <>
            <div className="dns-toolbar">
              <div className="dns-zone-select">
                <Select
                  value={zoneName}
                  onChange={pickZone}
                  aria-label="Domain"
                  options={zones.map((z) => ({ value: z.name, label: z.name, hint: z.providerName || dnsTypeLabel(typeDefs, z.providerType) }))}
                />
              </div>
              <div className="dns-filter">
                <Icon name="search" size={14} />
                <Input
                  inputSize="sm"
                  value={filter}
                  placeholder="Filter by name or value"
                  aria-label="Filter records"
                  onChange={(e) => setFilter(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === 'Escape') {
                      setFilter('')
                      e.currentTarget.blur()
                    }
                  }}
                />
                {filter && (
                  <button type="button" className="dns-filter-clear" onClick={() => setFilter('')} aria-label="Clear filter">
                    <Icon name="close" size={12} />
                  </button>
                )}
              </div>
              <div className="dns-type-select">
                <Select inputSize="sm" value={typeFilter} placeholder="All types" options={types} onChange={setTypeFilter} aria-label="Record type" />
              </div>
              <div className="spacer" style={{ flex: 1 }} />
              <Tooltip content="Reload from provider">
                <IconButton icon="reload" bare label="Reload records" onClick={invalidate} />
              </Tooltip>
            </div>

            {recordsQ.isError && <Callout tone="danger" title="Couldn’t load records">{errorMessage(recordsQ.error)}</Callout>}

            <div className="card" style={{ overflow: 'hidden' }}>
              {recordsQ.isLoading ? (
                <div className="card-body col gap-8"><Skeleton height={28} /><Skeleton height={28} /><Skeleton height={28} /></div>
              ) : recordsQ.data && recordsQ.data.records.length === 0 ? (
                <EmptyState
                  icon="expose"
                  title="No records yet"
                  description={`${zoneName} has no records at ${zone?.providerName || 'your provider'}.`}
                  actions={canWrite && <Button variant="primary" icon="plus" onClick={() => setDialog({ open: true })}>Add record</Button>}
                />
              ) : recordsQ.data ? (
                <div className="table-wrap">
                  <table className="table compact dns-table">
                    <thead>
                      <tr>
                        <th className="dns-col-type">Type</th>
                        <th>Name</th>
                        <th>Value</th>
                        <th className="dns-col-ttl">TTL</th>
                        <th>Used by</th>
                        <th className="dns-col-actions" aria-label="Actions" />
                      </tr>
                    </thead>
                    <tbody>
                      {records.length === 0 && (
                        <tr>
                          <td colSpan={6} className="muted small" style={{ textAlign: 'center', padding: 24 }}>No records match.</td>
                        </tr>
                      )}
                      {records.map((r) => {
                        const used = r.hosts ?? []
                        const editable = canWrite && !r.readOnly && EDITABLE.has(r.type)
                        return (
                          <tr key={`${r.id}:${r.type}:${r.name}`}>
                            <td className="dns-col-type"><Badge tone="outline" className="mono">{r.type}</Badge></td>
                            <td>
                              <span className="dns-name" title={r.fqdn}>
                                {r.name === '@' ? zoneName : <>{r.name}<span className="faint">.{zoneName}</span></>}
                              </span>
                            </td>
                            <td>
                              <div className="dns-value">
                                {r.priority !== undefined && r.type === 'MX' && <span className="mono faint" title="Priority">{r.priority}</span>}
                                <span className="dns-value-text" title={r.data}>{r.data}</span>
                                {r.proxied && <Badge tone="warn" title="Proxied through Cloudflare">proxied</Badge>}
                              </div>
                            </td>
                            <td className="dns-col-ttl mono muted">{ttlText(r.ttl)}</td>
                            <td>
                              {used.length === 0 ? (
                                <span className="faint">—</span>
                              ) : (
                                <div className="dns-hosts">
                                  {used.slice(0, 3).map((id) => (
                                    <Link key={id} to={`/hosts?edit=${id}`} className="dns-host-chip" title={hostDomain.get(id) ?? id}>
                                      {hostDomain.get(id) ?? id}
                                    </Link>
                                  ))}
                                  {used.length > 3 && <span className="small faint">+{used.length - 3}</span>}
                                </div>
                              )}
                            </td>
                            <td className="dns-col-actions">
                              {editable ? (
                                <span className="dns-row-actions">
                                  <IconButton icon="edit" bare label={`Edit ${r.type} ${r.fqdn}`} onClick={() => setDialog({ open: true, record: r })} />
                                  <IconButton icon="trash" bare label={`Delete ${r.type} ${r.fqdn}`} onClick={() => setDeleting(r)} />
                                </span>
                              ) : r.readOnly ? (
                                <Badge tone="plain" title="Managed by your provider">read-only</Badge>
                              ) : null}
                            </td>
                          </tr>
                        )
                      })}
                    </tbody>
                  </table>
                </div>
              ) : null}
            </div>
            <div className="small faint">
              Changes go straight to {zone?.providerName || 'your DNS provider'} and are recorded in the audit log. Resolvers may keep old answers until the TTL runs out.
            </div>
          </>
        )}
      </div>

      {zone && (
        <RecordDialog
          open={dialog.open}
          onClose={() => setDialog({ open: false })}
          zone={zoneName}
          providerType={providerType}
          record={dialog.record}
          publicIp={status.data?.publicIp}
        />
      )}
      <ConfirmDialog
        open={!!deleting}
        onClose={() => setDeleting(undefined)}
        danger
        title={`Delete ${deleting?.type} record ${deleting?.fqdn}?`}
        message={
          deleting?.hosts?.length
            ? `It serves ${deleting.hosts.length === 1 ? 'a proxy host' : `${deleting.hosts.length} proxy hosts`}. They stop resolving once caches expire (${ttlText(deleting.ttl)}).`
            : 'The record is removed at your DNS provider right away.'
        }
        confirmLabel="Delete record"
        onConfirm={async () => {
          if (!deleting) return
          try {
            await del.mutateAsync(deleting.id)
            toast.success('Record deleted', `${deleting.type} ${deleting.fqdn}`)
          } catch (err) {
            toast.error(err, 'Could not delete record')
            throw err
          }
        }}
      />
    </>
  )
}
