// Settings → HAProxy engine (design 16a). Owner: slice lb.
import { useEffect, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Button, Card, ConfirmDialog, Dot, Field, Input, Select, SectionHeader, Skeleton, Toggle, ToggleRow, useToast } from '../../components/ui'
import { api } from '../../lib/api'
import { keys, useEngine, useEntities, useRole, useSaveSettings, useSettings } from '../../lib/queries'
import { ago } from '../../lib/format'
import type { HAProxySettings as Settings } from '../../lib/types'
import ConfigDrawer from '../loadbalancer/ConfigDrawer'
import { applyNowAction, fieldErrors, useHAProxyConfig, type ValidationResult } from '../loadbalancer/lbApi'
import '../loadbalancer/loadbalancer.css'

function isLTS(version: string) {
  const m = version.match(/^(\d+)\.(\d+)/)
  return !!m && Number(m[1]) >= 2 && Number(m[2]) % 2 === 0
}

export default function HAProxySettings() {
  const { data, isLoading } = useSettings('haproxy')
  const save = useSaveSettings('haproxy')
  const engine = useEngine('haproxy')
  const lists = useEntities('access-lists').data ?? []
  const cfg = useHAProxyConfig(true).data
  const { isAdmin, canWrite } = useRole()
  const toast = useToast()
  const qc = useQueryClient()
  const [draft, setDraft] = useState<Settings | null>(null)
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [validation, setValidation] = useState<ValidationResult | null>(null)
  const [validating, setValidating] = useState(false)
  const [reloading, setReloading] = useState(false)
  const [toggling, setToggling] = useState(false)
  const [confirmStop, setConfirmStop] = useState(false)
  const [showCfg, setShowCfg] = useState(false)

  useEffect(() => {
    if (data && !draft) setDraft(data)
  }, [data, draft])

  const validate = async (quiet: boolean) => {
    setValidating(true)
    try {
      const v = await api.post<ValidationResult>('/api/haproxy/validate')
      setValidation(v)
      if (!quiet) {
        if (v.valid) {
          toast.show({
            kind: 'success',
            title: 'haproxy.cfg is valid',
            message: `${v.checked === 'haproxy' ? `haproxy -c passed in ${v.durationMs} ms` : 'Checked by Relay · HAProxy agent offline'} · ${v.lines} lines`,
          })
        } else {
          toast.show({ kind: 'error', title: 'haproxy.cfg is invalid', message: v.output })
        }
      }
    } catch (err) {
      if (!quiet) toast.error(err, 'Validation failed')
    } finally {
      setValidating(false)
    }
  }

  useEffect(() => {
    if (canWrite) validate(true)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [canWrite])

  if (isLoading || !draft) {
    return (
      <>
        <SectionHeader title="HAProxy engine" description="Global defaults for the load balancer. Per-backend settings override these." />
        <Skeleton height={320} />
      </>
    )
  }

  const readOnly = !isAdmin
  const set = (p: Partial<Settings>) => setDraft((d) => (d ? { ...d, ...p } : d))
  const dirty = JSON.stringify(draft) !== JSON.stringify(data)
  const err = (k: string) => errors[k]
  const num = (v: string) => Number(v.replace(/\D/g, '')) || 0

  const engineAction = async (action: 'start' | 'stop') => {
    setToggling(true)
    try {
      await api.post(`/api/engines/haproxy/${action}`)
      toast.success(action === 'start' ? 'HAProxy started' : 'HAProxy stopped')
    } catch (e) {
      toast.error(e, `Could not ${action} HAProxy`)
    } finally {
      setToggling(false)
      qc.invalidateQueries({ queryKey: keys.engines })
    }
  }

  const reload = async () => {
    setReloading(true)
    try {
      await api.post('/api/engines/haproxy/reload')
      toast.show({ kind: 'success', title: 'HAProxy reloaded', message: 'The live config was reloaded. Pending changes still need Apply.' })
      qc.invalidateQueries({ queryKey: keys.engines })
    } catch (e) {
      toast.error(e, 'Reload failed')
    } finally {
      setReloading(false)
    }
  }

  const onSave = async () => {
    try {
      const saved = await save.mutateAsync(draft)
      setDraft(saved)
      setErrors({})
      qc.invalidateQueries({ queryKey: ['haproxy'] })
      toast.show({ kind: 'success', title: 'HAProxy settings saved', message: 'Added to pending changes.', actions: [applyNowAction] })
    } catch (e) {
      const f = fieldErrors(e)
      if (Object.keys(f).length) setErrors(f)
      else toast.error(e, 'Could not save settings')
    }
  }

  const lines = validation?.lines ?? cfg?.lines
  const configTone = validation ? (validation.valid ? (validation.checked === 'haproxy' ? 'ok' : 'muted') : 'danger') : 'muted'
  const configText = validation
    ? validation.valid
      ? `${validation.checked === 'haproxy' ? 'valid' : 'not checked'} · ${lines} lines`
      : 'invalid'
    : lines !== undefined
      ? `${lines} lines`
      : '—'
  const running = !!engine?.running
  const statsList = lists.find((l) => l.id === draft.statsAccessListId)

  return (
    <>
      <div className="row-top gap-16">
        <div className="grow">
          <div className="h1">HAProxy engine</div>
          <div className="muted" style={{ marginTop: 4 }}>Global defaults for the load balancer. Per-backend settings override these.</div>
        </div>
        <div className="row gap-10 medium" title={!engine?.reachable ? engine?.error || 'HAProxy agent not reachable' : undefined}>
          {engine?.reachable ? (running ? 'Running' : 'Stopped') : 'Unreachable'}
          <Toggle checked={running} disabled={!isAdmin || !engine?.reachable || toggling} onChange={(v) => (v ? engineAction('start') : setConfirmStop(true))} label="Run HAProxy" />
        </div>
      </div>

      <div className="grid-3">
        <div className="lb-mini">
          <div className="k">Version</div>
          <div className="v">{engine?.version ? `${engine.version}${isLTS(engine.version) ? ' · LTS' : ''}` : '—'}</div>
        </div>
        <div className="lb-mini">
          <div className="k">Last reload</div>
          <div className="v">{engine?.lastReloadAt ? `${ago(engine.lastReloadAt)} · ${engine.lastReloadMs} ms` : '—'}</div>
        </div>
        <div className="lb-mini">
          <div className="k">Config</div>
          <div className="v" title={validation && !validation.valid ? validation.output : undefined}>
            <Dot tone={configTone} />
            {configText}
          </div>
        </div>
      </div>

      <Card title="Default timeouts & limits">
        <div className="card-body grid-3" style={{ gap: 14 }}>
          <Field label="Connect" error={err('timeoutConnect')}>
            <Input mono value={draft.timeoutConnect} disabled={readOnly} invalid={!!err('timeoutConnect')} onChange={(e) => set({ timeoutConnect: e.target.value })} />
          </Field>
          <Field label="Client" error={err('timeoutClient')}>
            <Input mono value={draft.timeoutClient} disabled={readOnly} invalid={!!err('timeoutClient')} onChange={(e) => set({ timeoutClient: e.target.value })} />
          </Field>
          <Field label="Server" error={err('timeoutServer')}>
            <Input mono value={draft.timeoutServer} disabled={readOnly} invalid={!!err('timeoutServer')} onChange={(e) => set({ timeoutServer: e.target.value })} />
          </Field>
          <Field label="Max connections" error={err('maxConn')}>
            <Input mono inputMode="numeric" value={draft.maxConn || ''} disabled={readOnly} invalid={!!err('maxConn')} onChange={(e) => set({ maxConn: num(e.target.value) })} />
          </Field>
          <Field label="Health check interval" error={err('checkInterval')}>
            <Input mono value={draft.checkInterval} disabled={readOnly} invalid={!!err('checkInterval')} onChange={(e) => set({ checkInterval: e.target.value })} />
          </Field>
          <Field label="Rise / fall" error={err('rise') ?? err('fall')}>
            <div className="lb-risefall">
              <Input mono inputMode="numeric" aria-label="Rise" value={draft.rise || ''} disabled={readOnly} invalid={!!err('rise')} onChange={(e) => set({ rise: num(e.target.value) })} />
              <span className="faint">/</span>
              <Input mono inputMode="numeric" aria-label="Fall" value={draft.fall || ''} disabled={readOnly} invalid={!!err('fall')} onChange={(e) => set({ fall: num(e.target.value) })} />
            </div>
          </Field>
        </div>
      </Card>

      <Card title="Reload & observability">
        <ToggleRow
          title="Seamless reloads"
          description="Hand off listening sockets to the new process · no dropped connections"
          checked={draft.seamlessReload}
          disabled={readOnly}
          onChange={(v) => set({ seamlessReload: v })}
        />
        <ToggleRow
          title="Stats endpoint"
          description={
            <>
              Feeds the Prometheus scrape and HAProxy's own stats page · {statsList ? <>protected by <span className="mono">{statsList.name}</span></> : 'no access list'}
            </>
          }
        >
          <div className="row gap-10">
            <div style={{ width: 160 }}>
              <Input mono inputSize="sm" value={draft.statsBind} disabled={readOnly || !draft.statsEnabled} invalid={!!err('statsBind')} onChange={(e) => set({ statsBind: e.target.value })} />
            </div>
            <Toggle checked={draft.statsEnabled} disabled={readOnly} onChange={(v) => set({ statsEnabled: v })} label="Stats endpoint" />
          </div>
        </ToggleRow>
        {err('statsBind') && <div className="field-error" style={{ padding: '0 18px 10px' }}>{err('statsBind')}</div>}
        {draft.statsEnabled && (
          <ToggleRow title="Stats access list" description="Who may open the stats port">
            <div style={{ width: 220 }}>
              <Select
                inputSize="sm"
                value={draft.statsAccessListId ?? ''}
                placeholder="No restriction"
                disabled={readOnly}
                invalid={!!err('statsAccessListId')}
                options={lists.map((l) => ({ value: l.id, label: l.name }))}
                onChange={(v) => set({ statsAccessListId: v || undefined })}
              />
            </div>
          </ToggleRow>
        )}
        <ToggleRow
          title="Prometheus metrics"
          description={<><span className="mono">/metrics</span> on the stats port</>}
          checked={draft.prometheus && draft.statsEnabled}
          disabled={readOnly || !draft.statsEnabled}
          onChange={(v) => set({ prometheus: v })}
        />
        <ToggleRow title="Expose wizard ports" description="Localhost frontends for exposed backends use the first free port from here">
          <div style={{ width: 110 }}>
            <Input mono inputSize="sm" inputMode="numeric" value={draft.exposePortStart || ''} disabled={readOnly} invalid={!!err('exposePortStart')} onChange={(e) => set({ exposePortStart: num(e.target.value) })} />
          </div>
        </ToggleRow>
        {err('exposePortStart') && <div className="field-error" style={{ padding: '0 18px 10px' }}>{err('exposePortStart')}</div>}
      </Card>

      <div className="row gap-10">
        <Button size="md" onClick={() => setShowCfg(true)}>View full haproxy.cfg</Button>
        {canWrite && <Button size="md" loading={validating} onClick={() => validate(false)}>Validate</Button>}
        {canWrite && <Button size="md" loading={reloading} disabled={!engine?.reachable || !running} onClick={reload}>Reload now</Button>}
        <span className="spacer" />
        {readOnly ? (
          <span className="small muted">Only admins can change engine settings</span>
        ) : (
          <Button size="md" variant="primary" loading={save.isPending} disabled={!dirty} onClick={onSave}>Save changes</Button>
        )}
      </div>

      <ConfigDrawer open={showCfg} onClose={() => setShowCfg(false)} />
      <ConfirmDialog
        open={confirmStop}
        onClose={() => setConfirmStop(false)}
        danger
        title="Stop HAProxy?"
        message="All load-balanced traffic, including backends exposed through the reverse proxy, stops until you start it again."
        confirmLabel="Stop HAProxy"
        onConfirm={() => engineAction('stop')}
      />
    </>
  )
}
