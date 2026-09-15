// Owner: slice ops. Import from Nginx Proxy Manager wizard (design 16c).
import { useRef, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { api, errorMessage } from '../../lib/api'
import { keys } from '../../lib/queries'
import { Badge, Button, Callout, Checkbox, Dialog, Field, Input, Segmented, Stepper, useToast } from '../../components/ui'
import { bytes } from '../../lib/format'
import { applyNowAction, type NpmCommitResult, type NpmKind, type NpmPreview } from './ops'
import './ops.css'

const KIND_LABEL: Record<NpmKind, string> = {
  hosts: 'Proxy hosts',
  redirects: 'Redirects',
  streams: 'Streams',
  accessLists: 'Access lists',
  certificates: 'Certificates',
}
const KIND_SINGULAR: Record<NpmKind, string> = {
  hosts: 'host',
  redirects: 'redirect',
  streams: 'stream',
  accessLists: 'access list',
  certificates: 'certificate',
}
const KINDS: NpmKind[] = ['hosts', 'redirects', 'streams', 'accessLists', 'certificates']

export default function NpmImportWizard({ open, onClose }: { open: boolean; onClose: () => void }) {
  const qc = useQueryClient()
  const toast = useToast()
  const [step, setStep] = useState(0)
  const [mode, setMode] = useState<'upload' | 'path'>('path')
  const [file, setFile] = useState<File | null>(null)
  const [path, setPath] = useState('')
  const [preview, setPreview] = useState<NpmPreview | null>(null)
  const [overwrite, setOverwrite] = useState(false)
  const [result, setResult] = useState<NpmCommitResult | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const fileRef = useRef<HTMLInputElement>(null)

  const close = () => {
    onClose()
    setTimeout(() => {
      setStep(0)
      setPreview(null)
      setResult(null)
      setError('')
      setOverwrite(false)
      setFile(null)
    }, 200)
  }

  const runPreview = async () => {
    setBusy(true)
    setError('')
    try {
      let pv: NpmPreview
      if (mode === 'upload') {
        if (!file) return
        const form = new FormData()
        form.append('database', file)
        pv = await api.upload<NpmPreview>('/api/import/npm/preview', form)
      } else {
        pv = await api.post<NpmPreview>('/api/import/npm/preview', { path: path.trim() })
      }
      setPreview(pv)
      setStep(1)
    } catch (err) {
      // fetch rejects without a response when the connection is cut: most often
      // a proxy in front of Relay limiting uploads, or the upload timing out.
      if (mode === 'upload' && err instanceof TypeError) {
        setError(
          'The upload was cut off before Relay answered. If you open Relay through a domain, the proxy in front of it may limit uploads ' +
            '(update Relay, or raise Max body size on that host). You can also copy the NPM data folder to the server, mount it into the ' +
            'Relay container and use “Mounted folder” instead.',
        )
      } else setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }

  const willImport = (it: NpmPreview['items'][number]) => !it.conflict || (overwrite && !!it.overwritable)
  const importable = preview ? preview.items.filter(willImport).length : 0

  const commit = async () => {
    if (!preview) return
    setBusy(true)
    setError('')
    try {
      const res = await api.post<NpmCommitResult>('/api/import/npm/commit', { token: preview.token, overwrite })
      setResult(res)
      setStep(2)
      qc.invalidateQueries({ queryKey: ['entities'] })
      qc.invalidateQueries({ queryKey: keys.pending })
      const n = Object.values(res.created).reduce((a, b) => a + (b ?? 0), 0) + Object.values(res.updated).reduce((a, b) => a + (b ?? 0), 0)
      if (n > 0) {
        toast.show({ kind: 'success', title: `Imported ${n} items from Nginx Proxy Manager`, message: 'Added to pending changes', actions: [applyNowAction] })
      }
    } catch (err) {
      setError(errorMessage(err))
      if (errorMessage(err).includes('expired')) setStep(0)
    } finally {
      setBusy(false)
    }
  }

  const footer =
    step === 0 ? (
      <>
        <Button onClick={close}>Cancel</Button>
        <Button variant="primary" loading={busy} disabled={mode === 'upload' ? !file : !path.trim().startsWith('/')} onClick={runPreview}>
          Preview import →
        </Button>
      </>
    ) : step === 1 ? (
      <>
        <Button onClick={() => setStep(0)} style={{ marginRight: 'auto' }}>← Back</Button>
        <Button onClick={close}>Cancel</Button>
        <Button variant="primary" loading={busy} disabled={importable === 0} onClick={commit}>
          Import {importable} {importable === 1 ? 'item' : 'items'}
        </Button>
      </>
    ) : (
      <>
        <Button onClick={close}>Close</Button>
        <Button variant="primary" onClick={() => { applyNowAction.onClick(); close() }}>Apply now</Button>
      </>
    )

  return (
    <Dialog open={open} onClose={close} width={760} title="Import from Nginx Proxy Manager" description="Hosts, certs and access lists are converted; nothing is changed until you confirm." footer={footer}>
      <div className="col gap-16">
        <Stepper steps={['Source', 'Review', 'Done']} current={step} />
        {error && <Callout tone="danger">{error}</Callout>}

        {step === 0 && (
          <div className="col gap-14">
            <Segmented
              value={mode}
              onChange={setMode}
              options={[
                { value: 'path', label: 'NPM data folder' },
                { value: 'upload', label: 'Upload database.sqlite' },
              ]}
            />
            {mode === 'path' ? (
              <Field
                label="Folder inside the Relay container"
                hint={<>Mount NPM's <span className="mono">data</span> folder (with <span className="mono">database.sqlite</span>, <span className="mono">custom_ssl/</span>) and its <span className="mono">letsencrypt</span> folder next to it, e.g. <span className="mono">/import/npm/data</span> and <span className="mono">/import/npm/letsencrypt</span>. Certificates are only imported this way.</>}
              >
                <Input mono placeholder="/import/npm/data" value={path} onChange={(e) => setPath(e.target.value)} onKeyDown={(e) => e.key === 'Enter' && runPreview()} autoFocus />
              </Field>
            ) : (
              <Field label="NPM database" hint="Only SQLite installs are supported. Custom certificates are read from the database; Let's Encrypt certificates are created as pending and need to be requested again.">
                <div
                  className="ops-dropzone"
                  onClick={() => fileRef.current?.click()}
                  onDragOver={(e) => e.preventDefault()}
                  onDrop={(e) => {
                    e.preventDefault()
                    const f = e.dataTransfer.files[0]
                    if (f) setFile(f)
                  }}
                >
                  <div className="medium">{file ? file.name : 'Drop database.sqlite or browse'}</div>
                  <div className="small faint">{file ? bytes(file.size) : 'From /data/database.sqlite of your NPM container'}</div>
                  <input ref={fileRef} type="file" hidden accept=".sqlite,.db,.sqlite3" onChange={(e) => setFile(e.target.files?.[0] ?? null)} />
                </div>
              </Field>
            )}
          </div>
        )}

        {step === 1 && preview && (
          <div className="col gap-14">
            <div className="ops-count-grid">
              {KINDS.map((k) => (
                <div key={k} className="ops-count">
                  <div className="n">{preview.counts[k]?.total ?? 0}</div>
                  <div className="l">{KIND_LABEL[k]}</div>
                  {(preview.counts[k]?.conflicts ?? 0) > 0 && <div className="c">{preview.counts[k].conflicts} already exist</div>}
                </div>
              ))}
            </div>
            {preview.warnings.map((w) => (
              <Callout key={w} tone="warn">{w}</Callout>
            ))}
            {preview.items.length === 0 ? (
              <div className="muted small">The database contains nothing to import.</div>
            ) : (
              <div className="ops-scroll">
                <table className="table compact">
                  <thead>
                    <tr>
                      <th>Type</th>
                      <th>Name</th>
                      <th>Details</th>
                      <th />
                    </tr>
                  </thead>
                  <tbody>
                    {preview.items.map((it) => (
                      <tr key={`${it.kind}-${it.npmId}`} className={willImport(it) ? undefined : 'dim'}>
                        <td className="ops-kind">{KIND_SINGULAR[it.kind]}</td>
                        <td className="mono small">
                          {it.name}
                          {it.conflict && <div className="micro warn-text" style={{ fontFamily: 'var(--font-sans)' }}>{it.conflict}</div>}
                          {it.warnings.map((w) => (
                            <div key={w} className="micro faint" style={{ fontFamily: 'var(--font-sans)' }}>{w}</div>
                          ))}
                        </td>
                        <td className="mono small faint truncate" style={{ maxWidth: 220 }} title={it.detail}>{it.detail}</td>
                        <td style={{ textAlign: 'right' }}>
                          {it.conflict ? (
                            <Badge tone="warn">{willImport(it) ? 'overwrite' : 'skip'}</Badge>
                          ) : it.warnings.length ? (
                            <Badge tone="info">review</Badge>
                          ) : (
                            <Badge tone="ok">new</Badge>
                          )}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
            <Checkbox checked={overwrite} onChange={setOverwrite} label="Overwrite items that already exist in Relay (otherwise they are skipped)" />
            <div className="small faint">Source: <span className="mono">{preview.source}</span></div>
          </div>
        )}

        {step === 2 && result && (
          <div className="col gap-12">
            <table className="table compact" style={{ border: '1px solid var(--hairline)', borderRadius: 8 }}>
              <thead>
                <tr>
                  <th />
                  <th className="num">Created</th>
                  <th className="num">Updated</th>
                  <th className="num">Skipped</th>
                </tr>
              </thead>
              <tbody>
                {KINDS.map((k) => (
                  <tr key={k}>
                    <td>{KIND_LABEL[k]}</td>
                    <td className="num">{result.created[k] ?? 0}</td>
                    <td className="num">{result.updated[k] ?? 0}</td>
                    <td className="num">{result.skipped[k] ?? 0}</td>
                  </tr>
                ))}
              </tbody>
            </table>
            {result.errors.length > 0 && (
              <Callout tone="warn" title={`${result.errors.length} items could not be imported`}>
                <ul style={{ margin: '4px 0 0', paddingLeft: 16 }}>
                  {result.errors.map((e) => (
                    <li key={e}>{e}</li>
                  ))}
                </ul>
              </Callout>
            )}
            <div className="small muted">Imported configuration is pending. Review it on the Hosts page, then apply.</div>
          </div>
        )}
      </div>
    </Dialog>
  )
}
