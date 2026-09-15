// Dark approval toasts for new pending approvals (design 23).
import { useEffect, useRef } from 'react'
import { useLocation, useNavigate } from 'react-router-dom'
import { useToast } from '../../components/ui'
import { usePendingApprovals, useRole } from '../../lib/queries'
import { approvalVerb, expiresIn, type ApprovalItem } from './api'
import { useApprovalActions } from './parts'

const toastId = (id: string) => `approval-${id}`
const MAX_TOASTS = 3

export default function ApprovalToasts() {
  const { canWrite } = useRole()
  return canWrite ? <Toasts /> : null
}

function message(a: ApprovalItem, now: number): string {
  return [a.target, expiresIn(a.expiresAt, now)].filter(Boolean).join(' · ')
}

function Toasts() {
  const pending = usePendingApprovals()
  const toast = useToast()
  const navigate = useNavigate()
  const location = useLocation()
  const act = useApprovalActions()
  const shown = useRef(new Map<string, ApprovalItem>())
  const onApprovalsPage = location.pathname.startsWith('/logs/approvals')

  // Keep callbacks fresh without re-running the diff effect.
  const latest = useRef({ toast, navigate, act })
  latest.current = { toast, navigate, act }

  useEffect(() => {
    const items = pending.data as ApprovalItem[] | undefined
    if (!items) return
    const { toast, navigate, act } = latest.current
    const live = new Set(items.map((a) => a.id))
    for (const id of [...shown.current.keys()]) {
      if (!live.has(id)) {
        toast.dismiss(toastId(id))
        shown.current.delete(id)
      }
    }
    if (onApprovalsPage) return
    const now = Date.now()
    let visible = shown.current.size
    for (const a of items) {
      if (visible >= MAX_TOASTS) break
      if (shown.current.has(a.id)) continue
      const remaining = new Date(a.expiresAt).getTime() - now
      if (remaining <= 0) continue
      shown.current.set(a.id, a)
      visible++
      toast.show({
        id: toastId(a.id),
        kind: 'approval',
        title: `${a.clientName} wants to ${approvalVerb(a)}`,
        message: message(a, now),
        duration: remaining,
        actions: [
          { label: 'Approve', primary: true, onClick: () => void act(a, true) },
          { label: 'Deny', onClick: () => void act(a, false) },
          { label: 'Details', onClick: () => navigate('/logs/approvals') },
        ],
      })
    }
  }, [pending.data, onApprovalsPage])

  // Refresh the "expires in" countdown on visible toasts.
  useEffect(() => {
    const t = window.setInterval(() => {
      const now = Date.now()
      for (const a of shown.current.values()) latest.current.toast.update(toastId(a.id), { message: message(a, now) })
    }, 15_000)
    return () => window.clearInterval(t)
  }, [])

  return null
}
