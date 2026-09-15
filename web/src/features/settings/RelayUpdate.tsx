// Relay self-update (Settings → Updates): what's new on the update branch,
// confirmation, live progress while the stack rebuilds and restarts.
import { useEffect, useRef, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Badge, Button, Callout, Card, Checkbox, Dialog, Dot, Icon, Meter, Spinner, useToast } from '../../components/ui'
import { api } from '../../lib/api'
import { ago } from '../../lib/format'
import type { RelayUpdateInfo, RelayUpdateJob, UpgradeStep } from '../../lib/types'
import { relayJobKey, shortSha, updatesKey } from './enginesApi'

const VISIBLE_STEPS = [
  { id: 'pull', label: 'Pull from GitHub' },
  { id: 'build', label: 'Build image' },
  { id: 'restart', label: 'Restart Relay' },
  { id: 'verify', label: 'Verify' },
]

function repoUrl(remote: string): string {
  const m = remote.match(/github\.com[/:]([^/]+)\/([^/]+?)(?:\.git)?\/?$/)
  return m ? `https://github.com/${m[1]}/${m[2]}` : ''
}

export function RelayCard({ info, isAdmin, busy, onUpdate }: { info: RelayUpdateInfo; isAdmin: boolean; busy: boolean; onUpdate: () => void }) {
  const repo = repoUrl(info.remote)
  const checked = !!info.checkedAt
  const behind = info.behind
  const shown = info.commits.slice(0, 8)
  const compare = repo && behind > 0 && info.commit && info.remoteHead ? `${repo}/compare/${info.commit}...${info.remoteHead}` : ''

  return (
    <Card
      className="eng-card"
      title={
        <span className="row gap-8">
          <Dot tone="ok" />
          <span className="eng-name">Relay</span>
        </span>
      }
      sub={info.container ? <span className="mono">{info.container}</span> : undefined}
      actions={
        info.updateAvailable ? (
          <Badge tone="info">Upgrade available</Badge>
        ) : checked && !info.checkError && !info.blocker ? (
          <Badge tone="ok">Up to date</Badge>
        ) : undefined
      }
    >
      <div className="eng-grid">
        <div className="eng-mini">
          <div className="k">Running</div>
          <div className="v">{info.version}</div>
          <div className="sub">{info.commit ? `commit ${shortSha(info.commit)}` : 'build commit unknown'}</div>
        </div>
        <div className="eng-mini">
          <div className="k">Latest on {info.branch}</div>
          <div className="v">
            {info.remoteVersion || shortSha(info.remoteHead) || '—'}
            {info.updateAvailable && <span className="upd-badge-dot" />}
          </div>
          <div className="sub">
            {info.remoteVersion && info.remoteHead && `commit ${shortSha(info.remoteHead)} · `}
            {!checked ? 'not checked yet' : behind > 0 ? `${behind} new commit${behind === 1 ? '' : 's'}` : info.checkError ? 'check failed' : 'no new commits'}
            {info.checkedAt && ` · checked ${ago(info.checkedAt)}`}
          </div>
        </div>
      </div>

      {info.workingDir && (
        <div className="eng-row">
          <span className="label">Source</span>
          <span className="mono small truncate" title={info.workingDir}>{info.workingDir}</span>
          {info.remote && <span className="small faint truncate" title={info.remote}>{info.remote}</span>}
        </div>
      )}

      {shown.length > 0 && (
        <div className="relay-commits">
          {shown.map((c) => (
            <div key={c.sha} className="relay-commit">
              {c.url ? (
                <a className="mono" href={c.url} target="_blank" rel="noreferrer">{c.short}</a>
              ) : (
                <span className="mono">{c.short}</span>
              )}
              <span className="subject truncate" title={c.subject}>{c.subject}</span>
              <span className="meta">{c.author}{c.date ? ` · ${ago(c.date)}` : ''}</span>
            </div>
          ))}
          {behind > shown.length && <div className="small faint" style={{ padding: '6px 0 0' }}>+ {behind - shown.length} more</div>}
        </div>
      )}

      {info.checkError && (
        <div className="eng-callout">
          <Callout tone="warn" title="Couldn't check for Relay updates">{info.checkError}</Callout>
        </div>
      )}
      {info.blocker && (
        <div className="eng-callout">
          <Callout tone="info">{info.blocker}</Callout>
        </div>
      )}
      {!info.blocker && info.dirty > 0 && (
        <div className="eng-callout">
          <Callout tone="warn">
            {info.dirty} tracked file{info.dirty === 1 ? ' has' : 's have'} local changes in <span className="mono">{info.workingDir}</span>. The update stops if git can’t fast-forward over them.
          </Callout>
        </div>
      )}
      {!info.blocker && info.ahead > 0 && (
        <div className="eng-callout">
          <Callout tone="warn">
            The checkout has {info.ahead} local commit{info.ahead === 1 ? '' : 's'} that {info.ahead === 1 ? "isn't" : "aren't"} on origin/{info.branch}. Push or reset them on the Docker host before updating.
          </Callout>
        </div>
      )}
      {info.rebuildNeeded && (
        <div className="eng-callout">
          <Callout tone="info">
            The checkout is at <span className="mono">{info.checkoutVersion || shortSha(info.checkoutHead)}</span> but Relay runs <span className="mono">{info.version}</span>. Upgrade to run the checked-out version.
          </Callout>
        </div>
      )}

      <div className="eng-foot">
        {compare && (
          <a href={compare} target="_blank" rel="noreferrer">
            <Icon name="external" size={12} />
            Compare on GitHub
          </a>
        )}
        <div className="spacer" />
        {isAdmin && info.canUpdate && (behind > 0 || info.rebuildNeeded) && (
          <Button variant="primary" icon="reload" disabled={busy} onClick={onUpdate}>
            {behind > 0 ? `Upgrade to ${info.remoteVersion || shortSha(info.remoteHead)}` : 'Upgrade'}
          </Button>
        )}
      </div>
    </Card>
  )
}

export function RelayUpdateDialog({ info, open, onClose }: { info: RelayUpdateInfo; open: boolean; onClose: () => void }) {
  const qc = useQueryClient()
  const toast = useToast()
  const [busy, setBusy] = useState(false)
  const [restartEngines, setRestartEngines] = useState(false)
  const target = info.behind > 0 ? info.remoteVersion || shortSha(info.remoteHead) : info.checkoutVersion || shortSha(info.checkoutHead)

  const start = async () => {
    setBusy(true)
    try {
      const job = await api.post<RelayUpdateJob>('/api/engines/relay/update', { restartEngines })
      qc.setQueryData(relayJobKey, job)
      onClose()
    } catch (err) {
      toast.error(err, "Couldn't start the Relay upgrade")
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog
      open={open}
      onClose={onClose}
      width={540}
      icon="reload"
      title={`Upgrade Relay to ${target}?`}
      description={
        <>
          <span className="mono">{info.version}</span> → <span className="mono">{target}</span> · {info.branch}
        </>
      }
      footer={
        <>
          <Button onClick={onClose} disabled={busy}>Cancel</Button>
          <Button variant="primary" icon="reload" loading={busy} onClick={start}>
            Upgrade
          </Button>
        </>
      }
    >
      <ol className="upg-list">
        <li><span className="n">1</span><span><b>Pull</b> origin/{info.branch} into <span className="mono">{info.workingDir}</span> (fast-forward only, as the folder’s owner).</span></li>
        <li><span className="n">2</span><span><b>Build</b> the new image with docker compose. Relay keeps running if the build fails.</span></li>
        <li><span className="n">3</span><span><b>Restart</b> with <span className="mono">docker compose up -d</span> and wait until Relay is healthy. If it doesn’t come up, the previous image is restored.</span></li>
      </ol>
      <div style={{ marginTop: 14 }}>
        <Checkbox
          checked={restartEngines}
          onChange={setRestartEngines}
          label="Also restart the proxy and load balancer engines so they use the new agent (traffic pauses 1–3 s)"
        />
      </div>
      <div className="small muted" style={{ marginTop: 12, lineHeight: 1.5 }}>
        A backup is created first. Proxy traffic keeps flowing while Relay builds the new version. The admin UI disconnects for a few seconds during the restart, and this page reconnects and reloads by itself.
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

export function RelayUpdateProgress({ job, onDismiss }: { job: RelayUpdateJob; onDismiss?: () => void }) {
  const running = job.status === 'running'
  const [showLog, setShowLog] = useState(false)
  const title = running
    ? `Upgrading Relay${job.toVersion || job.to ? ` to ${job.toVersion || shortSha(job.to)}` : ''}`
    : job.status === 'succeeded'
      ? job.message || 'Relay upgraded'
      : 'Relay upgrade failed'
  const steps = VISIBLE_STEPS.map((v) => ({ ...v, step: job.steps.find((s) => s.id === v.id) }))
  const rolledBack = job.steps.some((s) => s.id === 'rollback' && (s.status === 'running' || s.status === 'done' || s.status === 'failed'))
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
              <div className="d">{step?.status === 'skipped' ? 'skipped' : step?.detail || ' '}</div>
            </div>
          ))}
        </div>
        <div className="small muted" style={job.status === 'failed' ? { color: 'var(--danger-text)' } : undefined}>
          {job.status === 'succeeded' ? `Started by ${job.actor}` : job.message}
          {rolledBack && job.status === 'failed' && <div className="small" style={{ marginTop: 4 }}>The previous version was restored.</div>}
        </div>
        {job.output && (
          <>
            <Button size="sm" variant="ghost" icon="terminal" onClick={() => setShowLog((v) => !v)} style={{ alignSelf: 'flex-start' }}>
              {showLog ? 'Hide output' : running ? 'Show build output' : 'Show output'}
            </Button>
            {showLog && <pre className="upg-output">{job.output}</pre>}
          </>
        )}
      </div>
    </Card>
  )
}

/** Toasts when an update finishes; reloads the page after a successful update so the new UI loads. */
export function useRelayUpdateToasts(job: RelayUpdateJob | null | undefined) {
  const toast = useToast()
  const qc = useQueryClient()
  const seen = useRef<Record<string, string>>({})
  useEffect(() => {
    if (!job) return
    const prev = seen.current[job.id]
    seen.current[job.id] = job.status
    if (prev !== 'running' || job.status === 'running') return
    qc.invalidateQueries({ queryKey: updatesKey })
    if (job.status === 'succeeded') {
      toast.show({ kind: 'success', title: job.message || 'Relay upgraded', message: 'Reloading to load the new version…' })
      window.setTimeout(() => window.location.reload(), 2500)
    } else {
      toast.show({ kind: 'error', title: 'Relay upgrade failed', message: job.message })
    }
  }, [job, qc, toast])
}
