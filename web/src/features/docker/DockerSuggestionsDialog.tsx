// Owner: slice ops. "Create hosts from Docker" dialog (design 29c) — every container, any app port,
// grouped by Docker host.
import { Fragment, useEffect, useMemo, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { api, errorMessage } from '../../lib/api'
import { keys, useContainers, useEntities, useRole, useSaveSettings, useSettings } from '../../lib/queries'
import type { Container, HealthStatus } from '../../lib/types'
import { Badge, Button, Checkbox, Dialog, EmptyState, IconButton, Input, Menu, Skeleton, useToast } from '../../components/ui'
import { applyNowAction, certCovers, domainFromPattern, isMultiHost, schemeForPort, useDockerStatus, type CreateHostsResult } from './ops'
import './ops.css'

const keyOf = (c: { endpointId: string; id: string }) => `${c.endpointId}/${c.id}`

function parsePort(v: string | undefined): number {
  const n = Number((v ?? '').trim())
  return Number.isInteger(n) && n >= 1 && n <= 65535 ? n : 0
}

/** Container port behind an upstream port (published ports map back to the container port). */
function containerPort(c: Container, port: number): number {
  if (!c.upstreamHost || c.upstreamHost === c.ip) return port
  return c.ports.find((p) => p.public === port && (!p.proto || p.proto === 'tcp'))?.private ?? port
}

function schemeFor(c: Container, port: number): 'http' | 'https' {
  const label = c.labels['relay.scheme']
  if (label === 'http' || label === 'https') return label
  return schemeForPort(containerPort(c, port))
}

type Probe = { loading: boolean; ok?: boolean; text?: string }

export default function DockerSuggestionsDialog(props: {
  open: boolean
  onClose: () => void
  /** Container ids (or endpointId/id keys) to select initially (defaults to running web apps). */
  preselect?: string[]
}) {
  const { open, onClose, preselect } = props
  const qc = useQueryClient()
  const toast = useToast()
  const { isAdmin } = useRole()
  const status = useDockerStatus(open)
  const containers = useContainers(open)
  const docker = useSettings('docker')
  const general = useSettings('general')
  const certs = useEntities('certificates', { enabled: open })
  const accessLists = useEntities('access-lists', { enabled: open })
  const backends = useEntities('backends', { enabled: open })
  const saveDocker = useSaveSettings('docker')

  const [pattern, setPattern] = useState('{name}.home.lan')
  const [keepInSync, setKeepInSync] = useState(true)
  const [showStopped, setShowStopped] = useState(true)
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [domains, setDomains] = useState<Record<string, string>>({})
  const [edited, setEdited] = useState<Set<string>>(new Set())
  const [ports, setPorts] = useState<Record<string, string>>({})
  const [probes, setProbes] = useState<Record<string, Probe>>({})
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [busy, setBusy] = useState(false)
  const [initialized, setInitialized] = useState(false)

  const allRows = useMemo(() => (containers.data ?? []).filter((c) => !c.hostId), [containers.data])
  const rows = showStopped ? allRows : allRows.filter((c) => c.state === 'running')
  const multiHost = isMultiHost(containers.data)
  const addressable = (c: Container) => !c.backendId && (!!c.upstreamHost || !!c.linkOnStart)
  const portOf = (c: Container) => parsePort(ports[keyOf(c)])
  const eligible = (c: Container) => addressable(c) && portOf(c) > 0
  const looksWeb = (c: Container) => c.http && c.suggestedPort > 0
  // Relay's own containers (app + engines) are listed but never preselected.
  const isRelaySelf = (c: Container) => !!c.labels?.['relay.engine'] || /^relay(-[a-z]+)?(:|$)/.test(c.image)

  const groups = useMemo(() => {
    const order = new Map((docker.data?.endpoints ?? []).map((e, i) => [e.id, i]))
    const byEp = new Map<string, Container[]>()
    for (const c of rows) byEp.set(c.endpointId, [...(byEp.get(c.endpointId) ?? []), c])
    const rank = (c: Container) => (c.state === 'running' ? 0 : 4) + (addressable(c) ? 0 : 2) + (looksWeb(c) ? 0 : 1)
    return [...byEp.entries()]
      .sort(([a], [b]) => (order.get(a) ?? 99) - (order.get(b) ?? 99))
      .map(([id, list]) => ({
        id,
        name: list[0]?.endpointName || id,
        address: status.data?.endpoints?.find((e) => e.id === id)?.upstreamAddress ?? '',
        list: [...list].sort((a, b) => rank(a) - rank(b) || a.name.localeCompare(b.name)),
      }))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [rows, docker.data, status.data])

  // Initialise once per opening, when data is available.
  useEffect(() => {
    if (!open) {
      setInitialized(false)
      return
    }
    if (initialized || !containers.data || !docker.data) return
    const pat = docker.data.domainPattern || '{name}.home.lan'
    setPattern(pat)
    setKeepInSync(docker.data.keepInSync)
    const d: Record<string, string> = {}
    const p: Record<string, string> = {}
    for (const c of allRows) {
      d[keyOf(c)] = domainFromPattern(pat, c.name)
      p[keyOf(c)] = c.suggestedPort ? String(c.suggestedPort) : ''
    }
    setDomains(d)
    setPorts(p)
    setEdited(new Set())
    setErrors({})
    setProbes({})
    const initial = preselect?.length
      ? allRows.filter((c) => (preselect.includes(c.id) || preselect.includes(keyOf(c))) && addressable(c))
      : allRows.filter((c) => addressable(c) && looksWeb(c) && c.state === 'running' && !isRelaySelf(c))
    setSelected(new Set(initial.map(keyOf)))
    setInitialized(true)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, initialized, containers.data, docker.data, allRows, preselect])

  const changePattern = (v: string) => {
    setPattern(v)
    setDomains((prev) => {
      const next = { ...prev }
      for (const c of allRows) if (!edited.has(keyOf(c))) next[keyOf(c)] = domainFromPattern(v, c.name)
      return next
    })
  }

  const setPort = (c: Container, v: string) => {
    const k = keyOf(c)
    setPorts((prev) => ({ ...prev, [k]: v.replace(/[^0-9]/g, '').slice(0, 5) }))
    setProbes(({ [k]: _, ...rest }) => rest)
    if (parsePort(v) && addressable(c)) setSelected((prev) => new Set(prev).add(k))
  }

  const probe = async (c: Container) => {
    const k = keyOf(c)
    const port = portOf(c)
    if (!c.upstreamHost || !port) return
    setProbes((p) => ({ ...p, [k]: { loading: true } }))
    try {
      const r = await api.post<HealthStatus>('/api/health/probe', { upstream: { scheme: schemeFor(c, port), host: c.upstreamHost, port } })
      const ok = r.status !== 'down'
      const text = ok ? `reachable · ${r.httpStatus ?? '—'} · ${r.latencyMs} ms` : r.detail || 'unreachable'
      setProbes((p) => ({ ...p, [k]: { loading: false, ok, text } }))
    } catch (err) {
      setProbes((p) => ({ ...p, [k]: { loading: false, ok: false, text: errorMessage(err) } }))
    }
  }

  const backendName = (id?: string) => backends.data?.find((b) => b.id === id)?.name ?? id
  const sampleDomain = domainFromPattern(pattern, 'app')
  const tlsCert = useMemo(() => {
    const list = certs.data ?? []
    const def = list.find((c) => c.id === docker.data?.defaultCertificateId)
    if (def) return def
    return list.find((c) => c.status === 'valid' && certCovers(c.domains, sampleDomain))
  }, [certs.data, docker.data, sampleDomain])
  const accessId = docker.data?.defaultAccessListId || general.data?.defaults.accessListId
  const accessName = accessLists.data?.find((l) => l.id === accessId)?.name

  const webCount = rows.filter(looksWeb).length
  const selectableRows = rows.filter(eligible)
  const chosen = rows.filter((c) => selected.has(keyOf(c)) && addressable(c))
  const missingPort = chosen.filter((c) => !portOf(c))

  const description = (
    <>
      {rows.length} {rows.length === 1 ? 'container' : 'containers'} · {webCount} look like web apps. Pick the ones to proxy, check the port and adjust domains
      {tlsCert || accessName ? ' — ' : '.'}
      {tlsCert && <>TLS uses <span className="mono">{tlsCert.name}</span></>}
      {tlsCert && accessName && ', '}
      {accessName && <>access list <span className="mono">{accessName}</span></>}
      {(tlsCert || accessName) && '.'}
    </>
  )

  const submit = async () => {
    if (!chosen.length || missingPort.length) return
    setBusy(true)
    setErrors({})
    try {
      const res = await api.post<CreateHostsResult>('/api/docker/hosts', {
        items: chosen.map((c) => {
          const port = portOf(c)
          return { endpointId: c.endpointId, containerId: c.id, domain: (domains[keyOf(c)] ?? '').trim(), port, scheme: schemeFor(c, port) }
        }),
        keepInSync,
      })
      if (isAdmin && docker.data && (pattern !== docker.data.domainPattern || keepInSync !== docker.data.keepInSync) && pattern.includes('{name}')) {
        saveDocker.mutate({ ...docker.data, domainPattern: pattern, keepInSync })
      }
      qc.invalidateQueries({ queryKey: keys.entities('hosts') })
      qc.invalidateQueries({ queryKey: keys.pending })
      qc.invalidateQueries({ queryKey: ['docker'] })
      if (res.created.length) {
        const waiting = res.created.filter((c) => c.linkOnStart)
        toast.show({
          kind: 'success',
          title: `${res.created.length} ${res.created.length === 1 ? 'host' : 'hosts'} created from Docker`,
          message: `${res.created.map((c) => c.domain).join(', ')} added to pending changes${waiting.length ? ` · ${waiting.map((c) => c.domain).join(', ')} enabled when the container starts` : ''}`,
          actions: [applyNowAction],
        })
      }
      if (res.errors.length) {
        const errs: Record<string, string> = {}
        for (const e of res.errors) errs[keyOf({ endpointId: e.endpointId, id: e.containerId })] = e.error
        setErrors(errs)
        setSelected(new Set(Object.keys(errs)))
      } else {
        onClose()
      }
    } catch (err) {
      toast.error(err, 'Could not create hosts')
    } finally {
      setBusy(false)
    }
  }

  const notConnected = status.data && !status.data.connected
  const loading = containers.isLoading || docker.isLoading
  const stoppedCount = allRows.filter((c) => c.state !== 'running').length

  const renderRow = (c: Container) => {
    const k = keyOf(c)
    const running = c.state === 'running'
    const canAddress = addressable(c)
    const port = portOf(c)
    const cands = c.candidatePorts ?? []
    const isSelected = selected.has(k) && canAddress
    const blocked = c.backendId ? `belongs to backend ${backendName(c.backendId)}` : c.reason || 'no reachable address'
    const pr = probes[k]
    return (
      <tr key={k} className={running && canAddress ? undefined : 'dim'}>
        <td>
          <Checkbox
            checked={isSelected}
            disabled={!canAddress}
            onChange={(v) => {
              const next = new Set(selected)
              if (v) next.add(k)
              else next.delete(k)
              setSelected(next)
            }}
          />
        </td>
        <td className="mono" style={{ whiteSpace: 'nowrap' }}>
          <span className="row gap-6">
            <span className="truncate" style={{ maxWidth: 170 }} title={c.name}>{c.name}</span>
            {!running && <Badge>stopped</Badge>}
            {isRelaySelf(c) && <Badge>relay</Badge>}
          </span>
          <div className="micro faint truncate" style={{ maxWidth: 180, fontFamily: 'var(--font-sans)' }} title={c.image}>{c.image}</div>
          {canAddress && !c.http && c.reason && !c.linkOnStart && <div className="micro warn-text" style={{ fontFamily: 'var(--font-sans)' }}>{c.reason}</div>}
        </td>
        <td style={{ width: '34%', minWidth: 190 }}>
          {canAddress ? (
            <>
              <Input
                mono
                inputSize="sm"
                style={{ width: '100%', minWidth: 170 }}
                value={domains[k] ?? ''}
                invalid={!!errors[k]}
                onChange={(e) => {
                  setDomains({ ...domains, [k]: e.target.value })
                  setEdited(new Set(edited).add(k))
                }}
              />
              {errors[k] && <div className="ops-row-error">{errors[k]}</div>}
            </>
          ) : (
            <span className="faint">— {blocked}</span>
          )}
        </td>
        <td style={{ whiteSpace: 'nowrap' }}>
          {canAddress && (
            <span className="row gap-2">
              <Input
                mono
                inputSize="sm"
                inputMode="numeric"
                aria-label={`Port for ${c.name}`}
                placeholder="port"
                value={ports[k] ?? ''}
                invalid={isSelected && !port}
                onChange={(e) => setPort(c, e.target.value)}
                style={{ width: 70 }}
              />
              {cands.length > 0 && (
                <Menu
                  trigger={<IconButton icon="chevron" bare size={12} label="Detected ports" />}
                  items={[
                    { header: 'Detected ports' },
                    ...cands.map((p) => ({ label: `${p}${p === c.suggestedPort ? ' · suggested' : ''}`, onSelect: () => setPort(c, String(p)) })),
                  ]}
                />
              )}
            </span>
          )}
        </td>
        <td className="mono small" style={{ whiteSpace: 'nowrap' }}>
          {c.upstreamHost ? (
            <span className="row gap-6">
              {`${c.upstreamHost}:${port || '—'}`}
              {running && port > 0 && (
                <Button size="sm" variant="ghost" loading={pr?.loading} onClick={() => probe(c)} style={{ height: 22, padding: '0 6px' }}>Test</Button>
              )}
            </span>
          ) : c.linkOnStart ? (
            <span className="faint">{`${c.name}:${port || '—'}`}</span>
          ) : (
            '—'
          )}
          {c.linkOnStart && canAddress && <div className="micro faint" style={{ fontFamily: 'var(--font-sans)', whiteSpace: 'normal', maxWidth: 200 }}>stopped — the host is enabled automatically when the container starts</div>}
          {pr?.text && !pr.loading && (
            <div className={`micro ${pr.ok ? 'ok-text' : 'danger-text'} truncate`} style={{ fontFamily: 'var(--font-sans)', maxWidth: 220 }} title={pr.text}>{pr.text}</div>
          )}
        </td>
      </tr>
    )
  }

  return (
    <Dialog
      open={open}
      onClose={onClose}
      width={880}
      title="Create hosts from Docker"
      description={!loading && !notConnected ? description : undefined}
      icon="docker"
      footer={
        notConnected ? (
          <Button onClick={onClose}>Close</Button>
        ) : (
          <>
            <span className={`small ${missingPort.length ? 'danger-text' : 'muted'}`} style={{ marginRight: 'auto' }}>
              {chosen.length} selected{missingPort.length ? ` · enter the port for ${missingPort.map((c) => c.name).join(', ')}` : ''}
            </span>
            <Button onClick={onClose}>Cancel</Button>
            <Button variant="primary" disabled={!chosen.length || missingPort.length > 0} loading={busy} onClick={submit}>
              Create {chosen.length} {chosen.length === 1 ? 'host' : 'hosts'}
            </Button>
          </>
        )
      }
    >
      {notConnected ? (
        <EmptyState
          icon="docker"
          title="Docker is not connected"
          description={status.data?.error || 'Enable Docker discovery and add a Docker host to see containers.'}
          actions={<Link className="btn btn-sm" to="/settings/docker" onClick={onClose}>Open Docker settings</Link>}
        />
      ) : loading ? (
        <div className="col gap-8">
          <Skeleton height={32} />
          <Skeleton height={32} />
          <Skeleton height={32} />
        </div>
      ) : allRows.length === 0 ? (
        <EmptyState icon="docker" title="Nothing new to proxy" description="Every container is already behind a proxy host." />
      ) : (
        <div className="col gap-14 ops-suggest">
          <div className="ops-scroll" style={{ maxHeight: 380 }}>
            <table className="table">
              <thead>
                <tr>
                  <th style={{ width: 36 }}>
                    <Checkbox
                      checked={chosen.length > 0 && chosen.length === selectableRows.length}
                      indeterminate={chosen.length > 0 && chosen.length < selectableRows.length}
                      onChange={(v) => setSelected(new Set(v ? selectableRows.map(keyOf) : []))}
                    />
                  </th>
                  <th>Container</th>
                  <th>Domain</th>
                  <th>Port</th>
                  <th>Upstream</th>
                </tr>
              </thead>
              <tbody>
                {groups.map((g) => (
                  <Fragment key={g.id}>
                    {multiHost && (
                      <tr className="ops-group-row">
                        <td colSpan={5}>
                          {g.name} {g.address && <span className="mono">· {g.address}</span>}
                        </td>
                      </tr>
                    )}
                    {g.list.map(renderRow)}
                  </Fragment>
                ))}
              </tbody>
            </table>
          </div>
          <div className="row gap-16 wrap">
            <div className="row gap-8">
              <span className="small medium">Domain pattern</span>
              <Input mono inputSize="sm" style={{ width: 200 }} value={pattern} invalid={!pattern.includes('{name}')} onChange={(e) => changePattern(e.target.value)} />
            </div>
            <Checkbox checked={keepInSync} onChange={setKeepInSync} label="Keep in sync with container changes" />
            {stoppedCount > 0 && <Checkbox checked={showStopped} onChange={setShowStopped} label={`Show stopped (${stoppedCount})`} />}
          </div>
        </div>
      )}
    </Dialog>
  )
}
