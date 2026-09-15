import { useQuery } from '@tanstack/react-query'
import { api } from '../../lib/api'
import type { AccessList } from '../../lib/types'
import { ruleSummary } from './RuleEditor'

export interface AccessUsage {
  hosts: { id: string; domain: string; via: string }[]
  other: string[]
}

export function useAccessUsage() {
  return useQuery({
    // under ['entities','access-lists'] so saves invalidate it
    queryKey: ['entities', 'access-lists', '__usage'],
    queryFn: () => api.get<Record<string, AccessUsage>>('/api/access-lists/usage'),
    staleTime: 10_000,
  })
}

export interface AccessDecision {
  allowed: boolean
  requiresAuth: boolean
  matchedRule: { id: string; action: 'allow' | 'deny'; cidr: string; note: string } | null
  explanation: string
}

export interface Denial { id: number; ts: string; hostId: string; host: string; method: string; path: string; status: number; clientIp: string }

/** "Allow 2 CIDRs · deny all", "Basic auth · 4 users", "Allow 1 CIDR + auth · 1 user" */
export function listSummary(l: AccessList): string {
  const rules = ruleSummary(l.rules)
  const users = l.basicAuth.users.length
  const auth = l.basicAuth.enabled ? `${users} ${users === 1 ? 'user' : 'users'}` : ''
  if (rules && auth) return `${rules} + auth · ${auth}`
  if (auth) return `Basic auth · ${auth}`
  if (rules) return rules
  return 'No restrictions'
}

export function hostCount(u?: AccessUsage) {
  return u ? new Set(u.hosts.map((h) => h.id)).size : 0
}
