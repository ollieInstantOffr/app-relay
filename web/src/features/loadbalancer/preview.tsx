import { CopyButton, Dot, Spinner } from '../../components/ui'
import { pluralize } from '../../lib/format'
import type { PreviewResult } from './lbApi'

export interface PreviewState { data?: PreviewResult; loading: boolean; error?: string }

/** "Generated haproxy.cfg (this backend)" block with Copy. */
export function PreviewBlock({ title, state, readOnly }: { title: string; state: PreviewState; readOnly?: boolean }) {
  const cfg = state.data?.config ?? ''
  return (
    <div className="lb-preview">
      <div className="lb-preview-head">
        <div className="lb-preview-title">{title}</div>
        {state.loading && <Spinner />}
        <span className="spacer" />
        {cfg && <CopyButton text={cfg} />}
      </div>
      <pre>{cfg || (readOnly ? '# preview is available to editors' : state.error ? `# preview unavailable: ${state.error}` : '# rendering…')}</pre>
    </div>
  )
}

/** Footer validation status: "haproxy -c passed". */
export function PreviewStatus({ state, readOnly }: { state: PreviewState; readOnly?: boolean }) {
  if (readOnly) return <span className="lb-validate">Read-only · viewers can't change the load balancer</span>
  if (state.loading && !state.data) {
    return <span className="lb-validate"><Spinner /> Validating…</span>
  }
  if (state.error) {
    return <span className="lb-validate bad"><Dot tone="danger" />{state.error}</span>
  }
  const d = state.data
  if (!d) return null
  const nFields = d.fields ? Object.keys(d.fields).length : 0
  if (nFields) {
    return (
      <span className="lb-validate bad" title={d.output}>
        <Dot tone="warn" />
        {pluralize(nFields, 'field needs', 'fields need')} attention
      </span>
    )
  }
  if (d.checked === 'haproxy') {
    return d.valid ? (
      <span className="lb-validate"><Dot tone="ok" />haproxy -c passed</span>
    ) : (
      <span className="lb-validate bad" title={d.output}><Dot tone="danger" />haproxy -c failed</span>
    )
  }
  if (!d.valid) {
    return <span className="lb-validate bad" title={d.output}><Dot tone="danger" />{d.output.split('\n')[0]}</span>
  }
  return <span className="lb-validate" title={d.output}><Dot tone="muted" />Checked by Relay · HAProxy agent offline</span>
}
