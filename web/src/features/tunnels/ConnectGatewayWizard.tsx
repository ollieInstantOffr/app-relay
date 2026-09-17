// Set up a tunnel: point Relay at a server, install the gateway there with one
// command (Relay watches and pairs), choose what to publish, and go live with
// a real test through the gateway.
import { useEffect, useMemo, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { useQueryClient } from '@tanstack/react-query'
import { Badge, Button, Callout, Checkbox, CodeBlock, CopyButton, Dialog, Field, Icon, Input, SearchInput, Segmented, Spinner, Stepper, cx, matchesSearch, useToast } from '../../components/ui'
import { ApiError, api, errorMessage } from '../../lib/api'
import { keys, useEntities } from '../../lib/queries'
import type { Gateway, GatewayPairing, GatewayTransport, ProxyHost, Stream } from '../../lib/types'
import { usePublicDNS } from '../dns/dnsApi'
import { useApplyRunner } from '../history/api'
import { transportLabel, tunnelKeys, tunnelsDocs, untilLabel, useTunnelTests } from './api'

const STEPS = ['Server', 'Install', 'Publish', 'Go live']

interface CheckStep { id: string; status: 'ok' | 'fail' | 'waiting'; title: string; detail?: string }
interface SetupCheck { steps: CheckStep[]; paired: boolean; gateway: Gateway }

const hostOf = (address: string) => address.trim().replace(/^\[/, '').replace(/\](:\d+)?$/, '').replace(/:\d+$/, '')

/** gatewayId: continue with an existing gateway (install or publish step). */
export default function ConnectGatewayWizard({ gatewayId, startAt, onClose }: { gatewayId?: string; startAt?: 'install' | 'publish'; onClose: () => void }) {
  const qc = useQueryClient()
  const [step, setStep] = useState(gatewayId ? (startAt === 'publish' ? 2 : 1) : 0)
  const [gateway, setGateway] = useState<Gateway | null>(null)
  const [pairing, setPairing] = useState<GatewayPairing | null>(null)
  const [published, setPublished] = useState<string[]>([])
  const [error, setError] = useState('')
  const loaded = useRef(false)

  const refresh = () => {
    qc.invalidateQueries({ queryKey: tunnelKeys.overview })
    qc.invalidateQueries({ queryKey: keys.entities('gateways') })
  }

  const newPairing = async (g: Gateway, reset = false) => {
    setError('')
    try {
      setPairing(await api.post<GatewayPairing>(`/api/gateways/${g.id}/pairing`, reset ? { reset: true } : undefined))
      refresh()
    } catch (err) {
      setError(errorMessage(err))
    }
  }

  // Continue an existing gateway.
  useEffect(() => {
    if (!gatewayId || loaded.current) return
    loaded.current = true
    ;(async () => {
      try {
        const g = await api.get<Gateway>(`/api/gateways/${gatewayId}`)
        setGateway(g)
        if (startAt === 'publish' && g.pairState === 'paired') return
        setStep(1)
        await newPairing(g)
      } catch (err) {
        setError(errorMessage(err))
      }
    })()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [gatewayId])

  const title = step === 0 || !gateway ? 'Set up a tunnel' : `Tunnel through ${gateway.name}`

  return (
    <Dialog open onClose={onClose} dismissable={step === 0} width={800} className="wizard tun-wizard" title={title}
      description="Make hosts at home reachable from the internet through your own server, without opening ports on your router.">
      <div className="col gap-18">
        <Stepper steps={STEPS} current={step} />
        {error && <Callout tone="danger">{error}</Callout>}
        {step === 0 && (
          <ServerStep
            onCancel={onClose}
            onCreated={async (g) => {
              setGateway(g)
              refresh()
              setStep(1)
              await newPairing(g)
            }}
          />
        )}
        {step === 1 && gateway && (
          <InstallStep
            gateway={gateway}
            pairing={pairing}
            onNewCommand={(reset) => newPairing(gateway, reset)}
            onPaired={(g) => {
              setGateway(g)
              refresh()
              setStep(2)
            }}
            onLater={onClose}
          />
        )}
        {step === 2 && gateway && (
          <PublishStep
            gateway={gateway}
            onSkip={onClose}
            onPublished={(domains) => {
              setPublished(domains)
              setStep(3)
            }}
          />
        )}
        {step === 3 && gateway && <GoLiveStep gateway={gateway} domains={published} onDone={onClose} />}
      </div>
    </Dialog>
  )
}

// ---------------------------------------------------------------- 1 · server

function ServerStep({ onCancel, onCreated }: { onCancel: () => void; onCreated: (g: Gateway) => void }) {
  const [address, setAddress] = useState('')
  const [name, setName] = useState('')
  const [nameTouched, setNameTouched] = useState(false)
  const [transport, setTransport] = useState<GatewayTransport>('auto')
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const effectiveName = nameTouched ? name : hostOf(address)

  const create = async () => {
    setBusy(true)
    setErrors({})
    setError('')
    try {
      onCreated(await api.post<Gateway>('/api/gateways', { name: effectiveName || hostOf(address), address, transport, enabled: true }))
    } catch (err) {
      if (err instanceof ApiError && err.fields) setErrors(err.fields)
      else setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="col gap-16">
      <TunnelDiagram />
      {error && <Callout tone="danger">{error}</Callout>}
      <Field label="Your server's public IP address or host name" error={errors.address}
        hint="The server that receives visitors. Relay connects out to it on port 7443; add :port for another port.">
        <Input mono autoFocus placeholder="203.0.113.10 or gw.example.com" value={address} invalid={!!errors.address}
          onChange={(e) => setAddress(e.target.value.trim())} onKeyDown={(e) => e.key === 'Enter' && address && create()} />
      </Field>
      <div className="tun-needs">
        <div className="tun-needs-title">What you need</div>
        <ul>
          <li><Icon name="check" size={13} /> A Linux server with a public IP address, e.g. a small VPS (1 vCPU, 1 GB RAM is plenty)</li>
          <li><Icon name="check" size={13} /> SSH access with <span className="mono">sudo</span>. The installer sets up Docker for you</li>
          <li><Icon name="check" size={13} /> Ports 80, 443 and 7443 open to the internet (the installer opens them in ufw / firewalld)</li>
        </ul>
      </div>
      <details className="tun-advanced">
        <summary>Advanced</summary>
        <div className="grid-2" style={{ marginTop: 12 }}>
          <Field label="Name" error={errors.name}>
            <Input mono placeholder="vps-fra" value={effectiveName} onChange={(e) => { setNameTouched(true); setName(e.target.value) }} />
          </Field>
          <Field label="Transport" hint="Auto uses QUIC and falls back to TCP when UDP is blocked">
            <Segmented value={transport} onChange={setTransport} options={(['auto', 'quic', 'tcp'] as GatewayTransport[]).map((v) => ({ value: v, label: transportLabel[v] }))} />
          </Field>
        </div>
      </details>
      <div className="row gap-8">
        <Link to={tunnelsDocs} className="small muted" onClick={onCancel}>How tunnels work</Link>
        <span className="spacer" />
        <Button onClick={onCancel}>Cancel</Button>
        <Button variant="primary" loading={busy} disabled={!address} onClick={create}>Continue →</Button>
      </div>
    </div>
  )
}

export function TunnelDiagram({ compact }: { compact?: boolean }) {
  const nodes = [
    { icon: 'users' as const, title: 'Visitors', sub: 'app.example.com' },
    { icon: 'expose' as const, title: 'Your server', sub: 'ports 80 and 443' },
    { icon: 'overview' as const, title: 'Relay at home', sub: 'HTTPS and access rules' },
    { icon: 'docker' as const, title: 'Your apps', sub: 'on your network' },
  ]
  return (
    <div className={cx('tun-diagram', compact && 'compact')}>
      {nodes.flatMap((n, i) => [
        ...(i > 0 ? [<div key={`a${i}`} className={cx('tun-diagram-arrow', i === 2 && 'tunnel')}>{i === 2 && <span>tunnel · Relay dials out</span>}</div>] : []),
        <div key={n.title} className="tun-diagram-node">
          <span className="tun-diagram-icon"><Icon name={n.icon} size={16} /></span>
          <span className="tun-diagram-title">{n.title}</span>
          {!compact && <span className="tun-diagram-sub">{n.sub}</span>}
        </div>,
      ])}
    </div>
  )
}

// ---------------------------------------------------------------- 2 · install

function InstallStep({ gateway, pairing, onNewCommand, onPaired, onLater }: {
  gateway: Gateway
  pairing: GatewayPairing | null
  onNewCommand: (reset: boolean) => void
  onPaired: (g: Gateway) => void
  onLater: () => void
}) {
  const toast = useToast()
  const [method, setMethod] = useState<'script' | 'manual'>('script')
  const [check, setCheck] = useState<SetupCheck | null>(null)
  const [started] = useState(() => Date.now())
  const [now, setNow] = useState(() => Date.now())
  const host = hostOf(gateway.address)

  useEffect(() => {
    const t = window.setInterval(() => setNow(Date.now()), 1000)
    return () => window.clearInterval(t)
  }, [])

  // Relay checks the server and pairs as soon as the gateway answers.
  useEffect(() => {
    if (!pairing) return
    let stop = false
    let timer = 0
    const run = async () => {
      try {
        const c = await api.post<SetupCheck>(`/api/gateways/${gateway.id}/check`)
        if (stop) return
        setCheck(c)
        if (c.paired) {
          toast.success('Gateway connected', `${c.gateway.name} is paired with this Relay.`)
          window.setTimeout(() => !stop && onPaired(c.gateway), 900)
          return
        }
      } catch {
        // keep polling
      }
      if (!stop) timer = window.setTimeout(run, 3000)
    }
    run()
    return () => {
      stop = true
      window.clearTimeout(timer)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [gateway.id, pairing?.token])

  if (!pairing) {
    return <div className="row gap-8 muted small"><Spinner /> Creating your install command…</div>
  }
  const elapsed = Math.max(0, Math.floor((now - started) / 1000))
  const rejected = check?.steps.find((s) => s.id === 'pair' && s.status === 'fail')
  const steps: CheckStep[] = check?.steps ?? [
    { id: 'address', status: 'waiting', title: 'Server address' },
    { id: 'port', status: 'waiting', title: 'Gateway answers on port 7443' },
    { id: 'pair', status: 'waiting', title: 'Paired with this Relay' },
  ]
  const command = method === 'script' ? pairing.install : pairing.manual

  return (
    <div className="col gap-16">
      <div className="tun-install">
        <div className="tun-install-step">
          <span className="tun-num">1</span>
          <div className="grow col gap-6">
            <div className="tun-step-title">Connect to your server</div>
            <div className="tun-inline-cmd">
              <span className="mono">ssh root@{host}</span>
              <CopyButton text={`ssh root@${host}`} />
            </div>
          </div>
        </div>
        <div className="tun-install-step">
          <span className="tun-num">2</span>
          <div className="grow col gap-8">
            <div className="row between">
              <div className="tun-step-title">Paste and run this command</div>
              <Segmented value={method} onChange={setMethod} options={[{ value: 'script', label: 'One command' }, { value: 'manual', label: 'Docker Compose' }]} />
            </div>
            <div className="tun-cmd">
              <CodeBlock code={command} dark wrap />
              <div className="tun-cmd-copy"><CopyButton text={command} label="Copy command" size="default" /></div>
            </div>
            <div className="small faint">
              {method === 'script'
                ? 'Installs Docker if needed, builds the gateway from the same Relay version as this one, opens ports 80, 443 and 7443, and starts it. Takes a few minutes the first time.'
                : <>Needs Docker with Compose and <span className="mono">git</span>. Open {pairing.ports.join(', ')} in the firewall yourself.</>}
              {' '}The command contains a one-time pairing token (expires {untilLabel(pairing.expiresAt)}), so don't share it.
            </div>
          </div>
        </div>
        <div className="tun-install-step">
          <span className="tun-num">3</span>
          <div className="grow col gap-8">
            <div className="row between">
              <div className="tun-step-title">Relay connects automatically</div>
              <span className="small faint mono">{Math.floor(elapsed / 60)}:{String(elapsed % 60).padStart(2, '0')}</span>
            </div>
            <div className="tun-checklist">
              {steps.map((s, i) => {
                const active = s.status === 'waiting' && steps.slice(0, i).every((p) => p.status === 'ok')
                return (
                  <div key={s.id} className={cx('tun-check', s.status, active && 'active')}>
                    <span className="tun-check-icon">
                      {s.status === 'ok' ? <Icon name="check" size={13} /> : s.status === 'fail' ? <Icon name="warning" size={13} /> : active ? <Spinner /> : <span className="tun-check-dot" />}
                    </span>
                    <div className="grow">
                      <div className="tun-check-title">{s.title}</div>
                      {s.detail && <div className="tun-check-detail">{s.detail}</div>}
                    </div>
                  </div>
                )
              })}
            </div>
          </div>
        </div>
      </div>

      {rejected && (
        <Callout tone="warn" title="The gateway didn't accept the token"
          actions={<Button size="sm" variant="primary" onClick={() => onNewCommand(true)}>Create a new command</Button>}>
          It is probably paired with another Relay or still uses an older token. The new command resets it; run it on the server again.
        </Callout>
      )}
      {elapsed > 300 && !check?.paired && !rejected && (
        <Callout tone="info" title="Taking a while?">
          The first install builds the gateway, which can take 5–10 minutes on a small server. If the installer finished, check that port 7443 is
          allowed in your hosting provider's firewall (security group) too. <Link to={tunnelsDocs}>Troubleshooting</Link>
        </Callout>
      )}

      <div className="row gap-8">
        <Button size="sm" variant="link" onClick={() => onNewCommand(false)}>New token</Button>
        <span className="spacer" />
        <Button onClick={onLater}>Finish later</Button>
      </div>
    </div>
  )
}

// ---------------------------------------------------------------- 3 · publish

function gatewayIPs(g: Gateway): string[] {
  const host = hostOf(g.address)
  const ips = [...(g.publicIps ?? [])]
  if (/^[\d.]+$/.test(host) && !ips.includes(host)) ips.unshift(host)
  return ips
}

function PublishStep({ gateway, onSkip, onPublished }: { gateway: Gateway; onSkip: () => void; onPublished: (domains: string[]) => void }) {
  const toast = useToast()
  const qc = useQueryClient()
  const hostsQ = useEntities('hosts').data
  const streamsQ = useEntities('streams').data
  const gatewayName = new Map((useEntities('gateways').data ?? []).map((g) => [g.id, g.name]))
  const dns = usePublicDNS()
  const runner = useApplyRunner()
  const [search, setSearch] = useState('')
  const [hostIds, setHostIds] = useState<string[]>([])
  const [streamIds, setStreamIds] = useState<string[]>([])
  const [busy, setBusy] = useState(false)

  const hosts = useMemo(() => (hostsQ ?? []).filter((h) => h.enabled && h.tunnelGatewayId !== gateway.id), [hostsQ, gateway.id])
  const streams = useMemo(() => (streamsQ ?? []).filter((s) => s.enabled && s.protocol !== 'udp' && s.tunnelGatewayId !== gateway.id), [streamsQ, gateway.id])
  const visibleHosts = hosts.filter((h) => matchesSearch(search, ...h.domains))
  const visibleStreams = streams.filter((s) => matchesSearch(search, s.name, s.listenPorts))
  const toggle = (list: string[], id: string, on: boolean) => (on ? [...list, id] : list.filter((x) => x !== id))
  const selectedHosts = hosts.filter((h) => hostIds.includes(h.id))
  const domains = selectedHosts.flatMap((h) => h.domains.filter((d) => !d.startsWith('*')))
  const ipv4 = gatewayIPs(gateway).find((ip) => /^[\d.]+$/.test(ip))
  const n = hostIds.length + streamIds.length

  const publish = async () => {
    setBusy(true)
    try {
      for (const h of selectedHosts) await api.put<ProxyHost>(`/api/hosts/${h.id}`, { ...h, tunnelGatewayId: gateway.id })
      for (const s of streams.filter((s) => streamIds.includes(s.id))) await api.put<Stream>(`/api/streams/${s.id}`, { ...s, tunnelGatewayId: gateway.id })
      qc.invalidateQueries({ queryKey: ['entities'] })
      qc.invalidateQueries({ queryKey: keys.pending })
      const v = await runner.run('/api/apply')
      if (v && v.status === 'live') onPublished(domains)
    } catch (err) {
      toast.error(err, 'Could not publish')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="col gap-14">
      <Callout tone="ok" title={`${gateway.name} is connected to this Relay`}>
        Choose what should be reachable from the internet through it. Everything else stays private.
      </Callout>
      {hosts.length + streams.length === 0 ? (
        <div className="small muted">No hosts or TCP streams to publish yet. Create a host first, then choose Publish through tunnel in it.</div>
      ) : (
        <>
          <SearchInput value={search} onChange={setSearch} placeholder="Search hosts" label="Search hosts" />
          <div className="tun-pick">
            {visibleHosts.map((h) => (
              <label key={h.id} className="tun-pick-row">
                <Checkbox checked={hostIds.includes(h.id)} onChange={(v) => setHostIds(toggle(hostIds, h.id, v))} />
                <span className="grow col" style={{ minWidth: 0, gap: 1 }}>
                  <span className="mono truncate">{h.domains[0]}{h.domains.length > 1 ? ` +${h.domains.length - 1}` : ''}</span>
                  <span className="micro faint truncate">→ {h.upstream.scheme}://{h.upstream.host}:{h.upstream.port}</span>
                </span>
                {h.system && <Badge tone="warn">Relay admin UI</Badge>}
                {h.tunnelGatewayId && <Badge title="Publishing it here moves it from that gateway">on {gatewayName.get(h.tunnelGatewayId) ?? 'another gateway'}</Badge>}
                <Badge tone={h.certificateId ? 'ok' : undefined}>{h.certificateId ? 'HTTPS' : 'HTTP only'}</Badge>
              </label>
            ))}
            {visibleStreams.map((s) => (
              <label key={s.id} className="tun-pick-row">
                <Checkbox checked={streamIds.includes(s.id)} onChange={(v) => setStreamIds(toggle(streamIds, s.id, v))} />
                <span className="grow col" style={{ minWidth: 0, gap: 1 }}>
                  <span className="mono truncate">{s.name}</span>
                  <span className="micro faint">TCP stream · port {s.listenPorts}</span>
                </span>
                <Badge tone="info">TCP</Badge>
              </label>
            ))}
          </div>
        </>
      )}

      {domains.length > 0 && (
        <div className="tun-dns">
          <div className="tun-step-title">Point these domains at your server</div>
          <div className="small muted">
            {dns.enabled
              ? 'Public DNS creates missing records automatically after you publish. Existing records that point elsewhere must be changed by hand.'
              : 'Add these records at your DNS provider, or connect it in Public DNS to let Relay create them.'}
          </div>
          <table className="table compact tun-dns-table">
            <thead><tr><th>Name</th><th>Type</th><th>Value</th><th /></tr></thead>
            <tbody>
              {domains.slice(0, 8).map((d) => (
                <tr key={d}>
                  <td className="mono">{d}</td>
                  <td className="mono">{ipv4 ? 'A' : 'CNAME'}</td>
                  <td className="mono">{ipv4 ?? hostOf(gateway.address)}</td>
                  <td style={{ textAlign: 'right' }}><CopyButton text={ipv4 ?? hostOf(gateway.address)} label="Copy" /></td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <div className="row gap-8">
        <Button onClick={onSkip}>Skip for now</Button>
        <span className="spacer" />
        <Button variant="primary" disabled={!n} loading={busy || runner.busy} onClick={publish}>
          {n ? `Publish ${n} and go live` : 'Choose what to publish'}
        </Button>
      </div>
    </div>
  )
}

// ---------------------------------------------------------------- 4 · go live

export function TestResults({ domains, results, onTest }: { domains: string[]; results: ReturnType<typeof useTunnelTests>['results']; onTest: (d: string) => void }) {
  return (
    <div className="tun-checklist">
      {domains.map((d) => {
        const r = results[d]
        const res = typeof r === 'object' ? r : undefined
        const running = !r || r === 'running'
        const ok = !!res?.reachable && res.dnsOk && res.tlsValid && (res.statusCode ?? 500) < 500
        const state = running ? 'waiting' : ok ? 'ok' : res?.reachable ? 'warn' : 'fail'
        return (
          <div key={d} className={cx('tun-check', state, running && 'active')}>
            <span className="tun-check-icon">{running ? <Spinner /> : ok ? <Icon name="check" size={13} /> : <Icon name="warning" size={13} />}</span>
            <div className="grow" style={{ minWidth: 0 }}>
              <div className="tun-check-title row wrap gap-6">
                <span className="mono">{d}</span>
                {res && (
                  <>
                    <Badge tone={res.reachable ? 'ok' : 'danger'}>{res.reachable ? `reachable · ${res.statusCode}` : 'not reachable'}</Badge>
                    {res.reachable && <Badge tone={res.tlsValid ? 'ok' : 'warn'}>{res.tlsValid ? 'certificate ok' : 'certificate not trusted'}</Badge>}
                    <Badge tone={res.dnsOk ? 'ok' : 'warn'}>{res.dnsOk ? 'DNS ok' : 'DNS not pointing here'}</Badge>
                  </>
                )}
              </div>
              <div className="tun-check-detail">
                {running ? 'Testing through the gateway…' : typeof r === 'string' ? r : res?.detail}
                {res && !res.dnsOk && (
                  <> {res.dnsAddresses.length ? `DNS points to ${res.dnsAddresses.join(', ')}` : 'The domain has no DNS record yet'}; set it to {res.expected.join(' or ') || 'the server'}.</>
                )}
              </div>
            </div>
            {!running && <Button size="sm" onClick={() => onTest(d)}>Test again</Button>}
          </div>
        )
      })}
    </div>
  )
}

function GoLiveStep({ gateway, domains, onDone }: { gateway: Gateway; domains: string[]; onDone: () => void }) {
  const { results, test } = useTunnelTests(gateway.id)

  // Give the tunnel engine a moment to connect, then test every domain.
  useEffect(() => {
    const t = window.setTimeout(() => domains.forEach(test), 3000)
    return () => window.clearTimeout(t)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  return (
    <div className="col gap-14">
      <Callout tone="ok" title="You're live">
        The configuration is applied and Relay connects to {gateway.name}. Each domain is tested through the gateway, the way a visitor reaches it.
      </Callout>
      {domains.length === 0 ? (
        <div className="small muted">Your TCP streams are available on the same ports of {hostOf(gateway.address)}.</div>
      ) : (
        <TestResults domains={domains} results={results} onTest={test} />
      )}
      <div className="small faint">DNS changes can take a few minutes to reach everyone. Test again any time from the gateway details.</div>
      <div className="row end">
        <Button variant="primary" onClick={onDone}>Done</Button>
      </div>
    </div>
  )
}
