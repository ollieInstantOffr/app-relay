// Settings → Public DNS: connect GoDaddy / Cloudflare and create records for proxy hosts automatically.
// Applies immediately (not a pending change).
import { useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { useQueryClient } from '@tanstack/react-query'
import { api } from '../../lib/api'
import { keys, useEntities, useRole, useSaveSettings, useSettings } from '../../lib/queries'
import { pluralize } from '../../lib/format'
import type { DNSDomainCheck, DNSProvider, PublicDNSSettings as Settings } from '../../lib/types'
import {
  Badge, Button, Callout, Card, Checkbox, Dialog, Dot, Field, Icon, Input, SectionHeader, Segmented, Skeleton, ToggleRow, cx, useToast,
} from '../../components/ui'
import DNSProviderDialog from '../certificates/DNSProviderDialog'
import { dnsTypeLabel, fieldErrors, toastUnlessFields, useDNSProviderTypes } from '../certificates/common'
import { checkMeta, dnsKeys, syncDNS, ttlText, useDNSStatus, usePublicDNSProviderTypes } from '../dns/dnsApi'
import '../dns/dns.css'

const DESCRIPTION = 'Manage your domains’ public records from Relay and create them automatically for new proxy hosts.'

function normalize(s: Settings): Settings {
  return {
    enabled: !!s.enabled,
    providerIds: s.providerIds ?? [],
    autoCreate: !!s.autoCreate,
    recordType: s.recordType === 'CNAME' ? 'CNAME' : 'A',
    target: s.target ?? '',
    ttl: s.ttl ?? 600,
    excludedZones: s.excludedZones ?? [],
  }
}

export default function PublicDNSSettings() {
  const toast = useToast()
  const qc = useQueryClient()
  const { isAdmin, canWrite } = useRole()
  const settings = useSettings('public_dns')
  const save = useSaveSettings('public_dns')
  const providers = useEntities('dns-providers').data
  const typeDefs = useDNSProviderTypes().data
  const supported = usePublicDNSProviderTypes().data ?? ['godaddy', 'cloudflare']
  const savedEnabled = !!settings.data?.enabled
  const status = useDNSStatus(savedEnabled)
  const [draft, setDraft] = useState<Settings | undefined>()
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [dialog, setDialog] = useState<{ open: boolean; provider?: DNSProvider }>({ open: false })
  const [testing, setTesting] = useState<string | undefined>()
  const [syncing, setSyncing] = useState(false)
  const [syncResults, setSyncResults] = useState<DNSDomainCheck[] | null>(null)

  useEffect(() => {
    if (settings.data) setDraft(normalize(settings.data))
  }, [settings.data])

  const baseline = useMemo(() => (settings.data ? JSON.stringify(normalize(settings.data)) : ''), [settings.data])
  const dirty = !!draft && JSON.stringify(draft) !== baseline
  const readOnly = !isAdmin

  const eligible = useMemo(() => (providers ?? []).filter((p) => supported.includes(p.type)), [providers, supported])
  const zoneNames = useMemo(() => {
    const names = new Set<string>(status.data?.zones.map((z) => z.name) ?? [])
    draft?.excludedZones.forEach((z) => names.add(z))
    return [...names].sort()
  }, [status.data, draft?.excludedZones])

  const header = <SectionHeader title="Public DNS" description={DESCRIPTION} />

  if (settings.isError && !settings.data) {
    return (
      <>
        {header}
        <Callout tone="danger" title="Couldn’t load Public DNS settings">This version of Relay may not support Public DNS yet.</Callout>
      </>
    )
  }
  if (!draft) {
    return (
      <>
        {header}
        <Skeleton height={90} />
        <Skeleton height={220} />
        <Skeleton height={200} />
      </>
    )
  }

  const set = (patch: Partial<Settings>) => {
    setDraft((d) => (d ? { ...d, ...patch } : d))
    const changed = Object.keys(patch)
    setErrors((e) => Object.fromEntries(Object.entries(e).filter(([k]) => !changed.includes(k))))
  }
  const toggleProvider = (id: string) => {
    if (readOnly) return
    set({ providerIds: draft.providerIds.includes(id) ? draft.providerIds.filter((x) => x !== id) : [...draft.providerIds, id] })
  }
  const toggleZone = (zone: string) => {
    if (readOnly) return
    set({ excludedZones: draft.excludedZones.includes(zone) ? draft.excludedZones.filter((z) => z !== zone) : [...draft.excludedZones, zone] })
  }

  const submit = async () => {
    setErrors({})
    try {
      await save.mutateAsync(draft)
      qc.invalidateQueries({ queryKey: dnsKeys.all })
      toast.success('Public DNS settings saved', 'Changes apply right away.')
    } catch (err) {
      setErrors(fieldErrors(err))
      toastUnlessFields(toast, err, 'Could not save Public DNS settings')
    }
  }

  const testProvider = async (p: DNSProvider) => {
    setTesting(p.id)
    try {
      const res = await api.post<DNSProvider>(`/api/dns-providers/${p.id}/test`)
      qc.invalidateQueries({ queryKey: keys.entities('dns-providers') })
      qc.invalidateQueries({ queryKey: dnsKeys.status })
      if (res.status === 'ok') toast.success(`${res.name} works`, res.zones.length ? `Domains: ${res.zones.join(', ')}` : undefined)
      else if (res.status === 'unknown') toast.show({ kind: 'info', title: `${res.name} not verified`, message: res.lastError })
      else toast.show({ kind: 'error', title: `${res.name} was rejected`, message: res.lastError, actions: [{ label: 'Fix credentials', primary: true, onClick: () => setDialog({ open: true, provider: res }) }] })
    } catch (err) {
      toast.error(err, 'Test failed')
    } finally {
      setTesting(undefined)
    }
  }

  const runSync = async () => {
    setSyncing(true)
    try {
      const results = await syncDNS()
      setSyncResults(results)
      qc.invalidateQueries({ queryKey: dnsKeys.all })
    } catch (err) {
      toast.error(err, 'Sync failed')
    } finally {
      setSyncing(false)
    }
  }

  const providerLine = (p: DNSProvider, selected: boolean): { text: string; tone: 'ok' | 'danger' | 'muted' } => {
    const st = status.data?.providers.find((x) => x.id === p.id)
    if (st?.error) return { text: st.error, tone: 'danger' }
    if (st) return { text: st.zones.length ? `${pluralize(st.zones.length, 'domain')} · ${st.zones.slice(0, 4).join(', ')}${st.zones.length > 4 ? ` +${st.zones.length - 4}` : ''}` : 'No domains found', tone: st.zones.length ? 'ok' : 'muted' }
    if (selected && savedEnabled && status.isLoading) return { text: 'Checking domains…', tone: 'muted' }
    if (p.status === 'failed') return { text: p.lastError || 'Credentials rejected', tone: 'danger' }
    if (p.zones?.length) return { text: `${pluralize(p.zones.length, 'domain')} · ${p.zones.slice(0, 4).join(', ')}${p.zones.length > 4 ? ` +${p.zones.length - 4}` : ''}`, tone: p.status === 'ok' ? 'ok' : 'muted' }
    return { text: p.status === 'ok' ? 'Credentials work' : 'Not tested yet', tone: p.status === 'ok' ? 'ok' : 'muted' }
  }

  const isA = draft.recordType === 'A'
  const publicIp = status.data?.publicIp ?? ''
  const syncCounts = syncResults?.reduce<Record<string, number>>((acc, r) => ({ ...acc, [r.status]: (acc[r.status] ?? 0) + 1 }), {})

  return (
    <>
      <SectionHeader
        title="Public DNS"
        description={DESCRIPTION}
        actions={
          canWrite && savedEnabled ? (
            <>
              <Link to="/dns" className="btn">Manage records</Link>
              <Button icon="reload" loading={syncing} disabled={dirty} title={dirty ? 'Save your changes first' : undefined} onClick={runSync}>Sync now</Button>
            </>
          ) : savedEnabled ? (
            <Link to="/dns" className="btn">View records</Link>
          ) : undefined
        }
      />
      {readOnly && <Callout tone="info">Only admins can change these settings.</Callout>}

      <Card>
        <ToggleRow
          title="Use Public DNS"
          description="Relay reads and edits records at your DNS provider. Changes happen right away, not on apply."
          checked={draft.enabled}
          disabled={readOnly}
          onChange={(enabled) => set({ enabled })}
        />
      </Card>

      <Card
        title="DNS providers"
        sub="Relay manages the domains of the providers you tick"
        actions={canWrite && <Button variant="ghost" size="sm" icon="plus" onClick={() => setDialog({ open: true })}>Add DNS provider</Button>}
      >
        <div className="card-body" style={{ paddingTop: 14, paddingBottom: 14 }}>
          <Callout tone="info" title="GoDaddy and Cloudflare">
            GoDaddy only allows API access for accounts with 10 or more domains or a Discount Domain Club plan. Other accounts get “Access denied”.
            Cloudflare works on the free plan: the API token needs <span className="mono">Zone → DNS → Edit</span> and <span className="mono">Zone → Zone → Read</span>.
            A domain registered at GoDaddy can still use Cloudflare by pointing its nameservers there. <Link to="/docs/public-dns" style={{ textDecoration: 'underline' }}>How to</Link>
          </Callout>
        </div>
        {providers === undefined ? (
          <div className="card-body" style={{ paddingTop: 0 }}><Skeleton height={48} /></div>
        ) : eligible.length === 0 ? (
          <div className="card-body small muted" style={{ paddingTop: 0 }}>
            No GoDaddy or Cloudflare provider yet. Add one to get started — it also works for DNS-01 certificates.
          </div>
        ) : (
          eligible.map((p) => {
            const selected = draft.providerIds.includes(p.id)
            const line = providerLine(p, selected)
            return (
              <div key={p.id} className={cx('dns-provider-row', readOnly && 'readonly')} onClick={() => toggleProvider(p.id)}>
                <Checkbox checked={selected} disabled={readOnly} onChange={() => toggleProvider(p.id)} />
                <Dot tone={line.tone} />
                <div className="grow" style={{ minWidth: 0 }}>
                  <div className="medium">
                    {p.name}
                    <span className="small muted" style={{ fontWeight: 400 }}> · {dnsTypeLabel(typeDefs, p.type)}</span>
                  </div>
                  <div className="mono small truncate" style={{ marginTop: 2, color: line.tone === 'danger' ? 'var(--danger-text)' : 'var(--ink-faint)' }} title={line.text}>
                    {line.text}
                  </div>
                </div>
                {selected && dirty && !(settings.data?.providerIds ?? []).includes(p.id) && <Badge tone="pending">unsaved</Badge>}
                <Button
                  size="sm"
                  variant="ghost"
                  loading={testing === p.id}
                  disabled={!canWrite}
                  onClick={(e) => {
                    e.stopPropagation()
                    testProvider(p)
                  }}
                >
                  Test
                </Button>
              </div>
            )
          })
        )}
        {errors.providerIds && <div className="card-body field-error" style={{ paddingTop: 0 }}>{errors.providerIds}</div>}
      </Card>

      <Card title="Automation">
        <ToggleRow
          title="Create missing records"
          description="After each apply, Relay adds a record for enabled proxy hosts in your domains. It never changes or deletes existing records."
          checked={draft.autoCreate}
          disabled={readOnly}
          onChange={(autoCreate) => set({ autoCreate })}
        />
        <div className="toggle-row">
          <div className="grow">
            <div className="toggle-title">Record type</div>
            <div className="toggle-desc">
              {isA
                ? 'A → points at an IP address. Simple, but every record needs updating if your IP changes.'
                : 'CNAME → points at a hostname you manage, so there’s one place to change.'}
            </div>
            {errors.recordType && <div className="field-error">{errors.recordType}</div>}
          </div>
          <Segmented value={draft.recordType} disabled={readOnly} onChange={(recordType) => set({ recordType })} options={[{ value: 'A', label: 'A' }, { value: 'CNAME', label: 'CNAME' }]} />
        </div>
        <div className="card-body col gap-14">
          <div className="grid-2" style={{ gap: 14 }}>
            <Field
              label={isA ? 'Points to (IPv4)' : 'Points to (hostname)'}
              error={errors.target}
              hint={
                isA
                  ? draft.target ? 'Used for every automatic record' : publicIp ? `Leave empty to use Relay’s public IP (${publicIp})` : 'Leave empty to use Relay’s detected public IP'
                  : 'Required · e.g. a dynamic DNS name that follows your IP'
              }
            >
              <Input
                mono
                value={draft.target}
                disabled={readOnly}
                invalid={!!errors.target}
                placeholder={isA ? publicIp || '203.0.113.10' : 'home.example.com'}
                onChange={(e) => set({ target: e.target.value.trim() })}
              />
            </Field>
            <Field label="TTL (seconds)" error={errors.ttl} hint={`${ttlText(draft.ttl)} · GoDaddy needs at least 600`}>
              <Input
                mono
                type="number"
                min={60}
                value={draft.ttl || ''}
                disabled={readOnly}
                invalid={!!errors.ttl}
                placeholder="600"
                onChange={(e) => set({ ttl: e.target.value === '' ? 0 : Number(e.target.value) })}
              />
            </Field>
          </div>
          <Field label="Excluded domains" error={errors.excludedZones} hint="Relay never changes these automatically. You can still edit their records by hand.">
            {zoneNames.length === 0 ? (
              <div className="small muted">{savedEnabled ? 'No domains found yet.' : 'Turn on Public DNS and save to pick domains.'}</div>
            ) : (
              <div className="dns-zone-chips">
                {zoneNames.map((z) => {
                  const on = draft.excludedZones.includes(z)
                  return (
                    <button key={z} type="button" className={cx('dns-zone-chip', on && 'active')} disabled={readOnly} aria-pressed={on} onClick={() => toggleZone(z)}>
                      <Icon name={on ? 'close' : 'check'} size={12} />
                      {z}
                    </button>
                  )
                })}
              </div>
            )}
          </Field>
        </div>
      </Card>

      {!readOnly && (
        <div className="row gap-8">
          <Button variant="primary" onClick={submit} loading={save.isPending} disabled={!dirty}>Save changes</Button>
          {dirty && <Button variant="ghost" onClick={() => { setDraft(normalize(settings.data!)); setErrors({}) }}>Discard</Button>}
        </div>
      )}

      <DNSProviderDialog
        open={dialog.open}
        provider={dialog.provider}
        onClose={() => setDialog({ open: false })}
        onSaved={(p) => {
          if (!dialog.provider && supported.includes(p.type) && !readOnly && !draft.providerIds.includes(p.id)) set({ providerIds: [...draft.providerIds, p.id] })
          qc.invalidateQueries({ queryKey: dnsKeys.status })
        }}
      />

      <Dialog
        open={!!syncResults}
        onClose={() => setSyncResults(null)}
        width={620}
        title="Sync results"
        description={
          syncResults && syncResults.length
            ? Object.entries(syncCounts ?? {}).map(([s, n]) => `${n} ${s}`).join(' · ')
            : 'No enabled proxy hosts to check.'
        }
        footer={
          <>
            <Link to="/dns" className="btn">Open Public DNS</Link>
            <Button variant="primary" onClick={() => setSyncResults(null)}>Done</Button>
          </>
        }
      >
        {syncResults && syncResults.length > 0 && (
          <div className="dns-check-list dns-check-scroll">
            {syncResults.map((r) => {
              const m = checkMeta(r, draft.autoCreate)
              return (
                <div key={r.domain} className="dns-check-row">
                  <Badge tone={m.tone}>{m.label}</Badge>
                  <span className="dns-check-domain" title={r.domain}>{r.domain}</span>
                  <span className={cx('dns-check-text', r.status === 'error' && 'danger')} title={m.text}>
                    {r.status === 'missing' ? 'Not created' : m.text}
                  </span>
                </div>
              )
            })}
          </div>
        )}
      </Dialog>
    </>
  )
}
