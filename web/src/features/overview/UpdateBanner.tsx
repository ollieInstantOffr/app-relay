// Overview banner when Relay, nginx or HAProxy has an update, styled like the
// pending-changes bar. "Later" hides it until a newer version shows up.
import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { Button } from '../../components/ui'
import { pluralize } from '../../lib/format'
import { useRole } from '../../lib/queries'
import { currentVersion, engineTitle, shortSha, useEngineUpdates, useRelayUpdateJob, useUpgradeJob, type EngineName } from '../settings/enginesApi'

const DISMISS_KEY = 'relay.overview.updateDismissed'

function readDismissed(): string {
  try {
    return localStorage.getItem(DISMISS_KEY) ?? ''
  } catch {
    return ''
  }
}

export default function UpdateBanner() {
  const { isAdmin } = useRole()
  const updates = useEngineUpdates().data
  const relayJob = useRelayUpdateJob().data
  const engineJob = useUpgradeJob().data
  const navigate = useNavigate()
  const [dismissed, setDismissed] = useState(readDismissed)

  if (!updates) return null

  const parts: string[] = []
  const keys: string[] = []
  const relay = updates.relay
  if (relay?.updateAvailable) {
    const to = relay.remoteVersion || shortSha(relay.remoteHead)
    const commits = relay.behind > 0 ? ` (${pluralize(relay.behind, 'new commit')})` : ''
    parts.push(`Relay ${relay.version} → ${to}${commits}`)
    keys.push(`relay:${relay.remoteHead || to}`)
  }
  for (const name of ['nginx', 'haproxy'] as EngineName[]) {
    const info = updates[name]
    if (!info.inactive && info.updateAvailable && info.latest) {
      parts.push(`${engineTitle[name]} ${currentVersion(info) || '?'} → ${info.latest.version}`)
      keys.push(`${name}:${info.latest.version}`)
    }
  }

  const running = relayJob?.status === 'running' || engineJob?.status === 'running'
  const signature = keys.join('|')
  if (!running && (parts.length === 0 || dismissed === signature)) return null

  const later = () => {
    try {
      localStorage.setItem(DISMISS_KEY, signature)
    } catch {
      /* storage unavailable */
    }
    setDismissed(signature)
  }

  const title = running ? 'Upgrade in progress' : parts.length === 1 ? 'Update available' : `${parts.length} updates available`
  const summary = running ? 'Relay keeps serving traffic; follow the progress in Settings → Updates.' : parts.join(' · ')

  return (
    <div className="pending-bar update-bar" role="region" aria-label="Updates">
      <span className="pb-dot" />
      <span className="semibold nowrap">{title}</span>
      <span className="pb-summary truncate" title={summary}>{summary}</span>
      <div className="spacer" />
      {!running && (
        <Button size="sm" variant="on-dark" onClick={later}>
          Later
        </Button>
      )}
      <Button size="sm" variant="light" onClick={() => navigate('/settings/engines')}>
        {running ? 'View progress' : isAdmin ? 'Upgrade' : 'View'}
      </Button>
    </div>
  )
}
