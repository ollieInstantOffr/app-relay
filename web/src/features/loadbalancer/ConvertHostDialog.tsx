import { useState } from 'react'
import { Button, Callout, Dialog, Field, Select, useToast } from '../../components/ui'
import { api } from '../../lib/api'
import { useEntities } from '../../lib/queries'
import type { Backend, Frontend, ProxyHost } from '../../lib/types'
import { applyNowAction, useInvalidateLB } from './lbApi'

/** "Convert a host's upstream into a pool" (empty state 22f). */
export default function ConvertHostDialog({ open, onClose, onConverted }: { open: boolean; onClose: () => void; onConverted: (backendId: string) => void }) {
  const hosts = useEntities('hosts', { enabled: open }).data ?? []
  const [hostId, setHostId] = useState('')
  const [busy, setBusy] = useState(false)
  const invalidate = useInvalidateLB()
  const toast = useToast()
  const candidates = hosts.filter((h) => !h.system && !h.upstream.backendId && h.upstream.host && h.upstream.port)

  const convert = async () => {
    setBusy(true)
    try {
      const res = await api.post<{ backend: Backend; frontend: Frontend; host: ProxyHost }>(`/api/backends/from-host/${hostId}`)
      invalidate()
      toast.show({
        kind: 'success',
        title: `Backend ${res.backend.name} created`,
        message: `${res.host.domains[0]} now routes through the load balancer · added to pending changes. Add more servers to the pool.`,
        actions: [applyNowAction],
      })
      onClose()
      onConverted(res.backend.id)
    } catch (err) {
      toast.error(err, 'Could not convert host')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog
      open={open}
      onClose={onClose}
      icon="load-balancer"
      title="Convert a host's upstream into a pool"
      description="Creates a backend with the host's upstream as its first server plus a localhost frontend, and points the host at it. Add more servers afterwards."
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" loading={busy} disabled={!hostId} onClick={convert}>Convert</Button>
        </>
      }
    >
      {candidates.length === 0 ? (
        <Callout>No proxy hosts with a direct upstream to convert.</Callout>
      ) : (
        <Field label="Proxy host">
          <Select
            placeholder="Pick a host…"
            value={hostId}
            onChange={setHostId}
            options={candidates.map((h) => ({ value: h.id, label: `${h.domains[0]} → ${h.upstream.scheme}://${h.upstream.host}:${h.upstream.port}` }))}
          />
        </Field>
      )}
    </Dialog>
  )
}
