// Password strength meter and confirm field shared by setup, account and gate screens.
import { useMemo } from 'react'
import { Input, cx } from '../../components/ui'
import { passwordStrength } from './authApi'
import './auth.css'

export function StrengthMeter({ password }: { password: string }) {
  const s = useMemo(() => passwordStrength(password), [password])
  return (
    <>
      <div className={cx('strength', s.score > 0 && `s${s.score}`)} aria-hidden>
        {[1, 2, 3, 4].map((i) => (
          <span key={i} className={cx(i <= s.score && 'on')} />
        ))}
      </div>
      <div className="field-hint" style={{ color: 'var(--ink-subtle)' }} aria-live="polite">{s.message}</div>
    </>
  )
}

export function ConfirmPasswordInput({ value, password, onChange, invalid, id, autoComplete = 'new-password', disabled }: {
  value: string
  password: string
  onChange: (v: string) => void
  invalid?: boolean
  id?: string
  autoComplete?: string
  disabled?: boolean
}) {
  const matches = value.length > 0 && value === password
  return (
    <div className="input-affix">
      <Input id={id} type="password" autoComplete={autoComplete} value={value} invalid={invalid} disabled={disabled} onChange={(e) => onChange(e.target.value)} />
      {value && (
        <span className="match-dot" title={matches ? 'Passwords match' : "Passwords don't match"} style={{ background: matches ? 'var(--ok)' : 'var(--danger)' }} />
      )}
    </div>
  )
}
