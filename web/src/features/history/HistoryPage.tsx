// Config history (design 17): versions list, per-file diff, rollback and the
// apply pipeline strip. ?pending=1 shows the draft (rendered current config vs live).
import { useEffect, useMemo, useRef, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { TopBar } from '../../components/shell/TopBar'
import {
  AIChip, Avatar, Badge, Button, Callout, Card, CodeBlock, ConfirmDialog, DiffView, EmptyState, Icon, Skeleton, Tabs, cx,
} from '../../components/ui'
import { errorMessage } from '../../lib/api'
import { useBusEvent } from '../../lib/events'
import { useLBEngine, usePending, useProxyEngine, useRole } from '../../lib/queries'
import { lbCheckName } from '../../lib/types'
import { ago, ms, pluralize } from '../../lib/format'
import {
  engineLabel, useApplyRunner, usePendingDiff, useVersionDiff, useVersions,
  type ApplyFinished, type ApplyProgress, type DiffResponse, type PendingInfo, type VersionInfo,
} from './api'
import DiscardDialog from './DiscardDialog'
import './history.css'

type Selection = { kind: 'draft' } | { kind: 'version'; id: number }

export default function HistoryPage() {
  const [params, setParams] = useSearchParams()
  const versionsQ = useVersions()
  const versions = useMemo(() => versionsQ.data ?? [], [versionsQ.data])
  const pending = usePending().data as PendingInfo | undefined
  const { canWrite } = useRole()
  const live = versions.find((v) => v.status === 'live')
  const pendingCount = pending?.count ?? 0

  const selection: Selection | undefined = useMemo(() => {
    if (params.get('pending') === '1') return { kind: 'draft' }
    const id = Number(params.get('version'))
    if (id && versions.some((v) => v.id === id)) return { kind: 'version', id }
    if (live) return { kind: 'version', id: live.id }
    if (versions[0]) return { kind: 'version', id: versions[0].id }
    if (pendingCount > 0) return { kind: 'draft' }
    return undefined
  }, [params, versions, live, pendingCount])

  const againstParam = params.get('against') ? Number(params.get('against')) : undefined

  const select = (s: Selection) => {
    const next = new URLSearchParams()
    if (s.kind === 'draft') next.set('pending', '1')
    else next.set('version', String(s.id))
    setParams(next, { replace: true })
  }

  return (
    <>
      <TopBar
        title={
          <span className="row gap-10">
            Config history
            {live && <Badge tone="ok">v{live.id} live</Badge>}
            <span className="hist-sub">Every apply = a version. Rollback re-validates before swapping.</span>
          </span>
        }
        search
      />
      <div className="page hist-page">
        <div className="hist-grid">
          <Card className="hist-list" title="Versions" sub={versions.length ? pluralize(versions.length, 'version') : undefined}>
            <div className="hist-list-body">
              {versionsQ.isLoading ? (
                <div className="col gap-8" style={{ padding: 8 }}>
                  <Skeleton height={54} />
                  <Skeleton height={54} />
                  <Skeleton height={54} />
                </div>
              ) : (
                <>
                  {pendingCount > 0 && (
                    <button
                      type="button"
                      className={cx('hist-row draft', selection?.kind === 'draft' && 'selected')}
                      onClick={() => select({ kind: 'draft' })}
                    >
                      <div className="row gap-8">
                        <span className="hist-ver">Draft</span>
                        <Badge tone="pending">pending</Badge>
                        <span className="spacer" />
                        <span className="hist-meta">{pluralize(pendingCount, 'change')}</span>
                      </div>
                      {pending?.summary && <div className="hist-summary">{pending.summary}</div>}
                    </button>
                  )}
                  {versions.map((v, i) => (
                    <VersionRow
                      key={v.id}
                      v={v}
                      older={versions[i + 1]}
                      selected={selection?.kind === 'version' && selection.id === v.id}
                      onSelect={() => select({ kind: 'version', id: v.id })}
                    />
                  ))}
                  {versions.length === 0 && pendingCount === 0 && (
                    <EmptyState icon="history" title="No versions yet" description="Every apply creates a version with a full snapshot you can diff and roll back to." />
                  )}
                </>
              )}
            </div>
          </Card>
          <Card className="hist-diff">
            {selection ? (
              selection.kind === 'draft' ? (
                <DraftDiff pending={pending} liveId={live?.id} canWrite={canWrite} />
              ) : (
                <VersionDiff
                  key={`${selection.id}-${againstParam ?? ''}`}
                  version={versions.find((v) => v.id === selection.id)!}
                  versions={versions}
                  against={againstParam}
                  canWrite={canWrite}
                  pendingCount={pendingCount}
                />
              )
            ) : (
              <EmptyState icon="history" title="Nothing applied yet" description="Create a host, then Apply & reload to create the first version." />
            )}
          </Card>
        </div>
        <PipelineStrip />
      </div>
    </>
  )
}

// ---------------------------------------------------------------- list rows

function ActorLabel({ actor }: { actor: string }) {
  if (actor.startsWith('mcp:')) return <span className="hist-actor"><AIChip />{actor}</span>
  if (actor === 'system' || actor === 'docker') return <span className="hist-actor"><AIChip system />{actor}</span>
  if (actor.startsWith('token:')) return <span className="hist-actor"><Icon name="token" size={12} />{actor}</span>
  return <span className="hist-actor"><Avatar name={actor} />{actor}</span>
}

function StatusBadge({ v }: { v: VersionInfo }) {
  switch (v.status) {
    case 'live':
      return <Badge tone="ok">live</Badge>
    case 'rolled_back':
      return <Badge tone="warn">auto rolled back</Badge>
    case 'failed':
      return <Badge tone="danger">failed</Badge>
    case 'draft':
      return <Badge tone="pending">applying</Badge>
  }
  return null
}

/** Proxy engine badge: every Relay Edge version, plus the version that switched engines. */
function EngineBadge({ v, older }: { v: VersionInfo; older?: VersionInfo }) {
  const engine = v.proxyEngine ?? 'nginx'
  const switched = !!older && (older.proxyEngine ?? 'nginx') !== engine
  if (engine !== 'edge' && !switched) return null
  return (
    <Badge tone={switched ? 'info' : undefined} title={switched ? `Proxy engine: ${engineLabel[older?.proxyEngine ?? 'nginx']} → ${engineLabel[engine]}` : 'Rendered for Relay Edge'}>
      {switched ? `→ ${engineLabel[engine]}` : engineLabel[engine]}
    </Badge>
  )
}

/** Load balancer engine badge: every Relay Balancer version, plus the version that switched engines. */
function LBEngineBadge({ v, older }: { v: VersionInfo; older?: VersionInfo }) {
  const engine = v.lbEngine ?? 'haproxy'
  const switched = !!older && (older.lbEngine ?? 'haproxy') !== engine
  if (engine !== 'balancer' && !switched) return null
  return (
    <Badge tone={switched ? 'info' : undefined} title={switched ? `Load balancer engine: ${engineLabel[older?.lbEngine ?? 'haproxy']} → ${engineLabel[engine]}` : 'Rendered for Relay Balancer'}>
      {switched ? `→ ${engineLabel[engine]}` : engineLabel[engine]}
    </Badge>
  )
}

function VersionRow({ v, older, selected, onSelect }: { v: VersionInfo; older?: VersionInfo; selected: boolean; onSelect: () => void }) {
  return (
    <button type="button" className={cx('hist-row', selected && 'selected')} onClick={onSelect}>
      <div className="row gap-8">
        <span className="hist-ver">v{v.id}</span>
        <StatusBadge v={v} />
        <EngineBadge v={v} older={older} />
        <LBEngineBadge v={v} older={older} />
        <span className="spacer" />
        <span className="hist-meta">
          {ago(v.createdAt)} · <ActorLabel actor={v.actor} />
        </span>
      </div>
      <div className="hist-summary">{v.summary}</div>
      {v.error && (v.status === 'rolled_back' || v.status === 'failed') && (
        <div className={cx('hist-error', v.status === 'failed' && 'danger')}>{v.error}</div>
      )}
    </button>
  )
}

// ---------------------------------------------------------------- diff panels

function fileLabel(path: string) {
  if (path === 'haproxy.cfg' || path === 'nginx.conf' || path === 'balancer.json') return path
  if (path === 'balancer/balancer.json') return 'balancer.json'
  // Relay Edge files: edge/edge.json, edge/htpasswd/<access list id>
  if (path === 'edge/edge.json') return 'edge.json'
  if (path.startsWith('edge/htpasswd/')) return `htpasswd/${path.slice('edge/htpasswd/'.length)}`
  const base = path.split('/').pop() ?? path
  return base.replace(/\.conf$/, '')
}

function DiffBody({ data, isLoading, error, emptyText }: { data?: DiffResponse; isLoading: boolean; error: unknown; emptyText: string }) {
  const [file, setFile] = useState<string | undefined>(undefined)
  const files = data?.files ?? []
  const current = files.find((f) => f.path === file) ?? files[0]
  const bodyRef = useRef<HTMLDivElement>(null)
  useEffect(() => {
    bodyRef.current?.scrollTo({ top: 0 })
  }, [current?.path])

  if (isLoading) {
    return (
      <div className="hist-diff-body">
        <Skeleton height={18} width="40%" />
        <Skeleton height={260} />
      </div>
    )
  }
  if (error) {
    return <div className="hist-diff-body"><Callout tone="danger" title="Couldn't load the diff">{errorMessage(error)}</Callout></div>
  }
  if (data?.error) {
    return <div className="hist-diff-body"><Callout tone="danger" title="The current configuration can't be rendered">{data.error}</Callout></div>
  }
  if (!current) {
    return <div className="hist-diff-body"><EmptyState icon="check" title="No config differences" description={emptyText} /></div>
  }
  return (
    <>
      {files.length > 1 && (
        <div className="hist-files">
          <Tabs
            flush
            value={current.path}
            onChange={setFile}
            tabs={files.map((f) => ({
              id: f.path,
              label: (
                <span className="row gap-6" title={f.path}>
                  <span className="mono">{fileLabel(f.path)}</span>
                  <span className="hist-delta">
                    {f.added > 0 && <span className="add">+{f.added}</span>} {f.removed > 0 && <span className="del">−{f.removed}</span>}
                  </span>
                </span>
              ),
            }))}
          />
        </div>
      )}
      <div className="hist-diff-meta">
        <span>{current.path}</span>
        <span>·</span>
        <span className="add">+{current.added}</span>
        <span className="del">−{current.removed}</span>
        {current.status !== 'modified' && <><span>·</span><span>{current.status === 'added' ? 'new file' : 'file removed'}</span></>}
        {data && data.validateMs > 0 && <><span>·</span><span>validated in {ms(data.validateMs)}</span></>}
        {data && data.reloadMs > 0 && <><span>·</span><span>reloaded in {ms(data.reloadMs)}</span></>}
      </div>
      <div className="hist-diff-body" ref={bodyRef}>
        <DiffView lines={current.lines} />
      </div>
    </>
  )
}

function VersionDiff({ version, versions, against, canWrite, pendingCount }: {
  version: VersionInfo
  versions: VersionInfo[]
  against?: number
  canWrite: boolean
  pendingCount: number
}) {
  const diff = useVersionDiff(version.id, against)
  const { run, busy } = useApplyRunner()
  const [confirm, setConfirm] = useState(false)
  const againstId = diff.data?.against ?? against
  const againstVersion = versions.find((v) => v.id === againstId)

  // "Roll back to" the version before the live one, or restore the selected one.
  const target =
    version.status === 'live'
      ? againstVersion && (againstVersion.status === 'superseded' || againstVersion.status === 'live') ? againstVersion : undefined
      : version.status === 'superseded' ? version : undefined

  const title = againstId ? `v${version.id} vs v${againstId}` : `v${version.id} · first version`

  return (
    <>
      <div className="hist-diff-head">
        <span className="hist-diff-title">{title}</span>
        <StatusBadge v={version} />
        {version.proxyEngine && <Badge title="Proxy engine this version was rendered for">{engineLabel[version.proxyEngine]}</Badge>}
        {version.lbEngine && <Badge title="Load balancer engine this version was rendered for">{engineLabel[version.lbEngine]}</Badge>}
        <span className="faint small">{version.summary}</span>
        <div className="spacer" />
        <a className="btn btn-sm" href={`/api/versions/${version.id}/download`} download>
          <Icon name="download" size={14} />
          Download
        </a>
        {canWrite && target && (
          <Button size="sm" variant="primary" icon="rollback" loading={busy} onClick={() => setConfirm(true)}>
            Roll back to v{target.id}
          </Button>
        )}
      </div>
      {version.error && (version.status === 'rolled_back' || version.status === 'failed') && (
        <div style={{ padding: '12px 18px 0' }}>
          <Callout tone={version.status === 'failed' ? 'danger' : 'warn'} title={version.status === 'failed' ? 'This apply failed' : 'Rolled back automatically'}>
            {version.error}
            {version.output && version.failedStage !== 'health' && <CodeBlock code={version.output} wrap maxHeight={140} className="mt-8" />}
          </Callout>
        </div>
      )}
      <DiffBody data={diff.data} isLoading={diff.isLoading} error={diff.error} emptyText="The rendered proxy and load balancer configuration is identical to the compared version." />
      {target && (
        <ConfirmDialog
          open={confirm}
          onClose={() => setConfirm(false)}
          title={`Roll back to v${target.id}?`}
          message={`Relay restores hosts, redirects, streams, access lists, the load balancer and settings from v${target.id}, then validates, reloads and health-checks it as a new version. Certificates are not changed.`}
          confirmLabel={`Roll back to v${target.id}`}
          onConfirm={async () => {
            await run(`/api/versions/${target.id}/rollback`)
          }}
        >
          {pendingCount > 0 && (
            <Callout tone="warn">Your {pluralize(pendingCount, 'pending change')} will be replaced by v{target.id}.</Callout>
          )}
        </ConfirmDialog>
      )}
    </>
  )
}

function DraftDiff({ pending, liveId, canWrite }: { pending?: PendingInfo; liveId?: number; canWrite: boolean }) {
  const count = pending?.count ?? 0
  const diff = usePendingDiff(true)
  const [discardOpen, setDiscardOpen] = useState(false)
  return (
    <>
      <div className="hist-diff-head">
        <span className="hist-diff-title">{liveId ? `Draft vs v${liveId}` : 'Draft · nothing applied yet'}</span>
        <Badge tone="pending">{pluralize(count, 'pending change')}</Badge>
        <div className="spacer" />
        {canWrite && count > 0 && liveId && (
          <Button size="sm" onClick={() => setDiscardOpen(true)}>
            Discard
          </Button>
        )}
        {canWrite && count > 0 && (
          <Button size="sm" variant="primary" onClick={() => window.dispatchEvent(new CustomEvent('relay:apply'))}>
            Apply &amp; reload
          </Button>
        )}
      </div>
      {pending && pending.items.length > 0 && (
        <div className="hist-diff-meta" style={{ fontFamily: 'var(--font-sans)' }}>
          {pending.items.map((it) => (
            <Badge key={`${it.kind}-${it.id}`} tone={it.action === 'deleted' ? 'danger' : it.action === 'created' ? 'ok' : undefined}>
              {it.name} · {it.action}
            </Badge>
          ))}
        </div>
      )}
      <DiffBody
        data={diff.data}
        isLoading={diff.isLoading}
        error={diff.error}
        emptyText={count > 0 ? 'These changes don’t alter the rendered configuration.' : 'There are no pending changes.'}
      />
      {liveId && <DiscardDialog open={discardOpen} onClose={() => setDiscardOpen(false)} count={count} liveVersion={liveId} />}
    </>
  )
}

// ---------------------------------------------------------------- pipeline strip

const STEPS: { id: string; label: string; sub?: string }[] = [
  { id: 'render', label: 'Render' },
  { id: 'validate', label: 'Validate' },
  { id: 'swap', label: 'Atomic swap' },
  { id: 'reload', label: 'Reload' },
  { id: 'health', label: 'Health check', sub: '10 s' },
]

function PipelineStrip() {
  const proxy = useProxyEngine().engine
  const lb = useLBEngine().engine
  const validateSub = `${proxy === 'edge' ? 'relay edge check' : 'nginx -t'} · ${lbCheckName[lb]}`
  const [active, setActive] = useState<string | null>(null)
  const [result, setResult] = useState<'ok' | 'failed' | null>(null)
  const timer = useRef<number | undefined>(undefined)

  useBusEvent<ApplyProgress>('apply.progress', (ev) => {
    window.clearTimeout(timer.current)
    setResult(null)
    setActive(ev.data?.stage ?? null)
  })
  useBusEvent<ApplyFinished>('apply.finished', (ev) => {
    setResult(ev.data?.status === 'live' ? 'ok' : 'failed')
    setActive(ev.data?.status === 'live' ? 'done' : ev.data?.stage ?? 'validate')
    window.clearTimeout(timer.current)
    timer.current = window.setTimeout(() => {
      setActive(null)
      setResult(null)
    }, 5000)
  })
  useEffect(() => () => window.clearTimeout(timer.current), [])

  const order = ['render', 'validate', 'swap', 'reload', 'health']
  // The agent swaps and reloads in one step.
  const idx = active === 'done' ? order.length : active === 'reload' ? 3 : active ? order.indexOf(active) : -1

  return (
    <Card className="hist-pipeline">
      <span className="section-title">Apply pipeline</span>
      {STEPS.map((s, i) => {
        const state =
          idx < 0 ? undefined
          : result === 'failed' && i === idx ? 'failed'
          : i < idx || (i === 2 && idx === 3) ? 'done'
          : i === idx ? (result === 'ok' ? 'done' : 'active')
          : undefined
        return (
          <span key={s.id} className="row gap-8">
            {i > 0 && <span className="hist-arrow">→</span>}
            <span className={cx('hist-step', state)}>
              {state === 'done' && <Icon name="check" size={12} />}
              {s.label}
              {(s.id === 'validate' ? validateSub : s.sub) && <span className="sub">{s.id === 'validate' ? validateSub : s.sub}</span>}
            </span>
          </span>
        )
      })}
      <span className="spacer" />
      <span className="faint small">fails anywhere → previous version restored</span>
    </Card>
  )
}
