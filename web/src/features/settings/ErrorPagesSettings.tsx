// Settings → Error pages: branded error pages and the maintenance page.
import { useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { Button, Callout, Card, Field, Icon, Input, SectionHeader, Skeleton, Tabs, Textarea, ToggleRow, cx, useToast, type TabDef } from '../../components/ui'
import { useRole, useSaveSettings, useSettings } from '../../lib/queries'
import type { ErrorPage, ErrorPageKey, ErrorPagesSettings as Settings } from '../../lib/types'
import { fieldErrors, pendingToast, toastUnlessFields } from '../certificates/common'
import { ErrorPagePreviewFrame } from './ErrorPagePreview'
import { ERROR_PAGE_INFO, ERROR_PAGE_KEYS, HEX_COLOR, MAX_HTML_BYTES, byteLength, normalizeErrorPages, useErrorPagePreview } from './errorPagesApi'
import './errorPages.css'

const PRESETS = ['#2563eb', '#0f766e', '#16a34a', '#d97706', '#dc2626', '#7c3aed', '#141414']

const HTML_PLACEHOLDER = `<!doctype html>
<html>
  <head><meta charset="utf-8"><title>Back soon</title></head>
  <body style="font-family: system-ui; text-align: center; padding: 80px 20px">
    <h1>We'll be right back</h1>
    <p>Questions? Email help@example.com</p>
  </body>
</html>`

function kb(bytes: number) {
  return bytes < 1024 ? `${bytes} B` : `${(bytes / 1024).toFixed(bytes < 10240 ? 1 : 0)} KB`
}

/** Text input with a colour swatch and a few presets (no native colour picker). */
function ColorInput({ value, onChange, disabled, error }: { value: string; onChange: (v: string) => void; disabled?: boolean; error?: string }) {
  const valid = HEX_COLOR.test(value)
  const normalize = (v: string) => {
    const t = v.trim().toLowerCase()
    if (!t) return ''
    const hex = t.startsWith('#') ? t : `#${t}`
    if (/^#[0-9a-f]{3}$/.test(hex)) return `#${hex[1]}${hex[1]}${hex[2]}${hex[2]}${hex[3]}${hex[3]}`
    return hex
  }
  return (
    <>
      <div className="ep-color">
        <span className={cx('ep-color-swatch', !valid && 'empty')} style={valid ? { background: value } : undefined} />
        <Input
          mono
          value={value}
          disabled={disabled}
          invalid={!!error}
          placeholder="Default"
          maxLength={7}
          spellCheck={false}
          aria-label="Accent colour"
          onChange={(e) => onChange(e.target.value.trim())}
          onBlur={(e) => onChange(normalize(e.target.value))}
        />
      </div>
      <div className="ep-presets">
        <button type="button" className={cx('ep-preset default', value === '' && 'selected')} disabled={disabled} onClick={() => onChange('')}>
          Default
        </button>
        {PRESETS.map((c) => (
          <button
            key={c}
            type="button"
            className={cx('ep-preset', value.toLowerCase() === c && 'selected')}
            style={{ background: c }}
            disabled={disabled}
            aria-label={`Use ${c}`}
            title={c}
            onClick={() => onChange(c)}
          >
            {value.toLowerCase() === c && <Icon name="check" size={12} />}
          </button>
        ))}
      </div>
    </>
  )
}

export default function ErrorPagesSettings() {
  const settings = useSettings('error_pages')
  const save = useSaveSettings('error_pages')
  const { isAdmin } = useRole()
  const toast = useToast()
  const [draft, setDraft] = useState<Settings | undefined>()
  const [baseline, setBaseline] = useState('')
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [page, setPage] = useState<ErrorPageKey>('502')
  const [htmlOpen, setHtmlOpen] = useState(false)

  useEffect(() => {
    if (!settings.data) return
    const n = normalizeErrorPages(settings.data)
    setDraft(n)
    setBaseline(JSON.stringify(n))
  }, [settings.data])

  const current = draft?.pages[page]
  useEffect(() => {
    setHtmlOpen(!!current?.html)
    // Only when switching pages, not while typing.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [page, !!draft])

  const previewReq = useMemo(() => (draft ? { settings: draft, page } : null), [draft, page])
  const preview = useErrorPagePreview(previewReq, !!draft)

  const readOnly = !isAdmin
  const header = <SectionHeader title="Error pages" description="What visitors see when an app is down, a request is blocked, or a host is in maintenance." />

  if (!draft || !current) {
    return (
      <>
        {header}
        {settings.isError ? <Callout tone="danger">Couldn't load error page settings.</Callout> : (
          <>
            <Skeleton height={90} />
            <Skeleton height={140} />
            <Skeleton height={320} />
          </>
        )}
      </>
    )
  }

  const dirty = JSON.stringify(draft) !== baseline
  const info = ERROR_PAGE_INFO[page]
  const set = (patch: Partial<Settings>) => {
    setDraft((d) => (d ? { ...d, ...patch } : d))
    const keys = Object.keys(patch)
    setErrors((e) => Object.fromEntries(Object.entries(e).filter(([k]) => !keys.includes(k))))
  }
  const setPageField = (patch: Partial<ErrorPage>) => {
    setDraft((d) => {
      if (!d) return d
      const next: ErrorPage = { ...d.pages[page], ...patch }
      if (!next.html) delete next.html
      return { ...d, pages: { ...d.pages, [page]: next } }
    })
    const prefix = Object.keys(patch).map((k) => `pages.${page}.${k}`)
    setErrors((e) => Object.fromEntries(Object.entries(e).filter(([k]) => !prefix.includes(k))))
  }

  const htmlBytes = current.html ? byteLength(current.html) : 0
  const htmlTooBig = htmlBytes > MAX_HTML_BYTES
  const colorErr = errors.accentColor || (draft.accentColor && !HEX_COLOR.test(draft.accentColor) ? 'Use a hex colour like #2563eb' : '')
  const pageErr = (k: ErrorPageKey) => Object.keys(errors).some((f) => f.startsWith(`pages.${k}.`))

  const tabs: TabDef<ErrorPageKey>[] = ERROR_PAGE_KEYS.map((k) => ({
    id: k,
    label: (
      <>
        {ERROR_PAGE_INFO[k].label}
        {draft.pages[k].html && <span className="ep-tab-flag">html</span>}
        {pageErr(k) && <span className="dot danger" aria-label="has errors" />}
      </>
    ),
  }))

  const submit = async () => {
    setErrors({})
    try {
      await save.mutateAsync(draft)
      pendingToast(toast, 'Error pages saved', 'Error pages')
    } catch (err) {
      const f = fieldErrors(err)
      setErrors(f)
      const withErr = ERROR_PAGE_KEYS.find((k) => Object.keys(f).some((x) => x.startsWith(`pages.${k}.`)))
      if (withErr && !Object.keys(f).some((x) => x.startsWith(`pages.${page}.`))) setPage(withErr)
      toastUnlessFields(toast, err, 'Could not save error pages')
    }
  }

  return (
    <>
      {header}
      {readOnly && <Callout tone="info">Only admins can change these settings.</Callout>}

      <Card>
        <ToggleRow
          title="Use Relay's error pages"
          description="Replaces the proxy engine's plain pages for errors Relay itself returns. Error pages sent by your apps pass through untouched."
          checked={draft.enabled}
          disabled={readOnly}
          onChange={(enabled) => set({ enabled })}
        />
        <div className="card-row small muted">
          <Icon name="info" size={14} />
          <span>
            The maintenance page is always used for hosts in maintenance mode, even when this is off. Turn it on per host under <span className="medium">Advanced → Maintenance mode</span>.
          </span>
        </div>
      </Card>

      <Card title="Look">
        <div className="card-body grid-2" style={{ gap: 14 }}>
          <Field label="Brand name" error={errors.brandName} hint="Shown at the top of every page · optional">
            <Input value={draft.brandName} disabled={readOnly} invalid={!!errors.brandName} placeholder="Acme Home Lab" maxLength={80} onChange={(e) => set({ brandName: e.target.value })} />
          </Field>
          <Field label="Accent colour" error={colorErr}>
            <ColorInput value={draft.accentColor} disabled={readOnly} error={colorErr} onChange={(accentColor) => set({ accentColor })} />
          </Field>
        </div>
      </Card>

      <Card title="Pages">
        <div className="ep-tabs">
          <Tabs flush tabs={tabs} value={page} onChange={setPage} />
        </div>
        <div className="card-body col gap-14">
          <div className="small muted">{info.when}</div>
          <Field label="Title" error={errors[`pages.${page}.title`]}>
            <Input value={current.title} disabled={readOnly} invalid={!!errors[`pages.${page}.title`]} placeholder={info.title} maxLength={120} onChange={(e) => setPageField({ title: e.target.value })} />
          </Field>
          <Field label="Message" error={errors[`pages.${page}.message`]}>
            <Textarea rows={3} value={current.message} disabled={readOnly} invalid={!!errors[`pages.${page}.message`]} placeholder={info.message} onChange={(e) => setPageField({ message: e.target.value })} />
          </Field>

          <div className="col gap-10">
            <div className="row between">
              <button type="button" className={cx('ep-disclosure', htmlOpen && 'open')} aria-expanded={htmlOpen} onClick={() => setHtmlOpen((o) => !o)}>
                <Icon name="chevron" size={14} />
                Advanced: custom HTML
                {current.html && <span className="badge">in use</span>}
              </button>
              {htmlOpen && !readOnly && (
                <Button size="sm" variant="ghost" icon="rollback" disabled={!current.html} onClick={() => setPageField({ html: undefined })}>
                  Reset to built-in design
                </Button>
              )}
            </div>
            {htmlOpen && (
              <Field
                error={errors[`pages.${page}.html`] || (htmlTooBig ? `Too large: ${kb(htmlBytes)} of ${kb(MAX_HTML_BYTES)}` : '')}
                hint={current.html ? `Replaces the built-in design for this page · ${kb(htmlBytes)} of ${kb(MAX_HTML_BYTES)}` : 'Paste a complete HTML page to replace the built-in design · keep CSS inline · max 100 KB'}
              >
                <Textarea
                  mono
                  rows={12}
                  spellCheck={false}
                  value={current.html ?? ''}
                  disabled={readOnly}
                  invalid={!!errors[`pages.${page}.html`] || htmlTooBig}
                  placeholder={HTML_PLACEHOLDER}
                  onChange={(e) => setPageField({ html: e.target.value })}
                />
              </Field>
            )}
          </div>
        </div>
      </Card>

      <Card title="Preview" sub={page === 'maintenance' ? 'Maintenance · HTTP 503' : `HTTP ${page}`}>
        <div className="card-body col gap-10">
          {!draft.enabled && page !== 'maintenance' && (
            <Callout tone="info">Relay's error pages are off, so visitors still get the engine's plain page. Turn them on above to use this design.</Callout>
          )}
          <ErrorPagePreviewFrame state={preview} label={page === 'maintenance' ? 'Maintenance page' : `${page} · ${current.title || info.title}`} />
          <div className="small muted">
            Want the maintenance page on a host? Open it in <Link to="/hosts">Proxy hosts</Link> and choose <span className="medium">Start maintenance</span> from its menu.
          </div>
        </div>
      </Card>

      {!readOnly && (
        <div className="row gap-8">
          <Button variant="primary" onClick={submit} loading={save.isPending} disabled={!dirty || htmlTooBig}>Save changes</Button>
          {dirty && (
            <Button
              variant="ghost"
              onClick={() => {
                setDraft(JSON.parse(baseline) as Settings)
                setErrors({})
              }}
            >
              Discard
            </Button>
          )}
        </div>
      )}
    </>
  )
}
