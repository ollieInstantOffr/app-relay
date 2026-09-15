// Engine slice data layer: versions, diffs, engine logs and the shared apply runner.
import { useCallback, useEffect, useRef, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { api, ApiError } from '../../lib/api'
import { keys } from '../../lib/queries'
import { useBusEvent } from '../../lib/events'
import { ms } from '../../lib/format'
import { useToast } from '../../components/ui'
import type { Pending, Version } from '../../lib/types'

export interface VersionInfo extends Version {
  failedEngine?: EngineName
  failedStage?: string
  output?: string
  nginxHash: string
  /** Proxy engine this version was rendered for (older versions: absent = nginx). */
  proxyEngine?: 'nginx' | 'edge'
  proxyHash?: string
  haproxyHash: string
  haproxyRunning: boolean
}

export interface DiffLine { type: 'add' | 'del' | 'ctx' | 'hunk'; text: string; oldNo?: number; newNo?: number }
export interface FileDiff { path: string; status: 'added' | 'removed' | 'modified'; added: number; removed: number; lines: DiffLine[] }
export interface DiffResponse {
  version: number
  against: number
  files: FileDiff[]
  paths: string[]
  validateMs: number
  reloadMs: number
  error?: string
}

export interface PendingInfo extends Pending { summary?: string }

export interface ApplyProgress { version: number; stage: string; message: string; progress: number }
export interface ApplyFinished { version: number; status: string; error: string; engine?: string; stage?: string }

export interface EngineLogLine { at: string; stream: 'stdout' | 'stderr'; text: string }
export interface EngineListener { proto: 'tcp' | 'udp'; address: string; port: number; process?: string }

export type EngineName = 'nginx' | 'haproxy' | 'edge'
export const engineLabel: Record<EngineName, string> = { nginx: 'nginx', haproxy: 'HAProxy', edge: 'Relay Edge' }

export function useVersions(limit = 100) {
  return useQuery({
    queryKey: [...keys.versions, 'list', limit],
    queryFn: () => api.get<VersionInfo[]>(`/api/versions?limit=${limit}`),
    refetchInterval: 60_000,
  })
}

export function useVersionDiff(id: number | undefined, against: number | undefined) {
  return useQuery({
    queryKey: [...keys.versions, 'diff', id, against ?? 'prev'],
    queryFn: () => api.get<DiffResponse>(`/api/versions/${id}/diff${against !== undefined ? `?against=${against}` : ''}`),
    enabled: id !== undefined,
    staleTime: Infinity,
  })
}

/** Rendered draft vs live (lives under the pending key so it refreshes with pending changes). */
export function usePendingDiff(enabled: boolean) {
  return useQuery({
    queryKey: [...keys.pending, 'diff'],
    queryFn: () => api.get<DiffResponse>('/api/pending/diff'),
    enabled,
  })
}

export function useEngineLogs(engine: EngineName | null) {
  return useQuery({
    queryKey: [...keys.engines, 'logs', engine],
    queryFn: () => api.get<{ lines: EngineLogLine[] }>(`/api/engines/${engine}/logs?limit=500`),
    enabled: !!engine,
    refetchInterval: 5_000,
  })
}

export function useEngineListeners(engine: EngineName | null, enabled: boolean) {
  return useQuery({
    queryKey: [...keys.engines, 'listeners', engine],
    queryFn: () => api.get<{ listeners: EngineListener[] }>(`/api/engines/${engine}/listeners`),
    enabled: !!engine && enabled,
  })
}

/** Ticks every `interval` ms so relative times stay fresh. */
export function useNow(interval = 1000) {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const t = window.setInterval(() => setNow(Date.now()), interval)
    return () => window.clearInterval(t)
  }, [interval])
  return now
}

export function secondsAgo(iso: string | undefined, now: number): string {
  if (!iso) return '—'
  const s = Math.max(0, Math.round((now - new Date(iso).getTime()) / 1000))
  if (s < 60) return `${s} s ago`
  if (s < 3600) return `${Math.round(s / 60)} min ago`
  if (s < 86400) return `${Math.round(s / 3600)} h ago`
  return `${Math.round(s / 86400)} days ago`
}

/** "Applying v3… · validated · reloading Relay Edge" → "Validated · reloading Relay Edge" */
function stageMessage(m: string): string {
  const s = m.replace(/^Applying v\d+…\s*·\s*/, '')
  return s.charAt(0).toUpperCase() + s.slice(1)
}

export function firstErrorLine(output: string | undefined): string {
  if (!output) return ''
  const lines = output.split('\n').map((l) => l.trim()).filter(Boolean)
  return lines.find((l) => /\[(emerg|ALERT|alert|crit|error)\]/.test(l)) ?? lines[0] ?? ''
}

const APPLY_TOAST = 'relay-apply'

/**
 * Runs an apply-style request (POST /api/apply or a version rollback) with the
 * progress toast from design 23 and the success / auto-rollback toasts from 17.
 */
export function useApplyRunner() {
  const toast = useToast()
  const navigate = useNavigate()
  const qc = useQueryClient()
  const running = useRef(false)
  const [busy, setBusy] = useState(false)

  useBusEvent<ApplyProgress>('apply.progress', (ev) => {
    if (!running.current || !ev.data) return
    toast.show({
      id: APPLY_TOAST,
      kind: 'progress',
      title: `Applying v${ev.data.version}…`,
      message: stageMessage(ev.data.message),
      progress: ev.data.progress,
    })
  })

  const run = useCallback(
    async (path: string, body: unknown = {}) => {
      if (running.current) return undefined
      running.current = true
      setBusy(true)
      toast.show({ id: APPLY_TOAST, kind: 'progress', title: 'Applying…', message: 'Rendering config', progress: 2 })
      try {
        const v = await api.post<VersionInfo>(path, body)
        toast.dismiss(APPLY_TOAST)
        if (v.status === 'live') {
          toast.show({
            kind: 'success',
            title: `v${v.id} is live`,
            message: `${v.summary} · validated in ${ms(v.validateMs)} · reloaded in ${ms(v.reloadMs)}`,
          })
        } else {
          toast.show({
            kind: 'warning',
            title: 'Change rolled back automatically',
            message: v.error,
            duration: 0,
            actions: [
              { label: 'Open draft', onClick: () => navigate('/history?pending=1') },
              { label: 'View logs', onClick: () => navigate('/logs/error') },
            ],
          })
        }
        return v
      } catch (err) {
        toast.dismiss(APPLY_TOAST)
        if (err instanceof ApiError && err.code === 'no_changes') {
          toast.show({ kind: 'info', title: 'Nothing to apply', message: err.message })
        } else if (err instanceof ApiError && err.code === 'validation_failed') {
          toast.show({
            kind: 'error',
            title: 'Validation failed · nothing was reloaded',
            message: err.message,
            actions: [{ label: 'Review diff', onClick: () => navigate('/history?pending=1') }],
          })
        } else if (err instanceof ApiError && err.code === 'engine_unavailable') {
          toast.show({ kind: 'error', title: 'Engine unreachable', message: err.message })
        } else {
          toast.error(err, 'Apply failed')
        }
        return undefined
      } finally {
        running.current = false
        setBusy(false)
        qc.invalidateQueries({ queryKey: keys.pending })
        qc.invalidateQueries({ queryKey: keys.versions })
        qc.invalidateQueries({ queryKey: keys.engines })
        qc.invalidateQueries({ queryKey: ['entities'] })
        qc.invalidateQueries({ queryKey: ['settings'] })
      }
    },
    [toast, navigate, qc],
  )

  return { run, busy }
}
