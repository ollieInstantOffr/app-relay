// Settings → General (design 15a).
import { useEffect, useMemo, useRef, useState } from 'react'
import { Button, Card, Field, Input, SectionHeader, Select, Skeleton, Toggle, ToggleRow, useToast } from '../../components/ui'
import { useEntities, useRole, useSaveSettings, useSettings } from '../../lib/queries'
import type { GeneralSettings as General, HostDefaults } from '../../lib/types'
import NotAllowed from '../auth/NotAllowed'
import { fieldErrors } from '../auth/authApi'
import '../auth/auth.css'

const comparable = (g: General | null | undefined) => (g ? JSON.stringify({ ...g, publicIp: '' }) : '')

export default function GeneralSettings() {
  const { data, isLoading } = useSettings('general')
  const save = useSaveSettings('general')
  const lists = useEntities('access-lists').data ?? []
  const { isAdmin } = useRole()
  const toast = useToast()
  const [draft, setDraft] = useState<General | null>(null)
  const [errors, setErrors] = useState<Record<string, string>>({})
  const lastData = useRef('')

  // Take server data unless the user has unsaved edits.
  useEffect(() => {
    if (!data) return
    setDraft((prev) => (prev === null || comparable(prev) === lastData.current ? data : prev))
    lastData.current = comparable(data)
  }, [data])

  const timezones = useMemo(() => {
    const all = typeof Intl.supportedValuesOf === 'function' ? Intl.supportedValuesOf('timeZone') : []
    const set = new Set(['UTC', ...all])
    if (draft?.timezone) set.add(draft.timezone)
    return [...set]
  }, [draft?.timezone])

  if (isLoading || !draft) {
    return (
      <>
        <SectionHeader title="General" description="Instance identity, listening ports and defaults for new hosts." />
        <Skeleton height={180} />
        <Skeleton height={200} />
      </>
    )
  }

  const dirty = comparable(draft) !== comparable(data)
  const disabled = !isAdmin
  const set = <K extends keyof General>(k: K, v: General[K]) => setDraft((d) => (d ? { ...d, [k]: v } : d))
  const setDefault = <K extends keyof HostDefaults>(k: K, v: HostDefaults[K]) => setDraft((d) => (d ? { ...d, defaults: { ...d.defaults, [k]: v } } : d))
  const port = (v: string) => (v === '' ? 0 : Math.max(0, Math.min(65535, Math.trunc(Number(v)) || 0)))
  const lanList = lists.find((l) => l.name === 'lan-only') ?? lists[0]

  const applyAction = { label: 'Apply now', onClick: () => window.dispatchEvent(new CustomEvent('relay:apply')) }

  // Relay opens the new admin port next to the old one. When this page was
  // opened on the old port directly, follow it as soon as the browser can reach
  // the new port; the old port closes by itself after that.
  const followAdminPort = async (next: General, from: number) => {
    const target = new URL(window.location.href)
    target.port = String(next.adminPort)
    const id = toast.show({ kind: 'progress', title: `Moving the admin UI to port ${next.adminPort}…`, message: 'Waiting for Relay to answer on the new port', progress: 30 })
    for (let i = 0; i < 20; i++) {
      try {
        await fetch(`${target.origin}/healthz`, { mode: 'no-cors', cache: 'no-store' })
        toast.update(id, { progress: 100, message: 'Reconnecting…' })
        window.location.href = target.toString()
        return
      } catch {
        await new Promise((r) => setTimeout(r, 1000))
      }
    }
    toast.dismiss(id)
    toast.show({
      kind: 'warning',
      title: `Can't reach port ${next.adminPort} from this browser`,
      message: `Relay is listening on ${next.adminPort}, but requests from here don't get through (firewall?). Port ${from} stays open until ${next.adminPort} works.`,
      duration: 0,
      actions: [
        { label: 'Try again', primary: true, onClick: () => void followAdminPort(next, from) },
        {
          label: `Keep port ${from}`,
          onClick: async () => {
            try {
              const back = await save.mutateAsync({ ...next, adminPort: from })
              lastData.current = comparable(back)
              setDraft(back)
              toast.success('Admin UI stays on port ' + from)
            } catch (err) {
              toast.error(err, 'Couldn’t switch back')
            }
          },
        },
      ],
    })
  }

  const onSave = async () => {
    setErrors({})
    const before = data
    try {
      const saved = await save.mutateAsync(draft)
      lastData.current = comparable(saved)
      setDraft(saved)
      const portMoved = !!before && before.adminPort !== saved.adminPort
      if (portMoved && window.location.port === String(before.adminPort)) {
        void followAdminPort(saved, before.adminPort)
        return
      }
      toast.show({
        kind: 'success',
        title: 'Settings saved',
        message: portMoved
          ? `Relay now also listens on port ${saved.adminPort}. Apply to point the admin domain at it — port ${before.adminPort} closes once ${saved.adminPort} is in use.`
          : 'Changes that affect the proxy were added to pending changes.',
        actions: [applyAction],
      })
    } catch (err) {
      const f = fieldErrors(err)
      setErrors(f)
      if (!Object.keys(f).length) toast.error(err, 'Settings not saved')
    }
  }

  const portRow = (key: 'httpPort' | 'httpsPort' | 'adminPort', title: string, description: string) => (
    <div className="toggle-row" style={{ flexWrap: 'wrap' }}>
      <div className="grow">
        <div className="toggle-title">{title}</div>
        <div className="toggle-desc">{description}</div>
      </div>
      <Input
        mono
        inputSize="sm"
        className="port-input"
        type="number"
        min={1}
        max={65535}
        aria-label={`${title} port`}
        value={draft[key] || ''}
        invalid={!!errors[key]}
        disabled={disabled}
        onChange={(e) => set(key, port(e.target.value))}
      />
      {errors[key] && (
        <div className="field-error" style={{ flexBasis: '100%', textAlign: 'right' }}>
          {errors[key]}
        </div>
      )}
    </div>
  )

  return (
    <>
      <SectionHeader title="General" description="Instance identity, listening ports and defaults for new hosts." />
      {!isAdmin && <NotAllowed what="settings" />}

      <Card title="Instance">
        <div className="card-body grid-2" style={{ gap: 14, padding: '16px 18px' }}>
          <Field label="Instance name" htmlFor="gen-name" error={errors.instanceName}>
            <Input id="gen-name" value={draft.instanceName} invalid={!!errors.instanceName} disabled={disabled} onChange={(e) => set('instanceName', e.target.value)} />
          </Field>
          <Field label="Admin UI domain" htmlFor="gen-domain" error={errors.adminDomain}>
            <Input
              id="gen-domain"
              mono
              placeholder="proxy.home.lan"
              autoCapitalize="none"
              spellCheck={false}
              value={draft.adminDomain}
              invalid={!!errors.adminDomain}
              disabled={disabled}
              onChange={(e) => set('adminDomain', e.target.value)}
            />
          </Field>
          <Field label="Timezone" htmlFor="gen-tz" error={errors.timezone}>
            <Select id="gen-tz" value={draft.timezone} options={timezones} invalid={!!errors.timezone} disabled={disabled} onChange={(v) => set('timezone', v)} />
          </Field>
          <Field label="Public IP (detected)" htmlFor="gen-ip">
            <Input id="gen-ip" mono className="ro-input" readOnly value={draft.publicIp || '—'} />
          </Field>
        </div>
      </Card>

      <Card title="Listening ports">
        {portRow('httpPort', 'HTTP', 'Redirects to HTTPS when Force HTTPS is on')}
        {portRow('httpsPort', 'HTTPS', draft.http3 ? 'HTTP/2 and HTTP/3 (QUIC) enabled' : 'HTTP/2 enabled')}
        <ToggleRow
          title="HTTP/3 (QUIC)"
          description={`Needs UDP ${draft.httpsPort || 443} forwarded on your router in addition to TCP · hosts can override it${draft.proxyEngine === 'edge' ? ' · built into Relay Edge' : ''}`}
          checked={draft.http3}
          disabled={disabled}
          onChange={(v) => set('http3', v)}
        />
        {portRow('adminPort', 'Admin UI', 'Relay listens here directly · the admin domain proxies to it · changes take effect without a restart')}
      </Card>

      <Card title="Defaults for new hosts">
        <div className="defaults-grid">
          <div className="row">
            <div className="grow">Force HTTPS</div>
            <Toggle label="Force HTTPS" checked={draft.defaults.forceHttps} disabled={disabled} onChange={(v) => setDefault('forceHttps', v)} />
          </div>
          <div className="row">
            <div className="grow">Block common exploits</div>
            <Toggle label="Block common exploits" checked={draft.defaults.blockExploits} disabled={disabled} onChange={(v) => setDefault('blockExploits', v)} />
          </div>
          <div className="row">
            <div className="grow">Websockets</div>
            <Toggle label="Websockets" checked={draft.defaults.websockets} disabled={disabled} onChange={(v) => setDefault('websockets', v)} />
          </div>
          <div className="row">
            <div className="grow">HTTP/2</div>
            <Toggle label="HTTP/2" checked={draft.defaults.http2} disabled={disabled} onChange={(v) => setDefault('http2', v)} />
          </div>
          <div className="row" style={{ gridColumn: '1 / -1' }}>
            <div className="grow row gap-8">
              Attach access list
              {draft.defaults.accessListId ? (
                <Select
                  inputSize="sm"
                  mono
                  style={{ width: 'auto' }}
                  value={draft.defaults.accessListId}
                  disabled={disabled}
                  options={[
                    ...(lists.some((l) => l.id === draft.defaults.accessListId) ? [] : [{ value: draft.defaults.accessListId, label: '(deleted list)' }]),
                    ...lists.map((l) => ({ value: l.id, label: l.name })),
                  ]}
                  onChange={(v) => setDefault('accessListId', v)}
                />
              ) : lanList ? (
                <span className="mono small muted">{lanList.name}</span>
              ) : (
                <span className="small faint">no access lists yet</span>
              )}
            </div>
            <Toggle
              label="Attach access list"
              checked={!!draft.defaults.accessListId}
              disabled={disabled || (!draft.defaults.accessListId && !lanList)}
              onChange={(v) => setDefault('accessListId', v ? lanList?.id ?? '' : '')}
            />
          </div>
          {errors['defaults.accessListId'] && <div className="field-error" style={{ gridColumn: '1 / -1' }}>{errors['defaults.accessListId']}</div>}
        </div>
      </Card>

      {isAdmin && (
        <div className="settings-footer">
          {dirty && (
            <Button
              variant="ghost"
              onClick={() => {
                setDraft(data ?? null)
                setErrors({})
              }}
            >
              Discard
            </Button>
          )}
          <Button variant="primary" size="md" disabled={!dirty} loading={save.isPending} onClick={onSave}>
            Save changes
          </Button>
        </div>
      )}
    </>
  )
}
