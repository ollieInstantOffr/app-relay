// Owner: slice hosts. Domain chips with syntax validation and live conflict messages.
import { useState, type ReactNode } from 'react'
import { cx } from '../../components/ui'
import { domainError, normalizeDomain } from './lib'

export function DomainsInput({
  values, onChange, conflicts, serverErrors, disabled, autoFocus, placeholder = 'add another…', hint, emptyPlaceholder = 'grafana.home.lan',
}: {
  values: string[]
  onChange: (v: string[]) => void
  conflicts?: Record<string, string>
  serverErrors?: string[]
  disabled?: boolean
  autoFocus?: boolean
  placeholder?: string
  emptyPlaceholder?: string
  hint?: ReactNode
}) {
  const [draft, setDraft] = useState('')
  const [draftErr, setDraftErr] = useState('')

  const commit = (raw = draft) => {
    const parts = raw.split(/[\s,]+/).map(normalizeDomain).filter(Boolean)
    if (!parts.length) {
      setDraft('')
      setDraftErr('')
      return
    }
    const next = [...values]
    const rest: string[] = []
    let err = ''
    for (const p of parts) {
      const e = domainError(p)
      if (e) {
        err = e
        rest.push(p)
      } else if (!next.includes(p)) {
        next.push(p)
      }
    }
    if (next.length !== values.length) onChange(next)
    setDraft(rest.join(' '))
    setDraftErr(err)
  }

  const conflictMsgs = values.filter((d) => conflicts?.[d]).map((d) => `${d} — ${conflicts![d]}`)
  const messages = [...new Set([draftErr, ...conflictMsgs, ...(serverErrors ?? [])].filter(Boolean))]

  return (
    <>
      <div
        className={cx('chips-input', messages.length > 0 && 'invalid')}
        onClick={(e) => (e.currentTarget.querySelector('input') as HTMLInputElement | null)?.focus()}
      >
        {values.map((v) => (
          <span key={v} className={cx('chip-x', conflicts?.[v] && 'hosts-chip-bad')} title={conflicts?.[v]}>
            {v}
            {!disabled && (
              <button
                type="button"
                onClick={(e) => {
                  e.stopPropagation()
                  onChange(values.filter((x) => x !== v))
                }}
                aria-label={`Remove ${v}`}
              >
                ×
              </button>
            )}
          </span>
        ))}
        <input
          value={draft}
          disabled={disabled}
          autoFocus={autoFocus}
          aria-label="Add domain"
          placeholder={disabled ? '' : values.length ? placeholder : emptyPlaceholder}
          onChange={(e) => {
            setDraft(e.target.value)
            if (draftErr) setDraftErr('')
          }}
          onBlur={() => commit()}
          onPaste={(e) => {
            const text = e.clipboardData.getData('text')
            if (/[\s,]/.test(text.trim())) {
              e.preventDefault()
              commit(draft + text)
            }
          }}
          onKeyDown={(e) => {
            if (e.key === 'Enter' || e.key === ',' || e.key === ' ') {
              if (e.key === 'Enter' && !draft.trim()) return
              e.preventDefault()
              commit()
            } else if (e.key === 'Backspace' && !draft && values.length) {
              onChange(values.slice(0, -1))
            }
          }}
        />
      </div>
      {messages.length > 0
        ? messages.map((m) => <div key={m} className="field-error">{m}</div>)
        : hint ? <div className="field-hint">{hint}</div> : null}
    </>
  )
}
