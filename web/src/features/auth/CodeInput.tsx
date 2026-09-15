// Six single-digit boxes for TOTP codes: auto-advance, backspace, arrows, paste.
import { useEffect, useRef } from 'react'
import { cx } from '../../components/ui'
import './auth.css'

const LEN = 6

export function CodeInput({ value, onChange, onComplete, disabled, autoFocus, invalid, label = '2FA code' }: {
  value: string
  onChange: (v: string) => void
  onComplete?: (v: string) => void
  disabled?: boolean
  autoFocus?: boolean
  invalid?: boolean
  label?: string
}) {
  const refs = useRef<(HTMLInputElement | null)[]>([])

  useEffect(() => {
    if (autoFocus) refs.current[Math.min(value.length, LEN - 1)]?.focus()
    // Only on mount / when re-enabled.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [autoFocus, disabled])

  const focus = (i: number) => refs.current[Math.max(0, Math.min(LEN - 1, i))]?.focus()

  const commit = (next: string) => {
    const v = next.replace(/\D/g, '').slice(0, LEN)
    onChange(v)
    if (v.length === LEN) onComplete?.(v)
    return v
  }

  return (
    <div className={cx('code-boxes', invalid && 'invalid')} role="group" aria-label={label}>
      {Array.from({ length: LEN }, (_, i) => (
        <input
          key={i}
          ref={(el) => {
            refs.current[i] = el
          }}
          className="code-box"
          inputMode="numeric"
          autoComplete={i === 0 ? 'one-time-code' : 'off'}
          aria-label={`${label} digit ${i + 1}`}
          maxLength={LEN}
          disabled={disabled}
          value={value[i] ?? ''}
          onFocus={(e) => e.currentTarget.select()}
          onChange={(e) => {
            const typed = e.target.value.replace(/\D/g, '')
            if (!typed) {
              commit(value.slice(0, i) + value.slice(i + 1))
              return
            }
            // Autofill or IME may deliver several digits at once.
            const at = Math.min(i, value.length)
            const v = commit(value.slice(0, at) + typed + value.slice(at + typed.length))
            focus(at + typed.length >= LEN ? LEN - 1 : Math.min(v.length, at + typed.length))
          }}
          onKeyDown={(e) => {
            if (e.key === 'Backspace' && !value[i]) {
              e.preventDefault()
              if (i > 0) {
                commit(value.slice(0, i - 1) + value.slice(i))
                focus(i - 1)
              }
            } else if (e.key === 'ArrowLeft') {
              e.preventDefault()
              focus(i - 1)
            } else if (e.key === 'ArrowRight') {
              e.preventDefault()
              focus(i + 1)
            }
          }}
          onPaste={(e) => {
            const digits = e.clipboardData.getData('text').replace(/\D/g, '')
            if (!digits) return
            e.preventDefault()
            const v = commit(digits)
            focus(v.length)
          }}
        />
      ))}
    </div>
  )
}
