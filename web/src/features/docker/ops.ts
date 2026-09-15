// Owner: slice ops. Types and helpers shared by the Docker, Notifications and
// Backup settings screens.
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../../lib/api'
import { Topics, useBusEvent } from '../../lib/events'
import type { ToastAction } from '../../components/ui'

// ---------------------------------------------------------------- docker
export interface DockerStatus {
  enabled: boolean
  connected: boolean
  endpoint: string
  version: string
  apiVersion?: string
  containers: number
  error?: string
  socketMissing?: boolean
  checkedAt?: string
}

export const opsKeys = {
  dockerStatus: ['docker', 'status'] as const,
  backups: ['backups'] as const,
  notificationLog: ['notifications', 'log'] as const,
}

export function useDockerStatus(enabled = true) {
  const qc = useQueryClient()
  useBusEvent(Topics.DockerChanged, () => {
    qc.invalidateQueries({ queryKey: ['docker'] })
  })
  return useQuery({
    queryKey: opsKeys.dockerStatus,
    queryFn: () => api.get<DockerStatus>('/api/docker/status'),
    refetchInterval: 20_000,
    enabled,
  })
}

export interface CreateHostsResult {
  created: { containerId: string; hostId: string; domain: string }[]
  errors: { containerId: string; domain: string; error: string; fields?: Record<string, string> }[]
}

export function schemeForPort(port: number): 'http' | 'https' {
  return port === 443 || port === 8443 || port === 9443 ? 'https' : 'http'
}

export function domainFromPattern(pattern: string, name: string): string {
  const clean = name.toLowerCase().replace(/[^a-z0-9-]+/g, '-').replace(/^-+|-+$/g, '')
  return (pattern || '{name}.home.lan').replaceAll('{name}', clean)
}

/** True when a certificate name list covers domain (exact or one-level wildcard). */
export function certCovers(names: string[], domain: string): boolean {
  const d = domain.toLowerCase()
  return names.some((n) => {
    const name = n.toLowerCase()
    if (name === d) return true
    if (name.startsWith('*.')) {
      const suffix = name.slice(1)
      return d.endsWith(suffix) && !d.slice(0, -suffix.length).includes('.')
    }
    return false
  })
}

// ---------------------------------------------------------------- backups
export interface BackupRow {
  id: string
  createdAt: string
  size: number
  contents: Record<string, number>
  trigger: 'scheduled' | 'manual' | 'before-upgrade' | 'before-restore' | string
  file: string
  status: 'ok' | 'failed' | 'running'
  error?: string
}

export interface BackupStatus {
  enabled: boolean
  time: string
  keep: number
  nextRunAt?: string
  lastRun?: BackupRow
  warning?: string
  destination: { kind: 'local'; path: string; freeBytes?: number; writable: boolean }
  timezone: string
}

export interface BackupsResponse {
  items: BackupRow[]
  status: BackupStatus
}

export interface RestoreResult {
  manifest: { format: number; version: string; created: string; counts: Record<string, number>; tables: string[]; privateKeys: boolean }
  beforeRestoreBackupId: string
  sessionKept: boolean
  reviewPending: boolean
  restoredTables: number
}

export function useBackups() {
  return useQuery({
    queryKey: opsKeys.backups,
    queryFn: () => api.get<BackupsResponse>('/api/backups'),
    refetchInterval: (q) => (q.state.data?.items.some((b) => b.status === 'running') ? 1500 : 60_000),
  })
}

// ---------------------------------------------------------------- notifications
export interface NotificationLogRow {
  id: number
  at: string
  event: string
  channelId: string
  status: 'sent' | 'failed' | 'queued'
  title: string
  error?: string
}

export function useNotificationLog() {
  return useQuery({
    queryKey: opsKeys.notificationLog,
    queryFn: () => api.get<NotificationLogRow[]>('/api/notifications/log?limit=300'),
    refetchInterval: 30_000,
  })
}

// ---------------------------------------------------------------- NPM import
export type NpmKind = 'hosts' | 'redirects' | 'streams' | 'accessLists' | 'certificates'

export interface NpmPreviewItem {
  kind: NpmKind
  npmId: number
  name: string
  detail: string
  conflict?: string
  warnings: string[]
}

export interface NpmPreview {
  token: string
  source: string
  expiresAt: string
  counts: Record<NpmKind, { total: number; conflicts: number; warnings: number }>
  items: NpmPreviewItem[]
  warnings: string[]
}

export interface NpmCommitResult {
  created: Partial<Record<NpmKind, number>>
  updated: Partial<Record<NpmKind, number>>
  skipped: Partial<Record<NpmKind, number>>
  errors: string[]
}

// ---------------------------------------------------------------- misc
export const applyNowAction: ToastAction = {
  label: 'Apply now',
  primary: true,
  onClick: () => window.dispatchEvent(new CustomEvent('relay:apply')),
}

/** ISO time in the future → "in 12 h", "in 5 min". */
export function until(iso: string | undefined | null, now = Date.now()): string {
  if (!iso) return '—'
  const s = Math.round((new Date(iso).getTime() - now) / 1000)
  if (s <= 60) return 'in a moment'
  if (s < 3600) return `in ${Math.round(s / 60)} min`
  if (s < 86400) return `in ${Math.round(s / 3600)} h`
  return `in ${Math.round(s / 86400)} days`
}
