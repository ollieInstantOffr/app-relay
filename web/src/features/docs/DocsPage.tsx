// In-app documentation: grouped article list with search, article body,
// "On this page" outline and previous/next links.
import { useEffect, useMemo, useRef, useState } from 'react'
import { Link, Navigate, NavLink, useLocation, useNavigate, useParams } from 'react-router-dom'
import { TopBar } from '../../components/shell/TopBar'
import { Icon, Input, Kbd } from '../../components/ui'
import { DOC_GROUPS, DOC_SECTIONS } from './content'
import { GoTo } from './parts'
import './docs.css'

interface TocItem { id: string; text: string }

function matches(needle: string) {
  const terms = needle.toLowerCase().split(/\s+/).filter(Boolean)
  return DOC_SECTIONS.filter((s) => {
    const hay = `${s.title} ${s.summary} ${s.group} ${s.keywords ?? ''}`.toLowerCase()
    return terms.every((t) => hay.includes(t))
  })
}

export default function DocsPage() {
  const { section } = useParams()
  const current = DOC_SECTIONS.find((s) => s.id === section)
  const location = useLocation()
  const navigate = useNavigate()
  const [q, setQ] = useState('')
  const [toc, setToc] = useState<TocItem[]>([])
  const [activeId, setActiveId] = useState('')
  const mainRef = useRef<HTMLDivElement>(null)
  const articleRef = useRef<HTMLElement>(null)
  const searchRef = useRef<HTMLInputElement>(null)

  useEffect(() => {
    const focus = () => searchRef.current?.focus()
    window.addEventListener('relay:focus-search', focus)
    return () => window.removeEventListener('relay:focus-search', focus)
  }, [])

  // New article: collect headings, then jump to the hash or the top.
  useEffect(() => {
    const main = mainRef.current
    const article = articleRef.current
    if (!main || !article) return
    const items = Array.from(article.querySelectorAll<HTMLElement>('h2[id]')).map((h) => ({ id: h.id, text: h.textContent ?? '' }))
    setToc(items)
    setActiveId(items[0]?.id ?? '')
    const hash = decodeURIComponent(location.hash.slice(1))
    const target = hash ? document.getElementById(hash) : null
    if (target) target.scrollIntoView({ block: 'start' })
    else main.scrollTop = 0
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [current?.id])

  useEffect(() => {
    const main = mainRef.current
    if (!main || toc.length === 0) return
    const onScroll = () => {
      const top = main.getBoundingClientRect().top
      const line = Math.max(90, main.clientHeight * 0.3)
      let id = toc[0].id
      for (const t of toc) {
        const el = document.getElementById(t.id)
        if (el && el.getBoundingClientRect().top - top < line) id = t.id
      }
      if (main.scrollTop + main.clientHeight >= main.scrollHeight - 4) id = toc[toc.length - 1].id
      setActiveId(id)
    }
    main.addEventListener('scroll', onScroll, { passive: true })
    return () => main.removeEventListener('scroll', onScroll)
  }, [toc])

  const hits = useMemo(() => (q.trim() ? matches(q) : []), [q])

  if (!current) return <Navigate to="/docs/introduction" replace />

  const index = DOC_SECTIONS.indexOf(current)
  const prev = DOC_SECTIONS[index - 1]
  const next = DOC_SECTIONS[index + 1]
  const Body = current.Body

  const jump = (id: string) => {
    document.getElementById(id)?.scrollIntoView({ behavior: 'smooth', block: 'start' })
    history.replaceState(history.state, '', `#${id}`)
  }

  return (
    <>
      <TopBar title="Documentation" search />
      <div className="docs-layout">
        <nav className="docs-nav" aria-label="Documentation">
          <div className="docs-nav-search">
            <Icon name="search" size={14} className="icon-left" />
            <Input
              ref={searchRef}
              inputSize="sm"
              value={q}
              placeholder="Search the docs"
              aria-label="Search the docs"
              onChange={(e) => setQ(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Escape') {
                  setQ('')
                  e.currentTarget.blur()
                } else if (e.key === 'Enter' && hits[0]) {
                  navigate(`/docs/${hits[0].id}`)
                  setQ('')
                }
              }}
            />
          </div>
          <div className="docs-nav-list">
            {q.trim() ? (
              hits.length === 0 ? (
                <div className="docs-nav-empty">No articles match “{q.trim()}”.</div>
              ) : (
                <>
                  <div className="docs-nav-group">{hits.length} {hits.length === 1 ? 'result' : 'results'} <Kbd>↵</Kbd></div>
                  {hits.map((s) => (
                    <NavLink key={s.id} to={`/docs/${s.id}`} onClick={() => setQ('')}>
                      <Icon name={s.icon} size={15} />
                      <span className="docs-nav-hit">
                        {s.title}
                        <small>{s.summary}</small>
                      </span>
                    </NavLink>
                  ))}
                </>
              )
            ) : (
              DOC_GROUPS.map((g) => (
                <div key={g} className="col" style={{ gap: 1 }}>
                  <div className="docs-nav-group">{g}</div>
                  {DOC_SECTIONS.filter((s) => s.group === g).map((s) => (
                    <NavLink key={s.id} to={`/docs/${s.id}`}>
                      <Icon name={s.icon} size={15} />
                      {s.title}
                    </NavLink>
                  ))}
                </div>
              ))
            )}
          </div>
        </nav>

        <div className="docs-main" ref={mainRef}>
          <div className="docs-main-inner">
            <article className="docs-article" ref={articleRef} key={current.id}>
              <header className="docs-header">
                <div className="docs-eyebrow">
                  <Icon name={current.icon} size={13} />
                  {current.group}
                </div>
                <h1 className="docs-title">{current.title}</h1>
                <p className="docs-lead">{current.summary}</p>
                {current.app && (
                  <div className="docs-header-actions">
                    {current.app.map((a) => <GoTo key={a.to} to={a.to}>{a.label}</GoTo>)}
                  </div>
                )}
              </header>
              <Body />
              <nav className="docs-pager" aria-label="Previous and next article">
                {prev && (
                  <Link to={`/docs/${prev.id}`} className="prev">
                    <small>← Previous</small>
                    <span>{prev.title}</span>
                  </Link>
                )}
                {next && (
                  <Link to={`/docs/${next.id}`} className="next">
                    <small>Next →</small>
                    <span>{next.title}</span>
                  </Link>
                )}
              </nav>
            </article>
            {toc.length > 1 && (
              <aside className="docs-toc">
                <div className="docs-toc-title">On this page</div>
                {toc.map((t) => (
                  <a
                    key={t.id}
                    href={`#${t.id}`}
                    className={t.id === activeId ? 'active' : undefined}
                    onClick={(e) => {
                      e.preventDefault()
                      jump(t.id)
                    }}
                  >
                    {t.text}
                  </a>
                ))}
              </aside>
            )}
          </div>
        </div>
      </div>
    </>
  )
}
