import { useEffect, useRef, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { Dialog, Kbd } from '../ui'

const GO: Record<string, string> = {
  o: '/', f: '/topology', h: '/hosts', l: '/load-balancer', c: '/certificates', a: '/access', t: '/streams', g: '/logs', v: '/history', s: '/settings', d: '/docs', n: '/dns',
}

function typing(e: KeyboardEvent) {
  const el = e.target as HTMLElement | null
  if (!el) return false
  return el.isContentEditable || ['INPUT', 'TEXTAREA', 'SELECT'].includes(el.tagName)
}

/**
 * Global keyboard shortcuts (see design 30a). Window events emitted for
 * features: relay:apply, relay:focus-search, relay:shortcuts, relay:palette.
 */
export function useGlobalShortcuts() {
  const navigate = useNavigate()
  const pendingG = useRef<number | null>(null)
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
        e.preventDefault()
        window.dispatchEvent(new CustomEvent('relay:apply'))
        return
      }
      if (typing(e) || e.metaKey || e.ctrlKey || e.altKey) return
      if (document.querySelector('.drawer, .dialog')) return
      if (pendingG.current !== null) {
        window.clearTimeout(pendingG.current)
        pendingG.current = null
        const to = GO[e.key.toLowerCase()]
        if (to) {
          e.preventDefault()
          navigate(to)
        }
        return
      }
      switch (e.key) {
        case 'g':
          pendingG.current = window.setTimeout(() => (pendingG.current = null), 1200)
          break
        case '?':
          window.dispatchEvent(new CustomEvent('relay:shortcuts'))
          break
        case 'n':
          e.preventDefault()
          navigate('/hosts?new=1')
          break
        case '/':
          e.preventDefault()
          window.dispatchEvent(new CustomEvent('relay:focus-search'))
          break
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [navigate])
}

const SECTIONS: { title: string; rows: [string, string[]][] }[] = [
  { title: 'Global', rows: [['Command palette', ['⌘', 'K']], ['New host', ['N']], ['Apply pending changes', ['⌘', '⏎']], ['Search / filter', ['/']], ['This sheet', ['?']]] },
  { title: 'Go to', rows: [['Overview', ['G', 'O']], ['Topology', ['G', 'F']], ['Hosts', ['G', 'H']], ['Load balancer', ['G', 'L']], ['Certificates', ['G', 'C']], ['Access lists · Streams', ['G', 'A', '·', 'G', 'T']], ['Logs · History · Settings', ['G', 'G', '·', 'G', 'V', '·', 'G', 'S']], ['Public DNS · Documentation', ['G', 'N', '·', 'G', 'D']]] },
  { title: 'Lists', rows: [['Move · open', ['J', 'K', '·', '⏎']], ['Select · select all', ['X', '·', '⌘', 'A']], ['Edit · delete', ['E', '·', '⌫']], ['Toggle grid / table', ['V']]] },
  { title: 'Drawers', rows: [['Save to pending', ['⌘', 'S']], ['Next / previous tab', ['⌘', ']', '·', '⌘', '[']], ['Close', ['esc']], ['Logs: pause tail', ['space']]] },
]

export function ShortcutsSheet() {
  const [open, setOpen] = useState(false)
  useEffect(() => {
    const h = () => setOpen((o) => !o)
    window.addEventListener('relay:shortcuts', h)
    return () => window.removeEventListener('relay:shortcuts', h)
  }, [])
  return (
    <Dialog open={open} onClose={() => setOpen(false)} width={640} title={<div className="row between">Keyboard shortcuts <Kbd>esc</Kbd></div>}>
      <div className="grid-2" style={{ gap: 24 }}>
        {SECTIONS.map((s) => (
          <div key={s.title} className="col gap-8">
            <div className="micro faint mono">{s.title}</div>
            {s.rows.map(([label, ks]) => (
              <div key={label} className="row between small">
                <span>{label}</span>
                <span className="row gap-4">
                  {ks.map((k, i) => (k === '·' ? <span key={i} className="faint">·</span> : <Kbd key={i}>{k}</Kbd>))}
                </span>
              </div>
            ))}
          </div>
        ))}
      </div>
    </Dialog>
  )
}
