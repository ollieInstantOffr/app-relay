// Settings → Load balancer engine: choose HAProxy or Relay Balancer (beta) and see
// what each engine is doing. The choice lives in the general settings document so a
// switch is a pending change (validated and applied with Apply & reload).
import { useEffect, useState, type ReactNode } from 'react'
import { Link } from 'react-router-dom'
import { Badge, Button, Callout, Card, Dot, RadioCard, SectionHeader, Skeleton, useToast } from '../../components/ui'
import { useEngines, useEntities, useRole, useSaveSettings, useSettings } from '../../lib/queries'
import { asLBEngine, lbCheckName, lbEngineLabel, type EngineState, type LBEngineName } from '../../lib/types'
import NotAllowed from '../auth/NotAllowed'
import { fieldErrors } from '../auth/authApi'

const ENGINES: { value: LBEngineName; description: string }[] = [
  { value: 'haproxy', description: 'The default · battle-tested · official Docker image, upgraded in Settings → Updates' },
  { value: 'balancer', description: 'Built into Relay, in beta: seamless reloads, no separate image, updates with Relay' },
]

const other = (e: LBEngineName): LBEngineName => (e === 'balancer' ? 'haproxy' : 'balancer')
const applyNow = () => window.dispatchEvent(new CustomEvent('relay:apply'))

export default function LBEngineSettings() {
  const { data, isLoading } = useSettings('general')
  const save = useSaveSettings('general')
  const lbSettings = useSettings('haproxy').data
  const engines = useEngines().data
  const backends = useEntities('backends').data ?? []
  const frontends = useEntities('frontends').data ?? []
  const { isAdmin } = useRole()
  const toast = useToast()
  const [choice, setChoice] = useState<LBEngineName | null>(null)
  const [error, setError] = useState('')

  const saved = asLBEngine(data?.lbEngine)
  useEffect(() => {
    if (data && choice === null) setChoice(saved)
  }, [data, choice, saved])

  const header = (
    <SectionHeader
      title="Load balancer engine"
      description="The engine that runs your backends and frontends. Both engines use the same configuration, so you can switch back and forth."
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
  const live: LBEngineName | undefined = engines ? asLBEngine(engines.lb) : undefined
  const dirty = selected !== saved
  const switchPending = !!live && saved !== live && !dirty

  const regexFrontends = frontends.filter((f) => f.rules.some((r) => r.conditions.some((c) => c.type === 'path_reg')))

  const onSave = async () => {
    setError('')
    try {
      await save.mutateAsync({ ...data, lbEngine: selected })
      toast.show({
        kind: 'success',
        title: `Load balancer engine: ${lbEngineLabel[saved]} → ${lbEngineLabel[selected]}`,
        message: `Added to pending changes. Applying validates the config with ${lbCheckName[selected]}, stops ${lbEngineLabel[saved]} and starts ${lbEngineLabel[selected]} with the same backends and frontends.`,
        actions: [{ label: 'Apply now', onClick: applyNow }],
      })
    } catch (err) {
      const f = fieldErrors(err)
      if (f.lbEngine) setError(f.lbEngine)
      else toast.error(err, 'Load balancer engine not saved')
    }
  }

  return (
    <>
      {header}
      {!isAdmin && <NotAllowed what="settings" />}

      {switchPending && live && (
        <Callout
          tone="info"
          title={`Switch to ${lbEngineLabel[saved]} is saved but not applied`}
          actions={isAdmin ? <Button size="sm" variant="primary" onClick={applyNow}>Apply switch</Button> : undefined}
        >
          {lbEngineLabel[live]} keeps running your backends until you apply.
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
                    {lbEngineLabel[e.value]}
                    {e.value === 'balancer' && <Badge tone="info">beta</Badge>}
                    {live === e.value && <Badge tone="ok">live</Badge>}
                    {live && live !== e.value && saved === e.value && <Badge tone="pending">pending apply</Badge>}
                  </span>
                }
                description={e.description}
              />
            ))}
          </div>
          <div className="small muted">
            Only one engine runs at a time. Relay stops the other engine and its container, and starts it again when you switch back, so switching takes a single apply.
          </div>
          {error && <div className="field-error">{error}</div>}
        </div>
      </Card>

      <Card title="Status">
        {(['haproxy', 'balancer'] as LBEngineName[]).map((name) => (
          <EngineRow key={name} name={name} state={engines?.[name]} live={live} backends={backends.length} loaded={!!engines} />
        ))}
      </Card>

      {selected === 'balancer' && (
        <Card title="Relay Balancer stats">
          {lbSettings?.statsEnabled ? (
            <>
              <Row title="Stats page" desc={<><span className="mono">http://{lbSettings.statsBind}/</span> · same numbers as HAProxy's stats page, laid out differently</>} />
              <Row
                title="Prometheus metrics"
                desc={lbSettings.prometheus ? <><span className="mono">http://{lbSettings.statsBind}/metrics</span> · <Link to="/docs/relay-balancer">scrape config</Link></> : 'Off · turn it on in Settings → Load balancer'}
              />
            </>
          ) : (
            <Row title="Stats endpoint is off" desc={<>Turn it on in <Link to="/settings/load-balancer">Settings → Load balancer</Link> to get the stats page and Prometheus metrics.</>} />
          )}
          <div className="card-body small faint" style={{ paddingTop: 0 }}>
            The stats address, access list and Prometheus switch are shared by both engines.
          </div>
        </Card>
      )}

      <Card title="What changes for your configuration">
        <Row title="Backends, frontends, routing rules, Expose" desc="Stored once in Relay and run by either engine" badge={<Badge tone="ok">shared</Badge>} />
        <Row title="Health checks, sticky sessions, weights, drain & maintenance" desc="Supported by both engines, including instant drain and weight changes" badge={<Badge tone="ok">both</Badge>} />
        <Row
          title="Path regex rules"
          desc={
            <>
              {regexFrontends.length === 0
                ? 'No frontend uses one'
                : <>{regexFrontends.length === 1 ? '1 frontend uses one' : `${regexFrontends.length} frontends use one`}: {regexFrontends.slice(0, 4).map((f, i) => (
                    <span key={f.id}>{i > 0 && ', '}<Link to={`/load-balancer/frontends?edit=${f.id}`} className="mono">{f.name}</Link></span>
                  ))}{regexFrontends.length > 4 && ` and ${regexFrontends.length - 4} more`}</>}
              {' '}· Relay Balancer uses RE2 syntax, so lookarounds and backreferences aren't supported
            </>
          }
          badge={<Badge tone={selected === 'balancer' && regexFrontends.length > 0 ? 'warn' : undefined}>{selected === 'balancer' ? 'RE2' : 'PCRE'}</Badge>}
        />
        <Row title="Stats & Prometheus" desc="Both engines · Relay Balancer's stats page looks different, the metrics and counters are the same" badge={<Badge tone="ok">both</Badge>} />
        <Row
          title="Seamless reloads setting"
          desc="HAProxy only · Relay Balancer always reloads without dropping connections"
          badge={<Badge>{selected === 'balancer' ? 'always on' : 'setting'}</Badge>}
        />
        <Row
          title="Updates"
          desc={<>HAProxy upgrades from <Link to="/settings/engines">Settings → Updates</Link>; Relay Balancer ships with Relay and updates with it</>}
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
            {dirty ? `Switch to ${lbEngineLabel[selected]}` : 'Save changes'}
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

function EngineRow({ name, state, live, backends, loaded }: { name: LBEngineName; state?: EngineState; live?: LBEngineName; backends: number; loaded: boolean }) {
  const label = lbEngineLabel[name]
  const active = live === name
  const container = `relay-${name}`
  let tone: 'ok' | 'warn' | 'danger' | 'muted' = 'muted'
  let desc: ReactNode = loaded ? '' : 'checking…'
  let badge: ReactNode = null
  if (state && state.standby) {
    desc = <>Stopped · the <span className="mono">{container}</span> container is stopped and holds no ports · it starts again when you switch back</>
    badge = <Badge>standby</Badge>
  } else if (state && !state.reachable && state.container === 'stopped') {
    tone = active && backends > 0 ? 'warn' : 'muted'
    desc = active && backends > 0
      ? <>The <span className="mono">{container}</span> container is stopped · Relay starts it within a few seconds</>
      : <>The <span className="mono">{container}</span> container is stopped · it starts once you create a backend</>
    badge = <Badge tone={active && backends > 0 ? 'warn' : undefined}>{active && backends > 0 ? 'starting' : 'stopped'}</Badge>
  } else if (state && !state.reachable) {
    tone = active ? 'danger' : 'warn'
    desc = <>Agent unreachable · check the <span className="mono">{container}</span> container</>
    badge = <Badge tone={active ? 'danger' : 'warn'}>unreachable</Badge>
  } else if (state && active) {
    const version = state.version ? <> · <span className="mono">{state.version}</span></> : null
    if (state.running) {
      tone = 'ok'
      desc = <>Running {backends === 1 ? '1 backend' : `${backends} backends`}{version}</>
      badge = <Badge tone="ok">serving</Badge>
    } else if (backends === 0) {
      desc = <>Idle · starts once you create the first backend and apply{version}</>
      badge = <Badge>idle</Badge>
    } else {
      tone = 'danger'
      desc = 'Selected but not running · see the engine banner for its error log'
      badge = <Badge tone="danger">not running</Badge>
    }
  } else if (state) {
    tone = state.running ? 'warn' : 'muted'
    desc = state.running
      ? `Still running · Relay stops it shortly because ${lbEngineLabel[other(name)]} is the load balancer engine`
      : <>Stopped · holds no ports · Relay stops the <span className="mono">{container}</span> container shortly</>
    badge = <Badge>{state.running ? 'stopping' : 'standby'}</Badge>
  } else if (loaded) {
    desc = <>No status yet · the <span className="mono">{container}</span> container may not exist on this install</>
    badge = <Badge>unknown</Badge>
  }
  return (
    <div className="toggle-row">
      <Dot tone={tone} />
      <div className="grow">
        <div className="toggle-title row gap-6">
          {label}
          {name === 'balancer' && <Badge tone="info">beta</Badge>}
        </div>
        <div className="toggle-desc">{desc}</div>
      </div>
      {badge}
    </div>
  )
}
