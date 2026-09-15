// Dark pending-changes bar (design 17). Also owns the global apply action:
// listens for window 'relay:apply' (⌘⏎, "Apply now" toasts, history page).
import { useCallback, useEffect, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { Button } from '../../components/ui'
import { usePending, useRole } from '../../lib/queries'
import { pluralize } from '../../lib/format'
import { useApplyRunner, type PendingInfo } from './api'
import DiscardDialog from './DiscardDialog'
import './history.css'

export default function PendingBar() {
  const { canWrite } = useRole()
  const pending = usePending().data as PendingInfo | undefined
  const navigate = useNavigate()
  const { run, busy } = useApplyRunner()
  const [discardOpen, setDiscardOpen] = useState(false)

  const apply = useCallback(() => {
    if (!canWrite) return
    void run('/api/apply')
  }, [canWrite, run])

  useEffect(() => {
    const onApply = () => apply()
    window.addEventListener('relay:apply', onApply)
    return () => window.removeEventListener('relay:apply', onApply)
  }, [apply])

  const count = pending?.count ?? 0
  if (!canWrite || count === 0) return null

  const summary =
    pending?.summary ||
    (pending?.items ?? [])
      .slice(0, 3)
      .map((i) => `${i.name} ${i.action}`)
      .join(' · ')

  return (
    <>
      <div className="pending-bar" role="region" aria-label="Pending changes">
        <span className="pb-dot" />
        <span className="semibold nowrap">{pluralize(count, 'pending change')}</span>
        <span className="pb-summary truncate" title={summary}>{summary}</span>
        <div className="spacer" />
        <Button size="sm" variant="on-dark" onClick={() => navigate('/history?pending=1')}>
          Review diff
        </Button>
        {(pending?.liveVersion ?? 0) > 0 && (
          <Button size="sm" variant="on-dark" onClick={() => setDiscardOpen(true)} disabled={busy}>
            Discard
          </Button>
        )}
        <Button size="sm" variant="light" loading={busy} onClick={apply} title="Apply & reload (⌘⏎)">
          Apply &amp; reload
        </Button>
        <span className="pb-kbd">⌘⏎</span>
      </div>
      <DiscardDialog open={discardOpen} onClose={() => setDiscardOpen(false)} count={count} liveVersion={pending?.liveVersion ?? 0} />
    </>
  )
}
