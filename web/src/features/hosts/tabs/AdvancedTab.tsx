// Owner: slice hosts. Host drawer · Advanced tab (design 18b).
import { useMemo, useRef, useState, type ReactNode } from 'react'
import { Link } from 'react-router-dom'
import { Badge, Button, Callout, Checkbox, ChipsInput, Field, Input, RadioCard, Select, Skeleton, Textarea, Toggle, ToggleCard, cx } from '../../../components/ui'
import { useEntities, useSettings, useUserDirectory } from '../../../lib/queries'
import type { DirectoryUser, HostMaintenance } from '../../../lib/types'
import { roleBadge } from '../../auth/authApi'
import { ErrorPagePreviewFrame } from '../../settings/ErrorPagePreview'
import { ERROR_PAGE_INFO, normalizeErrorPages, useErrorPagePreview } from '../../settings/errorPagesApi'
import { ConfigPreviewPanel } from '../ConfigPreview'
import type { HostFormCtx } from '../HostDrawer'
import { accessSummary, formatSize, parseSize, urlError, type SizeUnit } from '../lib'

const PROVIDERS: { value: string; label: string; hint?: string; verify: string; signIn: string }[] = [
  { value: 'relay', label: 'Relay login', hint: 'recommended', verify: '', signIn: '' },
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

/** First non-wildcard domain of the draft, for example URLs. */
function exampleOrigin(domains: string[]): string {
  const d = domains.find((x) => !x.startsWith('*.'))
  return `https://${d ?? 'your-domain'}`
}

// ---------------------------------------------------------------- maintenance mode

function MaintenanceSection({ ctx }: { ctx: HostFormCtx }) {
  const { draft, update, errors, readOnly } = ctx
  const m = draft.maintenance
  const lists = useEntities('access-lists').data ?? []
  const errorPages = useSettings('error_pages')
  const defaults = errorPages.data?.pages?.maintenance
  const [showPreview, setShowPreview] = useState(false)
  const set = (patch: Partial<HostMaintenance>) => update({ maintenance: { ...m, ...patch } })

  const previewReq = useMemo(
    () => (errorPages.data ? { settings: normalizeErrorPages(errorPages.data), page: 'maintenance' as const, maintenance: { title: m.title, message: m.message } } : null),
    [errorPages.data, m.title, m.message],
  )
  const preview = useErrorPagePreview(previewReq, m.enabled && showPreview)

  const missingList = !!m.bypassAccessListId && !lists.some((l) => l.id === m.bypassAccessListId)
  const listOptions = [
    { value: '', label: 'Nobody' },
    ...(missingList ? [{ value: m.bypassAccessListId!, label: '(deleted list)' }] : []),
    ...lists.map((l) => ({ value: l.id, label: l.name, hint: accessSummary(l) })),
  ]
  const domain = draft.domains.find((d) => !d.startsWith('*.')) ?? draft.domains[0] ?? 'this host'

  return (
    <Section
      title="Maintenance mode"
      badge={m.enabled ? <Badge tone="warn">on</Badge> : undefined}
      desc="Show a maintenance page instead of the app · visitors get 503"
      checked={m.enabled}
      disabled={readOnly}
      onToggle={(enabled) => set({ enabled })}
    >
      {m.enabled && (
        <>
          <Field
            label="Title"
            error={errors['maintenance.title']}
            hint={<>Optional · leave empty to use the page from <Link to="/settings/error-pages">Settings → Error pages</Link></>}
          >
            <Input
              value={m.title}
              invalid={!!errors['maintenance.title']}
              maxLength={120}
              placeholder={defaults?.title || ERROR_PAGE_INFO.maintenance.title}
              onChange={(e) => set({ title: e.target.value })}
            />
          </Field>
          <Field label="Message" error={errors['maintenance.message']}>
            <Textarea
              rows={3}
              value={m.message}
              invalid={!!errors['maintenance.message']}
              placeholder={defaults?.message || ERROR_PAGE_INFO.maintenance.message}
              onChange={(e) => set({ message: e.target.value })}
            />
          </Field>
          <Field label="Who still sees the app" error={errors['maintenance.bypassAccessListId']} hint="Addresses allowed by this access list skip the maintenance page">
            <Select
              value={m.bypassAccessListId ?? ''}
              options={listOptions}
              invalid={!!errors['maintenance.bypassAccessListId']}
              onChange={(v) => set({ bypassAccessListId: v || undefined })}
              aria-label="Bypass access list"
            />
          </Field>
          <Callout
            tone="info"
            actions={
              <Button size="sm" icon="reveal" onClick={() => setShowPreview((s) => !s)} disabled={!errorPages.data && !errorPages.isError}>
                {showPreview ? 'Hide preview' : 'Preview'}
              </Button>
            }
          >
            Visitors get HTTP 503 with the maintenance page. Certificate renewals and the Relay login page keep working. Starts on apply.
          </Callout>
          {showPreview &&
            (errorPages.isError ? (
              <Callout tone="warn">Couldn't load the error page settings for the preview.</Callout>
            ) : (
              <ErrorPagePreviewFrame state={preview} label={`${domain} · 503`} height={340} />
            ))}
        </>
      )}
    </Section>
  )
}

// ---------------------------------------------------------------- Relay login

function UserRowItem({ user, checked, disabled, onChange }: { user: DirectoryUser; checked: boolean; disabled: boolean; onChange: (v: boolean) => void }) {
  return (
    <label className={cx('fa-user', user.disabled && 'dim')}>
      <Checkbox checked={checked} disabled={disabled} onChange={onChange} />
      <span className="fa-user-avatar">{user.username.slice(0, 1).toUpperCase()}</span>
      <span className="grow" style={{ minWidth: 0 }}>
        <span className="fa-user-name truncate">{user.username}</span>
        {user.email && <span className="fa-user-sub truncate">{user.email}</span>}
      </span>
      {user.disabled && <Badge>disabled</Badge>}
      <Badge tone={user.role === 'admin' ? 'dark' : user.role === 'member' ? 'outline' : undefined}>{roleBadge[user.role] ?? user.role}</Badge>
    </label>
  )
}

function RelayLoginOptions({ ctx }: { ctx: HostFormCtx }) {
  const { draft, update, errors, readOnly } = ctx
  const fa = draft.forwardAuth
  const allowed = fa.allowedUsers ?? []
  const [restrict, setRestrict] = useState(allowed.length > 0)
  const [query, setQuery] = useState('')
  const directory = useUserDirectory()
  const origin = exampleOrigin(draft.domains)
  const setAllowed = (ids: string[]) => update({ forwardAuth: { ...fa, allowedUsers: ids } })

  const users = useMemo(
    () => [...(directory.data ?? [])].sort((a, b) => Number(a.disabled) - Number(b.disabled) || a.username.localeCompare(b.username)),
    [directory.data],
  )
  const q = query.trim().toLowerCase()
  const shown = q ? users.filter((u) => `${u.username} ${u.email} ${roleBadge[u.role] ?? u.role}`.toLowerCase().includes(q)) : users
  const unknown = directory.data ? allowed.filter((id) => !users.some((u) => u.id === id)) : []
  const allowedErr = errors['forwardAuth.allowedUsers']

  return (
    <>
      <Field label="Who can sign in" error={allowedErr}>
        <div className="grid-2">
          <RadioCard
            selected={!restrict}
            disabled={readOnly}
            title="Every Relay user"
            description="Any enabled account"
            onSelect={() => {
              setRestrict(false)
              if (allowed.length) setAllowed([])
            }}
          />
          <RadioCard selected={restrict} disabled={readOnly} title="Only selected users" description="Pick people from the list" onSelect={() => setRestrict(true)} />
        </div>
      </Field>

      {restrict && (
        <div className="fa-users">
          {users.length > 8 && (
            <div className="fa-users-search">
              <Input inputSize="sm" value={query} placeholder="Filter users" aria-label="Filter users" onChange={(e) => setQuery(e.target.value)} />
            </div>
          )}
          <div className="fa-users-list">
            {directory.isLoading && (
              <div className="col gap-8" style={{ padding: 12 }}>
                <Skeleton height={28} />
                <Skeleton height={28} />
              </div>
            )}
            {directory.isError && <div className="fa-users-empty danger-text">Couldn't load Relay users. Close the drawer and try again.</div>}
            {directory.data && shown.length === 0 && <div className="fa-users-empty">{q ? 'No users match.' : 'No Relay users yet.'}</div>}
            {shown.map((u) => (
              <UserRowItem
                key={u.id}
                user={u}
                checked={allowed.includes(u.id)}
                disabled={readOnly}
                onChange={(v) => setAllowed(v ? [...allowed, u.id] : allowed.filter((id) => id !== u.id))}
              />
            ))}
            {unknown.map((id) => (
              <label key={id} className="fa-user dim">
                <Checkbox checked disabled={readOnly} onChange={() => setAllowed(allowed.filter((x) => x !== id))} />
                <span className="grow" style={{ minWidth: 0 }}>
                  <span className="fa-user-name">Deleted user</span>
                  <span className="fa-user-sub mono truncate">{id}</span>
                </span>
              </label>
            ))}
          </div>
          <div className="fa-users-foot">
            {allowed.length === 0 ? (
              <span className="warn-text">Nobody picked yet · until you pick someone, every Relay user can sign in</span>
            ) : (
              <span>
                {allowed.length} {allowed.length === 1 ? 'person' : 'people'} can sign in
              </span>
            )}
          </div>
        </div>
      )}

      <Callout tone="info" title="How signing in works">
        People open the app and land on <span className="mono">{origin}/.relay/login</span>. They sign in with their Relay username and password, plus their
        authenticator code if they use 2FA. Sign out at <span className="mono">{origin}/.relay/logout</span>.
        <div style={{ marginTop: 6 }}>
          For people who should only use apps, give them the <span className="medium">App access only</span> role in <Link to="/settings/users">Settings → Users &amp; access</Link>.
        </div>
      </Callout>
    </>
  )
}

// ---------------------------------------------------------------- tab

export function AdvancedTab({ ctx }: { ctx: HostFormCtx }) {
  const { draft, update, errors, readOnly, preview } = ctx
  const lists = useEntities('access-lists').data ?? []
  // Selected engine (Settings → Proxy engine), not the live one.
  const edge = useSettings('general').data?.proxyEngine === 'edge'
  const fa = draft.forwardAuth
  const rl = draft.rateLimit
  const gb = draft.geoBlock
  const [size, setSize] = useState(() => parseSize(draft.maxBodySize))

  const relay = fa.provider === 'relay'
  // URLs of the last SSO provider, restored when switching away from Relay login.
  const lastSso = useRef<{ provider: string; verifyUrl: string; signInUrl: string } | null>(
    relay ? null : { provider: fa.provider, verifyUrl: fa.verifyUrl, signInUrl: fa.signInUrl ?? '' },
  )

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

  const changeProvider = (value: string) => {
    if (value === fa.provider) return
    if (value === 'relay') {
      lastSso.current = { provider: fa.provider, verifyUrl: fa.verifyUrl, signInUrl: fa.signInUrl ?? '' }
      update({ forwardAuth: { ...fa, provider: value, verifyUrl: '', signInUrl: '' } })
      return
    }
    const next = PROVIDERS.find((p) => p.value === value)!
    const from = relay ? lastSso.current : { provider: fa.provider, verifyUrl: fa.verifyUrl, signInUrl: fa.signInUrl ?? '' }
    const prev = PROVIDERS.find((p) => p.value === from?.provider)
    const verifyUrl = !from?.verifyUrl || from.verifyUrl === prev?.verify ? next.verify : from.verifyUrl
    const signInUrl = !from?.signInUrl || from.signInUrl === prev?.signIn ? next.signIn : from.signInUrl
    update({ forwardAuth: { ...fa, provider: value, verifyUrl, signInUrl } })
  }

  return (
    <>
      <MaintenanceSection ctx={ctx} />

      <Section
        title="Authentication"
        desc="Require sign-in with Relay login or your SSO provider, in addition to the access list"
        checked={fa.enabled}
        disabled={readOnly}
        onToggle={(enabled) =>
          update({ forwardAuth: { ...fa, enabled, verifyUrl: enabled && !fa.verifyUrl ? provider.verify : fa.verifyUrl, signInUrl: enabled && !fa.signInUrl ? provider.signIn : fa.signInUrl } })
        }
      >
        {fa.enabled && (
          <>
            <Field label="Provider" error={errors['forwardAuth.provider']} hint={relay ? 'Built in: people sign in with a Relay account' : undefined}>
              <Select value={fa.provider} options={PROVIDERS.map((p) => ({ value: p.value, label: p.label, hint: p.hint }))} onChange={changeProvider} />
            </Field>
            {relay ? (
              <RelayLoginOptions ctx={ctx} />
            ) : (
              <>
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
              </>
            )}
            <div className="grid-2">
              <ToggleCard
                title="Pass Remote-User"
                description={relay ? 'Username, email and name headers' : 'Username header to upstream'}
                checked={fa.passRemoteUser}
                disabled={readOnly}
                onChange={(passRemoteUser) => update({ forwardAuth: { ...fa, passRemoteUser } })}
              />
              <ToggleCard
                title="Pass Remote-Groups"
                description={relay ? "The user's Relay role" : 'Groups header to upstream'}
                checked={fa.passRemoteGroups}
                disabled={readOnly}
                onChange={(passRemoteGroups) => update({ forwardAuth: { ...fa, passRemoteGroups } })}
              />
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
        badge={edge ? <Badge>not enforced</Badge> : undefined}
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
        desc={edge ? 'Kept for nginx · skipped while Relay Edge is the proxy engine' : 'Escape hatch · inserted into the server block · validated on save'}
      >
        {edge && (
          <Callout tone="info" title="Skipped while Relay Edge is the proxy engine">
            {draft.customNginx.trim()
              ? <>The snippet stays saved and applies again if you switch back to nginx in <Link to="/settings/proxy">Settings → Proxy engine</Link>.</>
              : <>Snippets you add here are saved for nginx. Relay Edge doesn't run raw nginx directives; use the options above instead.</>}
          </Callout>
        )}
        <Field error={errors.customNginx} hint={edge ? 'Saved for nginx · not run by Relay Edge' : 'Checked for balanced braces on save · nginx -t validates it before every reload'}>
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
      </Section>

      <ConfigPreviewPanel state={preview} />
    </>
  )
}
