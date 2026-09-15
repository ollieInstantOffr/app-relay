// Owner: slice observe — presentation helpers for access log rows.
import { bytes, clock, ms } from '../../lib/format'
import type { AccessEntry } from '../../lib/types'

export type StatusTone = 'ok' | 'warn' | 'danger' | 'muted'

export function statusTone(status: number): StatusTone {
  if (!status) return 'muted'
  if (status >= 500) return 'danger'
  if (status >= 400) return 'warn'
  return 'ok'
}

/** Last address of an nginx upstream list ("a:80, b:80 : c:80" → "c:80"). */
export function lastUpstream(addr: string): string {
  const parts = addr.split(/\s*,\s*|\s+:\s+/).filter(Boolean)
  return parts[parts.length - 1] ?? ''
}

export interface UpstreamInfo {
  text: string
  error: boolean
}

/** Upstream column: latency, or "refused" / "timeout" / "reset" in red. */
export function upstreamInfo(e: AccessEntry): UpstreamInfo {
  if (e.kind === 'stream') {
    if (e.status >= 500 && e.upstreamConnectTime == null) return { text: e.status === 504 ? 'timeout' : 'refused', error: true }
    return { text: e.upstreamConnectTime != null ? ms(e.upstreamConnectTime * 1000) : '—', error: false }
  }
  if (!e.upstreamAddr) return { text: '—', error: false }
  if (e.status === 504 && e.upstreamHeaderTime == null) return { text: 'timeout', error: true }
  if (e.status === 502 && e.upstreamConnectTime == null) return { text: 'refused', error: true }
  if (e.status === 502 && e.upstreamHeaderTime == null) return { text: 'reset', error: true }
  const t = e.upstreamResponseTime ?? e.upstreamHeaderTime
  return { text: t != null ? ms(t * 1000) : '—', error: false }
}

export function requestLabel(e: AccessEntry): string {
  if (e.kind === 'stream') {
    const port = e.extra?.server_port
    return `${e.protocol || 'TCP'}${port ? ` :${port}` : ''} · session ${ms(e.requestTime * 1000)}`
  }
  const base = `${e.method} ${e.path}`.trim()
  return e.status === 101 ? `${base} · websocket upgrade` : base
}

export function sizeLabel(e: AccessEntry): string {
  return e.status === 101 ? '—' : bytes(e.bytesSent)
}

const pad = (n: number) => String(n).padStart(2, '0')

/** "14:03:41" today, "09-12 14:03" otherwise. */
export function timeCell(iso: string): string {
  const d = new Date(iso)
  if (d.toDateString() === new Date().toDateString()) return clock(iso)
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}

export function shortRequestId(id: string): string {
  return id.length > 12 ? `${id.slice(0, 4)}…${id.slice(-4)}` : id
}

export function tickLabel(iso: string, stepSeconds: number): string {
  const d = new Date(iso)
  const hm = `${pad(d.getHours())}:${pad(d.getMinutes())}`
  if (stepSeconds >= 3600) return `${d.toLocaleDateString([], { weekday: 'short' })} ${hm}`
  return hm
}

/** Starts a file download from an API URL (same-origin cookies apply). */
export function download(url: string) {
  const a = document.createElement('a')
  a.href = url
  a.rel = 'noopener'
  document.body.appendChild(a)
  a.click()
  a.remove()
}

/** True when a key event comes from a text field or control. */
export function isTypingTarget(target: EventTarget | null): boolean {
  const t = target as HTMLElement | null
  if (!t) return false
  return t.isContentEditable || ['INPUT', 'TEXTAREA', 'SELECT', 'BUTTON'].includes(t.tagName)
}
