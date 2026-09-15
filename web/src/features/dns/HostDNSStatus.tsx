// Compact Public DNS status for a host's domains (host drawer · Details tab).
import { useState } from 'react'
import { Link } from 'react-router-dom'
import { Badge, Button, Skeleton, Spinner, cx, useToast } from '../../components/ui'
import { errorMessage } from '../../lib/api'
import { useRole } from '../../lib/queries'
import type { DNSDomainCheck } from '../../lib/types'
import { checkMeta, createDNSRecord, useDNSCheck, useInvalidateDNS, usePublicDNS } from './dnsApi'
import './dns.css'

/** Pass only syntactically valid domains. Renders nothing while Public DNS is off. */
export function HostDNSStatus({ domains, readOnly }: { domains: string[]; readOnly: boolean }) {
  const { settings, enabled } = usePublicDNS()
  const { canWrite } = useRole()
  const toast = useToast()
  const invalidate = useInvalidateDNS()
  const check = useDNSCheck(domains, enabled)
  const [creating, setCreating] = useState('')

  if (!enabled || domains.length === 0) return null

  const results = (check.data ?? []).filter((r) => domains.includes(r.domain))
  const autoCreate = !!settings?.autoCreate

  const create = async (r: DNSDomainCheck) => {
    if (!r.zone || !r.planned) return
    setCreating(r.domain)
    try {
      await createDNSRecord(r.zone, r.planned)
      toast.success('Record created', `${r.planned.type} ${r.domain} → ${r.planned.data}`)
    } catch (err) {
      toast.error(err, 'Could not create record')
    } finally {
      setCreating('')
      invalidate()
    }
  }

  return (
    <div className="field">
      <div className="row between">
        <span className="field-label row gap-6">
          Public DNS
          {check.isFetching && <Spinner />}
        </span>
        <Link to="/dns" className="small muted">Open Public DNS</Link>
      </div>
      {check.isError ? (
        <div className="field-hint">Couldn’t check DNS · {errorMessage(check.error)}</div>
      ) : results.length === 0 ? (
        <Skeleton height={40} />
      ) : (
        <div className="dns-check-list">
          {results.map((r) => {
            const m = checkMeta(r, autoCreate)
            const canCreate = r.status === 'missing' && !autoCreate && !!r.planned && !!r.zone && canWrite && !readOnly
            return (
              <div key={r.domain} className="dns-check-row">
                <Badge tone={m.tone}>{m.label}</Badge>
                <span className="dns-check-domain" title={r.domain}>{r.domain}</span>
                <span className={cx('dns-check-text', r.status === 'error' && 'danger')} title={m.text}>{m.text}</span>
                {canCreate && (
                  <Button size="sm" loading={creating === r.domain} disabled={!!creating} onClick={() => create(r)} title={`${r.planned!.type} → ${r.planned!.data}`}>
                    Create record
                  </Button>
                )}
                {r.status === 'conflict' && <Link to="/dns" className="small muted">View</Link>}
              </div>
            )
          })}
        </div>
      )}
    </div>
  )
}
