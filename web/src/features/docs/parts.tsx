// Building blocks for documentation articles.
import type { ReactNode } from 'react'
import { Link } from 'react-router-dom'
import { Callout, CodeBlock, CopyButton, Icon, cx, type IconName } from '../../components/ui'

export function slug(s: string) {
  return s.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '')
}

function scrollToId(id: string) {
  const el = document.getElementById(id)
  if (!el) return
  el.scrollIntoView({ behavior: 'smooth', block: 'start' })
  history.replaceState(history.state, '', `#${id}`)
}

/** Section heading with an anchor; collected into "On this page". */
export function H2({ children, id }: { children: string; id?: string }) {
  const anchor = id ?? slug(children)
  return (
    <h2 id={anchor} className="docs-h2">
      <a href={`#${anchor}`} onClick={(e) => { e.preventDefault(); scrollToId(anchor) }}>{children}</a>
    </h2>
  )
}

export function H3({ children }: { children: ReactNode }) {
  return <h3 className="docs-h3">{children}</h3>
}

export function P({ children }: { children: ReactNode }) {
  return <p className="docs-p">{children}</p>
}

/** Inline code. */
export function C({ children }: { children: ReactNode }) {
  return <code className="docs-code">{children}</code>
}

/** A place in the UI, e.g. <UI>Settings → Docker discovery</UI>. */
export function UI({ children }: { children: ReactNode }) {
  return <span className="docs-ui">{children}</span>
}

export function List({ children, ordered }: { children: ReactNode; ordered?: boolean }) {
  return ordered ? <ol className="docs-list">{children}</ol> : <ul className="docs-list">{children}</ul>
}

/** Numbered walkthrough. */
export function Steps({ children }: { children: ReactNode }) {
  return <ol className="docs-steps">{children}</ol>
}

export function Step({ title, children }: { title: ReactNode; children?: ReactNode }) {
  return (
    <li className="docs-step">
      <div className="docs-step-title">{title}</div>
      {children && <div className="docs-step-body">{children}</div>}
    </li>
  )
}

/** Copyable code example with a caption. */
export function Example({ title, lang, code }: { title?: ReactNode; lang?: string; code: string }) {
  return (
    <div className="docs-example">
      <div className="docs-example-head">
        {title && <span className="docs-example-title">{title}</span>}
        {lang && <span className="docs-example-lang">{lang}</span>}
        <span className="spacer" />
        <CopyButton text={code.trim()} />
      </div>
      <CodeBlock code={code.trim()} className="docs-example-code" />
    </div>
  )
}

export function Tip({ title, children }: { title?: ReactNode; children: ReactNode }) {
  return <div className="docs-callout"><Callout tone="ok" icon="bolt" title={title ?? 'Tip'}>{children}</Callout></div>
}

export function Note({ title, children }: { title?: ReactNode; children: ReactNode }) {
  return <div className="docs-callout"><Callout tone="info" title={title}>{children}</Callout></div>
}

export function Warn({ title, children }: { title?: ReactNode; children: ReactNode }) {
  return <div className="docs-callout"><Callout tone="warn" title={title}>{children}</Callout></div>
}

export function Table({ head, rows, mono }: { head: ReactNode[]; rows: ReactNode[][]; mono?: number[] }) {
  return (
    <div className="docs-table-wrap">
      <table className="docs-table">
        <thead>
          <tr>{head.map((h, i) => <th key={i}>{h}</th>)}</tr>
        </thead>
        <tbody>
          {rows.map((r, i) => (
            <tr key={i}>{r.map((c, j) => <td key={j} className={cx(mono?.includes(j) && 'mono')}>{c}</td>)}</tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

/** Button-like link into the app. */
export function GoTo({ to, children, icon = 'external' }: { to: string; children: ReactNode; icon?: IconName }) {
  return (
    <Link to={to} className="docs-goto">
      <Icon name={icon} size={13} />
      {children}
    </Link>
  )
}

/** Link to another docs article. */
export function See({ id, children }: { id: string; children: ReactNode }) {
  return <Link to={`/docs/${id}`} className="docs-link">{children}</Link>
}

export interface CardItem { to: string; icon: IconName; title: string; desc: string }

export function Cards({ items }: { items: CardItem[] }) {
  return (
    <div className="docs-cards">
      {items.map((c) => (
        <Link key={c.to} to={c.to} className="docs-card">
          <span className="docs-card-icon"><Icon name={c.icon} size={16} /></span>
          <span className="docs-card-title">{c.title}</span>
          <span className="docs-card-desc">{c.desc}</span>
        </Link>
      ))}
    </div>
  )
}

/** Simple left-to-right flow diagram. */
export function Flow({ steps }: { steps: { label: string; sub?: string }[] }) {
  return (
    <div className="docs-flow">
      {steps.map((s, i) => (
        <div key={s.label} className="docs-flow-item">
          <div className="docs-flow-box">
            <div className="docs-flow-label">{s.label}</div>
            {s.sub && <div className="docs-flow-sub">{s.sub}</div>}
          </div>
          {i < steps.length - 1 && <span className="docs-flow-arrow">→</span>}
        </div>
      ))}
    </div>
  )
}

/** Term + explanation list. */
export function Defs({ items }: { items: [ReactNode, ReactNode][] }) {
  return (
    <dl className="docs-defs">
      {items.map(([t, d], i) => (
        <div key={i} className="docs-def">
          <dt>{t}</dt>
          <dd>{d}</dd>
        </div>
      ))}
    </dl>
  )
}
