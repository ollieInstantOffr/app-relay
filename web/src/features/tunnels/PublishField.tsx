// "Publish through tunnel" for the host and stream drawers.
import { Link } from 'react-router-dom'
import { Button, Dot, Field, Select } from '../../components/ui'
import { useEntities } from '../../lib/queries'
import { gatewayState, useTunnels } from './api'
import './tunnels.css'

/** Offers to set up a tunnel until a gateway exists. */
export function PublishField({ value, onChange, readOnly, error, disabled, note }: {
  value?: string
  onChange: (gatewayId: string | undefined) => void
  readOnly: boolean
  error?: string
  /** Can't be published (e.g. UDP streams); a selected gateway can still be removed. */
  disabled?: boolean
  note?: string
}) {
  const gateways = useEntities('gateways').data ?? []
  const tunnels = useTunnels(15_000).data
  if (gateways.length === 0 && !value) {
    if (readOnly || disabled) return null
    return (
      <Field label="Publish through tunnel">
        <div className="tun-publish-cta">
          <span className="grow">Make this reachable from the internet without port forwarding, through your own server.</span>
          <Link to="/tunnels?connect=new"><Button size="sm">Set up a tunnel</Button></Link>
        </div>
      </Field>
    )
  }

  const selected = gateways.find((g) => g.id === value)
  const view = tunnels?.gateways.find((g) => g.id === value)
  const state = view ? gatewayState(view, view.status, !!tunnels?.engine) : undefined
  const options = gateways.map((g) => ({ value: g.id, label: g.pairState === 'paired' ? g.name : `${g.name} · not paired yet` }))
  if (value && !selected) options.push({ value, label: gateways.length ? 'Missing gateway' : 'Loading…' })

  return (
    <Field
      label="Publish through tunnel"
      error={error}
      hint={note ?? (value ? undefined : 'Reachable from the internet through a gateway, without port forwarding')}
    >
      <Select value={value ?? ''} placeholder="Not published" options={options} disabled={readOnly || (disabled && !value)} onChange={(v) => onChange(v || undefined)} />
      {value && selected && (
        <div className="tun-publish-status">
          <Dot tone={state?.tone ?? 'muted'} />
          <span>
            {state?.label === 'connected'
              ? `${selected.name} is connected · applies with your next apply`
              : state?.label === 'idle'
                ? `Connects to ${selected.name} after you apply`
                : `${selected.name}: ${state?.label ?? '…'}`}
          </span>
          <Link to={`/tunnels?edit=${selected.id}`} className="muted" style={{ marginLeft: 'auto' }}>Gateway</Link>
        </div>
      )}
    </Field>
  )
}
