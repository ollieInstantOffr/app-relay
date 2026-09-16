// Connect a tunnel gateway: create it, install and pair it on the public
// server, then choose what to publish through it.
import { useEffect, useMemo, useRef, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Badge, Button, Callout, Checkbox, CodeBlock, CopyButton, Dialog, Field, Input, Segmented, Spinner, Stepper, useToast } from '../../components/ui'
import { ApiError, api, errorMessage } from '../../lib/api'
import { keys, useEntities } from '../../lib/queries'
import type { Gateway, GatewayPairing, GatewayTransport, ProxyHost, Stream } from '../../lib/types'
import { applyNowAction } from '../loadbalancer/lbApi'
import { pairGateway, shortFingerprint, startPairing, transportLabel, tunnelKeys, untilLabel } from './api'

const STEPS = ['Gateway', 'Install', 'Publish']

/** gatewayId: resume at the install step for an existing (pending) gateway. */
export default function ConnectGatewayWizard({ gatewayId, onClose }: { gatewayId?: string; onClose: () => void }) {
  const qc = useQueryClient()
  const toast = useToast()
  const [step, setStep] = useState(gatewayId ? 1 : 0)
  const [draft, setDraft] = useState({ name: '', address: '', transport: 'auto' as GatewayTransport })
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [gateway, setGateway] = useState<Gateway | null>(null)
  const [pairing, setPairing] = useState<GatewayPairing | null>(null)
  const [waiting, setWaiting] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const started = useRef(false)

  const refresh = () => {
    qc.invalidateQueries({ queryKey: tunnelKeys.overview })
    qc.invalidateQueries({ queryKey: keys.entities('gateways') })
  }

  // Resume an existing gateway: new pairing command straight away.
  useEffect(() => {
    if (!gatewayId || started.current) return
    started.current = true
    ;(async () => {
      try {
        const g = await api.get<Gateway>(`/api/gateways/${gatewayId}`)
        setGateway(g)
        setPairing(await startPairing(g.id))
        refresh()
      } catch (err) {
        setError(errorMessage(err))
      }
    })()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [gatewayId])

  // While on the install step, try to pair every few seconds.
  useEffect(() => {
    if (step !== 1 || !gateway || !pairing) return
    let stop = false
    let timer = 0
    const attempt = async () => {
      try {
        const g = await pairGateway(gateway.id)
        if (stop) return
        if (g.pairState === 'paired') {
          setGateway(g)
          setWaiting('')
          refresh()
          toast.success('Gateway paired', `${g.name} is ready to publish hosts.`)
          setStep(2)
          return
        }
      } catch (err) {
        if (stop) return
        if (err instanceof ApiError && (err.code === 'pairing_rejected' || err.code === 'token_expired')) {
          setError(err.message)
          setWaiting('')
          return
        }
        setWaiting(errorMessage(err))
      }
      timer = window.setTimeout(attempt, 3000)
    }
    timer = window.setTimeout(attempt, 1500)
    return () => {
      stop = true
      window.clearTimeout(timer)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [step, gateway?.id, pairing?.token])

  const create = async () => {
    setBusy(true)
    setError('')
    setErrors({})
    try {
      const g = await api.post<Gateway>('/api/gateways', { ...draft, enabled: true })
      setGateway(g)
      setPairing(await startPairing(g.id))
      refresh()
      setStep(1)
    } catch (err) {
      if (err instanceof ApiError && err.fields) setErrors(err.fields)
      else setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }

  const newToken = async () => {
    if (!gateway) return
    setError('')
    try {
      setPairing(await startPairing(gateway.id))
    } catch (err) {
      setError(errorMessage(err))
    }
  }

  const footer =
    step === 0 ? (
      <>
        <Button onClick={onClose}>Cancel</Button>
        <Button variant="primary" loading={busy} disabled={!draft.name.trim() || !draft.address.trim()} onClick={create}>
          Continue →
        </Button>
      </>
    ) : step === 1 ? (
      <>
        <span className="small faint" style={{ marginRight: 'auto' }}>
          {pairing ? `Token expires ${untilLabel(pairing.expiresAt)}` : ''}
        </span>
        <Button onClick={onClose}>Finish later</Button>
      </>
    ) : null

  return (
    <Dialog
      open
      onClose={onClose}
      dismissable={step !== 2}
      width={760}
      className="wizard"
      title={gatewayId ? `Pair ${gateway?.name ?? 'gateway'}` : 'Connect a tunnel gateway'}
      description="Publish hosts from a public server without opening ports at home. The gateway never decrypts HTTPS."
      footer={footer}
    >
      <div className="col gap-16">
        <Stepper steps={STEPS} current={step} />
        {error && <Callout tone="danger">{error}</Callout>}

        {step === 0 && (
          <div className="col gap-14">
            <div className="grid-2">
              <Field label="Name" error={errors.name}>
                <Input mono autoFocus placeholder="vps-fra" value={draft.name} invalid={!!errors.name} onChange={(e) => setDraft({ ...draft, name: e.target.value })} />
              </Field>
              <Field label="Transport" error={errors.transport} hint="Auto uses QUIC and falls back to TCP when UDP is blocked">
                <Segmented
                  value={draft.transport}
                  onChange={(transport) => setDraft({ ...draft, transport })}
                  options={(['auto', 'quic', 'tcp'] as GatewayTransport[]).map((v) => ({ value: v, label: transportLabel[v] }))}
                />
              </Field>
            </div>
            <Field label="Public address of the server" error={errors.address} hint="Host name or IP address Relay dials; port 7443 unless you add :port">
              <Input
                mono
                placeholder="gw.example.com"
                value={draft.address}
                invalid={!!errors.address}
                onChange={(e) => setDraft({ ...draft, address: e.target.value.trim() })}
                onKeyDown={(e) => e.key === 'Enter' && draft.name && draft.address && create()}
              />
            </Field>
            <Callout tone="info" title="What you need">
              A server with a public IP address (any small VPS), Docker with Compose, and a firewall that allows the ports shown on the next step.
            </Callout>
          </div>
        )}

        {step === 1 && (
          <InstallStep gateway={gateway} pairing={pairing} waiting={waiting} onNewToken={newToken} />
        )}

        {step === 2 && gateway && <PublishStep gateway={gateway} onDone={onClose} />}
      </div>
    </Dialog>
  )
}

function InstallStep({ gateway, pairing, waiting, onNewToken }: { gateway: Gateway | null; pairing: GatewayPairing | null; waiting: string; onNewToken: () => void }) {
  if (!gateway || !pairing) {
    return (
      <div className="row gap-8 muted small">
        <Spinner /> Creating the pairing command…
      </div>
    )
  }
  return (
    <div className="col gap-14">
      <div className="col gap-6">
        <div className="tun-step-title">1 · On the server, install and start the gateway</div>
        <div className="tun-code">
          <CodeBlock code={pairing.install} wrap />
          <div className="tun-code-copy"><CopyButton text={pairing.install} /></div>
        </div>
        <div className="small faint">
          The token works once and expires; anyone who has it before pairing can pair their own Relay, so don't share it.
          Already installed? Start it with <span className="mono">RELAY_GATEWAY_PAIR_TOKEN</span> set to the token.
        </div>
      </div>
      <div className="col gap-6">
        <div className="tun-step-title">2 · Allow these ports in the server's firewall</div>
        <div className="row wrap gap-6">
          {pairing.ports.map((p) => <Badge key={p}>{p}</Badge>)}
          <span className="small faint">and the TCP ports of streams you publish</span>
        </div>
      </div>
      <div className="tun-waiting">
        <Spinner />
        <div className="grow">
          <div className="medium">Waiting for {gateway.name} at <span className="mono">{gateway.address}</span></div>
          <div className="small faint">{waiting || 'Relay pairs as soon as the gateway answers.'}</div>
        </div>
        <Button size="sm" onClick={onNewToken}>New token</Button>
      </div>
      <div className="small faint">
        This Relay's key for the gateway <span className="mono">{shortFingerprint(pairing.homeFingerprint)}</span>
      </div>
    </div>
  )
}

function PublishStep({ gateway, onDone }: { gateway: Gateway; onDone: () => void }) {
  const toast = useToast()
  const qc = useQueryClient()
  const hosts = useEntities('hosts').data
  const streams = useEntities('streams').data
  const candidates = useMemo(() => (hosts ?? []).filter((h) => h.enabled && !h.tunnelGatewayId), [hosts])
  const tcpStreams = useMemo(() => (streams ?? []).filter((s) => s.enabled && s.protocol !== 'udp' && !s.tunnelGatewayId), [streams])
  const [hostIds, setHostIds] = useState<string[]>([])
  const [streamIds, setStreamIds] = useState<string[]>([])
  const [busy, setBusy] = useState(false)
  const toggle = (list: string[], id: string, on: boolean) => (on ? [...list, id] : list.filter((x) => x !== id))

  const publish = async (apply: boolean) => {
    setBusy(true)
    try {
      for (const h of candidates.filter((h) => hostIds.includes(h.id))) {
        await api.put<ProxyHost>(`/api/hosts/${h.id}`, { ...h, tunnelGatewayId: gateway.id })
      }
      for (const s of tcpStreams.filter((s) => streamIds.includes(s.id))) {
        await api.put<Stream>(`/api/streams/${s.id}`, { ...s, tunnelGatewayId: gateway.id })
      }
      qc.invalidateQueries({ queryKey: ['entities'] })
      qc.invalidateQueries({ queryKey: keys.pending })
      qc.invalidateQueries({ queryKey: tunnelKeys.overview })
      const n = hostIds.length + streamIds.length
      if (apply) applyNowAction.onClick()
      else if (n > 0) toast.show({ kind: 'success', title: `Publishing ${n} through ${gateway.name}`, message: 'Added to pending changes', actions: [applyNowAction] })
      onDone()
    } catch (err) {
      toast.error(err, 'Could not publish')
    } finally {
      setBusy(false)
    }
  }

  const n = hostIds.length + streamIds.length
  return (
    <div className="col gap-14">
      <Callout tone="ok" title={`${gateway.name} is paired`}>
        Choose what to publish through it. You can change this later in each host or stream (Publish through tunnel).
        Point the DNS records of published domains at the gateway; Public DNS does that for new records.
      </Callout>
      {candidates.length === 0 && tcpStreams.length === 0 ? (
        <div className="small muted">No hosts or TCP streams to publish yet.</div>
      ) : (
        <div className="tun-pick">
          {candidates.map((h) => (
            <label key={h.id} className="tun-pick-row">
              <Checkbox checked={hostIds.includes(h.id)} onChange={(v) => setHostIds(toggle(hostIds, h.id, v))} />
              <span className="mono grow truncate">{h.domains[0]}{h.domains.length > 1 ? ` +${h.domains.length - 1}` : ''}</span>
              {h.system && <Badge tone="warn">Relay admin UI</Badge>}
              <span className="small faint">{h.certificateId ? 'HTTPS' : 'HTTP'}</span>
            </label>
          ))}
          {tcpStreams.map((s) => (
            <label key={s.id} className="tun-pick-row">
              <Checkbox checked={streamIds.includes(s.id)} onChange={(v) => setStreamIds(toggle(streamIds, s.id, v))} />
              <span className="mono grow truncate">{s.name}</span>
              <span className="small faint">TCP {s.listenPorts}</span>
            </label>
          ))}
        </div>
      )}
      <div className="row end gap-8">
        <Button onClick={onDone}>{n ? 'Cancel' : 'Close'}</Button>
        <Button disabled={!n} loading={busy} onClick={() => publish(false)}>Add to pending</Button>
        <Button variant="primary" disabled={!n} loading={busy} onClick={() => publish(true)}>Publish and apply</Button>
      </div>
    </div>
  )
}
