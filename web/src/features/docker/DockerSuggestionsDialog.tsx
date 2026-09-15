// Owner: slice ops. "Create hosts from Docker" dialog (design 29c).
import { useEffect, useMemo, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { api } from '../../lib/api'
import { keys, useContainers, useEntities, useRole, useSaveSettings, useSettings } from '../../lib/queries'
import type { Container } from '../../lib/types'
import { Button, Checkbox, Dialog, EmptyState, Input, Select, Skeleton, useToast } from '../../components/ui'
import { applyNowAction, certCovers, domainFromPattern, schemeForPort, useDockerStatus, type CreateHostsResult } from './ops'
import './ops.css'

function tcpPorts(c: Container): number[] {
  return [...new Set(c.ports.filter((p) => !p.proto || p.proto === 'tcp').map((p) => p.private))].sort((a, b) => a - b)
}

export default function DockerSuggestionsDialog(props: {
  open: boolean
  onClose: () => void
  /** Container ids to select initially (defaults to every HTTP container). */
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
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [domains, setDomains] = useState<Record<string, string>>({})
  const [edited, setEdited] = useState<Set<string>>(new Set())
  const [ports, setPorts] = useState<Record<string, number>>({})
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [busy, setBusy] = useState(false)
  const [initialized, setInitialized] = useState(false)

  const rows = useMemo(() => (containers.data ?? []).filter((c) => !c.hostId), [containers.data])
  const eligible = (c: Container) => c.http && !c.backendId && !!c.ip

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
    const p: Record<string, number> = {}
    for (const c of rows) {
      d[c.id] = domainFromPattern(pat, c.name)
      p[c.id] = c.suggestedPort
    }
    setDomains(d)
    setPorts(p)
    setEdited(new Set())
    setErrors({})
    const initial = preselect?.length ? rows.filter((c) => preselect.includes(c.id) && eligible(c)) : rows.filter(eligible)
    setSelected(new Set(initial.map((c) => c.id)))
    setInitialized(true)
  }, [open, initialized, containers.data, docker.data, rows, preselect])

  const changePattern = (v: string) => {
    setPattern(v)
    setDomains((prev) => {
      const next = { ...prev }
      for (const c of rows) if (!edited.has(c.id)) next[c.id] = domainFromPattern(v, c.name)
      return next
    })
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

  const httpCount = rows.filter(eligible).length
  const chosen = rows.filter((c) => selected.has(c.id) && eligible(c))

  const description = (
    <>
      {httpCount} {httpCount === 1 ? 'container exposes' : 'containers expose'} an HTTP port. Pick the ones to proxy and adjust domains
      {tlsCert || accessName ? ' — ' : '.'}
      {tlsCert && <>TLS uses <span className="mono">{tlsCert.name}</span></>}
      {tlsCert && accessName && ', '}
      {accessName && <>access list <span className="mono">{accessName}</span></>}
      {(tlsCert || accessName) && '.'}
    </>
  )

  const submit = async () => {
    if (!chosen.length) return
    setBusy(true)
    setErrors({})
    try {
      const res = await api.post<CreateHostsResult>('/api/docker/hosts', {
        items: chosen.map((c) => ({ containerId: c.id, domain: (domains[c.id] ?? '').trim(), port: ports[c.id] || c.suggestedPort, scheme: schemeForPort(ports[c.id] || c.suggestedPort) })),
        keepInSync,
      })
      if (isAdmin && docker.data && (pattern !== docker.data.domainPattern || keepInSync !== docker.data.keepInSync) && pattern.includes('{name}')) {
        saveDocker.mutate({ ...docker.data, domainPattern: pattern, keepInSync })
      }
      qc.invalidateQueries({ queryKey: keys.entities('hosts') })
      qc.invalidateQueries({ queryKey: keys.pending })
      qc.invalidateQueries({ queryKey: ['docker'] })
      if (res.created.length) {
        toast.show({
          kind: 'success',
          title: `${res.created.length} ${res.created.length === 1 ? 'host' : 'hosts'} created from Docker`,
          message: `${res.created.map((c) => c.domain).join(', ')} added to pending changes`,
          actions: [applyNowAction],
        })
      }
      if (res.errors.length) {
        const errs: Record<string, string> = {}
        for (const e of res.errors) errs[e.containerId] = e.error
        setErrors(errs)
        setSelected(new Set(res.errors.map((e) => e.containerId)))
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

  return (
    <Dialog
      open={open}
      onClose={onClose}
      width={780}
      title="Create hosts from Docker"
      description={!loading && !notConnected ? description : undefined}
      icon="docker"
      footer={
        notConnected ? (
          <Button onClick={onClose}>Close</Button>
        ) : (
          <>
            <span className="small muted" style={{ marginRight: 'auto' }}>{chosen.length} selected</span>
            <Button onClick={onClose}>Cancel</Button>
            <Button variant="primary" disabled={!chosen.length} loading={busy} onClick={submit}>
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
          description={status.data?.error || 'Enable Docker discovery and mount the Docker socket to see containers.'}
          actions={<Link className="btn btn-sm" to="/settings/docker" onClick={onClose}>Open Docker settings</Link>}
        />
      ) : loading ? (
        <div className="col gap-8">
          <Skeleton height={32} />
          <Skeleton height={32} />
          <Skeleton height={32} />
        </div>
      ) : rows.length === 0 ? (
        <EmptyState icon="docker" title="Nothing new to proxy" description="Every running container is already behind a proxy host." />
      ) : (
        <div className="col gap-14 ops-suggest">
          <div className="ops-scroll">
            <table className="table">
              <thead>
                <tr>
                  <th style={{ width: 36 }}>
                    <Checkbox
                      checked={chosen.length > 0 && chosen.length === httpCount}
                      indeterminate={chosen.length > 0 && chosen.length < httpCount}
                      onChange={(v) => setSelected(new Set(v ? rows.filter(eligible).map((c) => c.id) : []))}
                    />
                  </th>
                  <th>Container</th>
                  <th>Domain</th>
                  <th>Upstream</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((c) => {
                  const ok = eligible(c)
                  const reason = c.backendId ? `belongs to backend ${backendName(c.backendId)}` : !c.ip ? 'no reachable IP' : c.reason
                  const cps = tcpPorts(c)
                  return (
                    <tr key={c.id} className={ok ? undefined : 'dim'}>
                      <td>
                        <Checkbox
                          checked={ok && selected.has(c.id)}
                          disabled={!ok}
                          onChange={(v) => {
                            const next = new Set(selected)
                            if (v) next.add(c.id)
                            else next.delete(c.id)
                            setSelected(next)
                          }}
                        />
                      </td>
                      <td className="mono" style={{ whiteSpace: 'nowrap' }}>
                        {c.name}
                        <div className="micro faint truncate" style={{ maxWidth: 180, fontFamily: 'var(--font-sans)' }} title={c.image}>{c.image}</div>
                      </td>
                      <td style={{ width: '42%' }}>
                        {ok ? (
                          <>
                            <Input
                              mono
                              inputSize="sm"
                              value={domains[c.id] ?? ''}
                              invalid={!!errors[c.id]}
                              onChange={(e) => {
                                setDomains({ ...domains, [c.id]: e.target.value })
                                setEdited(new Set(edited).add(c.id))
                              }}
                            />
                            {errors[c.id] && <div className="ops-row-error">{errors[c.id]}</div>}
                          </>
                        ) : (
                          <span className="faint">— {reason || 'not suggested'}</span>
                        )}
                      </td>
                      <td className="mono small" style={{ whiteSpace: 'nowrap' }}>
                        {ok && cps.length > 1 ? (
                          <span className="row gap-4">
                            {c.ip}:
                            <Select
                              inputSize="sm"
                              mono
                              value={String(ports[c.id] || c.suggestedPort)}
                              options={cps.map((p) => String(p))}
                              onChange={(v) => setPorts({ ...ports, [c.id]: Number(v) })}
                              style={{ width: 86 }}
                            />
                          </span>
                        ) : c.ip ? (
                          `${c.ip}${c.suggestedPort ? `:${c.suggestedPort}` : ''}`
                        ) : (
                          '—'
                        )}
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
          <div className="row gap-16 wrap">
            <div className="row gap-8">
              <span className="small medium">Domain pattern</span>
              <Input mono inputSize="sm" style={{ width: 200 }} value={pattern} invalid={!pattern.includes('{name}')} onChange={(e) => changePattern(e.target.value)} />
            </div>
            <Checkbox checked={keepInSync} onChange={setKeepInSync} label="Keep in sync with container changes" />
          </div>
        </div>
      )}
    </Dialog>
  )
}
