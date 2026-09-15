// Owner: slice observe — Logs page: Access · Error · Audit · Approvals (design 07, 21).
import { useEffect, useState } from 'react'
import { Navigate, useParams, useSearchParams } from 'react-router-dom'
import { TopBar } from '../../components/shell/TopBar'
import { Button, Dot, Menu, cx } from '../../components/ui'
import { qs } from '../../lib/api'
import { usePendingApprovals } from '../../lib/queries'
import ApprovalsPanel from '../mcp/ApprovalsPanel'
import AccessTab from './AccessTab'
import AuditTab from './AuditTab'
import ErrorTab from './ErrorTab'
import { accessApiParams, auditApiParams, errorApiParams, readAccessFilters, readAuditFilters, readErrorFilters } from './filters'
import { download, isTypingTarget } from './util'
import './logs.css'

const TABS = ['access', 'error', 'audit', 'approvals'] as const
type Tab = (typeof TABS)[number]

export default function LogsPage() {
  const { tab } = useParams()
  const [sp] = useSearchParams()
  const approvals = usePendingApprovals().data
  const [live, setLive] = useState(true)
  const current = (TABS as readonly string[]).includes(tab ?? '') ? (tab as Tab) : null
  const tailing = current === 'access' || current === 'error'

  // Space pauses / resumes the live tail.
  useEffect(() => {
    if (!tailing) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== ' ' || e.metaKey || e.ctrlKey || e.altKey || isTypingTarget(e.target)) return
      if (document.querySelector('.drawer, .dialog, .palette')) return
      e.preventDefault()
      setLive((v) => !v)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [tailing])

  if (!current) return <Navigate to="/logs/access" replace />

  const pending = approvals?.length ?? 0
  const tabs = [
    { to: '/logs/access', label: 'Access' },
    { to: '/logs/error', label: 'Error' },
    { to: '/logs/audit', label: 'Audit' },
    { to: '/logs/approvals', label: 'Approvals', count: pending > 0 ? pending : undefined, hot: true },
  ]

  const exportUrl = (format: 'csv' | 'ndjson') =>
    current === 'error'
      ? `/api/logs/export${qs({ log: 'error', format, ...errorApiParams(readErrorFilters(sp)) })}`
      : `/api/logs/export${qs({ format, ...accessApiParams(readAccessFilters(sp)) })}`

  let actions: React.ReactNode
  if (tailing) {
    actions = (
      <>
        <button
          type="button"
          className={cx('live-badge', !live && 'off')}
          onClick={() => setLive((v) => !v)}
          title={live ? 'Pause live tail (space)' : 'Resume live tail (space)'}
        >
          <Dot tone={live ? 'ok' : 'muted'} pulse={live} />
          {live ? 'Live · tailing' : 'Paused'}
        </button>
        <Menu
          trigger={<Button icon="download">Export</Button>}
          items={[
            { header: `${current === 'error' ? 'Error' : 'Access'} log · current filters` },
            { label: 'CSV', icon: 'download', onSelect: () => download(exportUrl('csv')) },
            { label: 'NDJSON', icon: 'download', onSelect: () => download(exportUrl('ndjson')) },
          ]}
        />
      </>
    )
  } else if (current === 'audit') {
    actions = (
      <Button icon="download" onClick={() => download(`/api/audit/export${qs({ format: 'csv', ...auditApiParams(readAuditFilters(sp)) })}`)}>
        Export CSV
      </Button>
    )
  }

  return (
    <>
      <TopBar title="Logs" tabs={tabs} actions={actions} />
      {current === 'access' && <AccessTab live={live} />}
      {current === 'error' && <ErrorTab live={live} />}
      {current === 'audit' && <AuditTab />}
      {current === 'approvals' && (
        <div className="page">
          <ApprovalsPanel />
        </div>
      )}
    </>
  )
}
