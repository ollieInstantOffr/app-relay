// "Viewers can't change settings" card (design 30c).
import { useState } from 'react'
import { useLocation } from 'react-router-dom'
import { Button, Icon, useToast } from '../../components/ui'
import { api } from '../../lib/api'
import { roleLabel, useAuthSession } from './authApi'
import './auth.css'

export default function NotAllowed({ what, onViewReadOnly }: { what?: string; onViewReadOnly?: () => void }) {
  const { data: session } = useAuthSession()
  const location = useLocation()
  const toast = useToast()
  const [hidden, setHidden] = useState(false)
  const [requested, setRequested] = useState(false)
  const [busy, setBusy] = useState(false)

  if (hidden) return null
  const user = session?.user
  const role = user?.role ?? 'viewer'
  const subject = what ?? 'settings'
  const ask = role === 'viewer' ? 'Ask an admin to make you an editor' : 'Ask an admin for access'

  const request = async () => {
    setBusy(true)
    try {
      await api.post('/api/auth/request-access', { what: subject, path: location.pathname })
      setRequested(true)
      toast.success('Access requested', 'Admins can see your request in the audit log.')
    } catch (err) {
      toast.error(err, "Couldn't send the request")
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="card" style={{ padding: '16px 18px', display: 'flex', gap: 14, alignItems: 'flex-start' }}>
      <div className="dialog-icon warn">
        <Icon name="access" size={18} />
      </div>
      <div className="grow">
        <div className="semibold">
          {roleLabel[role]}s can't change {subject}
        </div>
        <div className="muted" style={{ marginTop: 2, lineHeight: 1.55 }}>
          You're signed in as <span className="medium">{user?.username ?? 'this account'}</span> ({role}). {ask}, or open this page read-only.
        </div>
        <div className="row gap-8" style={{ marginTop: 12 }}>
          <Button
            size="sm"
            onClick={() => {
              setHidden(true)
              onViewReadOnly?.()
            }}
          >
            View read-only
          </Button>
          <Button size="sm" variant="primary" loading={busy} disabled={requested} onClick={request}>
            {requested ? 'Access requested' : 'Request access'}
          </Button>
        </div>
      </div>
    </div>
  )
}
