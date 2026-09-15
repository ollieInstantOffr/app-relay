// Owner: slice hosts. Host drawer · Advanced tab (design 18b).
import { useState, type ReactNode } from 'react'
import { Link } from 'react-router-dom'
import { Badge, Button, Callout, ChipsInput, Field, Input, Select, Textarea, Toggle, ToggleCard } from '../../../components/ui'
import { useEntities, useSettings } from '../../../lib/queries'
import { ConfigPreviewPanel } from '../ConfigPreview'
import type { HostFormCtx } from '../HostDrawer'
import { formatSize, parseSize, urlError, type SizeUnit } from '../lib'

const PROVIDERS: { value: string; label: string; verify: string; signIn: string }[] = [
  { value: 'authelia', label: 'Authelia', verify: 'http://authelia:9091/api/verify?rd=https://auth.home.lan', signIn: '' },
  {
    value: 'authentik',
    label: 'Authentik',
    verify: 'http://authentik:9000/outpost.goauthentik.io/auth/nginx',
    signIn: 'https://auth.home.lan/outpost.goauthentik.io/start?rd=$scheme://$http_host$request_uri',
  },
  {
    value: 'oauth2-proxy',
    label: 'oauth2-proxy',
    verify: 'http://oauth2-proxy:4180/oauth2/auth',
    signIn: 'https://auth.home.lan/oauth2/start?rd=$scheme://$host$request_uri',
  },
  { value: 'custom', label: 'Custom', verify: '', signIn: '' },
]

function Section({ title, desc, badge, checked, onToggle, disabled, children }: {
  title: ReactNode
  desc?: ReactNode
  badge?: ReactNode
  checked?: boolean
  onToggle?: (v: boolean) => void
  disabled?: boolean
  children?: ReactNode
}) {
  return (
    <div className="hosts-section">
      <div className="hosts-section-head">
        <div className="grow">
          <div className="section-title row gap-8">
            {title}
            {badge}
          </div>
          {desc && <div className="section-desc">{desc}</div>}
        </div>
        {onToggle && <Toggle checked={!!checked} onChange={onToggle} disabled={disabled} label={typeof title === 'string' ? title : undefined} />}
      </div>
      {children}
    </div>
  )
}

function NumInput({ value, onChange, suffix, placeholder, invalid, min = 0 }: {
  value: number
  onChange: (v: number) => void
  suffix?: string
  placeholder?: string
  invalid?: boolean
  min?: number
}) {
  return (
    <div className="hosts-num-suffix">
      <Input
        mono
        type="number"
        min={min}
        value={value || ''}
        placeholder={placeholder}
        invalid={invalid}
        style={suffix ? { paddingRight: suffix.length * 7.5 + 22 } : undefined}
        onChange={(e) => onChange(e.target.value === '' ? 0 : Math.max(0, Math.floor(Number(e.target.value))))}
      />
      {suffix && <span className="suffix">{suffix}</span>}
    </div>
  )
}

export function AdvancedTab({ ctx }: { ctx: HostFormCtx }) {
  const { draft, update, errors, readOnly, preview } = ctx
  const lists = useEntities('access-lists').data ?? []
  // Selected engine (Settings → General), not the live one: saving is validated against it.
  const edge = useSettings('general').data?.proxyEngine === 'edge'
  const fa = draft.forwardAuth
  const rl = draft.rateLimit
  const gb = draft.geoBlock
  const [size, setSize] = useState(() => parseSize(draft.maxBodySize))

  const provider = PROVIDERS.find((p) => p.value === fa.provider) ?? PROVIDERS[PROVIDERS.length - 1]
  const verifyErr = errors['forwardAuth.verifyUrl'] || (fa.verifyUrl ? urlError(fa.verifyUrl) : '')
  const signInErr = errors['forwardAuth.signInUrl'] || (fa.signInUrl ? urlError(fa.signInUrl) : '')
  const countryErr =
    errors['geoBlock.allowCountries'] || Object.entries(errors).find(([k]) => k.startsWith('geoBlock.allowCountries.'))?.[1]
  const listOptions = [{ value: '', label: 'None' }, ...lists.map((l) => ({ value: l.id, label: l.name }))]

  const setSizeParts = (num: string, unit: SizeUnit) => {
    setSize({ num, unit })
    update({ maxBodySize: formatSize(num, unit) })
  }

  return (
    <>
      <Section
        title="Authentication"
        desc="Forward-auth to an SSO provider, in addition to the access list"
        checked={fa.enabled}
        disabled={readOnly}
        onToggle={(enabled) =>
          update({ forwardAuth: { ...fa, enabled, verifyUrl: enabled && !fa.verifyUrl ? provider.verify : fa.verifyUrl, signInUrl: enabled && !fa.signInUrl ? provider.signIn : fa.signInUrl } })
        }
      >
        {fa.enabled && (
          <>
            <Field label="Provider" error={errors['forwardAuth.provider']}>
              <Select
                value={fa.provider}
                options={PROVIDERS.map((p) => ({ value: p.value, label: p.label }))}
                onChange={(value) => {
                  const next = PROVIDERS.find((p) => p.value === value)!
                  const verifyUrl = !fa.verifyUrl || fa.verifyUrl === provider.verify ? next.verify : fa.verifyUrl
                  const signInUrl = !fa.signInUrl || fa.signInUrl === provider.signIn ? next.signIn : fa.signInUrl
                  update({ forwardAuth: { ...fa, provider: value, verifyUrl, signInUrl } })
                }}
              />
            </Field>
            <Field label="Verify URL" error={verifyErr} hint={provider.value === 'custom' ? 'Returns 2xx when the request is signed in' : `Adjust host and port to where ${provider.label} runs`}>
              <Input
                mono
                value={fa.verifyUrl}
                invalid={!!verifyErr}
                placeholder={provider.verify || 'http://sso.home.lan/verify'}
                onChange={(e) => update({ forwardAuth: { ...fa, verifyUrl: e.target.value.trim() } })}
              />
            </Field>
            <Field label="Sign-in URL" error={signInErr} hint="Optional · where signed-out browsers are sent">
              <Input
                mono
                value={fa.signInUrl ?? ''}
                invalid={!!signInErr}
                placeholder={provider.signIn || 'https://auth.home.lan'}
                onChange={(e) => update({ forwardAuth: { ...fa, signInUrl: e.target.value.trim() } })}
              />
            </Field>
            <div className="grid-2">
              <ToggleCard title="Pass Remote-User" description="Username header to upstream" checked={fa.passRemoteUser} disabled={readOnly} onChange={(passRemoteUser) => update({ forwardAuth: { ...fa, passRemoteUser } })} />
              <ToggleCard title="Pass Remote-Groups" description="Groups header to upstream" checked={fa.passRemoteGroups} disabled={readOnly} onChange={(passRemoteGroups) => update({ forwardAuth: { ...fa, passRemoteGroups } })} />
              <ToggleCard title="Skip for /.well-known/" description="ACME & discovery paths" checked={fa.skipWellKnown} disabled={readOnly} onChange={(skipWellKnown) => update({ forwardAuth: { ...fa, skipWellKnown } })} />
            </div>
          </>
        )}
      </Section>

      <Section
        title="Rate limiting"
        desc="Per client IP · 429 when exceeded"
        checked={rl.enabled}
        disabled={readOnly}
        onToggle={(enabled) => update({ rateLimit: { ...rl, enabled, requestsPerSecond: rl.requestsPerSecond || 30, burst: rl.burst || 60 } })}
      >
        {rl.enabled && (
          <div className="grid-3">
            <Field label="Requests" error={errors['rateLimit.requestsPerSecond']}>
              <NumInput value={rl.requestsPerSecond} min={1} suffix="/ s" invalid={!!errors['rateLimit.requestsPerSecond']} onChange={(requestsPerSecond) => update({ rateLimit: { ...rl, requestsPerSecond } })} />
            </Field>
            <Field label="Burst" error={errors['rateLimit.burst']}>
              <NumInput value={rl.burst} placeholder="0" invalid={!!errors['rateLimit.burst']} onChange={(burst) => update({ rateLimit: { ...rl, burst } })} />
            </Field>
            <Field label="Exempt" error={errors['rateLimit.exemptAccessListId']}>
              <Select value={rl.exemptAccessListId ?? ''} options={listOptions} onChange={(v) => update({ rateLimit: { ...rl, exemptAccessListId: v || undefined } })} aria-label="Exempt access list" />
            </Field>
          </div>
        )}
      </Section>

      <Section
        title="Geo-block"
        desc={edge ? 'Allow only selected countries · not enforced by Relay Edge: rules are saved but skipped' : 'Allow only selected countries · needs the GeoIP country database'}
        checked={gb.enabled}
        disabled={readOnly}
        onToggle={(enabled) => update({ geoBlock: { ...gb, enabled } })}
      >
        {gb.enabled && (
          <Field error={countryErr} hint="Two-letter ISO country codes · press Enter to add">
            <ChipsInput
              values={gb.allowCountries}
              invalid={!!countryErr}
              placeholder="DE, AT, CH…"
              validate={(v) => /^[a-zA-Z]{2}$/.test(v)}
              onChange={(v) => update({ geoBlock: { ...gb, allowCountries: [...new Set(v.map((x) => x.toUpperCase()))] } })}
            />
          </Field>
        )}
      </Section>

      <div className="hosts-section">
        <ToggleCard title="Hide from search engines" description="X-Robots-Tag: noindex" checked={draft.noIndex} disabled={readOnly} onChange={(noIndex) => update({ noIndex })} />
        <div className="grid-2">
          <Field label="Max upload size" error={errors.maxBodySize} hint={size.unit === '' ? 'Default · 1 MB' : size.unit === '0' ? 'No limit on request bodies' : undefined}>
            <div className="hosts-size">
              <Input
                mono
                type="number"
                min={1}
                value={size.num}
                disabled={size.unit === '' || size.unit === '0'}
                invalid={!!errors.maxBodySize}
                placeholder={size.unit === '' ? '1' : size.unit === '0' ? '∞' : '10'}
                onChange={(e) => setSizeParts(e.target.value.replace(/\D/g, ''), size.unit)}
                aria-label="Max upload size"
              />
              <Select
                value={size.unit}
                options={[
                  { value: '', label: 'Default' },
                  { value: 'k', label: 'KB' },
                  { value: 'm', label: 'MB' },
                  { value: 'g', label: 'GB' },
                  { value: '0', label: 'Unlimited' },
                ]}
                onChange={(v) => {
                  const unit = v as SizeUnit
                  setSizeParts(unit === '' || unit === '0' ? '' : size.num || (unit === 'g' ? '1' : '100'), unit)
                }}
                aria-label="Size unit"
              />
            </div>
          </Field>
          <Field label="Proxy timeouts" error={errors.proxyReadTimeout || errors.proxySendTimeout} hint="Seconds · empty = 60 s">
            <div className="grid-2" style={{ gap: 8 }}>
              <NumInput value={draft.proxyReadTimeout} placeholder="60" suffix="s read" invalid={!!errors.proxyReadTimeout} onChange={(proxyReadTimeout) => update({ proxyReadTimeout })} />
              <NumInput value={draft.proxySendTimeout} placeholder="60" suffix="s send" invalid={!!errors.proxySendTimeout} onChange={(proxySendTimeout) => update({ proxySendTimeout })} />
            </div>
          </Field>
        </div>
      </div>

      <Section
        title="Custom nginx snippet"
        badge={<Badge>{edge ? 'nginx only' : 'advanced'}</Badge>}
        desc={edge ? 'Not available while Relay Edge is the proxy engine' : 'Escape hatch · inserted into the server block · validated on save'}
      >
        {edge ? (
          <>
            <Callout
              tone={draft.customNginx.trim() ? 'warn' : 'info'}
              title="Relay Edge doesn't run nginx snippets"
              actions={
                draft.customNginx.trim() && !readOnly ? (
                  <Button size="sm" onClick={() => update({ customNginx: '' })}>Remove snippet</Button>
                ) : undefined
              }
            >
              {draft.customNginx.trim() ? (
                <>This host can't be saved with its snippet. Remove it, or switch back to nginx in <Link to="/settings/general">Settings → General</Link>.</>
              ) : (
                <>Use the options above instead, or switch to nginx in <Link to="/settings/general">Settings → General</Link> to add raw directives.</>
              )}
            </Callout>
            {draft.customNginx && (
              <Field error={errors.customNginx} hint="Read-only while Relay Edge is the proxy engine">
                <Textarea mono rows={4} spellCheck={false} readOnly value={draft.customNginx} invalid={!!errors.customNginx} aria-label="Custom nginx snippet" />
              </Field>
            )}
          </>
        ) : (
          <Field error={errors.customNginx} hint="Checked for balanced braces on save · nginx -t validates it before every reload">
            <Textarea
              mono
              rows={6}
              spellCheck={false}
              value={draft.customNginx}
              invalid={!!errors.customNginx}
              placeholder={'add_header X-Robots-Tag "noindex" always;\nproxy_hide_header X-Powered-By;'}
              onChange={(e) => update({ customNginx: e.target.value })}
            />
          </Field>
        )}
      </Section>

      <ConfigPreviewPanel state={preview} />
    </>
  )
}
