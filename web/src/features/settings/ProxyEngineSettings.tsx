// Settings → Proxy engine: choose nginx or Relay Edge (beta) and see what each
// engine is doing. The choice lives in the general settings document so a
// switch is a pending change (validated, health-checked, rolled back on failure).
import { useEffect, useState, type ReactNode } from 'react'
import { Link } from 'react-router-dom'
import { Badge, Button, Callout, Card, Dot, RadioCard, SectionHeader, Skeleton, useToast } from '../../components/ui'
import { useEngines, useEntities, useRole, useSaveSettings, useSettings } from '../../lib/queries'
import { proxyEngineLabel, type EngineState, type ProxyEngineName } from '../../lib/types'
import NotAllowed from '../auth/NotAllowed'
import { fieldErrors } from '../auth/authApi'

const ENGINES: { value: ProxyEngineName; description: string }[] = [
  { value: 'nginx', description: 'Industry standard · the default · runs custom nginx snippets' },
  { value: 'edge', description: "Relay's own engine, in beta: zero-downtime reloads, upstream keep-alive, built-in metrics, post-quantum TLS" },
]

const other = (e: ProxyEngineName): ProxyEngineName => (e === 'edge' ? 'nginx' : 'edge')
const applyNow = () => window.dispatchEvent(new CustomEvent('relay:apply'))

export default function ProxyEngineSettings() {
  const { data, isLoading } = useSettings('general')
  const save = useSaveSettings('general')
  const engines = useEngines().data
  const hosts = useEntities('hosts').data ?? []
  const streams = useEntities('streams').data ?? []
  const { isAdmin } = useRole()
  const toast = useToast()
  const [choice, setChoice] = useState<ProxyEngineName | null>(null)
  const [error, setError] = useState('')

  const saved: ProxyEngineName = data?.proxyEngine === 'edge' ? 'edge' : 'nginx'
  useEffect(() => {
    if (data && choice === null) setChoice(saved)
  }, [data, choice, saved])

  const header = (
    <SectionHeader
      title="Proxy engine"
      description="The reverse proxy that serves your hosts, redirects and streams. Both engines use the same configuration, so you can switch back and forth."
    />
  )
  if (isLoading || !data || choice === null) {
    return (
      <>
        {header}
        <Skeleton height={160} />
        <Skeleton height={140} />
      </>
    )
  }

  const selected = choice
  const live: ProxyEngineName | undefined = engines ? (engines.proxy === 'edge' ? 'edge' : 'nginx') : undefined
  const dirty = selected !== saved
  const switchPending = !!live && saved !== live && !dirty
  const ports = `${data.httpPort || 80}/${data.httpsPort || 443}${data.http3 ? ' + UDP ' + (data.httpsPort || 443) : ''}`

  const snippetHosts = hosts.filter((h) => h.customNginx?.trim())
  const geoHosts = hosts.filter((h) => h.enabled && h.geoBlock?.enabled && (h.geoBlock.allowCountries?.length ?? 0) > 0)
  const udpProxyStreams = streams.filter((s) => s.enabled && s.proxyProtocol && s.protocol !== 'tcp')

  const onSave = async () => {
    setError('')
    try {
      await save.mutateAsync({ ...data, proxyEngine: selected })
      toast.show({
        kind: 'success',
        title: `Proxy engine: ${proxyEngineLabel[saved]} → ${proxyEngineLabel[selected]}`,
        message: `Added to pending changes. Applying validates ${proxyEngineLabel[selected]}, stops ${proxyEngineLabel[saved]}, starts ${proxyEngineLabel[selected]} on the same ports and health-checks every host. If anything breaks it rolls back automatically.`,
        actions: [{ label: 'Apply now', onClick: applyNow }],
      })
    } catch (err) {
      const f = fieldErrors(err)
      if (f.proxyEngine) setError(f.proxyEngine)
      else toast.error(err, 'Proxy engine not saved')
    }
  }

  return (
    <>
      {header}
      {!isAdmin && <NotAllowed what="settings" />}

      {switchPending && live && (
        <Callout
          tone="info"
          title={`Switch to ${proxyEngineLabel[saved]} is saved but not applied`}
          actions={isAdmin ? <Button size="sm" variant="primary" onClick={applyNow}>Apply switch</Button> : undefined}
        >
          {proxyEngineLabel[live]} keeps serving traffic until you apply.
        </Callout>
      )}

      <Card title="Engine">
        <div className="card-body col gap-12" style={{ padding: '16px 18px' }}>
          <div className="grid-2" style={{ gap: 10 }}>
            {ENGINES.map((e) => (
              <RadioCard
                key={e.value}
                selected={selected === e.value}
                disabled={!isAdmin}
                onSelect={() => {
                  setChoice(e.value)
                  setError('')
                }}
                title={
                  <span className="row gap-6">
                    {proxyEngineLabel[e.value]}
                    {e.value === 'edge' && <Badge tone="info">beta</Badge>}
                    {live === e.value && <Badge tone="ok">live</Badge>}
                    {live && live !== e.value && saved === e.value && <Badge tone="pending">pending apply</Badge>}
                  </span>
                }
                description={e.description}
              />
            ))}
          </div>
          <div className="small muted">
            Only one engine serves traffic. Relay stops the other engine and its container, and starts it again when you switch back, so switching still takes a single apply.
          </div>
          {error && <div className="field-error">{error}</div>}
        </div>
      </Card>

      <Card title="Status">
        {(['nginx', 'edge'] as ProxyEngineName[]).map((name) => (
          <EngineRow key={name} name={name} state={engines?.[name]} live={live} ports={ports} loaded={!!engines} />
        ))}
      </Card>

      {selected === 'edge' && (
        <Card title="Relay Edge endpoints">
          <Row title="Health" desc={<>On the Docker host: <span className="mono">http://127.0.0.1:18081/healthz</span> · 503 while draining</>} />
          <Row title="Status" desc={<><span className="mono">http://127.0.0.1:18081/stub_status</span> · nginx-compatible connection counters</>} />
          <Row
            title="Prometheus metrics"
            desc={<><span className="mono">http://127.0.0.1:18081/metrics</span> · requests, latency, upstream errors and streams per host · <Link to="/docs/relay-edge">scrape config</Link></>}
          />
          <div className="card-body small faint" style={{ paddingTop: 0 }}>
            The port is <span className="mono">RELAY_EDGE_STATUS_PORT</span> (default 18081) and only listens on localhost.
          </div>
        </Card>
      )}

      <Card title="What changes for your configuration">
        <Row title="Hosts, redirects, access lists, certificates, streams" desc="Stored once in Relay and served by either engine" badge={<Badge tone="ok">shared</Badge>} />
        <Row
          title="Custom nginx snippets"
          desc={
            snippetHosts.length === 0
              ? 'No host uses one'
              : <>{snippetHosts.length === 1 ? '1 host has one' : `${snippetHosts.length} hosts have one`}: {snippetHosts.slice(0, 4).map((h, i) => (
                  <span key={h.id}>{i > 0 && ', '}<Link to={`/hosts?edit=${h.id}&tab=advanced`} className="mono">{h.domains[0] ?? h.id}</Link></span>
                ))}{snippetHosts.length > 4 && ` and ${snippetHosts.length - 4} more`} · kept either way, only nginx runs them</>
          }
          badge={<Badge tone={selected === 'edge' && snippetHosts.length > 0 ? 'warn' : undefined}>{selected === 'edge' ? 'skipped' : 'applied'}</Badge>}
        />
        <Row
          title="Geo-blocking by country"
          desc={`${geoHosts.length === 0 ? 'No host uses it' : `${geoHosts.length} host${geoHosts.length === 1 ? '' : 's'}`} · saved, but neither engine enforces it (the official nginx image has no GeoIP2 module)`}
          badge={<Badge>skipped</Badge>}
        />
        <Row
          title="PROXY protocol on UDP streams"
          desc={`${udpProxyStreams.length === 0 ? 'No UDP stream uses it' : `${udpProxyStreams.length} stream${udpProxyStreams.length === 1 ? '' : 's'}`} · Relay Edge sends the PROXY header on TCP only`}
          badge={<Badge tone={selected === 'edge' && udpProxyStreams.length > 0 ? 'warn' : undefined}>{selected === 'edge' ? 'TCP only' : 'applied'}</Badge>}
        />
        <Row title="HTTP/2, HTTP/3, forward auth, rate limits, caching, gzip" desc="Supported by both engines" badge={<Badge tone="ok">both</Badge>} />
        <Row
          title="Updates"
          desc={<>nginx upgrades from <Link to="/settings/engines">Settings → Updates</Link>; Relay Edge ships with Relay and updates with it</>}
        />
      </Card>

      {isAdmin && (
        <div className="settings-footer">
          {dirty && (
            <Button variant="ghost" onClick={() => { setChoice(saved); setError('') }}>
              Discard
            </Button>
          )}
          <Button variant="primary" size="md" disabled={!dirty} loading={save.isPending} onClick={onSave}>
            {dirty ? `Switch to ${proxyEngineLabel[selected]}` : 'Save changes'}
          </Button>
        </div>
      )}
    </>
  )
}

function Row({ title, desc, badge }: { title: ReactNode; desc?: ReactNode; badge?: ReactNode }) {
  return (
    <div className="toggle-row">
      <div className="grow">
        <div className="toggle-title">{title}</div>
        {desc && <div className="toggle-desc">{desc}</div>}
      </div>
      {badge}
    </div>
  )
}

function EngineRow({ name, state, live, ports, loaded }: { name: ProxyEngineName; state?: EngineState; live?: ProxyEngineName; ports: string; loaded: boolean }) {
  const label = proxyEngineLabel[name]
  const active = live === name
  const container = `relay-${name === 'edge' ? 'edge' : 'nginx'}`
  let tone: 'ok' | 'warn' | 'danger' | 'muted' = 'muted'
  let desc: ReactNode = loaded ? '' : 'checking…'
  let badge: ReactNode = null
  if (state && state.standby) {
    desc = <>Stopped · the <span className="mono">{container}</span> container is stopped and holds no ports · it starts again when you switch back</>
    badge = <Badge>standby</Badge>
  } else if (state && !state.reachable && state.container === 'stopped') {
    tone = 'warn'
    desc = <>The <span className="mono">{container}</span> container is stopped · Relay starts it within a few seconds</>
    badge = <Badge tone="warn">starting</Badge>
  } else if (state && !state.reachable) {
    tone = active ? 'danger' : 'warn'
    desc = <>Agent unreachable · check the <span className="mono">{container}</span> container</>
  } else if (state && active) {
    tone = state.running ? 'ok' : 'danger'
    desc = state.running ? <>Serving traffic on ports {ports}{state.version ? <> · <span className="mono">{state.version}</span></> : null}</> : 'Selected but not running · see the engine banner for its error log'
    badge = <Badge tone={state.running ? 'ok' : 'danger'}>{state.running ? 'serving' : 'not running'}</Badge>
  } else if (state) {
    tone = state.running ? 'warn' : 'muted'
    desc = state.running
      ? `Still running · Relay stops it shortly because ${proxyEngineLabel[other(name)]} is the proxy engine`
      : <>Stopped · holds no ports · Relay stops the <span className="mono">{container}</span> container shortly</>
    badge = <Badge>{state.running ? 'stopping' : 'standby'}</Badge>
  }
  return (
    <div className="toggle-row">
      <Dot tone={tone} />
      <div className="grow">
        <div className="toggle-title row gap-6">
          {label}
          {name === 'edge' && <Badge tone="info">beta</Badge>}
        </div>
        <div className="toggle-desc">{desc}</div>
      </div>
      {badge}
    </div>
  )
}
