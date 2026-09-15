// Engine image updates (slice engine): version check + in-place upgrade.
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../../lib/api'
import { useBusEvent } from '../../lib/events'
import type { EngineUpdateInfo, EngineUpdates, UpgradeJob } from '../../lib/types'

export type EngineName = 'nginx' | 'haproxy'
export const engineTitle: Record<EngineName, string> = { nginx: 'nginx', haproxy: 'HAProxy' }

export const updatesKey = ['engines', 'updates'] as const
export const upgradeKey = ['engines', 'upgrade-status'] as const

/** Cached version info (no Docker Hub request); refreshed by bus events. */
export function useEngineUpdates(enabled = true) {
  return useQuery({
    queryKey: updatesKey,
    queryFn: () => api.get<EngineUpdates>('/api/engines/updates'),
    enabled,
    refetchInterval: 60_000,
    staleTime: 15_000,
  })
}

/** Current / last upgrade job, kept live via the engine.upgrade topic. */
export function useUpgradeJob() {
  const qc = useQueryClient()
  const q = useQuery({
    queryKey: upgradeKey,
    queryFn: () => api.get<{ job: UpgradeJob | null }>('/api/engines/upgrade-status').then((r) => r.job),
    refetchInterval: (query) => (query.state.data?.status === 'running' ? 3_000 : false),
  })
  useBusEvent<UpgradeJob>('engine.upgrade', (ev) => {
    if (ev.data) qc.setQueryData(upgradeKey, ev.data)
  })
  return q
}

/** True when either engine has an update or a drift worth a badge. */
export function hasEngineNotice(u: EngineUpdates | undefined): boolean {
  return !!u && [u.nginx, u.haproxy].some((e) => e.updateAvailable || !!e.drift)
}

export function currentVersion(e: EngineUpdateInfo): string {
  return e.version || e.imageVersion || ''
}

/** -1 a<b, 0 equal, 1 a>b for x.y.z versions. */
export function compareVersions(a: string, b: string): number {
  const pa = a.split('.').map((x) => parseInt(x, 10) || 0)
  const pb = b.split('.').map((x) => parseInt(x, 10) || 0)
  for (let i = 0; i < 3; i++) {
    if ((pa[i] ?? 0) !== (pb[i] ?? 0)) return (pa[i] ?? 0) < (pb[i] ?? 0) ? -1 : 1
  }
  return 0
}

export const channelLabels: Record<EngineName, { value: string; label: string; hint: string }[]> = {
  nginx: [
    { value: 'stable', label: 'Stable', hint: 'Even minor versions (1.30.x) · recommended' },
    { value: 'mainline', label: 'Mainline', hint: 'Odd minor versions (1.31.x) · newest features' },
  ],
  haproxy: [
    { value: 'lts', label: 'LTS', hint: 'Long-term supported branches (even minor)' },
    { value: 'latest', label: 'Latest', hint: 'Newest release of any branch' },
  ],
}
