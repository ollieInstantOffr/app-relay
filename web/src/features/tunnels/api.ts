// Tunnels: gateways on a public server that this Relay dials out to, so hosts
// and TCP streams can be published without port forwarding.
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../../lib/api'
import { keys } from '../../lib/queries'
import type { Gateway, GatewayPairing, GatewayStatus, TunnelOverview } from '../../lib/types'

export const tunnelKeys = { overview: ['tunnels'] as const }

export function useTunnels(refetchMs = 5_000) {
  return useQuery({ queryKey: tunnelKeys.overview, queryFn: () => api.get<TunnelOverview>('/api/tunnels'), refetchInterval: refetchMs })
}

export type GatewayView = TunnelOverview['gateways'][number]

export function useInvalidateTunnels() {
  const qc = useQueryClient()
  return () => {
    qc.invalidateQueries({ queryKey: tunnelKeys.overview })
    qc.invalidateQueries({ queryKey: keys.entities('gateways') })
  }
}

export const startPairing = (id: string) => api.post<GatewayPairing>(`/api/gateways/${id}/pairing`)
export const pairGateway = (id: string) => api.post<Gateway>(`/api/gateways/${id}/pair`)

export type Tone = 'ok' | 'warn' | 'danger' | 'muted'

/** How a gateway's connection reads in the UI. */
export function gatewayState(g: Gateway, status: GatewayStatus | null | undefined, engineRunning: boolean): { tone: Tone; label: string; detail?: string } {
  if (!g.enabled) return { tone: 'muted', label: 'disabled' }
  if (g.pairState !== 'paired') return { tone: 'warn', label: 'waiting for pairing' }
  if (!engineRunning || !status) return { tone: 'muted', label: 'idle', detail: 'Publish a host or stream through it to connect' }
  switch (status.state) {
    case 'connected':
      return { tone: 'ok', label: 'connected' }
    case 'connecting':
      return { tone: 'warn', label: 'connecting', detail: status.lastError }
    case 'disconnected':
      return { tone: 'danger', label: 'disconnected', detail: status.lastError }
    case 'incompatible':
      return { tone: 'danger', label: 'version mismatch', detail: status.lastError || 'Update the gateway to this Relay version' }
    case 'idle':
      return { tone: 'muted', label: 'idle', detail: 'Nothing is published through it' }
    case 'unpaired':
      return { tone: 'warn', label: 'waiting for pairing' }
    default:
      return { tone: 'muted', label: status.state }
  }
}

export const transportLabel: Record<string, string> = { auto: 'Auto', quic: 'QUIC', tcp: 'TCP' }

/** Shortens a "sha256:…" fingerprint for display. */
export function shortFingerprint(fp?: string): string {
  if (!fp) return '—'
  const v = fp.replace(/^sha256:/, '')
  return `${v.slice(0, 8)}…${v.slice(-6)}`
}

export const newGatewayDraft = (): Partial<Gateway> => ({ name: '', address: '', transport: 'auto', enabled: true })

/** Future ISO time → "in 23 h", "in 5 min". */
export function untilLabel(iso?: string): string {
  if (!iso) return '—'
  const s = (new Date(iso).getTime() - Date.now()) / 1000
  if (s <= 0) return 'expired'
  if (s < 3600) return `in ${Math.max(1, Math.round(s / 60))} min`
  return `in ${Math.round(s / 3600)} h`
}

/** The tunnel gateway docs link. */
export const tunnelsDocs = '/docs/tunnels'
