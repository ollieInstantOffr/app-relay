// Live updates over SSE (/api/events). One connection for the app; features
// subscribe with useBusEvent. Query invalidation for common topics is built in.
import { useEffect, useRef } from 'react'
import { useQueryClient, type QueryClient } from '@tanstack/react-query'
import type { BusEvent } from './types'
import { keys } from './queries'

type Listener = (ev: BusEvent) => void
const listeners = new Set<Listener>()

export const Topics = {
  ConfigChanged: 'config.changed',
  PendingChanged: 'pending.changed',
  ApplyProgress: 'apply.progress',
  ApplyFinished: 'apply.finished',
  HealthChanged: 'health.changed',
  CertChanged: 'cert.changed',
  AccessLog: 'log.access',
  ErrorLog: 'log.error',
  AuditAppended: 'audit.appended',
  ApprovalChanged: 'approval.changed',
  EngineChanged: 'engine.changed',
  DockerChanged: 'docker.changed',
  BackupChanged: 'backup.changed',
  ActivityAppended: 'activity.appended',
  EngineUpgrade: 'engine.upgrade',
  EngineUpdates: 'engine.updates',
  RelayUpdate: 'relay.update',
  TunnelChanged: 'tunnel.changed',
} as const

function invalidateFor(qc: QueryClient, ev: BusEvent) {
  switch (ev.topic) {
    case Topics.ConfigChanged:
    case Topics.PendingChanged:
      qc.invalidateQueries({ queryKey: keys.pending })
      break
    case Topics.ApplyFinished:
      qc.invalidateQueries({ queryKey: keys.pending })
      qc.invalidateQueries({ queryKey: keys.versions })
      qc.invalidateQueries({ queryKey: keys.engines })
      qc.invalidateQueries({ queryKey: ['entities'] })
      qc.invalidateQueries({ queryKey: ['dns'] })
      break
    case Topics.HealthChanged:
      qc.invalidateQueries({ queryKey: keys.health })
      break
    case Topics.CertChanged:
      qc.invalidateQueries({ queryKey: keys.entities('certificates') })
      break
    case Topics.ApprovalChanged:
      qc.invalidateQueries({ queryKey: keys.approvals })
      break
    case Topics.EngineChanged:
      qc.invalidateQueries({ queryKey: keys.engines })
      break
    case Topics.EngineUpdates:
      qc.invalidateQueries({ queryKey: ['engines', 'updates'] })
      break
    case Topics.DockerChanged:
      qc.invalidateQueries({ queryKey: keys.containers })
      break
    case Topics.AuditAppended:
      qc.invalidateQueries({ queryKey: ['audit'] })
      break
    case Topics.ActivityAppended:
      qc.invalidateQueries({ queryKey: ['activity'] })
      break
    case Topics.BackupChanged:
      qc.invalidateQueries({ queryKey: ['backups'] })
      break
    case Topics.TunnelChanged:
      qc.invalidateQueries({ queryKey: ['tunnels'] })
      qc.invalidateQueries({ queryKey: keys.entities('gateways') })
      break
  }
}

/** Mount once (in the app shell). */
export function useEventStream(enabled: boolean) {
  const qc = useQueryClient()
  useEffect(() => {
    if (!enabled) return
    const es = new EventSource('/api/events')
    es.onmessage = (msg) => {
      try {
        const ev = JSON.parse(msg.data) as BusEvent
        invalidateFor(qc, ev)
        listeners.forEach((l) => l(ev))
      } catch {
        /* ignore */
      }
    }
    return () => es.close()
  }, [enabled, qc])
}

/** Subscribe to bus events matching a topic (or topic prefix ending in "."). */
export function useBusEvent<T = unknown>(topic: string, handler: (ev: BusEvent<T>) => void) {
  const ref = useRef(handler)
  ref.current = handler
  useEffect(() => {
    const l: Listener = (ev) => {
      if (ev.topic === topic || (topic.endsWith('.') && ev.topic.startsWith(topic))) ref.current(ev as BusEvent<T>)
    }
    listeners.add(l)
    return () => {
      listeners.delete(l)
    }
  }, [topic])
}

/**
 * Opens a dedicated SSE stream for high-volume topics (e.g. live log tail).
 * Returns nothing; handler receives each event.
 */
export function useTopicStream<T = unknown>(topics: string, enabled: boolean, handler: (ev: BusEvent<T>) => void) {
  const ref = useRef(handler)
  ref.current = handler
  useEffect(() => {
    if (!enabled) return
    const es = new EventSource(`/api/events?topics=${encodeURIComponent(topics)}`)
    es.onmessage = (msg) => {
      try {
        ref.current(JSON.parse(msg.data))
      } catch {
        /* ignore */
      }
    }
    return () => es.close()
  }, [topics, enabled])
}
