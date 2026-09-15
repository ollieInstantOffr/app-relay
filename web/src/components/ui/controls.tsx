import { forwardRef, useEffect, useLayoutEffect, useRef, useState, type ButtonHTMLAttributes, type InputHTMLAttributes, type ReactNode, type SelectHTMLAttributes, type TextareaHTMLAttributes } from 'react'
import { createPortal } from 'react-dom'
import { Icon, type IconName } from './Icon'

const cx = (...c: (string | false | undefined | null)[]) => c.filter(Boolean).join(' ')
export { cx }

// ---------------------------------------------------------------- buttons
type ButtonProps = ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: 'primary' | 'secondary' | 'ghost' | 'danger' | 'link' | 'on-dark' | 'light'
  size?: 'sm' | 'default' | 'md' | 'lg'
  icon?: IconName
  iconRight?: IconName
  loading?: boolean
  block?: boolean
}

export const Button = forwardRef<HTMLButtonElement, ButtonProps>(function Button(
  { variant = 'secondary', size = 'default', icon, iconRight, loading, block, className, children, disabled, type = 'button', ...rest },
  ref,
) {
  return (
    <button
      ref={ref}
      type={type}
      className={cx('btn', variant !== 'secondary' && `btn-${variant}`, size !== 'default' && `btn-${size}`, block && 'btn-block', className)}
      disabled={disabled || loading}
      {...rest}
    >
      {loading ? <span className="spinner" /> : icon ? <Icon name={icon} size={size === 'sm' ? 14 : 16} /> : null}
      {children}
      {iconRight && <Icon name={iconRight} size={14} />}
    </button>
  )
})

export const IconButton = forwardRef<HTMLButtonElement, ButtonHTMLAttributes<HTMLButtonElement> & { icon: IconName; bare?: boolean; size?: number; label?: string }>(
  function IconButton({ icon, bare, size = 16, label, className, type = 'button', ...rest }, ref) {
    return (
      <button ref={ref} type={type} aria-label={label} title={label} className={cx('icon-btn', bare && 'bare', className)} {...rest}>
        <Icon name={icon} size={size} />
      </button>
    )
  },
)

// ---------------------------------------------------------------- inputs
type InputProps = InputHTMLAttributes<HTMLInputElement> & { mono?: boolean; invalid?: boolean; inputSize?: 'sm' | 'default' }
export const Input = forwardRef<HTMLInputElement, InputProps>(function Input({ mono, invalid, inputSize, className, ...rest }, ref) {
  return <input ref={ref} className={cx('input', mono && 'mono', invalid && 'invalid', inputSize === 'sm' && 'sm', className)} {...rest} />
})

type TextareaProps = TextareaHTMLAttributes<HTMLTextAreaElement> & { mono?: boolean; invalid?: boolean }
export const Textarea = forwardRef<HTMLTextAreaElement, TextareaProps>(function Textarea({ mono, invalid, className, ...rest }, ref) {
  return <textarea ref={ref} className={cx('textarea', mono && 'mono', invalid && 'invalid', className)} {...rest} />
})

export interface Option {
  value: string
  label: string
  disabled?: boolean
  /** Muted text after the label. Defaults to the part after the first " · " in label. */
  hint?: string
}
type SelectProps = Omit<SelectHTMLAttributes<HTMLSelectElement>, 'onChange' | 'value'> & {
  options: (Option | string)[]
  value?: string | number | readonly string[]
  onChange?: (value: string) => void
  mono?: boolean
  invalid?: boolean
  inputSize?: 'sm' | 'default'
  placeholder?: string
}

function splitLabel(o: Option): [string, string | undefined] {
  if (o.hint !== undefined) return [o.label, o.hint]
  const i = o.label.indexOf(' · ')
  return i > 0 ? [o.label.slice(0, i), o.label.slice(i + 3)] : [o.label, undefined]
}

/** Custom listbox select (keyboard accessible, styled menu instead of the OS popup). */
export function Select({
  options, onChange, mono, invalid, inputSize, className, placeholder, value, disabled, style, id, title, autoFocus, onBlur,
  'aria-label': ariaLabel,
}: SelectProps) {
  const items: Option[] = [
    ...(placeholder !== undefined ? [{ value: '', label: placeholder }] : []),
    ...options.map((o) => (typeof o === 'string' ? { value: o, label: o } : o)),
  ]
  const current = value === undefined || value === null ? '' : String(value)
  const selectedIndex = items.findIndex((o) => o.value === current)
  const selected = selectedIndex >= 0 ? items[selectedIndex] : undefined

  const [open, setOpen] = useState(false)
  const [active, setActive] = useState(-1)
  const [pos, setPos] = useState<{ top: number; left: number; width: number; maxHeight: number } | null>(null)
  const triggerRef = useRef<HTMLButtonElement>(null)
  const menuRef = useRef<HTMLDivElement>(null)
  const typeahead = useRef({ text: '', at: 0 })
  const activeRef = useRef(active)
  activeRef.current = active

  const enabledIndex = (from: number, dir: 1 | -1) => {
    for (let i = from, n = 0; n < items.length; i += dir, n++) {
      const idx = (i + items.length) % items.length
      if (!items[idx].disabled) return idx
    }
    return -1
  }

  const openMenu = () => {
    if (disabled) return
    setActive(selectedIndex >= 0 ? selectedIndex : enabledIndex(0, 1))
    setOpen(true)
  }
  const close = (refocus = true) => {
    setOpen(false)
    if (refocus) triggerRef.current?.focus()
  }
  const choose = (i: number) => {
    const o = items[i]
    if (!o || o.disabled) return
    if (o.value !== current) onChange?.(o.value)
    close()
  }

  useLayoutEffect(() => {
    if (!open || !triggerRef.current) return
    const place = () => {
      const r = triggerRef.current!.getBoundingClientRect()
      const below = window.innerHeight - r.bottom - 12
      const above = r.top - 12
      const wanted = Math.min(320, items.length * 34 + 12)
      const flip = below < Math.min(wanted, 200) && above > below
      const maxHeight = Math.max(120, Math.min(320, flip ? above : below))
      const height = Math.min(wanted, maxHeight)
      const width = Math.max(r.width, 180)
      const left = Math.max(8, Math.min(r.left, window.innerWidth - width - 8))
      setPos({ top: flip ? r.top - 4 - height : r.bottom + 4, left, width, maxHeight })
    }
    place()
    const onScroll = (e: Event) => {
      if (menuRef.current && e.target instanceof Node && menuRef.current.contains(e.target)) return
      close(false)
    }
    window.addEventListener('resize', place)
    window.addEventListener('scroll', onScroll, true)
    return () => {
      window.removeEventListener('resize', place)
      window.removeEventListener('scroll', onScroll, true)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, items.length])

  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      const stop = () => {
        e.preventDefault()
        e.stopPropagation()
        e.stopImmediatePropagation()
      }
      switch (e.key) {
        case 'Escape':
          stop()
          close()
          return
        case 'Tab':
          close(false)
          return
        case 'ArrowDown':
          stop()
          setActive((a) => enabledIndex(a + 1, 1))
          return
        case 'ArrowUp':
          stop()
          setActive((a) => enabledIndex(a - 1, -1))
          return
        case 'Home':
          stop()
          setActive(enabledIndex(0, 1))
          return
        case 'End':
          stop()
          setActive(enabledIndex(items.length - 1, -1))
          return
        case 'Enter':
        case ' ':
          stop()
          choose(activeRef.current)
          return
      }
      if (e.key.length === 1 && !e.metaKey && !e.ctrlKey && !e.altKey) {
        stop()
        const now = Date.now()
        const t = typeahead.current
        t.text = now - t.at > 600 ? e.key.toLowerCase() : t.text + e.key.toLowerCase()
        t.at = now
        const i = items.findIndex((o) => !o.disabled && o.label.toLowerCase().startsWith(t.text))
        if (i >= 0) setActive(i)
      }
    }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, items, current])

  useEffect(() => {
    if (open) menuRef.current?.querySelector('.select-option.active')?.scrollIntoView({ block: 'nearest' })
  }, [open, active, pos])

  const [main, hint] = selected ? splitLabel(selected) : [current || placeholder || '', undefined]
  const isPlaceholder = !selected || (placeholder !== undefined && selected.value === '')

  return (
    <>
      <button
        ref={triggerRef}
        type="button"
        id={id}
        title={title}
        style={style}
        autoFocus={autoFocus}
        disabled={disabled}
        aria-label={ariaLabel}
        aria-haspopup="listbox"
        aria-expanded={open}
        className={cx('select select-trigger', mono && 'mono', invalid && 'invalid', inputSize === 'sm' && 'sm', className)}
        onClick={() => (open ? close() : openMenu())}
        onBlur={onBlur as unknown as React.FocusEventHandler<HTMLButtonElement>}
        onKeyDown={(e) => {
          if (!open && ['ArrowDown', 'ArrowUp', 'Enter', ' '].includes(e.key)) {
            e.preventDefault()
            openMenu()
          }
        }}
      >
        <span className={cx('select-value', isPlaceholder && 'placeholder')}>
          {main}
          {hint && <span className="select-hint"> · {hint}</span>}
        </span>
      </button>
      {open && pos && (
        createPortal(<>
          <div className="select-overlay" onMouseDown={() => close(false)} />
          <div
            ref={menuRef}
            className={cx('select-menu', mono && 'mono')}
            role="listbox"
            style={{ top: pos.top, left: pos.left, minWidth: pos.width, maxHeight: pos.maxHeight }}
          >
            {items.map((o, i) => {
              const [l, h] = splitLabel(o)
              return (
                <div
                  key={o.value + ':' + i}
                  role="option"
                  aria-selected={i === selectedIndex}
                  aria-disabled={o.disabled}
                  className={cx('select-option', i === selectedIndex && 'selected', i === active && 'active', o.disabled && 'disabled')}
                  onMouseEnter={() => !o.disabled && setActive(i)}
                  onMouseDown={(e) => e.preventDefault()}
                  onClick={() => choose(i)}
                >
                  <Icon name="check" size={14} className="select-check" />
                  <span className={cx('select-label', placeholder !== undefined && o.value === '' && 'placeholder')}>{l}</span>
                  {h && <span className="select-hint">{h}</span>}
                </div>
              )
            })}
            {items.length === 0 && <div className="select-empty">No options</div>}
          </div>
        </>, document.body)
      )}
    </>
  )
}

/** Password input with Show/Hide. */
export function PasswordInput(props: InputProps) {
  const [show, setShow] = useState(false)
  return (
    <div className="input-affix">
      <Input {...props} type={show ? 'text' : 'password'} style={{ paddingRight: 60, ...props.style }} />
      <button type="button" className="btn btn-ghost btn-sm affix-btn" onClick={() => setShow(!show)}>
        {show ? 'Hide' : 'Show'}
      </button>
    </div>
  )
}

export function Field({ label, hint, error, children, className, aside, htmlFor }: {
  label?: ReactNode; hint?: ReactNode; error?: ReactNode; children: ReactNode; className?: string; aside?: ReactNode; htmlFor?: string
}) {
  return (
    <div className={cx('field', className)}>
      {(label || aside) && (
        <div className="row between">
          {label && <label className="field-label" htmlFor={htmlFor}>{label}</label>}
          {aside}
        </div>
      )}
      {children}
      {error ? <div className="field-error">{error}</div> : hint ? <div className="field-hint">{hint}</div> : null}
    </div>
  )
}

// ---------------------------------------------------------------- toggles & choices
export function Toggle({ checked, onChange, disabled, label }: { checked: boolean; onChange?: (v: boolean) => void; disabled?: boolean; label?: string }) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      disabled={disabled}
      className={cx('toggle', checked && 'on')}
      onClick={() => onChange?.(!checked)}
    />
  )
}

/** Bordered card with title, description and a toggle (drawer grids). */
export function ToggleCard({ title, description, checked, onChange, disabled }: { title: ReactNode; description?: ReactNode; checked: boolean; onChange?: (v: boolean) => void; disabled?: boolean }) {
  return (
    <div className="toggle-card">
      <div className="grow">
        <div className="toggle-title">{title}</div>
        {description && <div className="toggle-desc">{description}</div>}
      </div>
      <Toggle checked={checked} onChange={onChange} disabled={disabled} />
    </div>
  )
}

/** Row inside a card with title/description left and control right (settings pages). */
export function ToggleRow({ title, description, checked, onChange, disabled, children }: {
  title: ReactNode; description?: ReactNode; checked?: boolean; onChange?: (v: boolean) => void; disabled?: boolean; children?: ReactNode
}) {
  return (
    <div className="toggle-row">
      <div className="grow">
        <div className="toggle-title">{title}</div>
        {description && <div className="toggle-desc">{description}</div>}
      </div>
      {children ?? <Toggle checked={!!checked} onChange={onChange} disabled={disabled} />}
    </div>
  )
}

export function Checkbox({ checked, onChange, label, disabled, indeterminate }: { checked: boolean; onChange?: (v: boolean) => void; label?: ReactNode; disabled?: boolean; indeterminate?: boolean }) {
  const box = (
    <input
      type="checkbox"
      className="checkbox"
      checked={checked}
      disabled={disabled}
      ref={(el) => {
        if (el) el.indeterminate = !!indeterminate
      }}
      onChange={(e) => onChange?.(e.target.checked)}
      onClick={(e) => e.stopPropagation()}
    />
  )
  if (!label) return box
  return (
    <label className="check-label">
      {box}
      <span>{label}</span>
    </label>
  )
}

export function RadioCard({ selected, onSelect, title, description, disabled, children }: {
  selected: boolean; onSelect?: () => void; title: ReactNode; description?: ReactNode; disabled?: boolean; children?: ReactNode
}) {
  return (
    <div className={cx('radio-card', selected && 'selected', disabled && 'disabled')} onClick={() => !disabled && onSelect?.()}>
      <input type="radio" className="radio" checked={selected} disabled={disabled} readOnly />
      <div className="grow">
        <div className="toggle-title">{title}</div>
        {description && <div className="toggle-desc" style={{ fontSize: 12 }}>{description}</div>}
        {children}
      </div>
    </div>
  )
}

export function Segmented<T extends string>({ value, onChange, options, disabled }: {
  value: T; onChange: (v: T) => void; options: ({ value: T; label: ReactNode } | T)[]; disabled?: boolean
}) {
  return (
    <div className="segmented" role="tablist">
      {options.map((o) => {
        const opt = typeof o === 'string' ? { value: o, label: o } : o
        return (
          <button key={opt.value} type="button" disabled={disabled} className={cx(opt.value === value && 'active')} onClick={() => onChange(opt.value)}>
            {opt.label}
          </button>
        )
      })}
    </div>
  )
}

/** Tag input for domains etc. Enter / comma / space adds. */
export function ChipsInput({ values, onChange, placeholder = 'add another…', invalid, validate, mono = true }: {
  values: string[]; onChange: (v: string[]) => void; placeholder?: string; invalid?: boolean; validate?: (v: string) => boolean; mono?: boolean
}) {
  const [draft, setDraft] = useState('')
  const commit = () => {
    const v = draft.trim()
    if (!v) return
    if (validate && !validate(v)) return
    if (!values.includes(v)) onChange([...values, v])
    setDraft('')
  }
  return (
    <div className={cx('chips-input', invalid && 'invalid')} onClick={(e) => (e.currentTarget.querySelector('input') as HTMLInputElement)?.focus()}>
      {values.map((v) => (
        <span key={v} className="chip-x" style={mono ? undefined : { fontFamily: 'var(--font-sans)' }}>
          {v}
          <button type="button" onClick={() => onChange(values.filter((x) => x !== v))} aria-label={`Remove ${v}`}>×</button>
        </span>
      ))}
      <input
        value={draft}
        placeholder={values.length ? placeholder : placeholder}
        onChange={(e) => setDraft(e.target.value)}
        onBlur={commit}
        onKeyDown={(e) => {
          if (e.key === 'Enter' || e.key === ',' || e.key === ' ') {
            e.preventDefault()
            commit()
          } else if (e.key === 'Backspace' && !draft && values.length) {
            onChange(values.slice(0, -1))
          }
        }}
      />
    </div>
  )
}

export function Spinner({ large }: { large?: boolean }) {
  return <span className={cx('spinner', large && 'lg')} />
}

export function Kbd({ children, dark }: { children: ReactNode; dark?: boolean }) {
  return <span className={cx('kbd', dark && 'dark')}>{children}</span>
}

export function CopyButton({ text, label = 'Copy', size = 'sm' }: { text: string; label?: string; size?: 'sm' | 'default' }) {
  const [done, setDone] = useState(false)
  return (
    <Button
      size={size}
      icon={done ? 'check' : 'copy'}
      onClick={() => {
        navigator.clipboard?.writeText(text)
        setDone(true)
        setTimeout(() => setDone(false), 1500)
      }}
    >
      {done ? 'Copied' : label}
    </Button>
  )
}
