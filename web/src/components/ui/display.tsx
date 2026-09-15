import type { ReactNode } from 'react'
import { Icon, type IconName } from './Icon'
import { cx } from './controls'
import type { HealthState } from '../../lib/types'

// ---------------------------------------------------------------- badges & status
export type Tone = 'ok' | 'warn' | 'danger' | 'info' | 'muted' | 'dark' | 'pending' | 'outline' | 'plain' | undefined

export function Badge({ tone, children, title, className }: { tone?: Tone; children: ReactNode; title?: string; className?: string }) {
  return <span className={cx('badge', tone && tone !== 'muted' && tone, className)} title={title}>{children}</span>
}

export function Dot({ tone, pulse, large }: { tone?: 'ok' | 'warn' | 'danger' | 'muted' | 'info' | 'pending'; pulse?: boolean; large?: boolean }) {
  return <span className={cx('dot', tone, pulse && 'pulse', large && 'lg')} />
}

export const healthTone: Record<HealthState, 'ok' | 'warn' | 'danger' | 'muted'> = {
  healthy: 'ok', degraded: 'warn', down: 'danger', disabled: 'muted', unknown: 'muted',
}

/** Single dot + mono label, the design's status convention. */
export function Status({ tone, children, pulse }: { tone?: 'ok' | 'warn' | 'danger' | 'muted' | 'info' | 'pending'; children: ReactNode; pulse?: boolean }) {
  return (
    <span className="status">
      <Dot tone={tone} pulse={pulse} />
      {children}
    </span>
  )
}

/** Status badge for healthy/degraded/down/disabled/pending/draining. */
export function StateBadge({ state }: { state: 'healthy' | 'degraded' | 'down' | 'disabled' | 'pending' | 'draining' | 'unknown' }) {
  const tone: Record<string, Tone> = { healthy: 'ok', degraded: 'warn', down: 'danger', disabled: undefined, pending: 'pending', draining: 'info', unknown: undefined }
  const dot: Record<string, 'ok' | 'warn' | 'danger' | 'muted' | 'info' | 'pending'> = { healthy: 'ok', degraded: 'warn', down: 'danger', disabled: 'muted', pending: 'pending', draining: 'info', unknown: 'muted' }
  return (
    <Badge tone={tone[state]}>
      <Dot tone={dot[state]} />
      {state}
    </Badge>
  )
}

export function AIChip({ system }: { system?: boolean }) {
  return <span className={cx('ai-chip', system && 'sys')}>{system ? 'SYS' : 'AI'}</span>
}

export function Avatar({ name }: { name: string }) {
  return <span className="avatar">{name.slice(0, 1).toUpperCase()}</span>
}

// ---------------------------------------------------------------- layout
export function Card({ title, sub, actions, children, className, pad, style }: {
  title?: ReactNode; sub?: ReactNode; actions?: ReactNode; children?: ReactNode; className?: string; pad?: boolean; style?: React.CSSProperties
}) {
  return (
    <div className={cx('card', className)} style={style}>
      {(title || actions) && (
        <div className="card-header">
          {title}
          {sub && <span className="sub">{sub}</span>}
          <div className="spacer" />
          {actions}
        </div>
      )}
      {pad ? <div className="card-body">{children}</div> : children}
    </div>
  )
}

export function StatCard({ label, value, meta, metaTone, children }: { label: ReactNode; value: ReactNode; meta?: ReactNode; metaTone?: 'ok' | 'warn' | 'danger' | 'muted'; children?: ReactNode }) {
  const color = metaTone === 'ok' ? 'var(--ok)' : metaTone === 'warn' ? 'var(--warn)' : metaTone === 'danger' ? 'var(--danger)' : 'var(--ink-faint)'
  return (
    <div className="card stat-card">
      <div className="stat-label">{label}</div>
      <div className="row gap-8" style={{ alignItems: 'baseline' }}>
        <span className="stat-value">{value}</span>
        {meta && <span className="mono small" style={{ color }}>{meta}</span>}
      </div>
      {children}
    </div>
  )
}

export function SectionHeader({ title, description, actions }: { title: ReactNode; description?: ReactNode; actions?: ReactNode }) {
  return (
    <div className="row between" style={{ alignItems: 'flex-end' }}>
      <div>
        <div className="h1">{title}</div>
        {description && <div className="muted" style={{ marginTop: 4 }}>{description}</div>}
      </div>
      {actions && <div className="row">{actions}</div>}
    </div>
  )
}

export function EmptyState({ icon, title, description, actions }: { icon?: IconName; title: ReactNode; description?: ReactNode; actions?: ReactNode }) {
  return (
    <div className="empty">
      {icon && <div className="empty-icon"><Icon name={icon} size={22} /></div>}
      <div className="empty-title">{title}</div>
      {description && <div className="empty-desc">{description}</div>}
      {actions && <div className="row gap-8" style={{ marginTop: 10 }}>{actions}</div>}
    </div>
  )
}

export function Callout({ tone, icon, children, title, actions }: { tone?: 'warn' | 'danger' | 'ok' | 'info'; icon?: IconName; children?: ReactNode; title?: ReactNode; actions?: ReactNode }) {
  const defaultIcon: IconName = tone === 'danger' || tone === 'warn' ? 'warning' : tone === 'ok' ? 'check' : 'info'
  return (
    <div className={cx('callout', tone)}>
      <Icon name={icon ?? defaultIcon} size={16} className="icon" />
      <div className="grow">
        {title && <div className="semibold" style={{ marginBottom: 2 }}>{title}</div>}
        {children}
      </div>
      {actions}
    </div>
  )
}

// ---------------------------------------------------------------- tabs
export interface TabDef<T extends string = string> { id: T; label: ReactNode; count?: number; hot?: boolean }
export function Tabs<T extends string>({ tabs, value, onChange, flush }: { tabs: TabDef<T>[]; value: T; onChange: (v: T) => void; flush?: boolean }) {
  return (
    <div className={cx('tabs', flush && 'flush')} role="tablist">
      {tabs.map((t) => (
        <button key={t.id} role="tab" type="button" className={cx('tab', t.id === value && 'active')} onClick={() => onChange(t.id)}>
          {t.label}
          {t.count !== undefined && <span className={cx('tab-count', t.hot && 'hot')}>{t.count}</span>}
        </button>
      ))}
    </div>
  )
}

// ---------------------------------------------------------------- code
export function CodeBlock({ code, dark, wrap, maxHeight, className }: { code: string; dark?: boolean; wrap?: boolean; maxHeight?: number; className?: string }) {
  return <pre className={cx('code', dark && 'dark', wrap && 'wrap', className)} style={maxHeight ? { maxHeight } : undefined}>{code}</pre>
}

/** Unified diff renderer for simple line diffs. */
export function DiffView({ lines, maxHeight }: { lines: { type: 'add' | 'del' | 'ctx' | 'hunk'; text: string; oldNo?: number; newNo?: number }[]; maxHeight?: number }) {
  return (
    <div className="diff" style={maxHeight ? { maxHeight } : undefined}>
      {lines.map((l, i) => (
        <div key={i} className={cx('diff-line', l.type === 'add' && 'add', l.type === 'del' && 'del', l.type === 'hunk' && 'hunk')}>
          <span className="ln">{l.type === 'del' ? l.oldNo : l.newNo ?? l.oldNo ?? ''}</span>
          <span className="sign">{l.type === 'add' ? '+' : l.type === 'del' ? '-' : ' '}</span>
          <span>{l.text}</span>
        </div>
      ))}
    </div>
  )
}

// ---------------------------------------------------------------- charts
export function Bars({ values, height = 28, highlightLast, color, gap = 2 }: { values: number[]; height?: number; highlightLast?: boolean; color?: string; gap?: number }) {
  const max = Math.max(1, ...values)
  return (
    <div className="bars" style={{ height, gap }}>
      {values.map((v, i) => (
        <div
          key={i}
          style={{
            height: `${Math.max(3, (v / max) * 100)}%`,
            background: color ?? (highlightLast && i === values.length - 1 ? 'var(--ink)' : undefined),
          }}
        />
      ))}
    </div>
  )
}

export function Sparkline({ values, width = 120, height = 32, stroke = 'var(--ink)', fill = 'rgba(20,20,20,.06)' }: { values: number[]; width?: number; height?: number; stroke?: string; fill?: string }) {
  if (values.length < 2) return <svg width={width} height={height} />
  const max = Math.max(1, ...values)
  const step = width / (values.length - 1)
  const pts = values.map((v, i) => [i * step, height - 2 - (v / max) * (height - 4)] as const)
  const d = pts.map((p, i) => `${i ? 'L' : 'M'}${p[0].toFixed(1)},${p[1].toFixed(1)}`).join(' ')
  return (
    <svg width={width} height={height} viewBox={`0 0 ${width} ${height}`} preserveAspectRatio="none" style={{ display: 'block', width: '100%' }}>
      <path d={`${d} L${width},${height} L0,${height} Z`} fill={fill} />
      <path d={d} fill="none" stroke={stroke} strokeWidth="1.5" vectorEffect="non-scaling-stroke" />
    </svg>
  )
}

export function Meter({ value, tone }: { value: number; tone?: 'ok' | 'warn' | 'danger' }) {
  return (
    <div className={cx('meter', tone)}>
      <div style={{ width: `${Math.max(0, Math.min(100, value))}%` }} />
    </div>
  )
}

export function Stepper({ steps, current }: { steps: string[]; current: number }) {
  return (
    <div className="stepper">
      {steps.map((s, i) => (
        <div key={s} className="row gap-10">
          {i > 0 && <span className="step-line" />}
          <span className={cx('step', i === current && 'active', i < current && 'done')}>
            <span className="step-num">{i < current ? <Icon name="check" size={12} /> : i + 1}</span>
            {s}
          </span>
        </div>
      ))}
    </div>
  )
}

export function Skeleton({ height = 16, width = '100%' }: { height?: number; width?: number | string }) {
  return <div className="skeleton" style={{ height, width }} />
}
