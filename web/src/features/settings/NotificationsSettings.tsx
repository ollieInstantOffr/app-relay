// Owner: slice ops. Settings → Notifications (design 16b).
import { useEffect, useMemo, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { api, ApiError, errorMessage } from '../../lib/api'
import { useRole, useSaveSettings, useSettings } from '../../lib/queries'
import type { NotificationChannel, NotificationEvent, NotificationSettings as NotificationSettingsT } from '../../lib/types'
import { ago } from '../../lib/format'
import {
  Button, Callout, Card, Checkbox, Dialog, Field, IconButton, Input, Menu, PasswordInput, SectionHeader, Segmented, Select, Skeleton, Toggle, useToast,
} from '../../components/ui'
import { opsKeys, useNotificationLog } from '../docker/ops'
import '../docker/ops.css'

type ChannelType = NotificationChannel['type']

const EVENTS: { key: NotificationEvent; label: string; critical?: boolean }[] = [
  { key: 'upstream_down', label: 'Upstream or LB server goes down', critical: true },
  { key: 'cert_renew_failed', label: 'Certificate renewal failed', critical: true },
  { key: 'cert_expiring', label: 'Certificate expires in < 14 days' },
  { key: 'reload_failed', label: 'Config reload failed', critical: true },
  { key: 'unknown_sign_in', label: 'New sign-in from unknown IP' },
  { key: 'mcp_write_executed', label: 'MCP write tool executed' },
  { key: 'weekly_summary', label: 'Weekly summary' },
]

const TYPE_LABEL: Record<ChannelType, string> = { ntfy: 'ntfy', smtp: 'Email (SMTP)', webhook: 'Webhook' }
const TYPE_HINT: Record<ChannelType, string> = {
  ntfy: 'Push notifications to your phone',
  smtp: 'Any SMTP server · Fastmail, Gmail, Postmark…',
  webhook: 'Discord, Slack, Home Assistant…',
}
const SECRET: Record<ChannelType, string> = { ntfy: 'token', smtp: 'password', webhook: 'secret' }

function target(ch: NotificationChannel): string {
  if (ch.type === 'smtp') {
    const port = ch.config.port || (ch.config.security === 'tls' ? '465' : '587')
    return `${ch.config.host}:${port} → ${ch.config.to}`
  }
  return ch.config.url ?? ''
}

function clone<T>(v: T): T {
  return JSON.parse(JSON.stringify(v))
}

// ---------------------------------------------------------------- channel dialog

function ChannelDialog({ open, initial, onClose, onSave }: {
  open: boolean
  initial: NotificationChannel | null
  onClose: () => void
  onSave: (ch: NotificationChannel) => Promise<void>
}) {
  const toast = useToast()
  const [ch, setCh] = useState<NotificationChannel | null>(initial)
  const [fields, setFields] = useState<Record<string, string>>({})
  const [busy, setBusy] = useState(false)
  const [testing, setTesting] = useState(false)
  const [testResult, setTestResult] = useState<{ ok: boolean; error?: string } | null>(null)

  useEffect(() => {
    if (open) {
      setCh(initial ? clone(initial) : null)
      setFields({})
      setTestResult(null)
    }
  }, [open, initial])

  if (!ch) return null
  const cfg = ch.config
  const set = (k: string, v: string) => setCh({ ...ch, config: { ...cfg, [k]: v } })
  const secretKey = SECRET[ch.type]
  const secretStored = cfg[secretKey + 'Set'] === 'true'
  const fieldErr = (k: string) => fields[`config.${k}`]

  const test = async () => {
    setTesting(true)
    setTestResult(null)
    try {
      const res = await api.post<{ ok: boolean; error?: string }>('/api/notifications/test', { channel: ch })
      setTestResult(res)
      setFields({})
    } catch (err) {
      if (err instanceof ApiError && err.fields) setFields(err.fields)
      setTestResult({ ok: false, error: errorMessage(err) })
    } finally {
      setTesting(false)
    }
  }

  const save = async () => {
    setBusy(true)
    try {
      await onSave(ch)
      onClose()
    } catch (err) {
      if (err instanceof ApiError && err.fields) {
        const f: Record<string, string> = {}
        for (const [k, v] of Object.entries(err.fields)) {
          const m = k.match(/^channels\.\d+\.(.+)$/)
          if (m) f[m[1]] = v
        }
        setFields(f)
      }
      toast.error(err, 'Could not save channel')
    } finally {
      setBusy(false)
    }
  }

  const secretPlaceholder = secretStored ? '•••••••• stored — leave empty to keep' : ''

  return (
    <Dialog
      open={open}
      onClose={onClose}
      width={560}
      title={initial?.id ? `Edit ${TYPE_LABEL[ch.type]}` : `Add ${TYPE_LABEL[ch.type]} channel`}
      description={TYPE_HINT[ch.type]}
      footer={
        <>
          <Button loading={testing} onClick={test} style={{ marginRight: 'auto' }}>Send test</Button>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" loading={busy} onClick={save}>Save channel</Button>
        </>
      }
    >
      <div className="col gap-14">
        <Field label="Name">
          <Input value={ch.name} placeholder={TYPE_LABEL[ch.type]} onChange={(e) => setCh({ ...ch, name: e.target.value })} />
        </Field>

        {ch.type === 'ntfy' && (
          <>
            <Field label="Topic URL" error={fieldErr('url')} hint="Your ntfy server and topic, e.g. https://ntfy.sh/relay-a8f3">
              <Input mono value={cfg.url ?? ''} placeholder="https://ntfy.home.lan/relay" invalid={!!fieldErr('url')} onChange={(e) => set('url', e.target.value)} autoFocus />
            </Field>
            <div className="grid-2">
              <Field label="Access token" hint="Optional · tk_… or user:password">
                <PasswordInput mono value={cfg.token ?? ''} placeholder={secretPlaceholder} onChange={(e) => set('token', e.target.value)} />
              </Field>
              <Field label="Priority">
                <Select
                  value={cfg.priority ?? ''}
                  placeholder="By severity"
                  options={['min', 'low', 'default', 'high', 'urgent']}
                  onChange={(v) => set('priority', v)}
                />
              </Field>
            </div>
          </>
        )}

        {ch.type === 'smtp' && (
          <>
            <div className="row gap-12" style={{ alignItems: 'flex-start' }}>
              <Field label="SMTP host" error={fieldErr('host')} className="grow">
                <Input mono value={cfg.host ?? ''} placeholder="smtp.fastmail.com" invalid={!!fieldErr('host')} onChange={(e) => set('host', e.target.value)} autoFocus />
              </Field>
              <Field label="Port" error={fieldErr('port')}>
                <Input mono style={{ width: 90 }} value={cfg.port ?? ''} placeholder={cfg.security === 'tls' ? '465' : cfg.security === 'none' ? '25' : '587'} onChange={(e) => set('port', e.target.value.replace(/\D/g, ''))} />
              </Field>
            </div>
            <Field label="Encryption">
              <Segmented
                value={(cfg.security as 'tls' | 'starttls' | 'none') || (cfg.port === '465' ? 'tls' : 'starttls')}
                onChange={(v) => setCh({ ...ch, config: { ...cfg, security: v, port: cfg.port || '' } })}
                options={[
                  { value: 'tls', label: 'TLS · 465' },
                  { value: 'starttls', label: 'STARTTLS · 587' },
                  { value: 'none', label: 'None' },
                ]}
              />
            </Field>
            <div className="grid-2">
              <Field label="Username">
                <Input value={cfg.username ?? ''} autoComplete="off" onChange={(e) => set('username', e.target.value)} />
              </Field>
              <Field label="Password">
                <PasswordInput value={cfg.password ?? ''} autoComplete="new-password" placeholder={secretPlaceholder} onChange={(e) => set('password', e.target.value)} />
              </Field>
            </div>
            <div className="grid-2">
              <Field label="From" error={fieldErr('from')}>
                <Input value={cfg.from ?? ''} placeholder="Relay <relay@example.com>" invalid={!!fieldErr('from')} onChange={(e) => set('from', e.target.value)} />
              </Field>
              <Field label="To" error={fieldErr('to')} hint="Separate multiple addresses with commas">
                <Input value={cfg.to ?? ''} placeholder="jonas@example.com" invalid={!!fieldErr('to')} onChange={(e) => set('to', e.target.value)} />
              </Field>
            </div>
          </>
        )}

        {ch.type === 'webhook' && (
          <>
            <Field label="Webhook URL" error={fieldErr('url')} hint="Discord and Slack webhook URLs are detected and formatted automatically; anything else receives JSON {event, level, title, message, url, at}.">
              <Input mono value={cfg.url ?? ''} placeholder="https://discord.com/api/webhooks/…" invalid={!!fieldErr('url')} onChange={(e) => set('url', e.target.value)} autoFocus />
            </Field>
            <div className="grid-2">
              <Field label="Secret header" hint="Optional">
                <Input mono value={cfg.secretHeader ?? ''} placeholder="X-Relay-Secret" onChange={(e) => set('secretHeader', e.target.value)} />
              </Field>
              <Field label="Secret value">
                <PasswordInput mono value={cfg.secret ?? ''} placeholder={secretPlaceholder} onChange={(e) => set('secret', e.target.value)} />
              </Field>
            </div>
          </>
        )}

        {testResult && (
          <Callout tone={testResult.ok ? 'ok' : 'danger'}>
            {testResult.ok ? 'Test notification sent. Check that it arrived.' : testResult.error}
          </Callout>
        )}
      </div>
    </Dialog>
  )
}

// ---------------------------------------------------------------- page

export default function NotificationsSettings() {
  const qc = useQueryClient()
  const toast = useToast()
  const { isAdmin } = useRole()
  const settings = useSettings('notifications')
  const save = useSaveSettings('notifications')
  const log = useNotificationLog()
  const [draft, setDraft] = useState<NotificationSettingsT | null>(null)
  const [editing, setEditing] = useState<{ open: boolean; channel: NotificationChannel | null }>({ open: false, channel: null })
  const [quietOpen, setQuietOpen] = useState(false)
  const [quiet, setQuiet] = useState({ start: '23:00', end: '07:00' })
  const [testing, setTesting] = useState('')

  useEffect(() => {
    if (settings.data) setDraft(clone(settings.data))
  }, [settings.data])

  const dirty = useMemo(() => !!draft && !!settings.data && JSON.stringify(draft) !== JSON.stringify(settings.data), [draft, settings.data])

  const lastByChannel = useMemo(() => {
    const m: Record<string, { status: string; at: string; error?: string }> = {}
    for (const row of log.data ?? []) {
      if (row.status === 'queued') continue
      if (!m[row.channelId]) m[row.channelId] = row
    }
    return m
  }, [log.data])

  if (!draft || !settings.data) {
    return (
      <>
        <SectionHeader title="Notifications" description="Where to send alerts, and which events are worth waking you up for." />
        <Skeleton height={260} />
      </>
    )
  }

  const persist = async (next: NotificationSettingsT, success?: string) => {
    const saved = await save.mutateAsync(next)
    setDraft(clone(saved))
    qc.invalidateQueries({ queryKey: opsKeys.notificationLog })
    if (success) toast.success(success)
  }

  const saveChannel = async (ch: NotificationChannel) => {
    const isNew = !ch.id
    const channels = isNew ? [...draft.channels, ch] : draft.channels.map((c) => (c.id === ch.id ? ch : c))
    let next: NotificationSettingsT = { ...draft, channels }
    if (isNew) {
      // Route critical events to a new channel so it is useful immediately.
      const before = new Set(draft.channels.map((c) => c.id))
      const saved = await save.mutateAsync(next)
      const added = saved.channels.find((c) => !before.has(c.id))
      if (added) {
        const routes = { ...saved.routes }
        for (const e of EVENTS.filter((x) => x.critical)) routes[e.key] = [...(routes[e.key] ?? []), added.id]
        next = { ...saved, routes }
        await persist(next, `${added.name} added`)
      } else {
        setDraft(clone(saved))
      }
      return
    }
    await persist(next, `${ch.name} saved`)
  }

  const removeChannel = async (ch: NotificationChannel) => {
    const routes: NotificationSettingsT['routes'] = {}
    for (const [k, ids] of Object.entries(draft.routes)) routes[k as NotificationEvent] = (ids ?? []).filter((id) => id !== ch.id)
    try {
      await persist({ ...draft, channels: draft.channels.filter((c) => c.id !== ch.id), routes }, `${ch.name} removed`)
    } catch (err) {
      toast.error(err, 'Could not remove channel')
    }
  }

  const toggleChannel = async (ch: NotificationChannel) => {
    try {
      await persist({ ...draft, channels: draft.channels.map((c) => (c.id === ch.id ? { ...c, enabled: !c.enabled } : c)) }, `${ch.name} ${ch.enabled ? 'disabled' : 'enabled'}`)
    } catch (err) {
      toast.error(err, 'Could not update channel')
    }
  }

  const sendTest = async (ch: NotificationChannel) => {
    setTesting(ch.id)
    try {
      const body = dirty ? { channel: ch } : { channelId: ch.id }
      const res = await api.post<{ ok: boolean; error?: string }>('/api/notifications/test', body)
      qc.invalidateQueries({ queryKey: opsKeys.notificationLog })
      if (res.ok) toast.success(`Test sent to ${ch.name}`, 'Check that it arrived.')
      else toast.show({ kind: 'error', title: `Test to ${ch.name} failed`, message: res.error })
    } catch (err) {
      toast.error(err, `Test to ${ch.name} failed`)
    } finally {
      setTesting('')
    }
  }

  const newChannel = (type: ChannelType): NotificationChannel => ({
    id: '', type, name: TYPE_LABEL[type], enabled: true, config: type === 'smtp' ? { security: 'tls', port: '465' } : {},
  })

  const toggleRoute = (event: NotificationEvent, channelId: string, on: boolean) => {
    const ids = new Set(draft.routes[event] ?? [])
    if (on) ids.add(channelId)
    else ids.delete(channelId)
    setDraft({ ...draft, routes: { ...draft.routes, [event]: [...ids] } })
  }

  const missingTypes = (['ntfy', 'smtp', 'webhook'] as ChannelType[]).filter((t) => !draft.channels.some((c) => c.type === t))
  const cols = draft.channels.length

  return (
    <>
      <SectionHeader title="Notifications" description="Where to send alerts, and which events are worth waking you up for." />

      <Card
        title="Channels"
        actions={
          isAdmin && (
            <Menu
              trigger={<Button size="sm" variant="ghost" icon="plus" style={{ color: 'var(--ink)' }}>Add channel</Button>}
              items={(['ntfy', 'smtp', 'webhook'] as ChannelType[]).map((t) => ({ label: TYPE_LABEL[t], onSelect: () => setEditing({ open: true, channel: newChannel(t) }) }))}
            />
          )
        }
      >
        {draft.channels.map((ch) => {
          const last = lastByChannel[ch.id]
          const tone = !ch.enabled ? '' : last?.status === 'failed' ? 'danger' : 'ok'
          return (
            <div key={ch.id} className="ops-list-row" style={{ padding: '14px 18px' }}>
              <span className={`ops-dot ${tone}`} title={!ch.enabled ? 'disabled' : last?.status === 'failed' ? 'last delivery failed' : 'active'} />
              <div className="grow">
                <div className="ops-row-title" style={ch.enabled ? undefined : { color: 'var(--ink-subtle)' }}>
                  {ch.name}
                  {!ch.enabled && <span className="small faint" style={{ fontWeight: 400 }}> · disabled</span>}
                </div>
                <div className="ops-row-sub mono truncate">
                  {target(ch)}
                  {last && (
                    <span style={{ fontFamily: 'var(--font-sans)', color: last.status === 'failed' ? 'var(--danger-text)' : undefined }} title={last.error}>
                      {' '}· {last.status === 'failed' ? `failed ${ago(last.at)}` : `last sent ${ago(last.at)}`}
                    </span>
                  )}
                </div>
              </div>
              {isAdmin && (
                <>
                  <Button size="sm" variant="ghost" loading={testing === ch.id} onClick={() => sendTest(ch)}>Send test</Button>
                  <Menu
                    trigger={<IconButton icon="more" bare label="Channel actions" />}
                    items={[
                      { label: 'Edit', icon: 'edit', onSelect: () => setEditing({ open: true, channel: ch }) },
                      { label: ch.enabled ? 'Disable' : 'Enable', icon: 'power', onSelect: () => toggleChannel(ch) },
                      'separator',
                      { label: 'Delete', icon: 'trash', danger: true, onSelect: () => removeChannel(ch) },
                    ]}
                  />
                </>
              )}
            </div>
          )
        })}
        {missingTypes.map((t) => (
          <div key={t} className="ops-list-row" style={{ padding: '14px 18px' }}>
            <span className="ops-hollow-dot" />
            <div className="grow">
              <div className="ops-row-title" style={{ color: 'var(--ink-subtle)' }}>{TYPE_LABEL[t]}</div>
              <div className="ops-row-sub">Not configured · {TYPE_HINT[t]}</div>
            </div>
            {isAdmin && (
              <Button size="sm" variant="ghost" style={{ color: 'var(--ink)' }} onClick={() => setEditing({ open: true, channel: newChannel(t) })}>Set up</Button>
            )}
          </div>
        ))}
      </Card>

      {cols > 0 ? (
        <Card>
          <div className="ops-matrix-head" style={{ ['--ops-cols' as string]: cols }}>
            <span>Event</span>
            {draft.channels.map((ch) => (
              <span key={ch.id} title={ch.name}>{ch.type === 'smtp' && ch.name === TYPE_LABEL.smtp ? 'Email' : ch.name}</span>
            ))}
          </div>
          {EVENTS.map((e) => (
            <div key={e.key} className="ops-matrix-row" style={{ ['--ops-cols' as string]: cols }}>
              <span>
                {e.label}
                {e.critical && draft.quietHours.enabled && <span className="critical">always sent</span>}
              </span>
              {draft.channels.map((ch) => (
                <span key={ch.id} className="cell">
                  <Checkbox
                    checked={(draft.routes[e.key] ?? []).includes(ch.id)}
                    disabled={!isAdmin}
                    onChange={(v) => toggleRoute(e.key, ch.id, v)}
                  />
                </span>
              ))}
            </div>
          ))}
        </Card>
      ) : (
        <Callout>Add a channel to choose which events are sent where.</Callout>
      )}

      <div className="ops-boxed-row">
        <div className="grow">
          <div className="ops-row-title">Quiet hours</div>
          <div className="ops-row-sub" style={{ marginTop: 0 }}>Batch non-critical alerts · down/failed events always go through</div>
        </div>
        <button
          type="button"
          className="ops-chip-btn"
          disabled={!isAdmin}
          onClick={() => {
            setQuiet({ start: draft.quietHours.start, end: draft.quietHours.end })
            setQuietOpen(true)
          }}
        >
          {draft.quietHours.start} – {draft.quietHours.end}
        </button>
        <Toggle
          checked={draft.quietHours.enabled}
          disabled={!isAdmin}
          label="Quiet hours"
          onChange={(v) => setDraft({ ...draft, quietHours: { ...draft.quietHours, enabled: v } })}
        />
      </div>

      {isAdmin && (
        <div className="row end gap-8">
          {dirty && <Button onClick={() => setDraft(clone(settings.data!))}>Discard</Button>}
          <Button
            variant="primary"
            disabled={!dirty}
            loading={save.isPending}
            onClick={async () => {
              try {
                await persist(draft, 'Notification settings saved')
              } catch (err) {
                toast.error(err, 'Could not save notification settings')
              }
            }}
          >
            Save changes
          </Button>
        </div>
      )}

      <ChannelDialog open={editing.open} initial={editing.channel} onClose={() => setEditing({ open: false, channel: null })} onSave={saveChannel} />

      <Dialog
        open={quietOpen}
        onClose={() => setQuietOpen(false)}
        title="Quiet hours"
        description="Non-critical alerts are held during this window and sent as one digest when it ends. Uses the timezone from Settings → General."
        width={420}
        footer={
          <>
            <Button onClick={() => setQuietOpen(false)}>Cancel</Button>
            <Button
              variant="primary"
              disabled={!quiet.start || !quiet.end || quiet.start === quiet.end}
              onClick={() => {
                setDraft({ ...draft, quietHours: { enabled: true, start: quiet.start, end: quiet.end } })
                setQuietOpen(false)
              }}
            >
              Done
            </Button>
          </>
        }
      >
        <div className="grid-2">
          <Field label="From">
            <Input type="time" mono value={quiet.start} onChange={(e) => setQuiet({ ...quiet, start: e.target.value })} />
          </Field>
          <Field label="Until">
            <Input type="time" mono value={quiet.end} onChange={(e) => setQuiet({ ...quiet, end: e.target.value })} />
          </Field>
        </div>
      </Dialog>
    </>
  )
}
