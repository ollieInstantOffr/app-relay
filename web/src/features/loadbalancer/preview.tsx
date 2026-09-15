import { CopyButton, Dot, Spinner } from '../../components/ui'
import { pluralize } from '../../lib/format'
import { useLBEngine } from '../../lib/queries'
import { lbCheckName, lbConfigFile, lbEngineLabel, type LBEngineName } from '../../lib/types'
import { checkedByEngine, type PreviewResult } from './lbApi'

export interface PreviewState { data?: PreviewResult; loading: boolean; error?: string }

/** Engine that rendered the preview, falling back to the active load balancer engine. */
function usePreviewEngine(state: PreviewState): LBEngineName {
  const live = useLBEngine().engine
  const e = state.data?.engine
  return e === 'haproxy' || e === 'balancer' ? e : live
}

/** "Generated haproxy.cfg (this backend)" block with Copy. */
export function PreviewBlock({ what, title, state, readOnly }: { what?: 'backend' | 'frontend'; title?: string; state: PreviewState; readOnly?: boolean }) {
  const engine = usePreviewEngine(state)
  const cfg = state.data?.config ?? ''
  const c = engine === 'balancer' ? '//' : '#'
  return (
    <div className="lb-preview">
      <div className="lb-preview-head">
        <div className="lb-preview-title">{title ?? `Generated ${lbConfigFile[engine]}${what ? ` (this ${what})` : ''}`}</div>
        {state.loading && <Spinner />}
        <span className="spacer" />
        {cfg && <CopyButton text={cfg} />}
      </div>
      <pre>{cfg || (readOnly ? `${c} preview is available to editors` : state.error ? `${c} preview unavailable: ${state.error}` : `${c} rendering…`)}</pre>
    </div>
  )
}

/** Footer validation status: "haproxy -c passed" / "relay balancer check passed". */
export function PreviewStatus({ state, readOnly }: { state: PreviewState; readOnly?: boolean }) {
  const engine = usePreviewEngine(state)
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
  if (checkedByEngine(d.checked)) {
    return d.valid ? (
      <span className="lb-validate"><Dot tone="ok" />{lbCheckName[d.checked]} passed</span>
    ) : (
      <span className="lb-validate bad" title={d.output}><Dot tone="danger" />{lbCheckName[d.checked]} failed</span>
    )
  }
  if (!d.valid) {
    return <span className="lb-validate bad" title={d.output}><Dot tone="danger" />{d.output.split('\n')[0]}</span>
  }
  return <span className="lb-validate" title={d.output}><Dot tone="muted" />Checked by Relay · {lbEngineLabel[engine]} agent offline</span>
}
