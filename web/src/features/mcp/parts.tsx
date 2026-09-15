// Shared approval UI pieces: summary with bold targets, change preview, decide actions.
import { CodeBlock, DiffView, useToast } from '../../components/ui'
import { useDecideApproval, type ApprovalItem } from './api'

/** Renders "Drain **10.0.0.92:9000** in backend **minio**" with bold targets. */
export function Summary({ text }: { text: string }) {
  const parts = text.split(/\*\*(.+?)\*\*/g)
  return (
    <>
      {parts.map((p, i) => (i % 2 === 1 ? <strong key={i}>{p}</strong> : <span key={i}>{p}</span>))}
    </>
  )
}

export function plainSummary(text: string): string {
  return text.replace(/\*\*(.+?)\*\*/g, '$1')
}

/** Preview of what an approval changes: a diff ("+ ", "- ", "  " lines) or plain text. */
export function PreviewView({ preview, maxHeight = 280 }: { preview: string; maxHeight?: number }) {
  if (!preview.trim()) return <div className="small muted">No preview available for this call.</div>
  const lines = preview.split('\n')
  const isDiff = lines.every((l) => /^[+\- ] /.test(l))
  if (!isDiff) return <CodeBlock code={preview} maxHeight={maxHeight} />
  let oldNo = 0
  let newNo = 0
  const out = lines.map((l) => {
    const text = l.slice(2)
    if (l[0] === '+') {
      newNo++
      return { type: 'add' as const, text, newNo }
    }
    if (l[0] === '-') {
      oldNo++
      return { type: 'del' as const, text, oldNo }
    }
    oldNo++
    newNo++
    return { type: 'ctx' as const, text, oldNo, newNo }
  })
  return <DiffView lines={out} maxHeight={maxHeight} />
}

/** Approve/deny with result toasts. Resolves true when the decision was recorded. */
export function useApprovalActions() {
  const decide = useDecideApproval()
  const toast = useToast()
  return async (a: ApprovalItem, approve: boolean): Promise<boolean> => {
    try {
      const res = await decide.mutateAsync({ id: a.id, approve })
      if (!approve) {
        toast.show({ kind: 'info', title: `Denied ${a.tool}`, message: `${a.clientName} was told the request was denied.` })
      } else if (res.failed) {
        toast.show({ kind: 'error', title: `${a.tool} failed after approval`, message: res.result })
      } else {
        toast.success(`Approved ${a.tool}`, res.result)
      }
      return true
    } catch (err) {
      toast.error(err, approve ? `Could not approve ${a.tool}` : `Could not deny ${a.tool}`)
      return false
    }
  }
}
