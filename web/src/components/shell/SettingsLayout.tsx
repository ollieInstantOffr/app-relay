import type { ComponentType } from 'react'
import { Navigate, NavLink, useParams } from 'react-router-dom'
import { Icon, cx, type IconName } from '../ui'
import { TopBar } from './TopBar'
import GeneralSettings from '../../features/settings/GeneralSettings'
import UsersSettings from '../../features/settings/UsersSettings'
import DockerSettings from '../../features/settings/DockerSettings'
import TLSSettings from '../../features/settings/TLSSettings'
import ProxyEngineSettings from '../../features/settings/ProxyEngineSettings'
import HAProxySettings from '../../features/settings/HAProxySettings'
import EnginesSettings from '../../features/settings/EnginesSettings'
import MCPSettings from '../../features/settings/MCPSettings'
import NotificationsSettings from '../../features/settings/NotificationsSettings'
import BackupSettings from '../../features/settings/BackupSettings'
import AboutSettings from '../../features/settings/AboutSettings'

export const SETTINGS_SECTIONS: { id: string; label: string; icon: IconName; component: ComponentType }[] = [
  { id: 'general', label: 'General', icon: 'settings', component: GeneralSettings },
  { id: 'users', label: 'Users & access', icon: 'users', component: UsersSettings },
  { id: 'docker', label: 'Docker discovery', icon: 'docker', component: DockerSettings },
  { id: 'tls', label: 'Default TLS', icon: 'certificates', component: TLSSettings },
  { id: 'proxy', label: 'Proxy engine', icon: 'hosts', component: ProxyEngineSettings },
  { id: 'haproxy', label: 'HAProxy engine', icon: 'load-balancer', component: HAProxySettings },
  { id: 'engines', label: 'Updates', icon: 'reload', component: EnginesSettings },
  { id: 'mcp', label: 'MCP server', icon: 'mcp', component: MCPSettings },
  { id: 'notifications', label: 'Notifications', icon: 'bolt', component: NotificationsSettings },
  { id: 'backup', label: 'Backup & restore', icon: 'download', component: BackupSettings },
  { id: 'about', label: 'About', icon: 'info', component: AboutSettings },
]

export default function SettingsLayout() {
  const { section } = useParams()
  const current = SETTINGS_SECTIONS.find((s) => s.id === section)
  if (!current) return <Navigate to="/settings/general" replace />
  const Page = current.component
  return (
    <>
      <TopBar title="Settings" search />
      <div className="settings-layout">
        <nav className="settings-nav">
          {SETTINGS_SECTIONS.map((s) => (
            <NavLink key={s.id} to={`/settings/${s.id}`} className={({ isActive }) => cx(isActive && 'active')}>
              <Icon name={s.icon} size={16} />
              {s.label}
            </NavLink>
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
