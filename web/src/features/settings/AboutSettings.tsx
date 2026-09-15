// Settings → About (design 16d). No update check exists, so no "Update to" button.
import { Link } from 'react-router-dom'
import { Badge, Button, Card, LogoMark, Skeleton, Status } from '../../components/ui'
import { useEngines } from '../../lib/queries'
import { useEngineUpdates } from './enginesApi'
import { ago, bytes, pluralize } from '../../lib/format'
import type { EngineState } from '../../lib/types'
import { useAbout, useAuthSession } from '../auth/authApi'
import '../auth/auth.css'

function engineStatus(state: EngineState | undefined, loading: boolean): { tone: 'ok' | 'warn' | 'danger' | 'muted'; label: string; detail: string } {
  if (!state) return { tone: 'muted', label: loading ? 'checking…' : 'unknown', detail: '' }
  if (!state.reachable && state.standby) return { tone: 'muted', label: 'standby', detail: 'container stopped · not needed right now' }
  if (!state.reachable) return { tone: 'danger', label: 'agent unreachable', detail: state.error ?? '' }
  if (state.running) {
    const detail = state.lastReloadAt ? `reloaded ${ago(state.lastReloadAt)}` : state.startedAt ? `started ${ago(state.startedAt)}` : ''
    return { tone: 'ok', label: 'running', detail }
  }
  if (state.exitError) return { tone: 'danger', label: 'exited', detail: state.exitError }
  return { tone: state.configured ? 'warn' : 'muted', label: 'not running', detail: state.configured ? '' : 'no configuration applied yet' }
}

export default function AboutSettings() {
  const about = useAbout()
  const engines = useEngines()
  const updates = useEngineUpdates().data
  const { data: session } = useAuthSession()
  const version = about.data?.version ?? session?.version
  const installedAt = about.data?.installedAt
  const days = installedAt ? Math.max(0, Math.floor((Date.now() - new Date(installedAt).getTime()) / 86_400_000)) : undefined

  const proxy = engines.data?.proxy === 'edge' ? 'edge' : 'nginx'
  const rows: { name: string; label: string; state?: EngineState; proxy?: boolean }[] = [
    { name: 'nginx', label: 'nginx', state: engines.data?.nginx, proxy: true },
    { name: 'edge', label: 'Relay Edge', state: engines.data?.edge, proxy: true },
    { name: 'haproxy', label: 'haproxy', state: engines.data?.haproxy },
  ]

  return (
    <>
      <div className="about-head">
        <LogoMark size={48} />
        <div>
          <div className="h1">
            Relay{' '}
            <span className="mono" style={{ fontSize: 14, fontWeight: 500, color: 'var(--ink-subtle)', marginLeft: 6 }}>
              {version ?? '—'}
            </span>
          </div>
          <div className="muted" style={{ marginTop: 2 }}>
            Reverse proxy + load balancer manager
            {days !== undefined && ` · installed ${days === 0 ? 'today' : `${pluralize(days, 'day')} ago`}`}
          </div>
        </div>
      </div>

      <Card title="Components">
        {rows.map(({ name, label, state, proxy: isProxy }) => {
          const active = isProxy && !!engines.data && proxy === name
          // The standby proxy engine is stopped on purpose; don't report that as a problem.
          const st = isProxy && engines.data && !active && (state?.standby || (state?.reachable && !state.running))
            ? { tone: 'muted' as const, label: 'standby', detail: 'stopped · not the proxy engine · Settings → Proxy engine' }
            : engineStatus(state, engines.isLoading)
          return (
            <div key={name} className="comp-row">
              <span className="row gap-6">
                {label}
                {name === 'edge' && <Badge tone="info">beta</Badge>}
                {isProxy && engines.data && (active ? <Badge tone="ok">active</Badge> : <Badge>standby</Badge>)}
              </span>
              <span className="mono small row gap-6">
                {state?.version || '—'}
                {name !== 'edge' && !(name === 'nginx' && updates?.nginx.inactive) && updates?.[name as 'nginx' | 'haproxy']?.updateAvailable && (
                  <Link to="/settings/engines" title={`${updates[name as 'nginx' | 'haproxy'].latest?.version} available`}>
                    <Badge tone="info">update</Badge>
                  </Link>
                )}
              </span>
              <Status tone={st.tone}>{st.label}</Status>
              <span className="small faint truncate" title={st.detail}>
                {st.detail}
              </span>
            </div>
          )
        })}
        <div className="comp-row">
          <span>relay</span>
          <span className="mono small">{version ?? '—'}</span>
          <Status tone="ok">running</Status>
          <span className="small faint">{about.data?.goVersion ?? ''}</span>
        </div>
        <div className="comp-row">
          <span>database</span>
          <span className="mono small">SQLite</span>
          <span className="mono small">{about.isLoading ? <Skeleton width={60} height={12} /> : about.data ? bytes(about.data.databaseBytes) : '—'}</span>
          <span className="small faint mono">relay.db</span>
        </div>
      </Card>

      <Card title="Help">
        <div className="toggle-row">
          <div className="grow">
            <div className="toggle-title">Keyboard shortcuts</div>
            <div className="toggle-desc">Everything you can do without the mouse</div>
          </div>
          <Button size="sm" onClick={() => window.dispatchEvent(new CustomEvent('relay:shortcuts'))}>
            Show
          </Button>
        </div>
        <div className="toggle-row">
          <div className="grow">
            <div className="toggle-title">Audit log</div>
            <div className="toggle-desc">Who changed what, including sign-ins and API tokens</div>
          </div>
          <Link to="/logs/audit" className="btn btn-sm">
            Open
          </Link>
        </div>
        <div className="toggle-row">
          <div className="grow">
            <div className="toggle-title">Config history</div>
            <div className="toggle-desc">Every applied version with diffs and rollback</div>
          </div>
          <Link to="/history" className="btn btn-sm">
            Open
          </Link>
        </div>
      </Card>
    </>
  )
}
