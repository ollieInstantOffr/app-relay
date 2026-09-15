import { useEffect, useRef, useState } from 'react'
import { Button, Callout, Checkbox, Drawer, Field, Icon, IconButton, Input, Segmented, Select, ToggleCard, cx, useToast } from '../../components/ui'
import { useEntities, useRole, useSaveEntity, useSettings } from '../../lib/queries'
import { rid } from '../../lib/format'
import type { Condition, ConditionType, Frontend, FrontendRule } from '../../lib/types'
import { CONDITION_TYPES, applyNowAction, bindKind, fieldErrors, nextFreePort, splitBind, usePreview } from './lbApi'
import { PreviewBlock, PreviewStatus } from './preview'

export default function FrontendDrawer({ initial, onClose }: { initial: Frontend; onClose: () => void }) {
  const [draft, setDraft] = useState<Frontend>(() => JSON.parse(JSON.stringify(initial)))
  const [errors, setErrors] = useState<Record<string, string>>({})
  const { canWrite } = useRole()
  const readOnly = !canWrite
  const isNew = !initial.id
  const backends = useEntities('backends').data ?? []
  const frontends = useEntities('frontends').data ?? []
  const hosts = useEntities('hosts').data ?? []
  const general = useSettings('general').data
  const haproxy = useSettings('haproxy').data
  const save = useSaveEntity('frontends')
  const toast = useToast()
  const preview = usePreview('/api/preview/lb/frontend', readOnly ? null : { frontend: draft })
  const [armed, setArmed] = useState<number | null>(null)
  const [dragIndex, setDragIndex] = useState<number | null>(null)
  const [overIndex, setOverIndex] = useState<number | null>(null)

  const http = draft.mode === 'http'
  const err = (k: string) => errors[k]
  const set = (patch: Partial<Frontend>) => setDraft((d) => ({ ...d, ...patch }))
  const setRule = (i: number, patch: Partial<FrontendRule>) => setDraft((d) => ({ ...d, rules: d.rules.map((r, j) => (j === i ? { ...r, ...patch } : r)) }))
  const setCond = (i: number, j: number, patch: Partial<Condition>) =>
    setDraft((d) => ({
      ...d,
      rules: d.rules.map((r, k) => (k === i ? { ...r, conditions: r.conditions.map((c, l) => (l === j ? { ...c, ...patch } : c)) } : r)),
    }))
  const defaultCond = (): Condition => ({ type: http ? 'host' : 'sni', value: '', negate: false })
  const addRule = () => set({ rules: [...draft.rules, { id: rid(), conditions: [defaultCond()], backendId: '' }] })
  const addCond = (i: number) => setRule(i, { conditions: [...draft.rules[i].conditions, { ...defaultCond(), type: http ? 'path_beg' : 'src' }] })
  const removeCond = (i: number, j: number) => {
    const conds = draft.rules[i].conditions.filter((_, l) => l !== j)
    if (conds.length === 0) set({ rules: draft.rules.filter((_, k) => k !== i) })
    else setRule(i, { conditions: conds })
  }
  const endDrag = () => {
    setArmed(null)
    setDragIndex(null)
    setOverIndex(null)
  }
  const move = (from: number, to: number) =>
    setDraft((d) => {
      const rules = [...d.rules]
      const [r] = rules.splice(from, 1)
      rules.splice(to, 0, r)
      return { ...d, rules }
    })

  const { addr, port } = splitBind(draft.bind)
  const bindPort = port ?? nextFreePort(frontends, haproxy?.exposePortStart ?? 10080, draft.id)
  const lanIp = general?.publicIp && bindKind(general.publicIp) === 'lan' ? general.publicIp : ''
  const presets = [
    { addr: '127.0.0.1', label: '127.0.0.1 — via reverse proxy' },
    ...(lanIp ? [{ addr: lanIp, label: `LAN ${lanIp}` }] : []),
    { addr: '0.0.0.0', label: '0.0.0.0 — public' },
  ]
  const host = hosts.find((h) => h.id === draft.hostId)

  const backendOptions = backends.map((b) => ({
    value: b.id,
    label: http && b.mode === 'tcp' ? `${b.name} (TCP)` : b.name,
    disabled: http && b.mode === 'tcp',
  }))

  const onSave = async () => {
    try {
      const saved = await save.mutateAsync(draft)
      toast.show({ kind: 'success', title: 'Frontend saved', message: `${saved.name} added to pending changes.`, actions: [applyNowAction] })
      onClose()
    } catch (e) {
      const f = fieldErrors(e)
      if (Object.keys(f).length) setErrors(f)
      else toast.error(e, 'Could not save frontend')
    }
  }
  const saveRef = useRef(onSave)
  saveRef.current = onSave
  useEffect(() => {
    const h = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 's') {
        e.preventDefault()
        if (!readOnly) saveRef.current()
      }
    }
    window.addEventListener('keydown', h)
    return () => window.removeEventListener('keydown', h)
  }, [readOnly])

  return (
    <Drawer
      open
      onClose={onClose}
      width="wide"
      title={
        isNew ? (
          'New frontend'
        ) : (
          <span className="row gap-10" style={{ alignItems: 'baseline' }}>
            {readOnly ? 'Frontend' : 'Edit frontend'} <span className="mono muted" style={{ fontSize: 14, fontWeight: 500 }}>{initial.name}</span>
          </span>
        )
      }
      subtitle="A listening port and the rules that choose a backend"
      footer={
        <>
          <PreviewStatus state={preview} readOnly={readOnly} />
          <span className="spacer" />
          <Button size="md" onClick={onClose}>{readOnly ? 'Close' : 'Cancel'}</Button>
          {!readOnly && <Button size="md" variant="primary" loading={save.isPending} onClick={onSave}>Save to pending</Button>}
        </>
      }
    >
      <div className="grid-2">
        <Field label="Name" error={err('name')}>
          <Input mono autoFocus={isNew} placeholder="internal-http" value={draft.name} disabled={readOnly} invalid={!!err('name')} onChange={(e) => set({ name: e.target.value })} />
        </Field>
        <Field label="Mode" error={err('mode')}>
          <div className="lb-seg-full">
            <Segmented
              value={draft.mode}
              disabled={readOnly}
              onChange={(mode) => set({ mode, compression: mode === 'tcp' ? false : draft.compression })}
              options={[{ value: 'http', label: 'HTTP' }, { value: 'tcp', label: 'TCP' }]}
            />
          </div>
        </Field>
      </div>

      <Field label="Bind" error={err('bind')} hint={bindKind(addr) === 'public' ? 'Binding to all interfaces exposes this port to the internet if your router forwards it.' : undefined}>
        <Input mono value={draft.bind} placeholder="127.0.0.1:10080" disabled={readOnly} invalid={!!err('bind')} onChange={(e) => set({ bind: e.target.value })} />
        {!readOnly && (
          <div className="row wrap gap-6" style={{ marginTop: 2 }}>
            {presets.map((p) => (
              <button key={p.addr} type="button" className={cx('lb-chip', addr === p.addr && 'active')} onClick={() => set({ bind: `${p.addr}:${bindPort}` })}>
                {p.label}
              </button>
            ))}
          </div>
        )}
      </Field>

      <div className="col gap-8">
        <div className="lb-section-label">
          <label className="field-label">Routing rules</label>
          <span className="aside">First match wins</span>
        </div>
        {draft.rules.map((r, i) => (
          <div
            key={r.id}
            className={cx('lb-rule', dragIndex === i && 'dragging', dragIndex !== null && overIndex === i && dragIndex !== i && 'drop-target')}
            draggable={!readOnly && armed === i}
            onDragStart={(e) => {
              setDragIndex(i)
              e.dataTransfer.effectAllowed = 'move'
              e.dataTransfer.setData('text/plain', String(i))
            }}
            onDragOver={(e) => {
              if (dragIndex === null) return
              e.preventDefault()
              setOverIndex(i)
            }}
            onDrop={(e) => {
              e.preventDefault()
              if (dragIndex !== null && dragIndex !== i) move(dragIndex, i)
              endDrag()
            }}
            onDragEnd={endDrag}
          >
            <span className="drag-handle" style={{ lineHeight: '32px' }} title="Drag to reorder" onMouseDown={() => setArmed(i)} onMouseUp={() => setArmed(null)}>
              <Icon name="drag" size={14} />
            </span>
            <div className="col gap-4" style={{ minWidth: 0 }}>
              {r.conditions.map((c, j) => {
                const meta = CONDITION_TYPES.find((t) => t.value === c.type)
                const e = err(`rules.${i}.conditions.${j}.value`) || err(`rules.${i}.conditions.${j}.type`) || err(`rules.${i}.conditions.${j}.name`)
                return (
                  <div key={j} className="col gap-2">
                    {j > 0 && <div className="lb-and">and</div>}
                    <div className={cx('lb-cond', c.type === 'header' && 'with-name')}>
                      <Select
                        value={c.type}
                        disabled={readOnly}
                        invalid={!!err(`rules.${i}.conditions.${j}.type`)}
                        options={CONDITION_TYPES.filter((t) => t.mode === 'both' || t.mode === draft.mode || t.value === c.type).map((t) => ({ value: t.value, label: t.label }))}
                        onChange={(v) => setCond(i, j, { type: v as ConditionType })}
                      />
                      {c.type === 'header' && (
                        <Input mono placeholder="X-Header" value={c.name ?? ''} disabled={readOnly} invalid={!!err(`rules.${i}.conditions.${j}.name`)} onChange={(ev) => setCond(i, j, { name: ev.target.value })} />
                      )}
                      <Input mono placeholder={meta?.placeholder} value={c.value} disabled={readOnly} invalid={!!err(`rules.${i}.conditions.${j}.value`)} onChange={(ev) => setCond(i, j, { value: ev.target.value })} />
                      <label className="lb-not" title="Negate this condition">
                        <Checkbox checked={c.negate} disabled={readOnly} onChange={(v) => setCond(i, j, { negate: v })} />
                        not
                      </label>
                      {!readOnly ? <IconButton icon="close" bare size={12} label="Remove condition" onClick={() => removeCond(i, j)} /> : <span />}
                    </div>
                    {e && <div className="field-error">{e}</div>}
                  </div>
                )
              })}
              {!readOnly && (
                <button type="button" className="lb-link" style={{ alignSelf: 'flex-start', fontSize: 11, color: 'var(--ink-subtle)' }} onClick={() => addCond(i)}>
                  + and
                </button>
              )}
            </div>
            <span className="lb-arrow">→</span>
            <div className="col gap-2">
              <Select
                value={r.backendId}
                disabled={readOnly}
                placeholder="backend…"
                invalid={!!err(`rules.${i}.backendId`)}
                options={backendOptions}
                onChange={(v) => setRule(i, { backendId: v })}
              />
              {err(`rules.${i}.backendId`) && <div className="field-error">{err(`rules.${i}.backendId`)}</div>}
            </div>
            {!readOnly ? <IconButton icon="close" bare size={14} label="Remove rule" onClick={() => set({ rules: draft.rules.filter((_, k) => k !== i) })} /> : <span />}
          </div>
        ))}
        <div className="lb-otherwise">
          <span />
          <span>{draft.rules.length ? 'otherwise' : 'all traffic'}</span>
          <span className="lb-arrow">→</span>
          <Select
            value={draft.defaultBackendId ?? ''}
            disabled={readOnly}
            placeholder="none (503)"
            invalid={!!err('defaultBackendId')}
            options={backendOptions}
            onChange={(v) => set({ defaultBackendId: v || undefined })}
          />
          <span />
        </div>
        {err('defaultBackendId') && <div className="field-error">{err('defaultBackendId')}</div>}
        {!readOnly && (
          <div className="row gap-10">
            <button type="button" className="lb-link" onClick={addRule}>+ Add rule</button>
            <span className="field-hint">{http ? 'Host · Path · Header · Source IP' : 'SNI · Source IP'} {http ? '· SNI (TCP)' : '· Host/Path need HTTP'}</span>
          </div>
        )}
      </div>

      <div className="grid-2">
        <ToggleCard title="Accept PROXY protocol" description="When fed by a proxy that sends it" checked={draft.acceptProxy} disabled={readOnly} onChange={(v) => set({ acceptProxy: v })} />
        {http && <ToggleCard title="Compression" description="gzip text/* and json" checked={draft.compression} disabled={readOnly} onChange={(v) => set({ compression: v })} />}
        <ToggleCard title="Enabled" description="Disabled frontends are left out of the load balancer config" checked={draft.enabled} disabled={readOnly} onChange={(v) => set({ enabled: v })} />
      </div>

      {host && (
        <Callout tone="info">
          Created by the Expose wizard for <span className="mono">{host.domains[0]}</span>. The reverse proxy forwards that host to {draft.bind}; changing the bind here breaks it until the host is updated.
        </Callout>
      )}

      <PreviewBlock what="frontend" state={preview} readOnly={readOnly} />
      {preview.data && (preview.data.checked === 'haproxy' || preview.data.checked === 'balancer') && !preview.data.valid && preview.data.output && (
        <pre className="code wrap" style={{ maxHeight: 200 }}>{preview.data.output}</pre>
      )}
    </Drawer>
  )
}
