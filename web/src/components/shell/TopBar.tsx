import type { ReactNode } from 'react'
import { NavLink } from 'react-router-dom'
import { Icon, Kbd, cx } from '../ui'

export interface TopBarTab {
  to: string
  label: ReactNode
  count?: number
  hot?: boolean
}

/** Page header: title (+count), optional route tabs, meta, search trigger and actions. */
export function TopBar({ title, count, tabs, meta, actions, search = false }: {
  title: ReactNode
  count?: number
  tabs?: TopBarTab[]
  meta?: ReactNode
  actions?: ReactNode
  search?: boolean
}) {
  return (
    <div className="topbar">
      <div className="row gap-10">
        <div className="page-title">{title}</div>
        {count !== undefined && <span className="badge">{count}</span>}
      </div>
      {tabs && (
        <div className="topbar-tabs">
          {tabs.map((t) => (
            <NavLink key={t.to} to={t.to} end className={({ isActive }) => cx('topbar-tab', isActive && 'active')}>
              {t.label}
              {t.count !== undefined && <span className={cx('tab-count', t.hot && 'hot')}>{t.count}</span>}
            </NavLink>
          ))}
        </div>
      )}
      <div className="spacer" />
      {meta}
      {search && (
        <button type="button" className="search-trigger" onClick={() => window.dispatchEvent(new CustomEvent('relay:palette'))}>
          <Icon name="search" size={14} />
          Search hosts, certs, logs
          <span style={{ marginLeft: 'auto' }}><Kbd>⌘K</Kbd></span>
        </button>
      )}
      {actions}
    </div>
  )
}
