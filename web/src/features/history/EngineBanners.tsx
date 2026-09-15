// Engine banners (design 22c/22d): nginx not running, HAProxy failed after
// apply (auto-restored), and unreachable agents.
import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useQueryClient } from '@tanstack/react-query'
import { Button, Dot, Icon, IconButton, useToast } from '../../components/ui'
import { api } from '../../lib/api'
import { keys, useEngines, useEntities, usePending, useRole } from '../../lib/queries'
import type { EngineState, Stream } from '../../lib/types'
import { engineLabel, firstErrorLine, secondsAgo, useNow, useVersions, type EngineName, type PendingInfo, type VersionInfo } from './api'
import EngineLogDrawer, { bindFailurePort } from './EngineLogDrawer'
import DiscardDialog from './DiscardDialog'
import './history.css'

const DISMISS_KEY = 'relay.engine.dismissedFailure'

function streamOnPort(streams: Stream[] | undefined, port: number | undefined): Stream | undefined {
  if (!streams || port === undefined) return undefined
  return streams.find((s) => {
    if (!s.enabled) return false
    const [a, b] = s.listenPorts.split('-').map((x) => Number(x.trim()))
    return port >= a && port <= (b || a)
  })
}

export default function EngineBanners() {
  const engines = useEngines()
  const hosts = useEntities('hosts').data
  const backends = useEntities('backends').data
  const streams = useEntities('streams').data
  const pending = usePending().data as PendingInfo | undefined
  const versions = useVersions(5).data
  const { canWrite } = useRole()
  const navigate = useNavigate()
  const toast = useToast()
  const qc = useQueryClient()
  const now = useNow(1000)
  const [logEngine, setLogEngine] = useState<EngineName | null>(null)
  const [starting, setStarting] = useState<EngineName | null>(null)
  const [discardOpen, setDiscardOpen] = useState(false)
  const [dismissed, setDismissed] = useState<number>(() => Number(sessionStorage.getItem(DISMISS_KEY) ?? 0))

  const data = engines.data
  const enabledHosts = (hosts ?? []).filter((h) => h.enabled).length
  const hasBackends = (backends ?? []).length > 0
  const liveVersion = pending?.liveVersion ?? 0
  const pendingCount = pending?.count ?? 0

  const start = async (engine: EngineName) => {
    setStarting(engine)
    try {
      const r = await api.post<{ ok: boolean; output: string }>(`/api/engines/${engine}/start`)
      if (r.ok) toast.success(`${engineLabel[engine]} started`)
      else
        toast.show({
          kind: 'error',
          title: `${engineLabel[engine]} failed to start`,
          message: firstErrorLine(r.output) || r.output,
          actions: [{ label: 'Error log', onClick: () => setLogEngine(engine) }],
        })
    } catch (err) {
      toast.error(err, `${engineLabel[engine]} failed to start`)
    } finally {
      setStarting(null)
      qc.invalidateQueries({ queryKey: keys.engines })
    }
  }

  const banners: React.ReactNode[] = []

  const unreachable = (engine: EngineName, st: EngineState) => (
    <div key={`${engine}-unreachable`} className="banner warn">
      <Icon name="warning" size={16} />
      <span className="banner-title">{engineLabel[engine]} engine unreachable</span>
      <span className="banner-detail truncate">
        {st.error ? `${st.error} · ` : ''}Changes can't be applied until the relay-{engine} container is running.
      </span>
      <div className="spacer" />
      <Button size="sm" icon="reload" onClick={() => engines.refetch()} loading={engines.isFetching}>
        Retry
      </Button>
    </div>
  )

  const notRunning = (engine: EngineName, st: EngineState, detail: string) => (
    <div key={`${engine}-down`} className="banner danger">
      <Dot tone="danger" large />
      <span className="banner-title">{engineLabel[engine]} is not running</span>
      <span className="banner-detail truncate" title={st.exitError}>
        {detail}
        {st.exitedAt ? ` · exited ${secondsAgo(st.exitedAt, now)}` : ''}
      </span>
      <div className="spacer" />
      {canWrite && (
        <Button size="sm" variant="danger" loading={starting === engine} onClick={() => start(engine)}>
          Start {engineLabel[engine]}
        </Button>
      )}
      <Button size="sm" onClick={() => setLogEngine(engine)}>
        Error log
      </Button>
    </div>
  )

  if (data) {
    if (!data.nginx.reachable) banners.push(unreachable('nginx', data.nginx))
    else if (!data.nginx.running && (data.nginx.configured || enabledHosts > 0)) {
      const detail = enabledHosts > 0 ? `All ${enabledHosts} host${enabledHosts === 1 ? '' : 's'} unreachable` : 'The default server and ACME challenges are unavailable'
      banners.push(notRunning('nginx', data.nginx, detail))
    }
    const live = versions?.find((v) => v.status === 'live')
    if (hasBackends && !data.haproxy.reachable) banners.push(unreachable('haproxy', data.haproxy))
    else if (hasBackends && data.haproxy.reachable && data.haproxy.configured && !data.haproxy.running && live?.haproxyRunning) {
      banners.push(notRunning('haproxy', data.haproxy, `${backends?.length ?? 0} backend${backends?.length === 1 ? '' : 's'} unavailable`))
    }
  }

  const latest: VersionInfo | undefined = versions?.[0]
  const failed =
    latest &&
    (latest.status === 'rolled_back' || latest.status === 'failed') &&
    latest.id > liveVersion &&
    pendingCount > 0 &&
    dismissed !== latest.id
      ? latest
      : undefined

  if (failed) {
    const engine = (failed.failedEngine ?? 'nginx') as EngineName
    const label = engineLabel[engine]
    const restored = failed.rolledBackTo ? `v${failed.rolledBackTo} was restored automatically and is serving traffic.` : 'The previous configuration was restored automatically.'
    let title: string
    let detail: string
    switch (failed.failedStage) {
      case 'start':
        title = `${label} failed to start after apply v${failed.id}`
        detail = `Validation passed but the process exited on startup. ${restored} Your change is kept as a draft.`
        break
      case 'reload':
      case 'swap':
        title = `${label} failed to reload after apply v${failed.id}`
        detail = `Validation passed but ${label} rejected the new configuration. ${restored} Your change is kept as a draft.`
        break
      case 'health':
        title = 'Change rolled back automatically'
        detail = failed.error ?? ''
        break
      default:
        title = `Apply v${failed.id} failed ${failed.failedStage === 'render' ? 'to render' : 'validation'}`
        detail = 'Nothing was reloaded — the live configuration is unchanged. Your change is kept as a draft.'
    }
    const code = failed.failedStage === 'health' ? '' : firstErrorLine(failed.output)
    const port = bindFailurePort(code)
    const stream = engine === 'haproxy' ? streamOnPort(streams, port) : undefined
    const against = failed.rolledBackTo ?? liveVersion
    const dismiss = () => {
      sessionStorage.setItem(DISMISS_KEY, String(failed.id))
      setDismissed(failed.id)
    }
    banners.push(
      <div key="failed" className="banner warn stacked">
        <Icon name="warning" size={16} style={{ marginTop: 2 }} />
        <div className="grow col gap-6">
          <span className="banner-title">{title}</span>
          <span className="banner-detail">{detail}</span>
          {code && <pre className="banner-code">{code}</pre>}
          {stream ? (
            <span className="small">
              Port {port} is used by the <b>{stream.name}</b> stream on the reverse proxy — pick another port or remove the stream.
            </span>
          ) : port !== undefined ? (
            <span className="small">
              Port {port} is already in use on this machine.{' '}
              <button type="button" className="btn-link" onClick={() => setLogEngine(engine)}>
                Show listeners
              </button>
            </span>
          ) : null}
        </div>
        <div className="row gap-8">
          <Button size="sm" onClick={() => navigate('/history?pending=1')}>
            Open draft
          </Button>
          {against > 0 && (
            <Button size="sm" onClick={() => navigate(`/history?version=${failed.id}&against=${against}`)}>
              Compare v{against} → v{failed.id}
            </Button>
          )}
          {canWrite && liveVersion > 0 && (
            <Button size="sm" onClick={() => setDiscardOpen(true)}>
              Discard draft
            </Button>
          )}
          <IconButton icon="close" bare label="Dismiss" onClick={dismiss} />
        </div>
      </div>,
    )
  }

  return (
    <>
      {banners}
      <EngineLogDrawer engine={logEngine} onClose={() => setLogEngine(null)} />
      <DiscardDialog
        open={discardOpen}
        onClose={() => setDiscardOpen(false)}
        count={pendingCount}
        liveVersion={liveVersion}
        title="Discard the draft?"
      />
    </>
  )
}
