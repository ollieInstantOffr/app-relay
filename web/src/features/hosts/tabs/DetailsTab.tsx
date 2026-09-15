// Owner: slice hosts. Host drawer · Details tab (design 03).
import { useMemo } from 'react'
import { Button, Callout, Dot, Field, Input, Select, Spinner, ToggleCard, cx } from '../../../components/ui'
import { useContainers, useEntities } from '../../../lib/queries'
import type { Container, Frontend, Upstream } from '../../../lib/types'
import { ConfigPreviewPanel } from '../ConfigPreview'
import { DomainsInput } from '../DomainsInput'
import type { HostFormCtx } from '../HostDrawer'
import { accessSummary, domainError, portError, probeMessage, upstreamHostError, useDomainChecks, useProbe } from '../lib'
import { HostDNSStatus } from '../../dns/HostDNSStatus'

const URL_PASTE = /^(https?):\/\/(\[[^\]]+\]|[^/:?#\s]+)(?::(\d+))?(\/[^\s]*)?$/i

export function ProbeLine({ probe }: { probe: ReturnType<typeof useProbe> }) {
  if (probe.checking) {
    return (
      <div className="hosts-probe">
        <Spinner />
        Checking upstream…
      </div>
    )
  }
  if (probe.result) {
    const m = probeMessage(probe.result)
    return (
      <div className={cx('hosts-probe', m.tone)}>
        <Dot tone={m.tone} />
        {m.text}
      </div>
    )
  }
  return null
}

/** Address a proxy host uses to reach a load-balancer frontend (null = unusable bind). */
export function frontendTarget(f: Frontend): { host: string; port: number } | null {
  const bind = f.bind.trim()
  const i = bind.lastIndexOf(':')
  if (i < 0) return null
  const port = Number(bind.slice(i + 1))
  if (!Number.isInteger(port) || port < 1 || port > 65535) return null
  let host = bind.slice(0, i).replace(/^\[|\]$/g, '')
  // A wildcard bind listens on every address; Relay reaches it over loopback.
  if (host === '' || host === '*' || host === '0.0.0.0' || host === '::') host = '127.0.0.1'
  return { host, port }
}

/** Best port of a discovered container usable with its upstreamHost (0 = unknown). */
const chipPort = (c: Container) => c.suggestedPort || c.candidatePorts?.[0] || 0

export function DetailsTab({ ctx }: { ctx: HostFormCtx }) {
  const { draft, update, errors, readOnly, preview, isNew } = ctx
  const lists = useEntities('access-lists').data ?? []
  const backends = useEntities('backends').data ?? []
  const allFrontends = useEntities('frontends').data
  const containersQ = useContainers()
  const conflicts = useDomainChecks(draft.domains, { kind: 'host', excludeId: draft.id, enabled: !readOnly })
  const u = draft.upstream
  const viaBackend = !!u.backendId
  const probe = useProbe(viaBackend ? null : u, !readOnly)

  const domainErrors = Object.entries(errors).flatMap(([k, v]) => {
    if (k === 'domains') return [v]
    const m = /^domains\.(\d+)$/.exec(k)
    return m ? [`${draft.domains[Number(m[1])] ?? ''} — ${v}`] : []
  })
  const hostErr = errors['upstream.host'] || (u.host ? upstreamHostError(u.host) : '')
  const portErr = errors['upstream.port'] || (u.port ? portError(u.port) : '')
  const setUpstream = (patch: Partial<Upstream>) => update({ upstream: { ...u, ...patch } })

  const multiHost = new Set((containersQ.data ?? []).map((c) => c.endpointId)).size > 1
  const containers = useMemo(() => {
    const label = (draft.domains.find((d) => !d.startsWith('*.')) ?? '').split('.')[0]
    const score = (c: Container) => (label && c.name.toLowerCase().includes(label) ? 4 : 0) + (c.http ? 1 : 0) - (c.hostId && c.hostId !== draft.id ? 2 : 0)
    return (containersQ.data ?? [])
      .filter((c) => c.state === 'running' && !!c.upstreamHost && chipPort(c) > 0 && !c.backendId)
      .sort((a, b) => score(b) - score(a) || a.name.localeCompare(b.name))
      .slice(0, 8)
  }, [containersQ.data, draft.domains, draft.id])

  // Enabled HTTP frontends can be proxied like any upstream (TCP frontends need a stream).
  const frontends = useMemo(
    () =>
      (allFrontends ?? [])
        .filter((f) => f.enabled && f.mode === 'http' && frontendTarget(f))
        .sort((a, b) => a.name.localeCompare(b.name)),
    [allFrontends],
  )
  const viaFrontend = frontends.find((f) => {
    const t = frontendTarget(f)
    return t && t.host === u.host && t.port === u.port
  })

  const selectedList = lists.find((l) => l.id === draft.accessListId)
  const listOptions = [
    { value: '', label: 'None — public' },
    ...lists.map((l) => {
      const s = accessSummary(l)
      return { value: l.id, label: s ? `${l.name} · ${s}` : l.name }
    }),
  ]
  if (draft.accessListId && !selectedList) listOptions.push({ value: draft.accessListId, label: lists.length ? 'Missing access list' : 'Loading…' })
  const backendName = backends.find((b) => b.id === u.backendId)?.name ?? u.backendId

  return (
    <>
      <Field label="Domain names">
        <DomainsInput
          values={draft.domains}
          onChange={(domains) => update({ domains })}
          conflicts={conflicts}
          serverErrors={domainErrors}
          disabled={readOnly || !!draft.system}
          autoFocus={isNew}
          hint={draft.system ? "Relay's admin UI domain · change it in Settings → General" : 'Wildcards allowed · press Enter to add'}
        />
      </Field>

      {!draft.system && <HostDNSStatus domains={draft.domains.filter((d) => !domainError(d))} readOnly={readOnly} />}

      <div className="field">
        <label className="field-label">Forward to</label>
        {viaBackend && (
          <Callout
            tone="info"
            actions={!readOnly ? <Button size="sm" onClick={() => setUpstream({ backendId: undefined })}>Detach</Button> : undefined}
          >
            {viaFrontend ? (
              <>
                Routed through load-balancer frontend <span className="mono">{viaFrontend.name}</span> ({viaFrontend.bind}) and backend{' '}
                <span className="mono">{backendName}</span>.
              </>
            ) : (
              <>
                Routed through load-balancer backend <span className="mono">{backendName}</span> via its localhost frontend.
              </>
            )}
          </Callout>
        )}
        <div className="hosts-upstream-grid">
          <Select
            mono
            value={u.scheme}
            disabled={viaBackend}
            onChange={(v) => setUpstream({ scheme: v as Upstream['scheme'] })}
            options={['http', 'https']}
            aria-label="Upstream scheme"
          />
          <Input
            mono
            value={u.host}
            disabled={viaBackend}
            invalid={!!hostErr}
            placeholder="10.0.0.x"
            aria-label="Upstream host"
            onChange={(e) => {
              const v = e.target.value.trim()
              const m = URL_PASTE.exec(v)
              if (m) {
                const scheme = m[1].toLowerCase() as Upstream['scheme']
                setUpstream({ scheme, host: m[2], port: m[3] ? Number(m[3]) : scheme === 'https' ? 443 : 80, path: m[4] && m[4] !== '/' ? m[4] : u.path })
              } else {
                setUpstream({ host: v })
              }
            }}
          />
          <Input
            mono
            type="number"
            min={1}
            max={65535}
            value={u.port || ''}
            disabled={viaBackend}
            invalid={!!portErr}
            placeholder="port"
            aria-label="Upstream port"
            onChange={(e) => setUpstream({ port: e.target.value === '' ? 0 : Number(e.target.value) })}
          />
        </div>
        {hostErr || portErr ? <div className="field-error">{hostErr || portErr}</div> : <ProbeLine probe={probe} />}
        {u.path && <div className="field-hint">Path prefix <span className="mono">{u.path}</span> is added to every proxied request.</div>}
      </div>

      {containers.length > 0 && (
        <div className="field">
          <label className="field-label">Discovered containers</label>
          <div className="row wrap gap-8">
            {containers.map((c) => {
              const host = c.upstreamHost ?? ''
              const port = chipPort(c)
              const active = u.host === host && u.port === port
              return (
                <button
                  key={`${c.endpointId}/${c.id}`}
                  type="button"
                  className={cx('hosts-container-chip', active && 'active')}
                  title={`${c.image} · ${host}:${port}${c.reason ? ` · ${c.reason}` : ''}${c.hostId && c.hostId !== draft.id ? ' · already proxied by another host' : ''}`}
                  disabled={viaBackend}
                  onClick={() => setUpstream({ scheme: port === 443 || port === 8443 || port === 9443 ? 'https' : 'http', host, port })}
                >
                  <Dot tone={c.hostId && c.hostId !== draft.id ? 'muted' : 'ok'} />
                  {multiHost && c.endpointName ? `${c.endpointName} · ` : ''}{c.name}:{port}
                </button>
              )
            })}
          </div>
        </div>
      )}

      {frontends.length > 0 && (
        <div className="field">
          <label className="field-label">Load balancer frontends</label>
          <div className="row wrap gap-8">
            {frontends.map((f) => {
              const t = frontendTarget(f)!
              const active = u.host === t.host && u.port === t.port
              const backend = backends.find((b) => b.id === f.defaultBackendId)
              const usedElsewhere = !!f.hostId && f.hostId !== draft.id
              return (
                <button
                  key={f.id}
                  type="button"
                  className={cx('hosts-container-chip', active && 'active')}
                  title={`${f.bind}${backend ? ` → backend ${backend.name}` : ' · routes by rules'}${usedElsewhere ? ' · already exposed by another host' : ''}`}
                  disabled={readOnly}
                  onClick={() => setUpstream({ scheme: 'http', host: t.host, port: t.port, path: undefined, backendId: f.defaultBackendId || undefined })}
                >
                  <Dot tone={usedElsewhere ? 'muted' : 'ok'} />
                  {f.name}:{t.port}
                </button>
              )
            })}
          </div>
          <div className="field-hint">Send this host's traffic through a load balancer frontend.</div>
        </div>
      )}

      <div className="grid-2">
        <ToggleCard title="Websockets" description="Upgrade headers" checked={draft.websockets} disabled={readOnly} onChange={(websockets) => update({ websockets })} />
        <ToggleCard title="Block exploits" description="Common attack paths" checked={draft.blockExploits} disabled={readOnly} onChange={(blockExploits) => update({ blockExploits })} />
        <ToggleCard title="Cache assets" description="Static files, 30d" checked={draft.cacheAssets} disabled={readOnly} onChange={(cacheAssets) => update({ cacheAssets })} />
        <ToggleCard title="HTTP/2" description="Requires TLS" checked={draft.http2} disabled={readOnly} onChange={(http2) => update({ http2 })} />
      </div>

      <Field
        label="Access list"
        error={errors.accessListId}
        hint={selectedList ? selectedList.description || undefined : 'Anyone who can reach Relay can reach this host'}
      >
        <Select value={draft.accessListId ?? ''} options={listOptions} onChange={(v) => update({ accessListId: v || undefined })} aria-label="Access list" />
      </Field>

      <ConfigPreviewPanel state={preview} collapsible />
    </>
  )
}
