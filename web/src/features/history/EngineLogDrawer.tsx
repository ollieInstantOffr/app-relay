import { useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Button, Callout, Card, Drawer, EmptyState, Skeleton, cx, useToast } from '../../components/ui'
import { api, errorMessage } from '../../lib/api'
import { keys, useEngine, useRole } from '../../lib/queries'
import { clock } from '../../lib/format'
import { engineLabel, useEngineListeners, useEngineLogs, type EngineName } from './api'

interface ActionResponse { ok: boolean; output: string }

/** Parses the port from bind errors (nginx bind(), HAProxy ALERT, Relay Edge "listen tcp :80: bind: …"). */
export function bindFailurePort(text: string): number | undefined {
  const m =
    text.match(/bind\(\) to \S*?:(\d{1,5}) failed/) ??
    text.match(/listen (?:tcp|udp)[46]?\s+\S*?:(\d{1,5}): bind: address already in use/i) ??
    text.match(/cannot bind (?:socket|UDP socket)[^[]*\[[^\]]*:(\d{1,5})\]/i) ??
    text.match(/Address already in use\)?\s*\[[^\]]*:(\d{1,5})\]/)
  return m ? Number(m[1]) : undefined
}

function lineClass(t: string) {
  if (/\[(emerg|alert|crit|error|ALERT)\]/.test(t)) return 'err'
  if (/\[(warn|WARNING)\]/.test(t)) return 'warn'
  return undefined
}

export default function EngineLogDrawer({ engine, onClose }: { engine: EngineName | null; onClose: () => void }) {
  const [showListeners, setShowListeners] = useState(false)
  const logs = useEngineLogs(engine)
  const listeners = useEngineListeners(engine, showListeners)
  const state = useEngine(engine ?? 'nginx')
  const { canWrite } = useRole()
  const toast = useToast()
  const qc = useQueryClient()
  const [starting, setStarting] = useState(false)
  if (!engine) return null
  const label = engineLabel[engine]
  const lines = logs.data?.lines ?? []
  const errorLine = [...lines].reverse().find((l) => bindFailurePort(l.text) !== undefined)
  const port = errorLine ? bindFailurePort(errorLine.text) : undefined

  const start = async () => {
    setStarting(true)
    try {
      const r = await api.post<ActionResponse>(`/api/engines/${engine}/start`)
      if (r.ok) toast.success(`${label} started`)
      else toast.show({ kind: 'error', title: `${label} failed to start`, message: r.output })
    } catch (err) {
      toast.error(err, `${label} failed to start`)
    } finally {
      setStarting(false)
      qc.invalidateQueries({ queryKey: keys.engines })
    }
  }

  return (
    <Drawer
      open
      onClose={onClose}
      width="wide"
      title={`${label} error log`}
      subtitle={`Engine output captured by the relay-${engine} agent · newest last`}
      footer={
        <>
          <Button onClick={() => setShowListeners((v) => !v)}>{showListeners ? 'Hide listeners' : 'Show listeners'}</Button>
          <Button icon="reload" onClick={() => logs.refetch()}>Refresh</Button>
          <div className="spacer" />
          {state && state.reachable && !state.running && canWrite && (
            <Button variant="primary" loading={starting} onClick={start}>
              Start {label}
            </Button>
          )}
        </>
      }
    >
      {errorLine && (
        <Callout tone="danger" title={<span className="mono" style={{ fontWeight: 500 }}>{clock(errorLine.at)} {errorLine.text}</span>}>
          Likely cause: another process holds port {port}.{' '}
          {!showListeners && (
            <button type="button" className="btn-link" onClick={() => setShowListeners(true)}>
              Show listeners
            </button>
          )}
        </Callout>
      )}
      {state && !state.running && state.exitError && !errorLine && (
        <Callout tone="danger" title={`${label} exited`}>
          <span className="mono">{state.exitError}</span>
        </Callout>
      )}
      {logs.isLoading ? (
        <Skeleton height={240} />
      ) : logs.isError ? (
        <Callout tone="warn" title="Couldn't read the engine log">{errorMessage(logs.error)}</Callout>
      ) : lines.length === 0 ? (
        <EmptyState icon="logs" title="No output yet" description={`${label} hasn't written anything to stdout or stderr since the agent started.`} />
      ) : (
        <div className="eng-log">
          {lines.map((l, i) => (
            <div key={i} className={lineClass(l.text)}>
              <span className="t">{clock(l.at)} </span>
              {l.text}
            </div>
          ))}
        </div>
      )}
      {showListeners && (
        <Card title="Listening sockets" sub="host network · TCP listen + UDP bound">
          {listeners.isLoading ? (
            <div className="card-body"><Skeleton height={80} /></div>
          ) : listeners.isError ? (
            <div className="card-body"><Callout tone="warn">{errorMessage(listeners.error)}</Callout></div>
          ) : (
            <div className="table-wrap" style={{ maxHeight: 320 }}>
              <table className="table compact eng-listeners">
                <thead>
                  <tr><th>Proto</th><th>Address</th><th className="num">Port</th><th>Process</th></tr>
                </thead>
                <tbody>
                  {(listeners.data?.listeners ?? []).map((l) => {
                    const hot = port !== undefined && l.port === port
                    return (
                      <tr key={`${l.proto}-${l.address}-${l.port}`}>
                        <td className={cx('mono', hot && 'hot')}>{l.proto}</td>
                        <td className={cx('mono', hot && 'hot')}>{l.address}</td>
                        <td className={cx('num', hot && 'hot')}>{l.port}</td>
                        <td className={cx('mono', hot && 'hot')}>{l.process || <span className="faint">other container / host</span>}</td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>
          )}
        </Card>
      )}
    </Drawer>
  )
}
