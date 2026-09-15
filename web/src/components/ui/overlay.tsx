import { useEffect, useLayoutEffect, useRef, useState, type ReactNode, type ReactElement, cloneElement } from 'react'
import { createPortal } from 'react-dom'
import { Icon, type IconName } from './Icon'
import { Button, IconButton, Input, cx } from './controls'
import { Tabs, type TabDef } from './display'

export function Portal({ children }: { children: ReactNode }) {
  return createPortal(children, document.body)
}

function useEscape(active: boolean, onEscape: () => void) {
  const ref = useRef(onEscape)
  ref.current = onEscape
  useEffect(() => {
    if (!active) return
    const h = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.stopPropagation()
        ref.current()
      }
    }
    window.addEventListener('keydown', h)
    return () => window.removeEventListener('keydown', h)
  }, [active])
}

// ---------------------------------------------------------------- drawer
export function Drawer<T extends string = string>({
  open, onClose, title, subtitle, width = 'default', tabs, tab, onTab, footer, children, headerExtra,
}: {
  open: boolean
  onClose: () => void
  title: ReactNode
  subtitle?: ReactNode
  width?: 'default' | 'wide' | 'xwide'
  tabs?: TabDef<T>[]
  tab?: T
  onTab?: (t: T) => void
  footer?: ReactNode
  children: ReactNode
  headerExtra?: ReactNode
}) {
  useEscape(open, onClose)
  useEffect(() => {
    if (!open || !tabs || !onTab || tab === undefined) return
    const h = (e: KeyboardEvent) => {
      if (!(e.metaKey || e.ctrlKey) || (e.key !== ']' && e.key !== '[')) return
      e.preventDefault()
      const i = tabs.findIndex((t) => t.id === tab)
      const next = tabs[(i + (e.key === ']' ? 1 : tabs.length - 1)) % tabs.length]
      onTab(next.id)
    }
    window.addEventListener('keydown', h)
    return () => window.removeEventListener('keydown', h)
  }, [open, tabs, tab, onTab])
  if (!open) return null
  return (
    <Portal>
      <div className="scrim" onClick={onClose} />
      <div className={cx('drawer', width !== 'default' && width)} role="dialog" aria-modal>
        <div className="drawer-header">
          <div className="grow">
            <div className="drawer-title">{title}</div>
            {subtitle && <div className="drawer-subtitle">{subtitle}</div>}
          </div>
          {headerExtra}
          <IconButton icon="close" onClick={onClose} label="Close" />
        </div>
        {tabs && tab !== undefined && onTab && <Tabs tabs={tabs} value={tab} onChange={onTab} />}
        <div className="drawer-body">{children}</div>
        {footer && <div className="drawer-footer">{footer}</div>}
      </div>
    </Portal>
  )
}

// ---------------------------------------------------------------- dialog
export function Dialog({ open, onClose, title, description, icon, iconTone, footer, children, width, className, dismissable = true }: {
  open: boolean
  onClose: () => void
  title?: ReactNode
  description?: ReactNode
  icon?: IconName
  iconTone?: 'danger' | 'ok' | 'warn'
  footer?: ReactNode
  children?: ReactNode
  width?: number
  className?: string
  dismissable?: boolean
}) {
  useEscape(open && dismissable, onClose)
  if (!open) return null
  return (
    <Portal>
      <div className="scrim dialog-scrim" onMouseDown={(e) => dismissable && e.target === e.currentTarget && onClose()}>
        <div className={cx('dialog', className)} style={width ? { width } : undefined} role="dialog" aria-modal>
          {(title || icon) && (
            <div className="dialog-header">
              {icon && <div className={cx('dialog-icon', iconTone)}><Icon name={icon} size={18} /></div>}
              <div className="grow">
                {title && <div className="dialog-title">{title}</div>}
                {description && <div className="dialog-desc">{description}</div>}
              </div>
            </div>
          )}
          {children && <div className="dialog-body">{children}</div>}
          {footer && <div className="dialog-footer">{footer}</div>}
        </div>
      </div>
    </Portal>
  )
}

export function ConfirmDialog({ open, onClose, onConfirm, title, message, confirmLabel = 'Confirm', danger, typeToConfirm, children }: {
  open: boolean
  onClose: () => void
  onConfirm: () => void | Promise<void>
  title: ReactNode
  message?: ReactNode
  confirmLabel?: string
  danger?: boolean
  typeToConfirm?: string
  children?: ReactNode
}) {
  const [typed, setTyped] = useState('')
  const [busy, setBusy] = useState(false)
  useEffect(() => {
    if (open) setTyped('')
  }, [open])
  const ok = !typeToConfirm || typed === typeToConfirm
  return (
    <Dialog
      open={open}
      onClose={onClose}
      title={title}
      description={message}
      icon={danger ? 'trash' : undefined}
      iconTone={danger ? 'danger' : undefined}
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button
            variant={danger ? 'danger' : 'primary'}
            disabled={!ok}
            loading={busy}
            onClick={async () => {
              setBusy(true)
              try {
                await onConfirm()
                onClose()
              } finally {
                setBusy(false)
              }
            }}
          >
            {confirmLabel}
          </Button>
        </>
      }
    >
      {children}
      {typeToConfirm && (
        <div className="field">
          <label className="field-label">Type the name to confirm</label>
          <Input mono value={typed} placeholder={typeToConfirm} onChange={(e) => setTyped(e.target.value)} autoFocus />
        </div>
      )}
    </Dialog>
  )
}

// ---------------------------------------------------------------- menu
export type MenuEntry =
  | { label: ReactNode; icon?: IconName; shortcut?: string; danger?: boolean; disabled?: boolean; onSelect: () => void }
  | { header: ReactNode }
  | 'separator'

export function Menu({ trigger, items, align = 'end' }: { trigger: ReactElement<{ onClick?: (e: React.MouseEvent) => void }>; items: MenuEntry[]; align?: 'start' | 'end' }) {
  const [rect, setRect] = useState<DOMRect | null>(null)
  const menuRef = useRef<HTMLDivElement>(null)
  const [pos, setPos] = useState<{ top: number; left: number }>({ top: 0, left: 0 })
  useEscape(!!rect, () => setRect(null))
  useLayoutEffect(() => {
    if (!rect || !menuRef.current) return
    const m = menuRef.current.getBoundingClientRect()
    let left = align === 'end' ? rect.right - m.width : rect.left
    let top = rect.bottom + 4
    if (top + m.height > window.innerHeight - 8) top = rect.top - m.height - 4
    left = Math.max(8, Math.min(left, window.innerWidth - m.width - 8))
    setPos({ top, left })
  }, [rect, align])
  return (
    <>
      {cloneElement(trigger, {
        onClick: (e: React.MouseEvent) => {
          e.stopPropagation()
          setRect(rect ? null : (e.currentTarget as HTMLElement).getBoundingClientRect())
        },
      })}
      {rect && (
        <Portal>
          <div style={{ position: 'fixed', inset: 0, zIndex: 79 }} onMouseDown={() => setRect(null)} />
          <div ref={menuRef} className="menu" style={pos} onClick={(e) => e.stopPropagation()}>
            {items.map((it, i) => {
              if (it === 'separator') return <div key={i} className="menu-sep" />
              if ('header' in it) return <div key={i} className="menu-header">{it.header}</div>
              return (
                <button
                  key={i}
                  type="button"
                  className={cx('menu-item', it.danger && 'danger')}
                  disabled={it.disabled}
                  onClick={() => {
                    setRect(null)
                    it.onSelect()
                  }}
                >
                  {it.icon && <Icon name={it.icon} size={16} />}
                  {it.label}
                  {it.shortcut && <span className="shortcut">{it.shortcut}</span>}
                </button>
              )
            })}
          </div>
        </Portal>
      )}
    </>
  )
}

// ---------------------------------------------------------------- tooltip
export function Tooltip({ content, children, shortcut, side = 'top' }: { content: ReactNode; children: ReactElement; shortcut?: string; side?: 'top' | 'right' | 'bottom' }) {
  const [rect, setRect] = useState<DOMRect | null>(null)
  const timer = useRef<number | undefined>(undefined)
  const tipRef = useRef<HTMLDivElement>(null)
  const [pos, setPos] = useState({ top: -999, left: -999 })
  useLayoutEffect(() => {
    if (!rect || !tipRef.current) return
    const t = tipRef.current.getBoundingClientRect()
    if (side === 'right') setPos({ top: rect.top + rect.height / 2 - t.height / 2, left: rect.right + 8 })
    else if (side === 'bottom') setPos({ top: rect.bottom + 6, left: rect.left + rect.width / 2 - t.width / 2 })
    else setPos({ top: rect.top - t.height - 6, left: Math.max(8, rect.left + rect.width / 2 - t.width / 2) })
  }, [rect, side])
  const show = (e: React.MouseEvent) => {
    const el = e.currentTarget as HTMLElement
    window.clearTimeout(timer.current)
    timer.current = window.setTimeout(() => setRect(el.getBoundingClientRect()), 300)
  }
  const hide = () => {
    window.clearTimeout(timer.current)
    setRect(null)
  }
  return (
    <span style={{ display: 'inline-flex' }} onMouseEnter={show} onMouseLeave={hide} onMouseDown={hide}>
      {children}
      {rect && (
        <Portal>
          <div ref={tipRef} className="tooltip" style={pos}>
            {content}
            {shortcut && <span className="kbd">{shortcut}</span>}
          </div>
        </Portal>
      )}
    </span>
  )
}
