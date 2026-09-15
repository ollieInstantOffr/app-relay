import { useState } from 'react'
import { Button, Callout, CodeBlock, CopyButton, Dot, Drawer, Segmented, Skeleton } from '../../components/ui'
import { api, errorMessage } from '../../lib/api'
import { useRole } from '../../lib/queries'
import { useHAProxyConfig, type ValidationResult } from './lbApi'

/** Full haproxy.cfg viewer (live version, or rendered from pending config). */
export default function ConfigDrawer({ open, onClose }: { open: boolean; onClose: () => void }) {
  const q = useHAProxyConfig(open)
  const { canWrite } = useRole()
  const [view, setView] = useState<'live' | 'pending'>('live')
  const [validation, setValidation] = useState<ValidationResult | null>(null)
  const [validating, setValidating] = useState(false)
  const [vErr, setVErr] = useState('')
  const d = q.data
  const showPending = view === 'pending' || !d?.live
  const text = (showPending ? d?.rendered : d?.live) ?? ''
  const lines = text ? text.split('\n').length - 1 : 0

  const subtitle = !d
    ? 'Loading…'
    : !d.live
      ? `rendered from the current config · ${lines} lines · not applied yet`
      : showPending
        ? `rendered with pending changes · ${lines} lines`
        : `live · v${d.liveVersion} · ${lines} lines${d.pending ? ' · pending changes differ' : ''}`

  const validate = async () => {
    setValidating(true)
    setVErr('')
    try {
      setValidation(await api.post<ValidationResult>('/api/haproxy/validate'))
    } catch (err) {
      setVErr(errorMessage(err))
    } finally {
      setValidating(false)
    }
  }

  return (
    <Drawer
      open={open}
      onClose={onClose}
      width="xwide"
      title="haproxy.cfg"
      subtitle={subtitle}
      headerExtra={
        d?.live && d.pending ? (
          <Segmented value={view} onChange={setView} options={[{ value: 'live', label: 'Live' }, { value: 'pending', label: 'With pending changes' }]} />
        ) : undefined
      }
      footer={
        <>
          {validation && (
            <span className={validation.valid ? 'lb-validate' : 'lb-validate bad'}>
              <Dot tone={validation.valid ? (validation.checked === 'haproxy' ? 'ok' : 'muted') : 'danger'} />
              {validation.valid
                ? validation.checked === 'haproxy'
                  ? `haproxy -c passed · ${validation.durationMs} ms`
                  : 'Checked by Relay · HAProxy agent offline'
                : 'haproxy -c failed'}
            </span>
          )}
          <span className="spacer" />
          {text && <CopyButton text={text} size="default" />}
          {canWrite && <Button loading={validating} onClick={validate}>Validate</Button>}
          <Button variant="primary" onClick={onClose}>Close</Button>
        </>
      }
    >
      {q.isLoading && <Skeleton height={400} />}
      {q.error && <Callout tone="danger">{errorMessage(q.error)}</Callout>}
      {d?.renderError && <Callout tone="danger" title="The current config can't be rendered">{d.renderError}</Callout>}
      {vErr && <Callout tone="danger">{vErr}</Callout>}
      {validation && !validation.valid && validation.output && <CodeBlock code={validation.output} wrap />}
      {d && <CodeBlock code={text || '# no configuration'} />}
    </Drawer>
  )
}
