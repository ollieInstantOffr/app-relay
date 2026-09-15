// Owner: slice observe — Log detail drawer (design 29a): timing, headers, block IP.
import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Badge, Button, Callout, CodeBlock, ConfirmDialog, Dialog, Drawer, Field, Input, Menu, Segmented, Skeleton, cx, useToast,
  type MenuEntry,
} from '../../components/ui'
import { ApiError, api, errorMessage } from '../../lib/api'
import { bytes, clock, date, ms, rid } from '../../lib/format'
import { keys, useEntities, useRole, useSaveEntity } from '../../lib/queries'
import type { AccessEntry, AccessList } from '../../lib/types'
import type { AccessDetail } from './api'
import { lastUpstream, requestLabel, shortRequestId, statusTone, timeCell, upstreamInfo } from './util'

const applyNow = () => window.dispatchEvent(new CustomEvent('relay:apply'))

export default function LogDetailDrawer({ id, onClose, onSelect, onFilterIp }: {
  id: number | null
  onClose: () => void
  onSelect: (id: number) => void
  onFilterIp: (ip: string) => void
}) {
  const q = useQuery({
    queryKey: ['logs', 'access', 'detail', id],
    queryFn: () => api.get<AccessDetail>(`/api/logs/access/${id}`),
    enabled: id !== null,
  })
  const d = q.data
  const e = d?.entry
  return (
    <Drawer
      open={id !== null}
      onClose={onClose}
      width="wide"
      title={
        e ? (
          <div className="ld-title">
            <StatusBadge status={e.status} />
            <span className="req" title={requestLabel(e)}>{e.kind === 'stream' ? requestLabel(e) : `${e.method} ${e.path}`}</span>
          </div>
        ) : (
          'Request'
        )
      }
      subtitle={e ? subtitle(e) : undefined}
    >
      {q.isLoading && (
        <>
          <Skeleton height={28} />
          <Skeleton height={64} />
          <Skeleton height={120} />
        </>
      )}
      {q.isError && (
        <Callout tone="danger" title="Couldn't load this entry">
          {q.error instanceof ApiError && q.error.status === 404 ? 'It was removed by log retention.' : errorMessage(q.error)}
        </Callout>
      )}
      {d && e && <DetailBody key={e.id} d={d} onSelect={onSelect} onFilterIp={onFilterIp} />}
    </Drawer>
  )
}

function subtitle(e: AccessEntry): string {
  const today = new Date(e.ts).toDateString() === new Date().toDateString()
  const time = `${today ? '' : `${date(e.ts)} `}${clock(e.ts, true)}`
  return [e.host || '—', time, e.requestId && `req ${shortRequestId(e.requestId)}`].filter(Boolean).join(' · ')
}

function StatusBadge({ status }: { status: number }) {
  const tone = statusTone(status)
  return <Badge tone={tone === 'muted' ? undefined : tone}>{status || '—'}</Badge>
}

function DetailBody({ d, onSelect, onFilterIp }: { d: AccessDetail; onSelect: (id: number) => void; onFilterIp: (ip: string) => void }) {
  const e = d.entry
  const navigate = useNavigate()
  const { canWrite } = useRole()
  const toast = useToast()
  const qc = useQueryClient()
  const lists = useEntities('access-lists', { enabled: canWrite })
  const [blockOpen, setBlockOpen] = useState(false)
  const [ruleList, setRuleList] = useState<AccessList | null>(null)
  const up = upstreamInfo(e)
  const ip = e.clientIp

  const block = async () => {
    try {
      await api.post('/api/blocklist', { cidr: ip, note: e.host ? `blocked from access log · ${e.host}` : 'blocked from access log' })
      toast.show({
        kind: 'success',
        title: 'Address blocked',
        message: `${ip} added to pending changes.`,
        actions: [{ label: 'Apply now', onClick: applyNow, primary: true }],
      })
      qc.invalidateQueries({ queryKey: ['logs', 'access', 'detail'] })
      qc.invalidateQueries({ queryKey: keys.settings('blocklist') })
      qc.invalidateQueries({ queryKey: keys.pending })
    } catch (err) {
      toast.error(err, `Couldn't block ${ip}`)
    }
  }

  const listItems: MenuEntry[] = [{ header: `Add ${ip} to…` }]
  if (lists.data && lists.data.length > 0) {
    for (const l of lists.data) listItems.push({ label: l.name, icon: 'access', onSelect: () => setRuleList(l) })
  } else {
    listItems.push({ label: lists.isLoading ? 'Loading…' : 'No access lists yet', disabled: true, onSelect: () => undefined })
  }
  listItems.push('separator', { label: 'Manage access lists', icon: 'external', onSelect: () => navigate('/access') })

  return (
    <>
      <div className="ld-actions">
        {canWrite && ip && (d.blocked ? (
          <Button size="sm" icon="check" disabled>Blocked</Button>
        ) : (
          <Button size="sm" onClick={() => setBlockOpen(true)}>Block {ip}</Button>
        ))}
        {canWrite && ip && <Menu align="start" trigger={<Button size="sm" iconRight="chevron">Add to access list</Button>} items={listItems} />}
        {ip && <Button size="sm" icon="filter" onClick={() => onFilterIp(ip)}>Filter by this IP</Button>}
        {d.host && (
          <Button size="sm" icon="external" onClick={() => navigate(d.host!.kind === 'stream' ? '/streams' : `/hosts?edit=${d.host!.id}`)}>
            {d.host.kind === 'stream' ? 'Open stream' : 'Open host'}
          </Button>
        )}
      </div>

      <div className="ld-stats">
        <Stat k="Client" v={ip || '—'} />
        <Stat k={e.kind === 'stream' ? 'Session' : 'Total'} v={ms(e.requestTime * 1000)} />
        <Stat k="Upstream" v={up.text} error={up.error} />
        <Stat k="Bytes" v={bytes(e.kind === 'stream' ? e.bytesSent + e.bytesReceived : e.bytesSent)} />
      </div>

      <div className="ld-section">
        <div className="section-title">Timing</div>
        <Waterfall e={e} />
      </div>

      {e.kind === 'http' && (
        <div className="ld-section">
          <div className="section-title">Request headers</div>
          <CodeBlock code={headerBlock(e)} wrap />
          <div className="faint small">
            {[e.sslProtocol && `TLS ${e.sslProtocol}`, e.extra.scheme, e.extra.remote_user && `user ${e.extra.remote_user}`, e.bytesReceived ? `request ${bytes(e.bytesReceived)}` : '', e.upstreamStatus && `upstream status ${e.upstreamStatus}`]
              .filter(Boolean)
              .join(' · ')}
          </div>
        </div>
      )}

      <div className="ld-section">
        <div className="row">
          <div className="section-title">Same client · last 10 min</div>
          <span className="badge">{d.sameClient.length}</span>
        </div>
        {d.sameClient.length === 0 ? (
          <div className="faint small">No other requests from {ip || 'this client'} around this time.</div>
        ) : (
          <div className="same-list">
            {d.sameClient.map((s) => (
              <div
                key={s.id}
                className="same-row"
                role="button"
                tabIndex={0}
                onClick={() => onSelect(s.id)}
                onKeyDown={(ev) => {
                  if (ev.key === 'Enter') onSelect(s.id)
                }}
              >
                <span className={`st-${statusTone(s.status)}`}>{s.status || '—'}</span>
                <span title={requestLabel(s)}>{requestLabel(s)}</span>
                <span className="faint" title={s.host}>{s.host}</span>
                <span className="faint">{timeCell(s.ts)}</span>
              </div>
            ))}
          </div>
        )}
      </div>

      <ConfirmDialog
        open={blockOpen}
        onClose={() => setBlockOpen(false)}
        onConfirm={block}
        danger
        confirmLabel="Block"
        title={`Block ${ip}?`}
        message="Adds the address to the global blocklist. After the next apply, the reverse proxy refuses it on every host."
      />
      {ruleList && <AddRuleDialog list={ruleList} ip={ip} onClose={() => setRuleList(null)} />}
    </>
  )
}

function Stat({ k, v, error }: { k: string; v: string; error?: boolean }) {
  return (
    <div className="ld-stat">
      <div className="k">{k}</div>
      <div className={cx('v', error && 'up-err')} title={v}>{v}</div>
    </div>
  )
}

interface Span {
  label: string
  start: number
  end: number
  value: string
  tone?: 'err' | 'muted'
}

/** Timing spans in ms. nginx upstream timings are cumulative from the start of the connection. */
function timingSpans(e: AccessEntry): Span[] {
  const sec = (v?: number) => (v != null ? v * 1000 : null)
  const total = e.requestTime * 1000
  const c = sec(e.upstreamConnectTime)
  const h = sec(e.upstreamHeaderTime)
  const r = sec(e.upstreamResponseTime)
  const addr = lastUpstream(e.upstreamAddr) || 'upstream'

  if (e.kind === 'stream') {
    if (c == null && e.status >= 500) return [{ label: `connect → ${addr}`, start: 0, end: Math.max(total, 1), value: 'failed', tone: 'err' }]
    const out: Span[] = []
    if (c != null) out.push({ label: `connect → ${addr}`, start: 0, end: c, value: ms(c) })
    out.push({ label: 'session', start: c ?? 0, end: Math.max(total, c ?? 0), value: ms(total - (c ?? 0)), tone: 'muted' })
    return out
  }
  if (!e.upstreamAddr) return [{ label: 'answered by the proxy', start: 0, end: Math.max(total, 0.5), value: ms(total), tone: 'muted' }]

  const up = upstreamInfo(e)
  if (up.error) {
    const out: Span[] = []
    if (c != null) out.push({ label: `connect → ${addr}`, start: 0, end: c, value: ms(c) })
    out.push({
      label: c != null ? `waiting · ${up.text}` : `connect → ${addr}`,
      start: c ?? 0,
      end: Math.max(total, (c ?? 0) + 0.5),
      value: up.text === 'refused' ? 'refused' : `${up.text} · ${ms(total)}`,
      tone: 'err',
    })
    return out
  }
  const out: Span[] = []
  if (c != null) out.push({ label: `connect → ${addr}`, start: 0, end: c, value: ms(c) })
  if (h != null) out.push({ label: 'waiting for headers', start: c ?? 0, end: h, value: ms(h - (c ?? 0)) })
  if (r != null) out.push({ label: 'response body', start: h ?? c ?? 0, end: r, value: ms(r - (h ?? c ?? 0)) })
  const end = r ?? h ?? c ?? 0
  if (total - end >= 1) out.push({ label: 'proxy ↔ client', start: end, end: total, value: ms(total - end), tone: 'muted' })
  return out
}

function Waterfall({ e }: { e: AccessEntry }) {
  const spans = timingSpans(e)
  const scale = Math.max(0.001, ...spans.map((s) => s.end))
  return (
    <div className="wf">
      {spans.map((s, i) => (
        <div key={i} className={cx('wf-row', s.tone === 'err' && 'err')}>
          <span className="lbl" title={s.label}>{s.label}</span>
          <div className="wf-track">
            <div
              className={cx('wf-bar', s.tone)}
              style={{ left: `${(s.start / scale) * 100}%`, width: `${(Math.max(0, s.end - s.start) / scale) * 100}%` }}
            />
          </div>
          <span className="val">{s.value}</span>
        </div>
      ))}
    </div>
  )
}

function headerBlock(e: AccessEntry): string {
  const lines = [`${e.method} ${e.path} ${e.protocol}`.trim(), `Host: ${e.host}`]
  if (e.userAgent) lines.push(`User-Agent: ${e.userAgent}`)
  if (e.extra.accept) lines.push(`Accept: ${e.extra.accept}`)
  if (e.referer) lines.push(`Referer: ${e.referer}`)
  if (e.extra.x_forwarded_for) lines.push(`X-Forwarded-For: ${e.extra.x_forwarded_for}`)
  lines.push('Cookie: [redacted]  # cookies and credentials are never logged')
  return lines.join('\n')
}

function AddRuleDialog({ list, ip, onClose }: { list: AccessList; ip: string; onClose: () => void }) {
  const [action, setAction] = useState<'allow' | 'deny'>('deny')
  const [note, setNote] = useState('from access log')
  const save = useSaveEntity('access-lists')
  const toast = useToast()
  const exists = list.rules.some((r) => r.cidr === ip)
  const submit = async () => {
    try {
      await save.mutateAsync({ ...list, rules: [{ id: rid(), action, cidr: ip, note: note.trim() }, ...list.rules] })
      toast.show({
        kind: 'success',
        title: 'Access list saved',
        message: `${list.name} added to pending changes.`,
        actions: [{ label: 'Apply now', onClick: applyNow, primary: true }],
      })
      onClose()
    } catch (err) {
      toast.error(err, `Couldn't update ${list.name}`)
    }
  }
  return (
    <Dialog
      open
      onClose={onClose}
      title={`Add ${ip} to ${list.name}`}
      description="Rules are evaluated top to bottom — the new rule goes first so it wins over the list's existing rules."
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" loading={save.isPending} onClick={submit}>Save to pending</Button>
        </>
      }
    >
      <Field label="Action">
        <Segmented value={action} onChange={setAction} options={[{ value: 'deny', label: 'Deny' }, { value: 'allow', label: 'Allow' }]} />
      </Field>
      <Field label="Note">
        <Input value={note} onChange={(ev) => setNote(ev.target.value)} />
      </Field>
      {exists && <Callout tone="warn">{list.name} already has a rule for {ip}.</Callout>}
    </Dialog>
  )
}
