// Engine upgrade: confirmation dialog, live progress panel, result toasts.
import { useEffect, useRef, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Button, Card, Dialog, Icon, Meter, Spinner, useToast } from '../../components/ui'
import { api } from '../../lib/api'
import { keys } from '../../lib/queries'
import { ago } from '../../lib/format'
import type { EngineUpdateInfo, UpgradeJob, UpgradeStep } from '../../lib/types'
import { currentVersion, engineTitle, updatesKey, upgradeKey } from './enginesApi'

const VISIBLE_STEPS: { id: string; label: string }[] = [
  { id: 'pull', label: 'Pull' },
  { id: 'validate', label: 'Validate config' },
  { id: 'swap', label: 'Swap container' },
  { id: 'health', label: 'Health check' },
]

export function UpgradeDialog({ info, version, open, onClose }: { info: EngineUpdateInfo; version: string; open: boolean; onClose: () => void }) {
  const qc = useQueryClient()
  const toast = useToast()
  const [busy, setBusy] = useState(false)
  const name = engineTitle[info.engine]
  const target = `${info.engine}:${version}-alpine`

  const start = async () => {
    setBusy(true)
    try {
      const job = await api.post<UpgradeJob>(`/api/engines/${info.engine}/upgrade`, { version })
      qc.setQueryData(upgradeKey, job)
      onClose()
    } catch (err) {
      toast.error(err, `Couldn't start the ${name} upgrade`)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog
      open={open}
      onClose={onClose}
      width={520}
      icon="reload"
      title={`Upgrade ${name} to ${version}?`}
      description={
        <>
          <span className="mono">{info.image || '—'}</span> → <span className="mono">{target}</span>
        </>
      }
      footer={
        <>
          <Button onClick={onClose} disabled={busy}>Cancel</Button>
          <Button variant="primary" icon="reload" loading={busy} onClick={start}>
            Upgrade to {version}
          </Button>
        </>
      }
    >
      <ol className="upg-list">
        <li><span className="n">1</span><span><b>Pull</b> {target} from Docker Hub.</span></li>
        <li><span className="n">2</span><span><b>Validate</b> the live configuration with {info.engine === 'nginx' ? 'nginx -t' : 'haproxy -c'} on the new version in a throwaway container. Nothing changes if it fails.</span></li>
        <li><span className="n">3</span><span><b>Swap</b> the {info.container || name} container: same volumes, network and settings, new image.</span></li>
        <li><span className="n">4</span><span><b>Health check</b>: wait for the agent, restore the live config and watch healthy hosts for 10 s.</span></li>
      </ol>
      <div className="small muted" style={{ marginTop: 14, lineHeight: 1.5 }}>
        {info.engine === 'nginx'
          ? 'Traffic stops briefly (usually 1–3 s) while the old container stops and the new one starts. '
          : 'Load balancer frontends pause briefly while the containers swap. '}
        If anything fails after the swap, Relay restores the previous container automatically. A backup is created first.
      </div>
    </Dialog>
  )
}

function stepIcon(s: UpgradeStep['status']) {
  if (s === 'done') return <Icon name="check" size={12} />
  if (s === 'running') return <Spinner />
  if (s === 'failed') return <Icon name="warning" size={12} />
  return null
}

export function UpgradeProgress({ job, onDismiss }: { job: UpgradeJob; onDismiss?: () => void }) {
  const name = engineTitle[job.engine]
  const running = job.status === 'running'
  const tone = job.status === 'succeeded' ? 'ok' : job.status === 'running' ? undefined : job.status === 'rolled_back' ? 'warn' : 'danger'
  const title = running
    ? `Upgrading ${name} ${job.from || ''} → ${job.to}`
    : job.status === 'succeeded'
      ? `${name} upgraded to ${job.to}`
      : job.status === 'rolled_back'
        ? `${name} ${job.to} rolled back`
        : `${name} upgrade to ${job.to} failed`
  const steps = VISIBLE_STEPS.map((v) => ({ ...v, step: job.steps.find((s) => s.id === v.id) }))
  return (
    <Card
      title={
        <span className="row gap-8">
          {running ? <Spinner /> : <Icon name={job.status === 'succeeded' ? 'check' : 'warning'} size={16} />}
          {title}
        </span>
      }
      sub={running ? `${job.progress}%` : job.finishedAt ? ago(job.finishedAt) : undefined}
      actions={!running && onDismiss ? <Button size="sm" variant="ghost" onClick={onDismiss}>Dismiss</Button> : undefined}
    >
      <div className="upg-panel">
        {running && <Meter value={job.progress} />}
        <div className="upg-steps">
          {steps.map(({ id, label, step }) => (
            <div key={id} className={`upg-step ${step?.status ?? 'pending'}`} title={step?.detail}>
              <div className="bar"><div /></div>
              <div className="t">{stepIcon(step?.status ?? 'pending')}{label}</div>
              <div className="d">{step?.status === 'skipped' ? `skipped${step.detail ? ` · ${step.detail}` : ''}` : step?.detail || ' '}</div>
            </div>
          ))}
        </div>
        <div className={`small ${tone === 'danger' ? '' : 'muted'}`} style={tone === 'danger' ? { color: 'var(--danger-text)' } : undefined}>
          {job.message}
          {job.error && job.error !== job.message && <div className="mono small faint" style={{ marginTop: 4 }}>{job.error}</div>}
        </div>
        {!running && job.output && <pre className="upg-output">{job.output}</pre>}
      </div>
    </Card>
  )
}

/** Shows a toast when a running job finishes (once per job). */
export function useUpgradeToasts(job: UpgradeJob | null | undefined, infos: EngineUpdateInfo[]) {
  const toast = useToast()
  const qc = useQueryClient()
  const seen = useRef<Record<string, string>>({})
  useEffect(() => {
    if (!job) return
    const prev = seen.current[job.id]
    seen.current[job.id] = job.status
    if (prev !== 'running' || job.status === 'running') return
    qc.invalidateQueries({ queryKey: updatesKey })
    qc.invalidateQueries({ queryKey: keys.engines })
    const name = engineTitle[job.engine]
    if (job.status === 'succeeded') toast.show({ kind: 'success', title: `${name} upgraded to ${job.to}`, message: job.message })
    else if (job.status === 'rolled_back') toast.show({ kind: 'error', title: `${name} upgrade rolled back`, message: job.message })
    else toast.show({ kind: 'error', title: `${name} upgrade failed`, message: job.message })
  }, [job, qc, toast, infos])
}

export { currentVersion }
