// Owner: slice hosts. Generated nginx config preview + validation status.
import { useState } from 'react'
import { Dot, Icon, Skeleton, Spinner } from '../../components/ui'
import { extractLocationBlocks, type PreviewState } from './lib'

export function PreviewStatus({ state }: { state: PreviewState }) {
  if (state.status === 'loading') {
    return (
      <span className="row gap-6 small muted">
        <Spinner />
        Validating config…
      </span>
    )
  }
  if (state.status === 'ready' && state.preview) {
    return state.preview.valid ? (
      <span className="row gap-6 small muted"><Dot tone="ok" />Config valid</span>
    ) : (
      <span className="row gap-6 small danger-text"><Dot tone="danger" />Config invalid</span>
    )
  }
  if (state.status === 'unavailable') {
    return <span className="row gap-6 small faint"><Dot tone="muted" />Preview unavailable</span>
  }
  return null
}

export function ConfigPreviewPanel({ state, collapsible, extract, title = 'Generated config preview' }: {
  state: PreviewState
  collapsible?: boolean
  extract?: 'location'
  title?: string
}) {
  const [open, setOpen] = useState(true)
  const config = state.preview?.config ?? ''
  const shown = extract === 'location' ? extractLocationBlocks(config) || config : config
  return (
    <div className="hosts-preview">
      <div className="row between">
        {collapsible ? (
          <button type="button" className="hosts-preview-title" onClick={() => setOpen(!open)} aria-expanded={open}>
            <Icon name="chevron" size={12} style={{ transform: open ? undefined : 'rotate(-90deg)', transition: 'transform .12s' }} />
            {title}
          </button>
        ) : (
          <div className="hosts-preview-title">{title}</div>
        )}
        {state.status !== 'idle' && <PreviewStatus state={state} />}
      </div>
      {open && (
        <>
          {state.status === 'idle' && state.hint && <div className="small faint">{state.hint}</div>}
          {state.status === 'unavailable' && (
            <div className="small faint">The config preview isn't available right now{state.error ? ` · ${state.error}` : ''}. The config is still validated when you apply.</div>
          )}
          {state.status === 'loading' && !state.preview && (
            <div className="col gap-6">
              <Skeleton height={12} width="60%" />
              <Skeleton height={12} width="80%" />
              <Skeleton height={12} width="45%" />
            </div>
          )}
          {state.preview && (
            <>
              {shown && <pre style={state.status === 'loading' ? { opacity: 0.6 } : undefined}>{shown}</pre>}
              {!state.preview.valid && state.preview.output && <pre className="error">{state.preview.output}</pre>}
            </>
          )}
        </>
      )}
    </div>
  )
}
