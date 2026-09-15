import { useQueryClient } from '@tanstack/react-query'
import { ConfirmDialog, useToast } from '../../components/ui'
import { api } from '../../lib/api'
import { keys } from '../../lib/queries'
import { pluralize } from '../../lib/format'

/** Confirms POST /api/pending/discard (restores the live snapshot). */
export default function DiscardDialog({ open, onClose, count, liveVersion, title }: {
  open: boolean
  onClose: () => void
  count: number
  liveVersion: number
  title?: string
}) {
  const toast = useToast()
  const qc = useQueryClient()
  return (
    <ConfirmDialog
      open={open}
      onClose={onClose}
      danger
      title={title ?? `Discard ${pluralize(count, 'pending change')}?`}
      message={`Relay restores the live configuration (v${liveVersion}). Hosts, redirects, streams, access lists, backends and settings edited since then are reverted. This can't be undone.`}
      confirmLabel="Discard changes"
      onConfirm={async () => {
        try {
          await api.post('/api/pending/discard')
          toast.success('Pending changes discarded', `The configuration matches v${liveVersion} again.`)
        } catch (err) {
          toast.error(err, 'Discard failed')
        } finally {
          qc.invalidateQueries({ queryKey: keys.pending })
          qc.invalidateQueries({ queryKey: keys.versions })
          qc.invalidateQueries({ queryKey: ['entities'] })
          qc.invalidateQueries({ queryKey: ['settings'] })
        }
      }}
    />
  )
}
