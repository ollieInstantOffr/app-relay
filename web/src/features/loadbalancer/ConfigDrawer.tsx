import { useState } from 'react'
import { Button, Callout, CodeBlock, CopyButton, Dot, Drawer, Segmented, Skeleton } from '../../components/ui'
import { api, errorMessage } from '../../lib/api'
import { useLBEngine, useRole } from '../../lib/queries'
import { lbConfigFile, lbEngineLabel } from '../../lib/types'
import { checkedByEngine, useLBConfig, validationSummary, type ValidationResult } from './lbApi'

/** Full config viewer for the active load balancer engine: haproxy.cfg or balancer.json (live version, or rendered from pending config). */
export default function ConfigDrawer({ open, onClose }: { open: boolean; onClose: () => void }) {
  const q = useLBConfig(open)
  const live = useLBEngine()
  const { canWrite } = useRole()
  const [view, setView] = useState<'live' | 'pending'>('live')
  const [validation, setValidation] = useState<ValidationResult | null>(null)
  const [validating, setValidating] = useState(false)
  const [vErr, setVErr] = useState('')
  const d = q.data
  const engine = d?.engine ?? live.engine
  const json = engine === 'balancer'
  const showPending = view === 'pending' || !d?.live
  const text = (showPending ? d?.rendered : d?.live) ?? ''
  const lines = text ? text.split('\n').length - 1 : 0

  const subtitle = !d
    ? 'Loading…'
    : !d.live
      ? `${lbEngineLabel[engine]} · rendered from the current config · ${lines} lines · not applied yet`
      : showPending
        ? `${lbEngineLabel[engine]} · rendered with pending changes · ${lines} lines`
        : `${lbEngineLabel[engine]} · live · v${d.liveVersion} · ${lines} lines${d.pending ? ' · pending changes differ' : ''}`

  const validate = async () => {
    setValidating(true)
    setVErr('')
    try {
      setValidation(await api.post<ValidationResult>('/api/lb/validate'))
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
      title={lbConfigFile[engine]}
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
              <Dot tone={validation.valid ? (checkedByEngine(validation.checked) ? 'ok' : 'muted') : 'danger'} />
              {validationSummary(validation, engine)}
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
      {d && <CodeBlock code={text || (json ? '{}' : '# no configuration')} />}
    </Drawer>
  )
}
