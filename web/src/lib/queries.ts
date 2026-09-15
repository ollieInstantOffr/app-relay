// Shared TanStack Query hooks for entities, settings and cross-slice resources.
import { useMutation, useQuery, useQueryClient, type UseQueryOptions } from '@tanstack/react-query'
import { api } from './api'
import { proxyEngineLabel } from './types'
import type {
  Approval, Container, DirectoryUser, EngineKind, EngineState, EnginesStatus, EntityKind, EntityMap, HealthStatus, LBStats, Pending, Session,
  ProxyEngineName, SettingsKey, SettingsMap,
} from './types'

export const keys = {
  session: ['session'] as const,
  entities: (kind: EntityKind) => ['entities', kind] as const,
  entity: (kind: EntityKind, id: string) => ['entities', kind, id] as const,
  settings: (key: SettingsKey) => ['settings', key] as const,
  pending: ['pending'] as const,
  versions: ['versions'] as const,
  engines: ['engines'] as const,
  health: ['health'] as const,
  lbStats: ['lb', 'stats'] as const,
  containers: ['docker', 'containers'] as const,
  approvals: ['approvals'] as const,
  userDirectory: ['auth', 'users', 'directory'] as const,
}

// ---------------------------------------------------------------- session
export function useSession() {
  return useQuery({ queryKey: keys.session, queryFn: () => api.get<Session>('/api/auth/session'), staleTime: 60_000 })
}

/** Current role helpers. */
export function useRole() {
  const { data } = useSession()
  const role = data?.user?.role
  return { role, isAdmin: role === 'admin', canWrite: role === 'admin' || role === 'editor' }
}

/** Relay users for pickers (e.g. who may use Relay login); readable by every signed-in user. */
export function useUserDirectory(enabled = true) {
  return useQuery({ queryKey: keys.userDirectory, queryFn: () => api.get<DirectoryUser[]>('/api/users/directory'), enabled, staleTime: 30_000 })
}

// ---------------------------------------------------------------- entities
export function useEntities<K extends EntityKind>(kind: K, opts?: Partial<UseQueryOptions<EntityMap[K][]>>) {
  return useQuery({
    queryKey: keys.entities(kind),
    queryFn: () => api.get<EntityMap[K][]>(`/api/${kind}`),
    ...opts,
  })
}

export function useEntity<K extends EntityKind>(kind: K, id: string | undefined) {
  return useQuery({
    queryKey: keys.entity(kind, id ?? ''),
    queryFn: () => api.get<EntityMap[K]>(`/api/${kind}/${id}`),
    enabled: !!id,
  })
}

/** Create (no id) or update (with id) an entity. */
export function useSaveEntity<K extends EntityKind>(kind: K) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: Partial<EntityMap[K]>) =>
      v.id ? api.put<EntityMap[K]>(`/api/${kind}/${v.id}`, v) : api.post<EntityMap[K]>(`/api/${kind}`, v),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['entities', kind] })
      qc.invalidateQueries({ queryKey: keys.pending })
    },
  })
}

export function useDeleteEntity(kind: EntityKind) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => api.del(`/api/${kind}/${id}`),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['entities', kind] })
      qc.invalidateQueries({ queryKey: keys.pending })
    },
  })
}

// ---------------------------------------------------------------- settings
export function useSettings<K extends SettingsKey>(key: K) {
  return useQuery({ queryKey: keys.settings(key), queryFn: () => api.get<SettingsMap[K]>(`/api/settings/${key}`) })
}

export function useSaveSettings<K extends SettingsKey>(key: K) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: SettingsMap[K]) => api.put<SettingsMap[K]>(`/api/settings/${key}`, v),
    onSuccess: (data) => {
      qc.setQueryData(keys.settings(key), data)
      qc.invalidateQueries({ queryKey: keys.pending })
    },
  })
}

// ---------------------------------------------------------------- cross-slice resources
export function usePending() {
  return useQuery({ queryKey: keys.pending, queryFn: () => api.get<Pending>('/api/pending'), refetchInterval: 30_000 })
}

export function useEngines() {
  return useQuery({ queryKey: keys.engines, queryFn: () => api.get<EnginesStatus>('/api/engines'), refetchInterval: 10_000 })
}

export function useEngine(engine: EngineKind): EngineState | undefined {
  return useEngines().data?.[engine]
}

/** The proxy engine chosen in Settings → Proxy engine (used from the next apply). */
export function useSelectedProxyEngine(): ProxyEngineName {
  return useSettings('general').data?.proxyEngine === 'edge' ? 'edge' : 'nginx'
}

/** The active reverse proxy engine (nginx or Relay Edge) and its state. */
export function useProxyEngine(): { engine: ProxyEngineName; label: string; state: EngineState | undefined; loaded: boolean } {
  const { data } = useEngines()
  const engine: ProxyEngineName = data?.proxy === 'edge' ? 'edge' : 'nginx'
  return { engine, label: proxyEngineLabel[engine], state: data?.[engine], loaded: !!data }
}

export function useHealth() {
  return useQuery({ queryKey: keys.health, queryFn: () => api.get<Record<string, HealthStatus>>('/api/health'), refetchInterval: 15_000 })
}

export function useLBStats(refetchMs = 5_000) {
  return useQuery({ queryKey: keys.lbStats, queryFn: () => api.get<LBStats>('/api/lb/stats'), refetchInterval: refetchMs })
}

export function useContainers(enabled = true) {
  return useQuery({ queryKey: keys.containers, queryFn: () => api.get<Container[]>('/api/docker/containers'), enabled, staleTime: 15_000 })
}

export function usePendingApprovals() {
  return useQuery({
    queryKey: [...keys.approvals, 'pending'],
    queryFn: () => api.get<Approval[]>('/api/approvals?status=pending'),
    refetchInterval: 20_000,
  })
}
