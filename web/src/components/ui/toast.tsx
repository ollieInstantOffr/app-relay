import { createContext, useCallback, useContext, useMemo, useRef, useState, type ReactNode } from 'react'
import { Icon } from './Icon'
import { Button } from './controls'
import { cx } from './controls'
import { AIChip } from './display'
import { errorMessage } from '../../lib/api'

export type ToastKind = 'success' | 'error' | 'warning' | 'info' | 'progress' | 'undo' | 'approval'

export interface ToastAction {
  label: string
  onClick: () => void
  primary?: boolean
}

export interface ToastOptions {
  id?: string
  kind?: ToastKind
  title: ReactNode
  message?: ReactNode
  actions?: ToastAction[]
  /** ms; 0 = persist. Defaults: 6 s, errors & approvals & progress persist. */
  duration?: number
  /** 0–100 for progress toasts */
  progress?: number
}

interface ToastItem extends ToastOptions {
  id: string
}

interface ToastApi {
  show: (t: ToastOptions) => string
  update: (id: string, patch: Partial<ToastOptions>) => void
  dismiss: (id: string) => void
  success: (title: ReactNode, message?: ReactNode) => string
  error: (err: unknown, title?: ReactNode) => string
}

const Ctx = createContext<ToastApi | null>(null)

export function useToast(): ToastApi {
  const v = useContext(Ctx)
  if (!v) throw new Error('useToast outside ToastProvider')
  return v
}

let seq = 0

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<ToastItem[]>([])
  const timers = useRef(new Map<string, number>())

  const dismiss = useCallback((id: string) => {
    window.clearTimeout(timers.current.get(id))
    timers.current.delete(id)
    setItems((xs) => xs.filter((x) => x.id !== id))
  }, [])

  const schedule = useCallback(
    (t: ToastItem) => {
      window.clearTimeout(timers.current.get(t.id))
      const persist = t.kind === 'error' || t.kind === 'approval' || t.kind === 'progress'
      const d = t.duration ?? (persist ? 0 : t.kind === 'undo' ? 5000 : 6000)
      if (d > 0) timers.current.set(t.id, window.setTimeout(() => dismiss(t.id), d))
    },
    [dismiss],
  )

  const show = useCallback(
    (t: ToastOptions) => {
      const id = t.id ?? `t${++seq}`
      const item = { ...t, id, kind: t.kind ?? 'success' }
      setItems((xs) => [...xs.filter((x) => x.id !== id), item].slice(-5))
      schedule(item)
      return id
    },
    [schedule],
  )

  const update = useCallback(
    (id: string, patch: Partial<ToastOptions>) => {
      setItems((xs) =>
        xs.map((x) => {
          if (x.id !== id) return x
          const next = { ...x, ...patch }
          if (patch.kind || patch.duration !== undefined) schedule(next)
          return next
        }),
      )
    },
    [schedule],
  )

  const api = useMemo<ToastApi>(
    () => ({
      show,
      update,
      dismiss,
      success: (title, message) => show({ kind: 'success', title, message }),
      error: (err, title = 'Something went wrong') => show({ kind: 'error', title, message: errorMessage(err) }),
    }),
    [show, update, dismiss],
  )

  return (
    <Ctx.Provider value={api}>
      {children}
      <div className="toast-stack">
        {items.map((t) => (
          <ToastView key={t.id} t={t} onDismiss={() => dismiss(t.id)} />
        ))}
      </div>
    </Ctx.Provider>
  )
}

function ToastView({ t, onDismiss }: { t: ToastItem; onDismiss: () => void }) {
  const dark = t.kind === 'approval'
  const icon =
    t.kind === 'success' ? <Icon name="check" size={16} style={{ color: 'var(--ok)' }} /> :
    t.kind === 'error' ? <Icon name="warning" size={16} style={{ color: 'var(--danger)' }} /> :
    t.kind === 'warning' ? <Icon name="warning" size={16} style={{ color: 'var(--warn)' }} /> :
    t.kind === 'progress' ? <span className="spinner" /> :
    t.kind === 'approval' ? <AIChip /> :
    t.kind === 'undo' ? <Icon name="rollback" size={16} /> :
    <Icon name="info" size={16} />
  return (
    <div className={cx('toast', dark && 'dark')} role="status">
      <div className="toast-icon">{icon}</div>
      <div className="grow">
        <div className="toast-title">{t.title}</div>
        {t.message && <div className="toast-msg">{t.message}</div>}
        {t.actions && t.actions.length > 0 && (
          <div className="toast-actions">
            {t.actions.map((a) => (
              <Button
                key={a.label}
                size="sm"
                variant={a.primary ? (dark ? 'light' : 'primary') : dark ? 'on-dark' : 'secondary'}
                onClick={() => {
                  a.onClick()
                  onDismiss()
                }}
              >
                {a.label}
              </Button>
            ))}
          </div>
        )}
      </div>
      <button className={cx('icon-btn bare')} style={dark ? { color: 'rgba(255,255,255,.6)' } : undefined} onClick={onDismiss} aria-label="Dismiss">
        <Icon name="close" size={14} />
      </button>
      {t.kind === 'progress' && t.progress !== undefined && <div className="toast-progress" style={{ width: `${t.progress}%` }} />}
    </div>
  )
}
