// Formatting helpers matching the design's terse, mono-friendly style.

/** 1240000 → "1.24M", 42000 → "42k", 512 → "512" */
export function compact(n: number | undefined | null): string {
  if (n === undefined || n === null || Number.isNaN(n)) return '—'
  const abs = Math.abs(n)
  if (abs >= 1e9) return trim(n / 1e9, 2) + 'B'
  if (abs >= 1e6) return trim(n / 1e6, 2) + 'M'
  if (abs >= 1e4) return trim(n / 1e3, 0) + 'k'
  if (abs >= 1e3) return trim(n / 1e3, 1) + 'k'
  return String(Math.round(n))
}

function trim(n: number, digits: number): string {
  return n.toFixed(digits).replace(/\.0+$/, '').replace(/(\.\d*[1-9])0+$/, '$1')
}

/** Bytes → "2.3 KB", "86.3 GB" */
export function bytes(n: number | undefined | null): string {
  if (n === undefined || n === null) return '—'
  if (n < 1024) return `${n} B`
  const units = ['KB', 'MB', 'GB', 'TB']
  let v = n / 1024
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v >= 100 ? v.toFixed(0) : v.toFixed(1)} ${units[i]}`
}

/** Bytes per second → "2.1 MB/s" */
export function rate(bps: number | undefined | null): string {
  if (!bps) return '—'
  return bytes(bps) + '/s'
}

/** Milliseconds → "38 ms", "3.01 s" */
export function ms(v: number | undefined | null): string {
  if (v === undefined || v === null) return '—'
  if (v < 1000) return `${Math.round(v)} ms`
  return `${(v / 1000).toFixed(v < 10000 ? 2 : 1)} s`
}

/** Seconds → "41d", "6m", "2h" */
export function duration(sec: number | undefined | null): string {
  if (sec === undefined || sec === null) return '—'
  if (sec < 60) return `${Math.round(sec)}s`
  if (sec < 3600) return `${Math.round(sec / 60)}m`
  if (sec < 86400) return `${Math.round(sec / 3600)}h`
  return `${Math.round(sec / 86400)}d`
}

/** ISO time → "2 min ago", "3 h ago", "Yesterday", "3 days ago" */
export function ago(iso: string | undefined | null, now = Date.now()): string {
  if (!iso) return '—'
  const t = new Date(iso).getTime()
  if (!t) return '—'
  const s = Math.round((now - t) / 1000)
  if (s < 0) return 'just now'
  if (s < 45) return 'just now'
  if (s < 3600) return `${Math.max(1, Math.round(s / 60))} min ago`
  if (s < 86400) return `${Math.round(s / 3600)} h ago`
  const days = Math.floor(s / 86400)
  if (days === 1) return 'Yesterday'
  if (days < 30) return `${days} days ago`
  return new Date(iso).toLocaleDateString()
}

/** Short relative: "2h", "1d", "30d" */
export function agoShort(iso: string | undefined | null): string {
  if (!iso) return '—'
  const s = (Date.now() - new Date(iso).getTime()) / 1000
  if (s < 60) return 'now'
  return duration(s)
}

/** Days until an ISO date (negative when past). */
export function daysUntil(iso: string | undefined | null): number | undefined {
  if (!iso) return undefined
  return Math.floor((new Date(iso).getTime() - Date.now()) / 86_400_000)
}

/** "14:03:41" */
export function clock(iso: string, withMs = false): string {
  const d = new Date(iso)
  const base = d.toLocaleTimeString([], { hour12: false, hour: '2-digit', minute: '2-digit', second: '2-digit' })
  return withMs ? `${base}.${String(d.getMilliseconds()).padStart(3, '0')}` : base
}

/** "2026-09-14 03:00" */
export function dateTime(iso: string | undefined | null): string {
  if (!iso) return '—'
  const d = new Date(iso)
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

export function date(iso: string | undefined | null): string {
  return iso ? dateTime(iso).slice(0, 10) : '—'
}

/** "http://10.0.0.21:3000" */
export function upstreamUrl(u: { scheme: string; host: string; port: number; path?: string; backendId?: string }): string {
  if (u.backendId) return `lb → ${u.backendId}`
  const port = u.port ? `:${u.port}` : ''
  return `${u.scheme}://${u.host}${port}${u.path ?? ''}`
}

export function pluralize(n: number, one: string, many = one + 's'): string {
  return `${n} ${n === 1 ? one : many}`
}

export function initials(name: string): string {
  return name.slice(0, 2).toUpperCase()
}

export function copyText(text: string): Promise<void> {
  return navigator.clipboard.writeText(text)
}

/** Random short id for nested rows (locations, rules, servers). */
export function rid(): string {
  return Math.random().toString(36).slice(2, 10)
}
