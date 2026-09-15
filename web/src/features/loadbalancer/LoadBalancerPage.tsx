import { useMemo, useState } from 'react'
import { Navigate, useParams, useSearchParams } from 'react-router-dom'
import { TopBar } from '../../components/shell/TopBar'
import { Button, Status } from '../../components/ui'
import { useEntities, useLBEngine, useRole, useSettings } from '../../lib/queries'
import { agoShort } from '../../lib/format'
import type { Backend, Frontend } from '../../lib/types'
import BackendsTab from './BackendsTab'
import FrontendsTab from './FrontendsTab'
import StatsTab from './StatsTab'
import BackendDrawer from './BackendDrawer'
import FrontendDrawer from './FrontendDrawer'
import ExposeWizard from './ExposeWizard'
import ConfigDrawer from './ConfigDrawer'
import { newBackendDraft, newFrontendDraft, nextFreePort } from './lbApi'
import './loadbalancer.css'

const TABS = ['backends', 'frontends', 'stats'] as const
type Tab = (typeof TABS)[number]

export default function LoadBalancerPage() {
  const { tab = 'backends' } = useParams()
  const [params, setParams] = useSearchParams()
  const { canWrite } = useRole()
  const backends = useEntities('backends').data
  const frontends = useEntities('frontends').data
  const settings = useSettings('haproxy').data
  const lb = useLBEngine()
  const engine = lb.state
  const [showConfig, setShowConfig] = useState(false)
  const [dupBackend, setDupBackend] = useState<Backend | null>(null)
  const [dupFrontend, setDupFrontend] = useState<Frontend | null>(null)

  const onFrontends = tab === 'frontends'
  const isNew = params.get('new') === '1'
  const editId = params.get('edit') ?? ''
  const exposeId = params.get('expose') ?? ''

  const update = (mut: (p: URLSearchParams) => void) => {
    const next = new URLSearchParams(params)
    mut(next)
    setParams(next, { replace: true })
  }
  const closeDrawer = () => {
    setDupBackend(null)
    setDupFrontend(null)
    update((p) => {
      p.delete('new')
      p.delete('edit')
      p.delete('backend')
    })
  }

  // Backend drawer (backends / stats tabs)
  const backendDrawer = useMemo(() => {
    if (onFrontends) return null
    if (dupBackend) return { key: `dup-${dupBackend.name}`, backend: dupBackend }
    if (isNew) return { key: 'new', backend: newBackendDraft(settings) }
    if (editId && backends) {
      const b = backends.find((x) => x.id === editId)
      return b ? { key: `edit-${b.id}`, backend: b } : null
    }
    return null
    // settings only seed new drafts; don't reset an open drawer when they load
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [onFrontends, dupBackend, isNew, editId, backends === undefined])

  const frontendDrawer = useMemo(() => {
    if (!onFrontends) return null
    if (dupFrontend) return { key: `dup-${dupFrontend.name}`, frontend: dupFrontend }
    if (isNew) {
      const port = nextFreePort(frontends ?? [], settings?.exposePortStart ?? 10080)
      return { key: 'new', frontend: newFrontendDraft(params.get('backend') ?? '', port) }
    }
    if (editId && frontends) {
      const f = frontends.find((x) => x.id === editId)
      return f ? { key: `edit-${f.id}`, frontend: f } : null
    }
    return null
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [onFrontends, dupFrontend, isNew, editId, frontends === undefined])

  if (!TABS.includes(tab as Tab)) return <Navigate to="/load-balancer/backends" replace />

  const meta = engine ? (
    <Status tone={!engine.reachable ? 'muted' : engine.running ? 'ok' : 'danger'}>
      {!engine.reachable
        ? engine.container === 'stopped'
          ? `${lb.label} · stopped`
          : `${lb.label} · agent unreachable`
        : `${lb.label}${engine.version ? ' ' + engine.version : ''} · ${engine.running ? 'running' : 'stopped'}${
            engine.lastReloadAt ? ` · reloaded ${agoShort(engine.lastReloadAt) === 'now' ? 'just now' : agoShort(engine.lastReloadAt) + ' ago'}` : ''
          }`}
    </Status>
  ) : null

  return (
    <>
      <TopBar
        title="Load balancer"
        tabs={[
          { to: '/load-balancer/backends', label: 'Backends' },
          { to: '/load-balancer/frontends', label: 'Frontends' },
          { to: '/load-balancer/stats', label: 'Stats' },
        ]}
        meta={meta}
        actions={
          <>
            <Button onClick={() => setShowConfig(true)}>View config</Button>
            {canWrite &&
              (onFrontends ? (
                <Button variant="primary" icon="plus" onClick={() => update((p) => p.set('new', '1'))}>
                  New frontend
                </Button>
              ) : (
                <Button variant="primary" icon="plus" onClick={() => update((p) => p.set('new', '1'))}>
                  New backend
                </Button>
              ))}
          </>
        }
      />
      <div className="page">
        {tab === 'backends' && (
          <BackendsTab
            onNew={() => update((p) => p.set('new', '1'))}
            onEdit={(id) => update((p) => p.set('edit', id))}
            onExpose={(id) => update((p) => p.set('expose', id))}
            onDuplicate={setDupBackend}
          />
        )}
        {tab === 'frontends' && (
          <FrontendsTab
            onNew={() => update((p) => p.set('new', '1'))}
            onEdit={(id) => update((p) => p.set('edit', id))}
            onDuplicate={setDupFrontend}
          />
        )}
        {tab === 'stats' && <StatsTab onEdit={(id) => update((p) => p.set('edit', id))} />}
      </div>

      {backendDrawer && (
        <BackendDrawer
          key={backendDrawer.key}
          initial={backendDrawer.backend}
          onClose={closeDrawer}
          onExpose={(id) => update((p) => {
            p.delete('edit')
            p.delete('new')
            p.set('expose', id)
          })}
        />
      )}
      {frontendDrawer && <FrontendDrawer key={frontendDrawer.key} initial={frontendDrawer.frontend} onClose={closeDrawer} />}
      {exposeId && canWrite && (
        <ExposeWizard key={exposeId} backendId={exposeId === 'new' ? '' : exposeId} onClose={() => update((p) => p.delete('expose'))} />
      )}
      <ConfigDrawer open={showConfig} onClose={() => setShowConfig(false)} />
    </>
  )
}
