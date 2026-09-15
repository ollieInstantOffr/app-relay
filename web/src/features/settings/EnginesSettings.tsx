// Settings → Updates (slice engine): Relay self-update from its git checkout,
// version check + in-UI upgrade of the official nginx / HAProxy images. Relay Edge
// is part of the relay binary and upgrades with Relay.
import { useEffect, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { Badge, Button, Callout, Card, Dot, Icon, Segmented, Select, SectionHeader, Skeleton, ToggleRow, useToast } from '../../components/ui'
import { api } from '../../lib/api'
import { useRole, useSaveSettings, useSettings } from '../../lib/queries'
import { ago, date } from '../../lib/format'
import type { EngineUpdateInfo, EngineUpdates, EnginesSettings as Settings } from '../../lib/types'
import { UpgradeDialog, UpgradeProgress, useUpgradeToasts } from './EngineUpgrade'
import { channelLabels, compareVersions, currentVersion, engineTitle, shortSha, updatesKey, useEngineUpdates, useRelayUpdateJob, useUpgradeJob, type EngineName } from './enginesApi'
import { RelayCard, RelayUpdateDialog, RelayUpdateProgress, useRelayUpdateToasts } from './RelayUpdate'
import './engines.css'

const INTERVALS = [
  { value: '6', label: 'Every 6 hours' },
  { value: '12', label: 'Every 12 hours' },
  { value: '24', label: 'Daily' },
  { value: '168', label: 'Weekly' },
]

export default function EnginesSettings() {
  const updates = useEngineUpdates()
  const job = useUpgradeJob().data
  const settings = useSettings('engines')
  const save = useSaveSettings('engines')
  const { isAdmin } = useRole()
  const toast = useToast()
  const qc = useQueryClient()
  const [checking, setChecking] = useState(false)
  const [dismissedJob, setDismissedJob] = useState<string | null>(null)
  const [confirm, setConfirm] = useState<{ info: EngineUpdateInfo; version: string } | null>(null)
  const [relayConfirm, setRelayConfirm] = useState(false)
  const [dismissedRelayJob, setDismissedRelayJob] = useState<string | null>(null)
  const relayJob = useRelayUpdateJob().data
  const u = updates.data
  useUpgradeToasts(job, u ? [u.nginx, u.haproxy] : [])
  useRelayUpdateToasts(relayJob)

  const check = async () => {
    setChecking(true)
    try {
      const next = await api.post<EngineUpdates>('/api/engines/updates/check')
      qc.setQueryData(updatesKey, next)
      if (next.checkError) toast.show({ kind: 'error', title: "Couldn't check for updates", message: next.checkError })
      else {
        const avail = [
          ...(next.relay?.updateAvailable ? [`Relay ${next.relay.remoteVersion || shortSha(next.relay.remoteHead) || 'upgrade'}`] : []),
          ...[next.nginx, next.haproxy].filter((e) => e.updateAvailable && !e.inactive).map((e) => `${engineTitle[e.engine]} ${e.latest?.version}`),
        ]
        if (next.relay?.checkError) toast.show({ kind: 'warning', title: "Couldn't check for Relay updates", message: next.relay.checkError })
        toast.success(avail.length ? `${avail.length} update${avail.length > 1 ? 's' : ''} available` : 'Everything is up to date', avail.join(' · ') || undefined)
      }
    } catch (err) {
      toast.error(err, "Couldn't check for updates")
    } finally {
      setChecking(false)
    }
  }

  const saveSettings = async (patch: Partial<Settings>) => {
    if (!settings.data) return
    try {
      await save.mutateAsync({ ...settings.data, ...patch })
      qc.invalidateQueries({ queryKey: updatesKey })
    } catch (err) {
      toast.error(err, "Couldn't save update settings")
    }
  }

  const header = (
    <SectionHeader
      title="Updates"
      description="Update Relay (including Relay Edge) from its GitHub repository, and upgrade nginx and HAProxy to new official Docker images."
      actions={
        isAdmin ? (
          <Button icon="reload" loading={checking} onClick={check}>
            Check now
          </Button>
        ) : undefined
      }
    />
  )

  if (updates.isLoading || !u) {
    return (
      <>
        {header}
        {updates.error ? <Callout tone="danger">{(updates.error as Error).message}</Callout> : <Skeleton height={260} />}
      </>
    )
  }

  const relayBusy = relayJob?.status === 'running'
  const busy = job?.status === 'running' || relayBusy
  const recent = (finishedAt?: string) => !!finishedAt && Date.now() - new Date(finishedAt).getTime() < 30 * 60_000
  const showJob = job && (job.status === 'running' || (job.id !== dismissedJob && recent(job.finishedAt)))
  const showRelayJob = relayJob && (relayBusy || (relayJob.id !== dismissedRelayJob && recent(relayJob.finishedAt)))

  return (
    <>
      {header}
      <div className="small faint row gap-6" style={{ marginTop: -8 }}>
        {u.checkedAt ? `Last checked ${ago(u.checkedAt)}` : 'Not checked yet'}
        {u.autoCheck && u.nextCheckAt && <> · next check {ago(u.nextCheckAt)}</>}
        {u.composeProject && <> · compose project <span className="mono">{u.composeProject}</span></>}
      </div>

      {u.checkError && (
        <Callout tone="warn" title="Couldn't check for updates">
          {u.checkError}. The versions below may be out of date — Relay retries every hour.
        </Callout>
      )}
      {u.dockerError && (
        <Callout tone="warn" title="Docker API unavailable">
          {u.dockerError}. Version checks still work; upgrades need the Docker socket mounted into the relay container.
        </Callout>
      )}

      {showRelayJob && relayJob && <RelayUpdateProgress job={relayJob} onDismiss={() => setDismissedRelayJob(relayJob.id)} />}
      {u.relay && <RelayCard info={u.relay} isAdmin={isAdmin} busy={busy} onUpdate={() => setRelayConfirm(true)} />}
      {u.proxyEngine === 'edge' && (
        <div className="small faint row gap-6">
          <Icon name="info" size={12} />
          Relay Edge (beta) is the proxy engine. It ships with Relay and updates with it; restart the engines when updating to run the new version right away.
        </div>
      )}

      {showJob && job && <UpgradeProgress job={job} onDismiss={() => setDismissedJob(job.id)} />}

      {(['nginx', 'haproxy'] as EngineName[]).map((e) => u[e].inactive ? (
        <InactiveEngineCard key={e} info={u[e]} running={currentVersion(u[e])} />
      ) : (
        <EngineCard
          key={e}
          info={u[e]}
          settings={settings.data}
          isAdmin={isAdmin}
          busy={busy}
          onChannel={(ch) => saveSettings(e === 'nginx' ? { nginxChannel: ch as Settings['nginxChannel'] } : { haproxyChannel: ch as Settings['haproxyChannel'] })}
          onUpgrade={(version) => setConfirm({ info: u[e], version })}
        />
      ))}

      <Card title="Update checks">
        <ToggleRow
          title="Check automatically"
          description="Check GitHub for new Relay commits and Docker Hub for new nginx and HAProxy tags · one notification per new version"
          checked={!!settings.data?.autoCheck}
          disabled={!isAdmin || !settings.data}
          onChange={(v) => saveSettings({ autoCheck: v })}
        />
        <div className="toggle-row">
          <div className="grow">
            <div className="toggle-title">Check interval</div>
            <div className="toggle-desc">Failed checks are retried hourly</div>
          </div>
          <Select
            aria-label="Check interval"
            style={{ width: 170 }}
            value={String(settings.data?.checkIntervalHours ?? 12)}
            options={INTERVALS.some((i) => i.value === String(settings.data?.checkIntervalHours)) || !settings.data ? INTERVALS : [...INTERVALS, { value: String(settings.data.checkIntervalHours), label: `Every ${settings.data.checkIntervalHours} hours` }]}
            disabled={!isAdmin || !settings.data?.autoCheck}
            onChange={(v) => saveSettings({ checkIntervalHours: Number(v) })}
          />
        </div>
      </Card>

      {confirm && <UpgradeDialog info={confirm.info} version={confirm.version} open onClose={() => setConfirm(null)} />}
      {relayConfirm && u.relay && <RelayUpdateDialog info={u.relay} open onClose={() => setRelayConfirm(false)} />}
    </>
  )
}

/** nginx while Relay Edge is the proxy engine: kept installed, not upgraded or checked for problems. */
function InactiveEngineCard({ info, running }: { info: EngineUpdateInfo; running: string }) {
  return (
    <Card
      className="eng-card inactive"
      title={
        <span className="row gap-8">
          <Dot tone="muted" />
          <span className="eng-name">{engineTitle[info.engine]}</span>
        </span>
      }
      sub={info.container ? <span className="mono">{info.container}</span> : undefined}
      actions={<Badge>Not in use</Badge>}
    >
      <div className="eng-callout" style={{ opacity: 0.75 }}>
        <div className="small muted">
          Not in use — Relay Edge (beta) is the proxy engine. Switch back to nginx in Settings → Proxy engine at any time. {running ? <>Installed: <span className="mono">{running}</span>{info.image ? <> · <span className="mono">{info.image}</span></> : null}. </> : null}
          Upgrades are available again after switching back to nginx in <Link to="/settings/proxy">Settings → Proxy engine</Link>.
        </div>
      </div>
    </Card>
  )
}

function EngineCard({ info, settings, isAdmin, busy, onChannel, onUpgrade }: {
  info: EngineUpdateInfo
  settings?: Settings
  isAdmin: boolean
  busy: boolean
  onChannel: (ch: string) => void
  onUpgrade: (version: string) => void
}) {
  const name = engineTitle[info.engine]
  const running = currentVersion(info)
  const latest = info.latest
  const tone = !info.reachable ? (info.standby ? 'muted' : 'danger') : info.running ? 'ok' : 'muted'
  const state = !info.reachable ? (info.standby ? 'stopped · container stopped' : 'agent unreachable') : info.running ? 'running' : info.engine === 'haproxy' ? 'idle · no backends' : 'stopped'
  const channel = (info.engine === 'nginx' ? settings?.nginxChannel : settings?.haproxyChannel) ?? info.channel
  const channelHint = channelLabels[info.engine].find((c) => c.value === channel)?.hint
  const [keeping, setKeeping] = useState(false)
  const [channelDraft, setChannelDraft] = useState(channel)
  useEffect(() => setChannelDraft(channel), [channel])
  const toast = useToast()
  const qc = useQueryClient()

  const keep = async () => {
    setKeeping(true)
    try {
      const next = await api.post<EngineUpdates>(`/api/engines/${info.engine}/keep-image`)
      qc.setQueryData(updatesKey, next)
      toast.success(`Keeping ${info.drift?.runningImage}`, 'Relay now treats the compose image as the desired version.')
    } catch (err) {
      toast.error(err, "Couldn't update the desired image")
    } finally {
      setKeeping(false)
    }
  }

  const drift = info.drift
  const driftNewer = drift && drift.runningVersion && drift.desiredVersion ? compareVersions(drift.runningVersion, drift.desiredVersion) > 0 : false

  return (
    <Card
      className="eng-card"
      title={
        <span className="row gap-8">
          <Dot tone={tone} />
          <span className="eng-name">{name}</span>
        </span>
      }
      sub={info.container ? <span className="mono">{info.container}</span> : undefined}
      actions={
        info.updateAvailable ? (
          <Badge tone="info">Update available</Badge>
        ) : latest && running ? (
          <Badge tone="ok">Up to date</Badge>
        ) : undefined
      }
    >
      <div className="eng-grid">
        <div className="eng-mini">
          <div className="k">Running</div>
          <div className="v">{running || '—'}<span className="small faint" style={{ fontFamily: 'var(--font-sans)' }}>{state}</span></div>
          <div className="sub" title={info.image}>{info.image || 'container not found'}</div>
        </div>
        <div className="eng-mini">
          <div className="k">Latest {channel}</div>
          <div className="v">
            {latest?.version ?? '—'}
            {info.updateAvailable && <span className="upd-badge-dot" />}
          </div>
          <div className="sub">{latest ? `${latest.tag}${latest.date ? ` · ${date(latest.date)}` : ''}` : 'not checked yet'}</div>
        </div>
      </div>

      <div className="eng-row">
        <span className="label">Channel</span>
        <Segmented
          value={channelDraft}
          disabled={!isAdmin}
          options={channelLabels[info.engine].map((c) => ({ value: c.value, label: c.label }))}
          onChange={(v) => {
            setChannelDraft(v)
            onChannel(v)
          }}
        />
        <span className="small faint">{channelHint}</span>
      </div>

      {drift && (
        <div className="eng-callout">
          <Callout
            tone="warn"
            title={`${name} runs ${drift.runningVersion || drift.runningImage}, not ${drift.desiredVersion || drift.desiredImage}`}
            actions={
              isAdmin ? (
                <div className="row gap-8">
                  {drift.desiredVersion && !driftNewer && (
                    <Button size="sm" variant="primary" disabled={busy || !info.canUpgrade} onClick={() => onUpgrade(drift.desiredVersion)}>
                      Upgrade again
                    </Button>
                  )}
                  <Button size="sm" loading={keeping} disabled={busy} onClick={keep}>
                    Keep compose version
                  </Button>
                </div>
              ) : undefined
            }
          >
            The container was recreated from docker-compose.yml (<span className="mono">{drift.runningImage}</span>) after Relay installed{' '}
            <span className="mono">{drift.desiredImage}</span>. Pin the version in <span className="mono">.env</span> so <span className="mono">docker compose up</span> keeps it.
          </Callout>
        </div>
      )}
      {info.upgradeBlocker && (
        <div className="eng-callout">
          <Callout tone="info">{info.upgradeBlocker}</Callout>
        </div>
      )}
      {info.missingModules.length > 0 && (
        <div className="eng-callout">
          <Callout tone="info">
            The official image has no <span className="mono">{info.missingModules.join(', ')}</span> module
            {info.missingModules.includes('geoip2') ? ' — country-based geo-blocking is skipped when rendering nginx.conf.' : '.'}
          </Callout>
        </div>
      )}

      <div className="eng-foot">
        <a href={info.changesUrl} target="_blank" rel="noreferrer">
          <Icon name="external" size={12} />
          {info.engine === 'nginx' ? 'nginx changelog' : `HAProxy ${(latest?.version ?? running).split('.').slice(0, 2).join('.')} changelog`}
        </a>
        <div className="spacer" />
        {isAdmin && info.updateAvailable && latest && (
          <Button variant="primary" icon="reload" disabled={busy || !info.canUpgrade} onClick={() => onUpgrade(latest.version)}>
            Upgrade to {latest.version}
          </Button>
        )}
      </div>
    </Card>
  )
}
