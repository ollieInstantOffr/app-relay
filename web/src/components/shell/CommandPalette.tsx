import { useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { Dialog, Icon, Kbd, cx, type IconName } from '../ui'
import { useEntities } from '../../lib/queries'
import { upstreamUrl } from '../../lib/format'
import { NAV } from './AppShell'

interface Item {
  id: string
  group: 'Hosts' | 'Backends' | 'Certificates' | 'Actions' | 'Navigate'
  label: string
  meta?: string
  icon: IconName
  run: () => void
}

export function CommandPalette() {
  const [open, setOpen] = useState(false)
  const [q, setQ] = useState('')
  const [active, setActive] = useState(0)
  const navigate = useNavigate()
  const hosts = useEntities('hosts', { enabled: open }).data ?? []
  const backends = useEntities('backends', { enabled: open }).data ?? []
  const certs = useEntities('certificates', { enabled: open }).data ?? []
  const listRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    const onOpen = () => setOpen(true)
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault()
        setOpen((o) => !o)
      }
    }
    window.addEventListener('relay:palette', onOpen)
    window.addEventListener('keydown', onKey)
    return () => {
      window.removeEventListener('relay:palette', onOpen)
      window.removeEventListener('keydown', onKey)
    }
  }, [])

  useEffect(() => {
    if (open) {
      setQ('')
      setActive(0)
    }
  }, [open])

  const items = useMemo<Item[]>(() => {
    const go = (to: string) => () => {
      setOpen(false)
      navigate(to)
    }
    const needle = q.trim().toLowerCase()
    const match = (...s: (string | undefined)[]) => !needle || s.some((x) => x?.toLowerCase().includes(needle))
    const out: Item[] = []
    for (const h of hosts) {
      if (match(...h.domains, h.upstream.host)) {
        out.push({ id: 'h' + h.id, group: 'Hosts', label: h.domains[0] ?? h.id, meta: '→ ' + upstreamUrl(h.upstream).replace(/^https?:\/\//, ''), icon: 'hosts', run: go(`/hosts?edit=${h.id}`) })
      }
    }
    if (needle) {
      const h = hosts.find((x) => x.domains.some((d) => d.toLowerCase().includes(needle)))
      if (h) {
        out.push({ id: 'ae' + h.id, group: 'Actions', label: `Edit ${h.domains[0]}`, icon: 'edit', run: go(`/hosts?edit=${h.id}`) })
        out.push({ id: 'al' + h.id, group: 'Actions', label: `Show logs · host:${h.domains[0]}`, icon: 'logs', run: go(`/logs/access?host=${encodeURIComponent(h.domains[0])}`) })
      }
    }
    for (const b of backends) {
      if (match(b.name)) out.push({ id: 'b' + b.id, group: 'Backends', label: b.name, meta: `${b.mode} · ${b.algorithm} · ${b.servers.length} servers`, icon: 'load-balancer', run: go(`/load-balancer/backends?edit=${b.id}`) })
    }
    for (const c of certs) {
      if (match(c.name, ...c.domains)) out.push({ id: 'c' + c.id, group: 'Certificates', label: c.name, meta: c.provider, icon: 'certificates', run: go(`/certificates?cert=${c.id}`) })
    }
    const actions: Item[] = [
      { id: 'new-host', group: 'Actions', label: 'New proxy host', icon: 'plus', meta: 'N', run: go('/hosts?new=1') },
      { id: 'new-backend', group: 'Actions', label: 'New backend', icon: 'plus', run: go('/load-balancer/backends?new=1') },
      { id: 'req-cert', group: 'Actions', label: 'Request certificate', icon: 'certificates', run: go('/certificates?request=1') },
      {
        id: 'apply', group: 'Actions', label: 'Apply pending changes', icon: 'bolt', meta: '⌘⏎', run: () => {
          setOpen(false)
          window.dispatchEvent(new CustomEvent('relay:apply'))
        },
      },
    ]
    out.push(...actions.filter((a) => match(a.label)))
    const nav: Item[] = [...NAV, { to: '/settings', label: 'Settings', icon: 'settings' as IconName, shortcut: 'G S' }].map((n) => ({
      id: 'n' + n.to, group: 'Navigate' as const, label: `Go to ${n.label}`, icon: n.icon, meta: n.shortcut, run: go(n.to),
    }))
    out.push(...nav.filter((n) => match(n.label)))
    return out.slice(0, 60)
  }, [q, hosts, backends, certs, navigate])

  useEffect(() => setActive(0), [q])
  useEffect(() => {
    listRef.current?.querySelector('.palette-item.active')?.scrollIntoView({ block: 'nearest' })
  }, [active])

  let lastGroup = ''
  return (
    <Dialog open={open} onClose={() => setOpen(false)} className="palette">
      <div style={{ margin: '-18px -24px' }}>
        <div className="palette-input">
          <Icon name="search" size={16} />
          <input
            autoFocus
            value={q}
            placeholder="Search hosts, backends, certificates, actions…"
            onChange={(e) => setQ(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'ArrowDown') {
                e.preventDefault()
                setActive((a) => Math.min(items.length - 1, a + 1))
              } else if (e.key === 'ArrowUp') {
                e.preventDefault()
                setActive((a) => Math.max(0, a - 1))
              } else if (e.key === 'Enter') {
                e.preventDefault()
                items[active]?.run()
              }
            }}
          />
          <Kbd>esc</Kbd>
        </div>
        <div className="palette-list" ref={listRef}>
          {items.length === 0 && <div className="empty-desc" style={{ padding: 20 }}>No matches</div>}
          {items.map((it, i) => {
            const header = it.group !== lastGroup ? <div className="palette-group">{it.group}</div> : null
            lastGroup = it.group
            return (
              <div key={it.id}>
                {header}
                <div className={cx('palette-item', i === active && 'active')} onMouseEnter={() => setActive(i)} onClick={it.run}>
                  <Icon name={it.icon} size={16} />
                  <span className="truncate">{it.label}</span>
                  {it.meta && <span className="meta">{it.meta}</span>}
                  {i === active && <Kbd>↵</Kbd>}
                </div>
              </div>
            )
          })}
        </div>
      </div>
    </Dialog>
  )
}
