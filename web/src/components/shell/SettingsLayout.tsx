import type { ComponentType } from 'react'
import { Navigate, NavLink, useParams } from 'react-router-dom'
import { Icon, cx, type IconName } from '../ui'
import { TopBar } from './TopBar'
import GeneralSettings from '../../features/settings/GeneralSettings'
import UsersSettings from '../../features/settings/UsersSettings'
import DockerSettings from '../../features/settings/DockerSettings'
import PublicDNSSettings from '../../features/settings/PublicDNSSettings'
import TLSSettings from '../../features/settings/TLSSettings'
import ErrorPagesSettings from '../../features/settings/ErrorPagesSettings'
import ProxyEngineSettings from '../../features/settings/ProxyEngineSettings'
import HAProxySettings from '../../features/settings/HAProxySettings'
import EnginesSettings from '../../features/settings/EnginesSettings'
import MCPSettings from '../../features/settings/MCPSettings'
import NotificationsSettings from '../../features/settings/NotificationsSettings'
import BackupSettings from '../../features/settings/BackupSettings'
import AboutSettings from '../../features/settings/AboutSettings'

type SettingsSection = { id: string; label: string; icon: IconName; component: ComponentType }

// Settings pages grouped the way people look for them: the instance itself,
// how traffic is served, what Relay connects to, and keeping it running.
export const SETTINGS_GROUPS: { label: string; sections: SettingsSection[] }[] = [
  {
    label: 'Instance',
    sections: [
      { id: 'general', label: 'General', icon: 'settings', component: GeneralSettings },
      { id: 'users', label: 'Users & access', icon: 'users', component: UsersSettings },
      { id: 'notifications', label: 'Notifications', icon: 'bolt', component: NotificationsSettings },
    ],
  },
  {
    label: 'Traffic',
    sections: [
      { id: 'proxy', label: 'Proxy engine', icon: 'hosts', component: ProxyEngineSettings },
      { id: 'haproxy', label: 'HAProxy engine', icon: 'load-balancer', component: HAProxySettings },
      { id: 'tls', label: 'Default TLS', icon: 'certificates', component: TLSSettings },
      { id: 'error-pages', label: 'Error pages', icon: 'warning', component: ErrorPagesSettings },
    ],
  },
  {
    label: 'Integrations',
    sections: [
      { id: 'docker', label: 'Docker discovery', icon: 'docker', component: DockerSettings },
      { id: 'public-dns', label: 'Public DNS', icon: 'expose', component: PublicDNSSettings },
      { id: 'mcp', label: 'MCP server', icon: 'mcp', component: MCPSettings },
    ],
  },
  {
    label: 'System',
    sections: [
      { id: 'engines', label: 'Updates', icon: 'reload', component: EnginesSettings },
      { id: 'backup', label: 'Backup & restore', icon: 'download', component: BackupSettings },
      { id: 'about', label: 'About', icon: 'info', component: AboutSettings },
    ],
  },
]

export const SETTINGS_SECTIONS: SettingsSection[] = SETTINGS_GROUPS.flatMap((g) => g.sections)

export default function SettingsLayout() {
  const { section } = useParams()
  const current = SETTINGS_SECTIONS.find((s) => s.id === section)
  if (!current) return <Navigate to="/settings/general" replace />
  const Page = current.component
  return (
    <>
      <TopBar title="Settings" search />
      <div className="settings-layout">
        <nav className="settings-nav" aria-label="Settings">
          {SETTINGS_GROUPS.map((g) => (
            <div key={g.label} className="settings-nav-group" role="group" aria-labelledby={`settings-nav-${g.label}`}>
              <div className="settings-nav-heading" id={`settings-nav-${g.label}`}>
                {g.label}
              </div>
              {g.sections.map((s) => (
                <NavLink key={s.id} to={`/settings/${s.id}`} className={({ isActive }) => cx(isActive && 'active')}>
                  <Icon name={s.icon} size={16} />
                  {s.label}
                </NavLink>
              ))}
            </div>
          ))}
        </nav>
        <div className="settings-page">
          <div className="inner">
            <Page />
          </div>
        </div>
      </div>
    </>
  )
}
