// Pending approvals list with Approve / Deny / Preview (design 21 right panel).
import { useState } from 'react'
import { AIChip, Badge, Button, Callout, EmptyState, Skeleton, cx } from '../../components/ui'
import { errorMessage } from '../../lib/api'
import { ago, dateTime } from '../../lib/format'
import { usePendingApprovals, useRole, useSettings } from '../../lib/queries'
import { expiresIn, useApprovalHistory, useNow, type ApprovalItem } from './api'
import { PreviewView, Summary, plainSummary, useApprovalActions } from './parts'
import './mcp.css'

export default function ApprovalsPanel({ compact }: { compact?: boolean }) {
  const pending = usePendingApprovals()
  const history = useApprovalHistory(!compact)
  const settings = useSettings('mcp')
  const { canWrite } = useRole()
  const now = useNow(1000)
  const items = (pending.data ?? []) as ApprovalItem[]
  const minutes = settings.data?.approvalTimeoutMinutes
  const decided = (history.data ?? []).filter((a) => a.status !== 'pending').slice(0, 12)

  return (
    <div className={cx('approvals-panel', compact && 'compact')}>
      <div className="approvals-head">
        <div className="card-title">Pending approvals</div>
        <div className="small muted">
          {minutes ? `Write tools wait here up to ${minutes} min` : 'Write tools set to confirm wait here'}
        </div>
      </div>

      {pending.isLoading ? (
        <Skeleton height={compact ? 90 : 130} />
      ) : pending.error ? (
        <Callout tone="danger">{errorMessage(pending.error)}</Callout>
      ) : items.length === 0 ? (
        <EmptyState icon="mcp" title="No pending approvals" description={compact ? undefined : 'When an assistant calls a write tool that needs confirmation, it waits here until someone approves or denies it.'} />
      ) : (
        items.map((a) => <ApprovalCard key={a.id} a={a} now={now} canWrite={canWrite} />)
      )}

      {!compact && (
        <div className="approvals-history">
          <div className="card-title">Recently decided</div>
          {history.isLoading ? (
            <Skeleton height={60} />
          ) : decided.length === 0 ? (
            <div className="small muted">Nothing decided yet.</div>
          ) : (
            decided.map((a) => <HistoryRow key={a.id} a={a} />)
          )}
        </div>
      )}
    </div>
  )
}

function ApprovalCard({ a, now, canWrite }: { a: ApprovalItem; now: number; canWrite: boolean }) {
  const [open, setOpen] = useState(false)
  const [busy, setBusy] = useState<'approve' | 'deny' | null>(null)
  const act = useApprovalActions()
  const expired = new Date(a.expiresAt).getTime() <= now

  const run = async (approve: boolean) => {
    setBusy(approve ? 'approve' : 'deny')
    await act(a, approve)
    setBusy(null)
  }

  return (
    <div className="approval-card">
      <div className="approval-meta">
        <AIChip />
        <span className="tool">{a.tool}</span>
        <span className="muted">· {a.clientName} · {ago(a.createdAt, now)}</span>
        <span className="expires" title={dateTime(a.expiresAt)}>{expiresIn(a.expiresAt, now)}</span>
      </div>
      <div className="approval-summary">
        <Summary text={a.summary} />.
        {a.reason && <> Reason given: “{a.reason}”</>}
      </div>
      <div className="approval-actions">
        {canWrite && (
          <>
            <Button size="sm" variant="primary" loading={busy === 'approve'} disabled={!!busy || expired} onClick={() => run(true)}>
              Approve
            </Button>
            <Button size="sm" loading={busy === 'deny'} disabled={!!busy || expired} onClick={() => run(false)}>
              Deny
            </Button>
          </>
        )}
        <Button size="sm" variant="ghost" onClick={() => setOpen(!open)} aria-expanded={open}>
          {open ? 'Hide preview' : 'Preview change'}
        </Button>
        {!canWrite && <span className="small faint">Waiting for an admin or editor</span>}
      </div>
      {open && <PreviewView preview={a.preview} />}
    </div>
  )
}

function HistoryRow({ a }: { a: ApprovalItem }) {
  const failed = a.status === 'approved' && a.result.startsWith('Failed:')
  const tone = a.status === 'approved' ? (failed ? 'danger' : 'ok') : a.status === 'denied' ? 'danger' : undefined
  const label = failed ? 'failed' : a.status
  const by = a.status === 'expired' ? 'no decision' : a.decidedBy ? `by ${a.decidedBy}` : ''
  return (
    <div className="approval-history-row">
      <span className="when mono faint">{ago(a.decidedAt ?? a.expiresAt)}</span>
      <div className="grow">
        <div className="mcp-ellipsis">
          <span className="mono">{a.tool}</span> <span className="muted">· {a.clientName}</span>
        </div>
        <div className="faint mcp-ellipsis" title={a.result || plainSummary(a.summary)}>
          {plainSummary(a.summary)}
          {by && ` · ${by}`}
        </div>
      </div>
      <Badge tone={tone}>{label}</Badge>
    </div>
  )
}
