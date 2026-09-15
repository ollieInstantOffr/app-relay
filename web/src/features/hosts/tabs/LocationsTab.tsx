// Owner: slice hosts. Host drawer · Locations tab (design 18a).
import { useEffect, useState } from 'react'
import { Badge, Field, Icon, IconButton, Input, Segmented, Select, ToggleCard, cx } from '../../../components/ui'
import { useEntities } from '../../../lib/queries'
import { pluralize, upstreamUrl } from '../../../lib/format'
import type { AccessList, Location, LocationKind, Upstream } from '../../../lib/types'
import { ConfigPreviewPanel } from '../ConfigPreview'
import type { HostFormCtx } from '../HostDrawer'
import { newLocation, portError, upstreamHostError } from '../lib'

function summary(l: Location, lists: AccessList[]): string {
  const target = l.kind === 'deny' ? '403 deny' : l.kind === 'same' ? 'same upstream' : l.upstream.host ? upstreamUrl(l.upstream) : 'upstream not set'
  const flags: string[] = []
  if (l.noAuth) flags.push('no auth')
  else if (l.accessListId) flags.push(lists.find((x) => x.id === l.accessListId)?.name ?? 'other access list')
  if (l.kind === 'proxy' && l.websockets) flags.push('ws')
  if (l.kind === 'proxy' && l.stripPrefix) flags.push('strip prefix')
  if (l.kind !== 'deny' && l.cache) flags.push('cache')
  if (l.kind !== 'deny' && l.headers.length) flags.push(pluralize(l.headers.length, 'header'))
  return ['→ ' + target, ...flags].join(' · ')
}

export function LocationsTab({ ctx }: { ctx: HostFormCtx }) {
  const { draft, update, errors, readOnly, preview } = ctx
  const lists = useEntities('access-lists').data ?? []
  const [expanded, setExpanded] = useState<string | null>(null)
  const [drag, setDrag] = useState<{ from: number; over: number | null } | null>(null)
  const locs = draft.locations

  const hasErr = (i: number) => Object.keys(errors).some((k) => k.startsWith(`locations.${i}.`))
  useEffect(() => {
    const i = locs.findIndex((_, j) => hasErr(j))
    if (i >= 0) setExpanded(locs[i].id)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [errors])

  const setLoc = (i: number, patch: Partial<Location>) => update({ locations: locs.map((l, j) => (j === i ? { ...l, ...patch } : l)) })
  const remove = (i: number) => update({ locations: locs.filter((_, j) => j !== i) })
  const add = () => {
    const l = newLocation()
    update({ locations: [...locs, l] })
    setExpanded(l.id)
  }
  const move = (from: number, to: number) => {
    if (from === to) return
    const next = [...locs]
    const [x] = next.splice(from, 1)
    next.splice(to, 0, x)
    update({ locations: next })
  }

  return (
    <>
      <div className="small muted">
        Route paths to different upstreams or add headers. <span className="mono">/</span> is the host's default upstream. Longest prefix wins.
      </div>
      <div className="hosts-loc-list">
        <div className="hosts-loc default">
          <div className="hosts-loc-head">
            <span className="drag-handle" style={{ visibility: 'hidden' }}>
              <Icon name="drag" size={14} />
            </span>
            <span className="path">/</span>
            <span className="target truncate">→ {draft.upstream.host ? upstreamUrl(draft.upstream) : 'set on the Details tab'}</span>
            <span className="faint small nowrap">(default)</span>
          </div>
        </div>
        {locs.map((l, i) => {
          const open = expanded === l.id
          const err = (field: string) => errors[`locations.${i}.${field}`]
          return (
            <div
              key={l.id}
              className={cx(
                'hosts-loc',
                open && 'expanded',
                hasErr(i) && 'invalid',
                drag?.from === i && 'dragging',
                drag && drag.over === i && drag.from !== i && 'drag-over',
                drag && drag.over === i && drag.from < i && 'below',
              )}
              draggable={!readOnly && !open}
              onDragStart={(e) => {
                e.dataTransfer.effectAllowed = 'move'
                e.dataTransfer.setData('text/plain', l.id)
                setDrag({ from: i, over: null })
              }}
              onDragOver={(e) => {
                if (!drag) return
                e.preventDefault()
                if (drag.over !== i) setDrag({ ...drag, over: i })
              }}
              onDrop={(e) => {
                e.preventDefault()
                if (drag) move(drag.from, i)
                setDrag(null)
              }}
              onDragEnd={() => setDrag(null)}
            >
              <div className="hosts-loc-head" onClick={() => setExpanded(open ? null : l.id)}>
                <span className="drag-handle" title={readOnly ? undefined : 'Drag to reorder'}>
                  <Icon name="drag" size={14} />
                </span>
                <span className="path">{l.path || <span className="faint">/path/</span>}</span>
                <span className="target truncate grow">{summary(l, lists)}</span>
                {hasErr(i) && !open && <Badge tone="danger">error</Badge>}
                <Icon name="chevron" size={14} className="hosts-chevron" />
                {!readOnly && (
                  <IconButton
                    bare
                    icon="trash"
                    size={14}
                    label="Remove location"
                    onClick={(e) => {
                      e.stopPropagation()
                      remove(i)
                    }}
                  />
                )}
              </div>
              {open && (
                <LocationEditor
                  loc={l}
                  lists={lists}
                  hostAccessListId={draft.accessListId}
                  readOnly={readOnly}
                  err={err}
                  onChange={(patch) => setLoc(i, patch)}
                />
              )}
            </div>
          )
        })}
        {!readOnly && (
          <button type="button" className="hosts-add-row" onClick={add}>
            <Icon name="plus" size={14} />
            Add location
          </button>
        )}
      </div>
      {locs.length === 0 && readOnly && <div className="small faint">No extra locations — every path goes to the default upstream.</div>}
      <ConfigPreviewPanel state={preview} extract="location" />
    </>
  )
}

function LocationEditor({ loc: l, lists, hostAccessListId, readOnly, err, onChange }: {
  loc: Location
  lists: AccessList[]
  hostAccessListId?: string
  readOnly: boolean
  err: (field: string) => string | undefined
  onChange: (patch: Partial<Location>) => void
}) {
  const u = l.upstream
  const setUpstream = (patch: Partial<Upstream>) => onChange({ upstream: { ...u, ...patch } })
  const hostErr = err('upstream.host') || (u.host ? upstreamHostError(u.host) : '')
  const portErr = err('upstream.port') || (u.port ? portError(u.port) : '')
  const headerErr = l.headers.map((_, j) => err(`headers.${j}.name`) || err(`headers.${j}.value`)).find(Boolean)

  return (
    <div className="hosts-loc-body">
      <div className="grid-2">
        <Field label="Path" error={err('path')}>
          <Input mono value={l.path} placeholder="/office/" autoFocus={!l.path} invalid={!!err('path')} onChange={(e) => onChange({ path: e.target.value.trim() })} />
        </Field>
        <Field label="Action" error={err('kind')} hint={l.kind === 'same' ? "Host's default upstream" : l.kind === 'deny' ? 'Responds 403' : undefined}>
          <Segmented<LocationKind>
            value={l.kind}
            disabled={readOnly}
            onChange={(kind) => onChange({ kind })}
            options={[
              { value: 'proxy', label: 'Proxy' },
              { value: 'same', label: 'Same' },
              { value: 'deny', label: 'Deny 403' },
            ]}
          />
        </Field>
      </div>

      {l.kind === 'proxy' && (
        <Field label="Forward to" error={hostErr || portErr}>
          <div className="hosts-upstream-grid sm">
            <Select mono inputSize="sm" value={u.scheme} options={['http', 'https']} onChange={(v) => setUpstream({ scheme: v as Upstream['scheme'] })} aria-label="Scheme" />
            <Input mono inputSize="sm" value={u.host} invalid={!!hostErr} placeholder="10.0.0.x" onChange={(e) => setUpstream({ host: e.target.value.trim() })} aria-label="Host" />
            <Input
              mono
              inputSize="sm"
              type="number"
              min={1}
              max={65535}
              value={u.port || ''}
              invalid={!!portErr}
              placeholder="port"
              onChange={(e) => setUpstream({ port: e.target.value === '' ? 0 : Number(e.target.value) })}
              aria-label="Port"
            />
          </div>
        </Field>
      )}

      {l.kind !== 'deny' && (
        <div className="grid-2">
          {l.kind === 'proxy' && (
            <ToggleCard title="Websockets" description="Upgrade headers" checked={l.websockets} disabled={readOnly} onChange={(websockets) => onChange({ websockets })} />
          )}
          {l.kind === 'proxy' && (
            <ToggleCard
              title="Strip prefix"
              description={`Upstream sees / instead of ${l.path || 'the path'}`}
              checked={l.stripPrefix}
              disabled={readOnly}
              onChange={(stripPrefix) => onChange({ stripPrefix })}
            />
          )}
          <ToggleCard title="Cache" description="Static files, 30d" checked={l.cache} disabled={readOnly} onChange={(cache) => onChange({ cache })} />
          <ToggleCard
            title="No auth"
            description="Skip access list & forward-auth"
            checked={l.noAuth}
            disabled={readOnly}
            onChange={(noAuth) => onChange(noAuth ? { noAuth, accessListId: undefined } : { noAuth })}
          />
          <ToggleCard
            title="Different access list"
            description={lists.length ? 'Instead of the host list' : 'No access lists yet'}
            checked={!!l.accessListId}
            disabled={readOnly || l.noAuth || lists.length === 0}
            onChange={(on) => onChange({ accessListId: on ? (lists.find((x) => x.id !== hostAccessListId) ?? lists[0])?.id : undefined })}
          />
        </div>
      )}

      {l.kind !== 'deny' && l.accessListId && !l.noAuth && (
        <Field label="Access list for this path" error={err('accessListId')}>
          <Select
            value={l.accessListId}
            options={[
              ...lists.map((x) => ({ value: x.id, label: x.name })),
              ...(lists.some((x) => x.id === l.accessListId) ? [] : [{ value: l.accessListId, label: 'Missing access list' }]),
            ]}
            onChange={(v) => onChange({ accessListId: v || undefined })}
          />
        </Field>
      )}

      {l.kind !== 'deny' && (
        <div className="field">
          <label className="field-label">Extra request headers</label>
          {l.headers.map((hd, j) => (
            <div key={j} className="hosts-header-row">
              <Input
                mono
                inputSize="sm"
                placeholder="X-Forwarded-Prefix"
                value={hd.name}
                invalid={!!err(`headers.${j}.name`)}
                onChange={(e) => onChange({ headers: l.headers.map((x, k) => (k === j ? { ...x, name: e.target.value } : x)) })}
                aria-label="Header name"
              />
              <Input
                mono
                inputSize="sm"
                placeholder={l.path.replace(/\/$/, '') || '/office'}
                value={hd.value}
                invalid={!!err(`headers.${j}.value`)}
                onChange={(e) => onChange({ headers: l.headers.map((x, k) => (k === j ? { ...x, value: e.target.value } : x)) })}
                aria-label="Header value"
              />
              {!readOnly && (
                <IconButton bare icon="close" size={14} label="Remove header" onClick={() => onChange({ headers: l.headers.filter((_, k) => k !== j) })} />
              )}
            </div>
          ))}
          {headerErr && <div className="field-error">{headerErr}</div>}
          {!readOnly && (
            <button type="button" className="hosts-add-row sm" onClick={() => onChange({ headers: [...l.headers, { name: '', value: '' }] })}>
              <Icon name="plus" size={12} />
              Add header
            </button>
          )}
        </div>
      )}
    </div>
  )
}
