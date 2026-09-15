// Inline authenticator-app enrolment: QR code, manual key, 6-digit verification.
import { useEffect, useRef, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Button, CopyButton, Skeleton } from '../../components/ui'
import { api } from '../../lib/api'
import { keys } from '../../lib/queries'
import { CodeInput } from './CodeInput'
import { describeError, type AuthSession, type TotpEnroll } from './authApi'
import './auth.css'

export function TotpEnrollment({ onDone, onCancel, cancelLabel = 'Cancel', doneLabel = 'Turn on 2FA' }: {
  onDone: () => void
  onCancel?: () => void
  cancelLabel?: string
  doneLabel?: string
}) {
  const qc = useQueryClient()
  const started = useRef(false)
  const [data, setData] = useState<TotpEnroll | null>(null)
  const [code, setCode] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [showKey, setShowKey] = useState(false)

  useEffect(() => {
    // Enrolment generates a new secret server-side: request it exactly once
    // (React StrictMode runs effects twice in development).
    if (started.current) return
    started.current = true
    api.post<TotpEnroll>('/api/auth/totp/enroll').then(setData).catch((e) => setError(describeError(e)))
  }, [])

  const verify = async (c = code) => {
    if (c.length !== 6 || busy) return
    setBusy(true)
    setError('')
    try {
      const s = await api.post<AuthSession>('/api/auth/totp/verify', { code: c })
      qc.setQueryData(keys.session, s)
      onDone()
    } catch (e) {
      setError(describeError(e))
      setCode('')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="col gap-14">
      <div className="qr-box">
        {data ? <img src={data.qrPng} alt="QR code to add Relay to your authenticator app" /> : <Skeleton width={148} height={148} />}
        <div className="col gap-6" style={{ minWidth: 0 }}>
          <div className="semibold">Scan with your authenticator app</div>
          <div className="muted small" style={{ lineHeight: 1.5 }}>
            1Password, Bitwarden, Aegis, Google Authenticator or any TOTP app — then enter the 6-digit code it shows.
          </div>
          {data &&
            (showKey ? (
              <>
                <div className="secret-key">{data.secret}</div>
                <div>
                  <CopyButton text={data.secret} label="Copy key" />
                </div>
              </>
            ) : (
              <button type="button" className="link-btn" style={{ alignSelf: 'flex-start' }} onClick={() => setShowKey(true)}>
                Can't scan? Enter the key manually
              </button>
            ))}
        </div>
      </div>
      <div className="field">
        <label className="field-label">6-digit code</label>
        <CodeInput value={code} onChange={setCode} onComplete={verify} disabled={!data || busy} invalid={!!error} autoFocus={!!data} />
        {error && <div className="field-error">{error}</div>}
      </div>
      <div className="row gap-8">
        {onCancel && <Button onClick={onCancel}>{cancelLabel}</Button>}
        <div className="spacer" />
        <Button variant="primary" loading={busy} disabled={!data || code.length !== 6} onClick={() => verify()}>
          {doneLabel}
        </Button>
      </div>
    </div>
  )
}
