// Sandboxed live preview of an error / maintenance page.
import { Callout, Skeleton, Spinner } from '../../components/ui'
import type { ErrorPagePreviewState } from './errorPagesApi'
import './errorPages.css'

export function ErrorPagePreviewFrame({ state, label, height = 420 }: { state: ErrorPagePreviewState; label: string; height?: number }) {
  return (
    <div className="ep-preview">
      <div className="ep-preview-bar">
        <span className="ep-preview-dots" aria-hidden>
          <i />
          <i />
          <i />
        </span>
        <span className="ep-preview-label truncate">{label}</span>
        {state.loading && <Spinner />}
      </div>
      {state.error ? (
        <div style={{ padding: 14 }}>
          <Callout tone="warn" title="Preview unavailable">{state.error}</Callout>
        </div>
      ) : state.html !== undefined ? (
        <iframe className="ep-preview-frame" title={`Preview: ${label}`} sandbox="" srcDoc={state.html} style={{ height }} />
      ) : (
        <div style={{ padding: 14 }}>
          <Skeleton height={height - 28} />
        </div>
      )}
    </div>
  )
}
