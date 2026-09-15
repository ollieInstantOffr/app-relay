import { useState } from 'react'
import { Button, ConfirmDialog, Dialog, Field, Input, Select, useToast, type MenuEntry } from '../../components/ui'
import { useSaveEntity } from '../../lib/queries'
import { pluralize } from '../../lib/format'
import type { Backend, Server, ServerStats } from '../../lib/types'
import { applyNowAction, useServerActions, type StateResult } from './lbApi'

const addr = (s: Server) => `${s.address}:${s.port}`

/** Server ··· menu (design 23) plus its dialogs: weight, drain, remove. */
export function useServerMenu(backend: Backend) {
  const toast = useToast()
  const actions = useServerActions()
  const save = useSaveEntity('backends')
  const [drain, setDrain] = useState<{ server: Server; stats?: ServerStats } | null>(null)
  const [weight, setWeight] = useState<Server | null>(null)
  const [remove, setRemove] = useState<Server | null>(null)

  const report = (res: StateResult, title: string) => {
    if (res.runtime) toast.show({ kind: 'success', title, message: 'Applied to HAProxy immediately and saved to the backend.' })
    else toast.show({ kind: 'info', title, message: res.note ?? 'Saved · takes effect on the next apply.', actions: [applyNowAction] })
  }

  const setState = async (s: Server, state: Server['state'], title: string, grace?: number) => {
    try {
      report(await actions.setState(backend.id, s.id, state, grace), title)
    } catch (err) {
      toast.error(err, `Could not change ${addr(s)}`)
    }
  }

  const saveBackend = async (next: Backend, title: string) => {
    try {
      await save.mutateAsync(next)
      toast.show({ kind: 'success', title, message: `${next.name} added to pending changes.`, actions: [applyNowAction] })
    } catch (err) {
      toast.error(err, 'Could not save backend')
    }
  }

  const runCheck = async (s: Server) => {
    const id = toast.show({ kind: 'progress', title: `Checking ${addr(s)}…` })
    try {
      const r = await actions.check(backend.id, s.id)
      toast.dismiss(id)
      toast.show({
        kind: r.ok ? 'success' : 'warning',
        title: r.ok ? `${addr(s)} is healthy` : `${addr(s)} failed its health check`,
        message: `${r.detail} · checked from Relay`,
      })
    } catch (err) {
      toast.dismiss(id)
      toast.error(err, 'Health check could not run')
    }
  }

  const items = (s: Server, st?: ServerStats): MenuEntry[] => {
    const list: (MenuEntry | false)[] = [
      { header: `server ${s.address}` },
      { label: 'Set weight…', icon: 'edit', onSelect: () => setWeight(s) },
      s.state !== 'ready' && { label: 'Return to rotation', icon: 'reload', onSelect: () => setState(s, 'ready', `${addr(s)} is back in rotation`) },
      { label: 'Drain (graceful)', icon: 'drain', disabled: s.state !== 'ready', onSelect: () => setDrain({ server: s, stats: st }) },
      { label: 'Maintenance (immediate)', icon: 'power', disabled: s.state === 'maint', onSelect: () => setState(s, 'maint', `${addr(s)} is in maintenance`) },
      {
        label: s.role === 'backup' ? 'Mark as active' : 'Mark as backup',
        icon: 'load-balancer',
        onSelect: () =>
          saveBackend(
            { ...backend, servers: backend.servers.map((x) => (x.id === s.id ? { ...x, role: x.role === 'backup' ? 'active' : 'backup' } : x)) },
            s.role === 'backup' ? `${addr(s)} marked active` : `${addr(s)} marked as backup`,
          ),
      },
      { label: 'Run health check', icon: 'check', onSelect: () => runCheck(s) },
      'separator',
      { label: 'Remove from pool', icon: 'trash', danger: true, onSelect: () => setRemove(s) },
    ]
    return list.filter((x): x is MenuEntry => x !== false)
  }

  const dialogs = (
    <>
      {drain && (
        <DrainDialog
          server={drain.server}
          stats={drain.stats}
          onClose={() => setDrain(null)}
          onConfirm={(grace) => setState(drain.server, 'drain', `${addr(drain.server)} is draining`, grace)}
        />
      )}
      {weight && (
        <WeightDialog
          server={weight}
          onClose={() => setWeight(null)}
          onConfirm={async (w) => {
            try {
              report(await actions.setWeight(backend.id, weight.id, w), `${addr(weight)} weight set to ${w}`)
            } catch (err) {
              toast.error(err, 'Could not change weight')
              throw err
            }
          }}
        />
      )}
      <ConfirmDialog
        open={!!remove}
        onClose={() => setRemove(null)}
        danger
        title={`Remove ${remove ? addr(remove) : ''} from ${backend.name}?`}
        message="The server stops receiving traffic on the next apply. Existing sessions finish on the old process."
        confirmLabel="Remove server"
        onConfirm={() =>
          remove ? saveBackend({ ...backend, servers: backend.servers.filter((x) => x.id !== remove.id) }, `${addr(remove)} removed from ${backend.name}`) : undefined
        }
      />
    </>
  )

  return { items, dialogs }
}

const GRACE = [
  { value: '60', label: '1 min' },
  { value: '300', label: '5 min' },
  { value: '900', label: '15 min' },
  { value: '1800', label: '30 min' },
  { value: '3600', label: '1 h' },
  { value: '0', label: 'No timeout · stay in drain' },
]

export function DrainDialog({ server, stats, onClose, onConfirm }: {
  server: Server
  stats?: ServerStats
  onClose: () => void
  onConfirm: (graceSeconds: number) => Promise<void>
}) {
  const [grace, setGrace] = useState('300')
  const [busy, setBusy] = useState(false)
  return (
    <Dialog
      open
      onClose={onClose}
      icon="drain"
      title={`Set ${server.address} to drain`}
      description="New sessions go to other servers; existing ones finish. The server is marked MAINT when idle or after the timeout."
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button
            variant="primary"
            loading={busy}
            onClick={async () => {
              setBusy(true)
              await onConfirm(Number(grace))
              setBusy(false)
              onClose()
            }}
          >
            Drain
          </Button>
        </>
      }
    >
      <div className="grid-2">
        <Field label="Grace timeout">
          <Select options={GRACE} value={grace} onChange={setGrace} />
        </Field>
        <Field label="Active sessions" hint={stats ? (stats.current ? 'switches to MAINT once these finish' : 'idle · goes to MAINT right away') : 'HAProxy not reporting this server'}>
          <div className="mono" style={{ fontSize: 22, fontWeight: 600, lineHeight: '40px' }}>
            {stats ? pluralize(stats.current, 'session') : '—'}
          </div>
        </Field>
      </div>
    </Dialog>
  )
}

function WeightDialog({ server, onClose, onConfirm }: { server: Server; onClose: () => void; onConfirm: (w: number) => Promise<void> }) {
  const [value, setValue] = useState(String(server.weight))
  const [busy, setBusy] = useState(false)
  const n = Number(value)
  const valid = Number.isInteger(n) && n >= 1 && n <= 256
  const submit = async () => {
    if (!valid) return
    setBusy(true)
    try {
      await onConfirm(n)
      onClose()
    } catch {
      /* toast shown */
    } finally {
      setBusy(false)
    }
  }
  return (
    <Dialog
      open
      onClose={onClose}
      width={420}
      title={`Set weight for ${server.address}`}
      description="Share of new sessions relative to the other servers. Applied to HAProxy immediately and saved."
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" loading={busy} disabled={!valid} onClick={submit}>Set weight</Button>
        </>
      }
    >
      <Field label="Weight" hint="1–256 · 100 is the default" error={value && !valid ? 'Weight must be a whole number from 1 to 256' : undefined}>
        <Input
          mono
          autoFocus
          inputMode="numeric"
          value={value}
          invalid={!!value && !valid}
          onChange={(e) => setValue(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && submit()}
        />
      </Field>
    </Dialog>
  )
}
