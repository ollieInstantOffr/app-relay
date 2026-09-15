import { useEffect, useMemo } from 'react'
import { Navigate, NavLink, Outlet, useLocation, useNavigate } from 'react-router-dom'
import { useQueryClient } from '@tanstack/react-query'
import { Icon, LogoMark, Menu, Tooltip, cx, type IconName } from '../ui'
import { useEventStream } from '../../lib/events'
import { keys, useHealth, useLBStats, usePending, usePendingApprovals, useSession, useEngines } from '../../lib/queries'
import { api } from '../../lib/api'
import { CommandPalette } from './CommandPalette'
import { ShortcutsSheet, useGlobalShortcuts } from './Shortcuts'
import PendingBar from '../../features/history/PendingBar'
import EngineBanners from '../../features/history/EngineBanners'
import SessionExpiredDialog from '../../features/auth/SessionExpiredDialog'
import ApprovalToasts from '../../features/mcp/ApprovalToasts'
import { hasEngineNotice, useEngineUpdates } from '../../features/settings/enginesApi'
import { usePublicDNS } from '../../features/dns/dnsApi'

export interface NavItem {
  to: string
  label: string
  icon: IconName
  shortcut: string
}

export const NAV: NavItem[] = [
  { to: '/', label: 'Overview', icon: 'overview', shortcut: 'G O' },
  { to: '/topology', label: 'Topology', icon: 'topology', shortcut: 'G F' },
  { to: '/hosts', label: 'Proxy hosts', icon: 'hosts', shortcut: 'G H' },
  { to: '/load-balancer', label: 'Load balancer', icon: 'load-balancer', shortcut: 'G L' },
  { to: '/certificates', label: 'Certificates', icon: 'certificates', shortcut: 'G C' },
  { to: '/access', label: 'Access lists', icon: 'access', shortcut: 'G A' },
  { to: '/streams', label: 'Streams', icon: 'streams', shortcut: 'G T' },
  { to: '/dns', label: 'Public DNS', icon: 'expose', shortcut: 'G N' },
  { to: '/logs', label: 'Logs', icon: 'logs', shortcut: 'G G' },
  { to: '/history', label: 'Config history', icon: 'history', shortcut: 'G V' },
]

/** NAV without pages for integrations that are turned off (Public DNS). */
export function useVisibleNav(): NavItem[] {
  const dns = usePublicDNS().enabled
  return useMemo(() => NAV.filter((n) => n.to !== '/dns' || dns), [dns])
}

/** Redirects to /setup or /login when needed. */
export function RequireSession({ children }: { children: React.ReactNode }) {
  const { data, isLoading, isError } = useSession()
  const loc = useLocation()
  if (isLoading) return <div className="auth-screen"><span className="spinner lg" /></div>
  if (isError || !data) return <div className="auth-screen muted">Relay API unreachable. Retrying…</div>
  if (data.setupRequired) return <Navigate to="/setup" replace />
  if (!data.authenticated) return <Navigate to={`/login?next=${encodeURIComponent(loc.pathname + loc.search)}`} replace />
  return <>{children}</>
}

export function AppShell() {
  useEventStream(true)
  useGlobalShortcuts()
  const pending = usePending().data
  const session = useSession().data

  useEffect(() => {
    const n = pending?.count ?? 0
    document.title = n > 0 ? `Relay · ${n} pending` : 'Relay'
  }, [pending?.count])

  return (
    <RequireSession>
      <div className="app">
        <Rail pendingCount={pending?.count ?? 0} username={session?.user?.username ?? ''} role={session?.user?.role ?? ''} />
        <div className="main">
          <EngineBanners />
          <PendingBar />
          <Outlet />
        </div>
      </div>
      <CommandPalette />
      <ShortcutsSheet />
      <SessionExpiredDialog />
      <ApprovalToasts />
    </RequireSession>
  )
}

function Rail({ pendingCount, username, role }: { pendingCount: number; username: string; role: string }) {
  const health = useHealth().data
  const lb = useLBStats(15_000).data
  const approvals = usePendingApprovals().data
  const engines = useEngines().data
  const navigate = useNavigate()
  const qc = useQueryClient()
  const nav = useVisibleNav()

  const engineNotice = hasEngineNotice(useEngineUpdates(role === 'admin').data)
  const badges = useMemo(() => {
    const b: Record<string, { tone?: 'warn' | 'danger'; count?: number }> = {}
    const hostStates = Object.entries(health ?? {}).filter(([k]) => k.startsWith('host:')).map(([, v]) => v.status)
    const proxy = engines ? engines[engines.proxy === 'edge' ? 'edge' : 'nginx'] : undefined
    if (proxy && proxy.reachable && !proxy.running) b['/hosts'] = { tone: 'danger' }
    else if (hostStates.includes('down')) b['/hosts'] = { tone: 'danger' }
    else if (hostStates.includes('degraded')) b['/hosts'] = { tone: 'warn' }
    if (lb?.running) {
      if (lb.backends.some((x) => x.status === 'DOWN')) b['/load-balancer'] = { tone: 'danger' }
      else if (lb.backends.some((x) => x.status === 'DEGRADED')) b['/load-balancer'] = { tone: 'warn' }
    }
    if (pendingCount > 0) b['/history'] = { count: pendingCount }
    if (approvals && approvals.length > 0) b['/logs'] = { count: approvals.length }
    return b
  }, [health, lb, pendingCount, approvals, engines])

  return (
    <nav className="rail">
      <NavLink to="/" className="rail-logo" aria-label="Relay">
        <LogoMark size={28} />
      </NavLink>
      {nav.map((item) => (
        <Tooltip key={item.to} content={item.label} shortcut={item.shortcut} side="right">
          <NavLink to={item.to} end={item.to === '/'} className={({ isActive }) => cx('rail-item', isActive && 'active')} aria-label={item.label}>
            <Icon name={item.icon} size={18} />
            {badges[item.to]?.count !== undefined ? (
              <span className="rail-badge num">{badges[item.to].count}</span>
            ) : badges[item.to]?.tone ? (
              <span className="rail-badge" style={{ background: badges[item.to].tone === 'danger' ? 'var(--danger)' : 'var(--warn)' }} />
            ) : null}
          </NavLink>
        </Tooltip>
      ))}
      <div className="spacer" />
      <Tooltip content="Documentation" shortcut="G D" side="right">
        <NavLink to="/docs" className={({ isActive }) => cx('rail-item', isActive && 'active')} aria-label="Documentation">
          <Icon name="docs" size={18} />
        </NavLink>
      </Tooltip>
      <Tooltip content="Settings" shortcut="G S" side="right">
        <NavLink to="/settings" className={({ isActive }) => cx('rail-item', isActive && 'active')} aria-label="Settings">
          <Icon name="settings" size={18} />
          {engineNotice && <span className="rail-badge" style={{ background: 'var(--info, var(--accent))' }} title="Engine update available" />}
        </NavLink>
      </Tooltip>
      <Menu
        align="start"
        trigger={<button className="rail-avatar" aria-label="Account">{username.slice(0, 1).toUpperCase()}</button>}
        items={[
          { header: `${username} · ${role}` },
          { label: 'Account & sessions', icon: 'users', onSelect: () => navigate('/settings/users') },
          { label: 'Documentation', icon: 'docs', onSelect: () => navigate('/docs') },
          { label: 'Keyboard shortcuts', icon: 'terminal', shortcut: '?', onSelect: () => window.dispatchEvent(new CustomEvent('relay:shortcuts')) },
          'separator',
          {
            label: 'Sign out',
            icon: 'power',
            onSelect: async () => {
              await api.post('/api/auth/logout').catch(() => undefined)
              qc.clear()
              await qc.invalidateQueries({ queryKey: keys.session })
              navigate('/login')
            },
          },
        ]}
      />
    </nav>
  )
}
