// Data hooks and helpers for the MCP slice (approvals, tool calls, tokens, server info).
import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../../lib/api'
import { keys } from '../../lib/queries'
import type { ApiToken, Approval, AuditRow, ToolPermission } from '../../lib/types'

/** Approval as returned by /api/approvals (with the short target label). */
export type ApprovalItem = Approval & { target?: string }
/** POST /api/approvals/{id}/approve|deny response. */
export type Decision = ApprovalItem & { failed: boolean }

export interface MCPInfo {
  endpoint: string
  enabled: boolean
  transports: ('http' | 'stdio')[]
  toolCount: number
  connectedSessions: number
  stdioCommand: string
}

export interface MCPTool {
  name: string
  title: string
  description: string
  kind: 'read' | 'write'
  defaultPermission: ToolPermission
  permission: ToolPermission
}

export const mcpKeys = {
  approvalsAll: [...keys.approvals, 'all'] as const,
  info: ['mcp', 'info'] as const,
  tools: ['mcp', 'tools'] as const,
  // under "audit" so audit.appended events refresh it
  calls: ['audit', 'mcp-calls'] as const,
  tokens: ['tokens', 'mcp'] as const,
}

export function useApprovalHistory(enabled = true) {
  return useQuery({
    queryKey: mcpKeys.approvalsAll,
    queryFn: () => api.get<ApprovalItem[]>('/api/approvals?status=all&limit=30'),
    enabled,
  })
}

export function useDecideApproval() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ id, approve }: { id: string; approve: boolean }) =>
      api.post<Decision>(`/api/approvals/${encodeURIComponent(id)}/${approve ? 'approve' : 'deny'}`),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: keys.approvals })
      qc.invalidateQueries({ queryKey: keys.pending })
      qc.invalidateQueries({ queryKey: mcpKeys.calls })
    },
  })
}

export function useMCPInfo() {
  return useQuery({ queryKey: mcpKeys.info, queryFn: () => api.get<MCPInfo>('/api/mcp/info'), refetchInterval: 30_000 })
}

export function useMCPTools() {
  return useQuery({ queryKey: mcpKeys.tools, queryFn: () => api.get<MCPTool[]>('/api/mcp/tools'), staleTime: 60_000 })
}

export function useMCPCalls(limit = 8) {
  return useQuery({ queryKey: [...mcpKeys.calls, limit], queryFn: () => api.get<AuditRow[]>(`/api/mcp/calls?limit=${limit}`) })
}

export function useMCPTokens() {
  return useQuery({ queryKey: mcpKeys.tokens, queryFn: () => api.get<ApiToken[]>('/api/tokens?surface=mcp'), retry: false })
}

export function useDeleteToken() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => api.del(`/api/tokens/${encodeURIComponent(id)}`),
    onSettled: () => qc.invalidateQueries({ queryKey: ['tokens'] }),
  })
}

/** Re-renders every `ms` for countdowns. */
export function useNow(ms = 1000): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const t = window.setInterval(() => setNow(Date.now()), ms)
    return () => window.clearInterval(t)
  }, [ms])
  return now
}

/** "expires in 9 min", "expires in 40 s", "expired" */
export function expiresIn(iso: string, now = Date.now()): string {
  const s = Math.round((new Date(iso).getTime() - now) / 1000)
  if (!Number.isFinite(s) || s <= 0) return 'expired'
  if (s < 60) return `expires in ${s} s`
  return `expires in ${Math.ceil(s / 60)} min`
}

const verbs: Record<string, string> = {
  create_host: 'create a host',
  update_host: 'update a host',
  delete_host: 'delete a host',
  drain_server: 'drain a server',
  request_certificate: 'request a certificate',
  apply_changes: 'apply pending changes',
}

/** "drain a server" — the approval toast's verb phrase. */
export function approvalVerb(a: Pick<ApprovalItem, 'tool' | 'args'>): string {
  if (a.tool === 'drain_server') {
    const state = (a.args as { state?: string } | undefined)?.state
    if (state === 'maint') return 'put a server into maintenance'
    if (state === 'ready') return 'put a server back into rotation'
  }
  return verbs[a.tool] ?? `run ${a.tool}`
}

/** Result text colour class for audit results (ok / confirmed / denied …). */
export function resultClass(result: string): string {
  switch (result) {
    case 'ok':
    case 'applied':
    case 'saved':
      return 'ok-text'
    case 'confirmed':
    case 'pending':
      return 'warn-text'
    case 'denied':
    case 'failed':
    case 'blocked':
      return 'danger-text'
  }
  return 'faint'
}
