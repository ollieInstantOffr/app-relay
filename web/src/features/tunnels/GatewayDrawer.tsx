// Edit a tunnel gateway: address and transport, connection details, keys and pairing.
import { useState } from 'react'
import { Link } from 'react-router-dom'
import { Badge, Button, Callout, CopyButton, Drawer, Field, Input, Segmented, Status, ToggleCard, useToast } from '../../components/ui'
import { ApiError, api } from '../../lib/api'
import { useRole } from '../../lib/queries'
import { ago, bytes } from '../../lib/format'
import type { Gateway, GatewayTransport } from '../../lib/types'
import { TestResults } from './ConnectGatewayWizard'
import { gatewayState, shortFingerprint, transportLabel, tunnelsDocs, useInvalidateTunnels, useTunnelTests, type GatewayView } from './api'

export default function GatewayDrawer({ view, engineRunning, onClose, onPair, onPublish }: {
  view: GatewayView
  engineRunning: boolean
  onClose: () => void
  onPair: (id: string) => void
  onPublish: (id: string) => void
}) {
  const { canWrite } = useRole()
  const readOnly = !canWrite
  const toast = useToast()
  const invalidate = useInvalidateTunnels()
  const [draft, setDraft] = useState<Gateway>(() => ({ ...view }))
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [busy, setBusy] = useState(false)
  const st = view.status
  const state = gatewayState(view, st, engineRunning)
  const paired = view.pairState === 'paired'
  const tests = useTunnelTests(view.id)
  const testDomains = view.published.hosts.filter((h) => !h.startsWith('*'))
  const tested = testDomains.filter((d) => tests.results[d])
  const dirty = draft.name !== view.name || draft.address !== view.address || draft.transport !== view.transport || draft.enabled !== view.enabled

  const save = async () => {
    setBusy(true)
    setErrors({})
    try {
      await api.put<Gateway>(`/api/gateways/${view.id}`, draft)
      invalidate()
      toast.success('Gateway saved', `${draft.name} takes effect immediately.`)
      onClose()
    } catch (err) {
      if (err instanceof ApiError && err.fields) setErrors(err.fields)
      else toast.error(err, 'Could not save gateway')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Drawer
      open
      onClose={onClose}
      title={
        <span className="row gap-10" style={{ alignItems: 'baseline' }}>
          {readOnly ? 'Gateway' : 'Edit gateway'} <span className="mono muted" style={{ fontSize: 14, fontWeight: 500 }}>{view.name}</span>
        </span>
      }
      subtitle="A public server this Relay dials out to"
      headerExtra={<Status tone={state.tone}>{state.label}</Status>}
      footer={
        <>
          <span className="small faint">Gateway changes don't need an apply</span>
          <span className="spacer" />
          <Button size="md" onClick={onClose}>{readOnly || !dirty ? 'Close' : 'Cancel'}</Button>
          {!readOnly && <Button size="md" variant="primary" loading={busy} disabled={!dirty} onClick={save}>Save</Button>}
        </>
      }
    >
      <div className="grid-2">
        <Field label="Name" error={errors.name}>
          <Input mono value={draft.name} disabled={readOnly} invalid={!!errors.name} onChange={(e) => setDraft({ ...draft, name: e.target.value })} />
        </Field>
        <Field label="Transport" error={errors.transport}>
          <Segmented
            value={draft.transport}
            disabled={readOnly}
            onChange={(transport) => setDraft({ ...draft, transport })}
            options={(['auto', 'quic', 'tcp'] as GatewayTransport[]).map((v) => ({ value: v, label: transportLabel[v] }))}
          />
        </Field>
      </div>
      <Field label="Address" error={errors.address} hint="Host name or IP of the server; port 7443 unless you add :port">
        <Input mono value={draft.address} disabled={readOnly} invalid={!!errors.address} onChange={(e) => setDraft({ ...draft, address: e.target.value.trim() })} />
      </Field>
      <ToggleCard
        title="Enabled"
        description="A disabled gateway is disconnected; hosts published through it are unreachable from the internet"
        checked={draft.enabled}
        disabled={readOnly}
        onChange={(enabled) => setDraft({ ...draft, enabled })}
      />

      {state.detail && state.tone !== 'ok' && <Callout tone={state.tone === 'danger' ? 'danger' : 'info'}>{state.detail}</Callout>}

      <div className="col gap-8">
        <div className="section-title">Connection</div>
        <div className="kv tun-kv">
          <span className="k">Status</span>
          <span>{state.label}{st?.state === 'connected' && st.connectedAt ? ` · since ${ago(st.connectedAt)}` : ''}</span>
          <span className="k">Transport</span>
          <span>{st?.transport ? st.transport.toUpperCase() : transportLabel[view.transport]}{st?.rttMs ? ` · ${Math.round(st.rttMs)} ms round trip` : ''}</span>
          <span className="k">Gateway version</span>
          <span className="mono">{st?.version || view.version || '—'}</span>
          <span className="k">Public IPs</span>
          <span className="mono">{(st?.publicIps?.length ? st.publicIps : view.publicIps).join(', ') || '—'}</span>
          <span className="k">Traffic</span>
          <span>{st ? `${bytes(st.bytesIn)} in · ${bytes(st.bytesOut)} out · ${st.activeStreams} open · ${st.rejected} rejected` : '—'}</span>
          {st && st.reconnects > 0 && (
            <>
              <span className="k">Reconnects</span>
              <span>{st.reconnects}{st.lastErrorAt ? ` · last error ${ago(st.lastErrorAt)}` : ''}</span>
            </>
          )}
        </div>
        {st?.portErrors?.map((pe) => (
          <Callout key={pe.port} tone="warn">Port {pe.port}: {pe.error}</Callout>
        ))}
      </div>

      <div className="col gap-8">
        <div className="row between">
          <div className="section-title">Published through this gateway</div>
          <div className="row gap-6">
            {testDomains.length > 0 && <Button size="sm" onClick={() => testDomains.forEach(tests.test)}>Test from the internet</Button>}
            {canWrite && paired && <Button size="sm" onClick={() => onPublish(view.id)}>Publish hosts…</Button>}
          </div>
        </div>
        {view.published.hosts.length + view.published.streams.length === 0 ? (
          <div className="small faint">Nothing yet · use <span className="medium">Publish hosts</span>, or choose <span className="medium">Publish through tunnel</span> in a host or TCP stream.</div>
        ) : (
          <div className="row wrap gap-6">
            {view.published.hosts.map((h) => <Badge key={h}>{h}</Badge>)}
            {view.published.streams.map((s) => <Badge key={s} tone="info">{s}</Badge>)}
          </div>
        )}
        {tested.length > 0 && <TestResults domains={tested} results={tests.results} onTest={tests.test} />}
      </div>

      <div className="col gap-8">
        <div className="section-title">Pairing</div>
        <div className="kv tun-kv">
          <span className="k">State</span>
          <span>{paired ? `paired ${ago(view.pairedAt)}` : 'waiting for the gateway'}</span>
          <span className="k">Gateway key</span>
          <span className="row gap-6">
            <span className="mono" title={view.gatewayPin}>{shortFingerprint(view.gatewayPin)}</span>
            {view.gatewayPin && <CopyButton text={view.gatewayPin} />}
          </span>
          <span className="k">This Relay's key</span>
          <span className="row gap-6">
            <span className="mono" title={view.homeFingerprint}>{shortFingerprint(view.homeFingerprint)}</span>
            {view.homeFingerprint && <CopyButton text={view.homeFingerprint} />}
          </span>
        </div>
        {canWrite && (
          <div className="row gap-8">
            <Button onClick={() => onPair(view.id)}>{paired ? 'Pair again…' : 'Continue setup…'}</Button>
            {paired && <span className="small faint">Pairing again replaces the keys. The new install command resets the gateway for you.</span>}
          </div>
        )}
      </div>

      <Callout tone="warn" title="Trust in the gateway server">
        The gateway can't read HTTPS traffic, but whoever controls the server receives port 80 for your domains and could
        request certificates for them. Prefer DNS-01 certificates and a CAA record that allows only your account.{' '}
        <Link to={tunnelsDocs}>Read more</Link>
      </Callout>
    </Drawer>
  )
}
