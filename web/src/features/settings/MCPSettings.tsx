// Settings → MCP server (design 13). Owner: slice mcp.
import { useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { useQueryClient } from '@tanstack/react-query'
import {
  Badge, Button, Callout, Card, Checkbox, CodeBlock, ConfirmDialog, CopyButton, Field, Input, Segmented, Select, Skeleton,
  Toggle, cx, useToast,
} from '../../components/ui'
import CreateTokenDialog from '../auth/CreateTokenDialog'
import NotAllowed from '../auth/NotAllowed'
import { ApiError, errorMessage } from '../../lib/api'
import { ago } from '../../lib/format'
import { useEntities, useRole, useSaveSettings, useSettings } from '../../lib/queries'
import type { ApiToken, MCPSettings as Settings, ToolPermission } from '../../lib/types'
import { mcpKeys, resultClass, useDeleteToken, useMCPCalls, useMCPInfo, useMCPTokens, useMCPTools, type MCPInfo, type MCPTool } from '../mcp/api'
import '../mcp/mcp.css'

type Transport = 'http' | 'stdio'

// Tool kinds when /api/mcp/tools is unavailable.
const WRITE_TOOLS = new Set([
  'create_host',
  'update_host',
  'delete_host',
  'drain_server',
  'request_certificate',
  'apply_changes',
  'update_host_config',
  'create_redirect',
  'update_redirect',
  'create_access_list',
  'update_access_list',
  'create_stream',
  'update_stream',
  'create_backend',
  'update_backend',
  'create_frontend',
  'update_frontend',
  'expose_backend',
  'set_default_host',
  'update_settings',
  'set_proxy_engine',
  'discard_changes',
  'engine_action',
  'upgrade_engine',
  'upgrade_relay',
  'create_hosts_from_docker',
  'renew_certificate',
  'create_backup',
  'block_ip',
  'check_for_updates',
  'delete_redirect',
  'delete_access_list',
  'delete_stream',
  'delete_backend',
  'delete_frontend',
  'delete_certificate',
  'unblock_ip',
  'rollback_version',
  'create_dns_record',
  'update_dns_record',
  'delete_dns_record',
  'sync_dns',
])
// Everything else is a read tool (e.g. list_dns_zones, list_dns_records, check_dns).

export default function MCPSettings() {
  const { isAdmin, role } = useRole()
  if (role === undefined) return <Skeleton height={240} />
  if (!isAdmin) return <NotAllowed what="Only admins can change the MCP server settings." />
  return <MCPSettingsPage />
}

function canonical(s: Settings): string {
  return JSON.stringify({
    ...s,
    accessListId: s.accessListId || '',
    transports: [...(s.transports ?? [])].sort(),
    tools: Object.fromEntries(Object.entries(s.tools ?? {}).sort(([a], [b]) => a.localeCompare(b))),
  })
}

function MCPSettingsPage() {
  const settings = useSettings('mcp')
  const save = useSaveSettings('mcp')
  const info = useMCPInfo()
  const lists = useEntities('access-lists')
  const qc = useQueryClient()
  const toast = useToast()
  const [draft, setDraft] = useState<Settings | null>(null)
  const [fields, setFields] = useState<Record<string, string>>({})
  const [newToken, setNewToken] = useState<string>()

  useEffect(() => {
    if (settings.data && !draft) setDraft(settings.data)
  }, [settings.data, draft])

  if (settings.error) return <Callout tone="danger" title="Could not load MCP settings">{errorMessage(settings.error)}</Callout>
  if (!draft || !settings.data) return <Skeleton height={320} />

  const saved = settings.data
  const dirty = canonical(draft) !== canonical(saved)
  const set = (patch: Partial<Settings>) => setDraft((d) => (d ? { ...d, ...patch } : d))
  const transports = (draft.transports ?? []) as Transport[]
  const toggleTransport = (t: Transport) => {
    const next = transports.includes(t) ? transports.filter((x) => x !== t) : [...transports, t]
    if (next.length === 0) return // keep at least one transport
    set({ transports: next })
  }

  const onSave = async () => {
    setFields({})
    try {
      const res = await save.mutateAsync(draft)
      setDraft(res)
      qc.invalidateQueries({ queryKey: mcpKeys.info })
      qc.invalidateQueries({ queryKey: mcpKeys.tools })
      toast.success('MCP settings saved', res.enabled ? 'Connected assistants refresh their tool list automatically.' : 'The MCP endpoint is turned off.')
    } catch (err) {
      if (err instanceof ApiError && err.fields) setFields(err.fields)
      toast.error(err, 'Could not save MCP settings')
    }
  }

  const endpoint = info.data?.endpoint ?? ''
  const accessOptions = [{ value: '', label: 'No restriction' }, ...(lists.data ?? []).map((l) => ({ value: l.id, label: l.name }))]

  return (
    <div className="mcp-settings">
      <div className="mcp-main">
        <div className="row-top gap-16">
          <div className="grow">
            <div className="h1">MCP server</div>
            <div className="muted mcp-lede">
              Let AI assistants manage Relay through the Model Context Protocol — create hosts, inspect logs, drain servers — under the
              permissions you set here.
            </div>
          </div>
          <label className="mcp-enabled">
            Enabled
            <Toggle checked={draft.enabled} onChange={(v) => set({ enabled: v })} label="MCP server enabled" />
          </label>
        </div>

        <Card title="Endpoint" pad>
          <div className="col gap-14">
            <div className="mcp-endpoint-grid">
              <Field label="URL">
                <div className="mcp-url">
                  <span title={endpoint}>{endpoint || '—'}</span>
                  {endpoint && <CopyButton text={endpoint} />}
                </div>
              </Field>
              <Field label="Transport" error={fields.transports}>
                <div className="segmented mcp-transports" role="group" aria-label="Transports">
                  {(['http', 'stdio'] as const).map((t) => (
                    <button key={t} type="button" className={cx(transports.includes(t) && 'active')} aria-pressed={transports.includes(t)} onClick={() => toggleTransport(t)}>
                      {t === 'http' ? 'HTTP' : 'stdio'}
                    </button>
                  ))}
                </div>
              </Field>
            </div>
            <div className="mcp-restrict">
              <div className="grow">
                <div className="toggle-title">Restrict to access list</div>
                <div className="toggle-desc">Only these clients can reach the endpoint</div>
              </div>
              <Select
                inputSize="sm"
                mono
                value={draft.accessListId ?? ''}
                options={accessOptions}
                invalid={!!fields.accessListId}
                onChange={(v) => set({ accessListId: v || undefined })}
                aria-label="Access list"
              />
            </div>
            {fields.accessListId && <div className="field-error">{fields.accessListId}</div>}
            {transports.includes('stdio') && (
              <div className="small muted">
                stdio for local clients: <span className="mono">{info.data?.stdioCommand ?? 'docker exec -i relay relay mcp-stdio --token rl_mcp_…'}</span>
              </div>
            )}
          </div>
        </Card>

        <TokensCard onCreated={setNewToken} />

        <ToolsCard draft={draft} set={set} fields={fields} />

        <div className="mcp-savebar">
          {dirty && <span className="small muted">Unsaved changes</span>}
          <span className="grow" />
          <Button disabled={!dirty || save.isPending} onClick={() => { setDraft(saved); setFields({}) }}>
            Discard
          </Button>
          <Button variant="primary" disabled={!dirty} loading={save.isPending} onClick={onSave}>
            Save changes
          </Button>
        </div>
      </div>

      <div className="mcp-side">
        <ConnectCard info={info.data} transports={(saved.transports ?? ['http']) as Transport[]} token={newToken} enabled={saved.enabled} />
        <RecentCalls />
      </div>
    </div>
  )
}

// ---------------------------------------------------------------- tokens

function TokensCard({ onCreated }: { onCreated: (token: string) => void }) {
  const tokens = useMCPTokens()
  const del = useDeleteToken()
  const toast = useToast()
  const [creating, setCreating] = useState(false)
  const [target, setTarget] = useState<ApiToken | null>(null)
  const now = Date.now()
  const rows = (tokens.data ?? []).filter((t) => !t.surfaces || t.surfaces.includes('mcp'))
  const inactive = (t: ApiToken) => !!t.revokedAt || (!!t.expiresAt && new Date(t.expiresAt).getTime() <= now)

  return (
    <Card title="API tokens" actions={<Button size="sm" variant="ghost" icon="plus" onClick={() => setCreating(true)}>Generate token</Button>}>
      {tokens.isLoading ? (
        <div className="card-body"><Skeleton height={44} /></div>
      ) : tokens.error ? (
        <div className="card-body"><Callout tone="warn">API tokens are unavailable: {errorMessage(tokens.error)}</Callout></div>
      ) : rows.length === 0 ? (
        <div className="card-body small muted">No MCP tokens yet — generate one to connect an assistant.</div>
      ) : (
        rows.map((t) => {
          const off = inactive(t)
          const status = t.revokedAt ? 'revoked' : off ? 'expired' : t.lastUsedAt ? `last used ${ago(t.lastUsedAt)}` : 'never used'
          return (
            <div key={t.id} className={cx('mcp-token-row', off && 'inactive')}>
              <div className="grow" style={{ minWidth: 0 }}>
                <div className="mcp-token-name">{t.name}</div>
                <div className="mono small faint mcp-token-meta">
                  {t.prefix || 'rl_mcp_'}••••••••••••{t.last4} · {status}
                  {t.limitTo?.length ? ` · limited to ${t.limitTo.join(', ')}` : ''}
                </div>
              </div>
              <Badge>{t.scope === 'write' ? 'read + write' : 'read-only'}</Badge>
              <Button size="sm" variant="ghost" className={off ? undefined : 'revoke'} onClick={() => setTarget(t)}>
                {off ? 'Delete' : 'Revoke'}
              </Button>
            </div>
          )
        })
      )}
      <CreateTokenDialog
        open={creating}
        onClose={() => setCreating(false)}
        surface="mcp"
        onCreated={(t) => {
          tokens.refetch()
          if (t.token) onCreated(t.token)
        }}
      />
      <ConfirmDialog
        open={!!target}
        onClose={() => setTarget(null)}
        danger
        title={target && inactive(target) ? `Delete ${target.name}?` : `Revoke ${target?.name ?? 'token'}?`}
        message={target && inactive(target) ? 'The token is removed from the list.' : 'Assistants using this token lose access immediately. This cannot be undone.'}
        confirmLabel={target && inactive(target) ? 'Delete token' : 'Revoke token'}
        onConfirm={async () => {
          if (!target) return
          try {
            await del.mutateAsync(target.id)
            toast.success(inactive(target) ? 'Token deleted' : 'Token revoked', `${target.name} can no longer reach the MCP endpoint.`)
          } catch (err) {
            toast.error(err, 'Could not remove the token')
          }
        }}
      />
    </Card>
  )
}

// ---------------------------------------------------------------- tools

interface ToolRow {
  name: string
  kind: 'read' | 'write'
  description: string
  defaultPermission: ToolPermission
}

function orderTools(rows: ToolRow[]): ToolRow[] {
  const reads = rows.filter((r) => r.kind === 'read')
  const writes = rows.filter((r) => r.kind === 'write')
  const out: ToolRow[] = []
  for (let i = 0; i < Math.max(reads.length, writes.length); i++) {
    if (reads[i]) out.push(reads[i])
    if (writes[i]) out.push(writes[i])
  }
  return out
}

function ToolsCard({ draft, set, fields }: { draft: Settings; set: (p: Partial<Settings>) => void; fields: Record<string, string> }) {
  const tools = useMCPTools()
  const rows = useMemo(() => {
    const byName = new Map<string, MCPTool>((tools.data ?? []).map((t) => [t.name, t]))
    const names = new Set([...byName.keys(), ...Object.keys(draft.tools ?? {})])
    return orderTools(
      [...names].map((name) => {
        const t = byName.get(name)
        return {
          name,
          kind: t?.kind ?? (WRITE_TOOLS.has(name) ? 'write' : 'read'),
          description: t?.description ?? '',
          defaultPermission: t?.defaultPermission ?? (WRITE_TOOLS.has(name) ? 'confirm' : 'read'),
        }
      }),
    )
  }, [tools.data, draft.tools])

  const setPerm = (name: string, perm: ToolPermission) => set({ tools: { ...draft.tools, [name]: perm } })

  return (
    <Card title="Exposed tools" actions={<span className="small muted">Write tools require confirmation by default</span>}>
      <div className="mcp-tools">
        {rows.map((t) => {
          const perm = (draft.tools?.[t.name] ?? t.defaultPermission) as ToolPermission
          const on = perm !== 'disabled'
          const enable = t.kind === 'write' ? (t.defaultPermission === 'allow' ? 'allow' : 'confirm') : 'read'
          return (
            <div key={t.name} className={cx('mcp-tool', !on && 'off')} title={t.description || undefined}>
              <Checkbox checked={on} onChange={(v) => setPerm(t.name, v ? enable : 'disabled')} />
              <span className="mcp-tool-name">{t.name}</span>
              {t.kind === 'write' && on ? (
                <Select
                  inputSize="sm"
                  className={cx('mcp-perm', perm)}
                  value={perm}
                  aria-label={`${t.name} permission`}
                  onChange={(v) => setPerm(t.name, v as ToolPermission)}
                  options={['confirm', 'allow']}
                />
              ) : (
                <span className="perm-text">{on ? 'read' : 'disabled'}</span>
              )}
              {fields[`tools.${t.name}`] && <span className="field-error">{fields[`tools.${t.name}`]}</span>}
            </div>
          )
        })}
      </div>
      <div className="mcp-tools-foot">
        <span className="small muted">Approvals expire after</span>
        <Input
          inputSize="sm"
          type="number"
          min={1}
          max={1440}
          className="mcp-timeout"
          invalid={!!fields.approvalTimeoutMinutes}
          value={Number.isFinite(draft.approvalTimeoutMinutes) ? draft.approvalTimeoutMinutes : ''}
          onChange={(e) => set({ approvalTimeoutMinutes: e.target.value === '' ? Number.NaN : Number(e.target.value) })}
          aria-label="Approval timeout in minutes"
        />
        <span className="small muted">minutes without a decision</span>
        {fields.approvalTimeoutMinutes && <span className="field-error">{fields.approvalTimeoutMinutes}</span>}
      </div>
    </Card>
  )
}

// ---------------------------------------------------------------- right column

function ConnectCard({ info, transports, token, enabled }: { info?: MCPInfo; transports: Transport[]; token?: string; enabled: boolean }) {
  const [mode, setMode] = useState<Transport>('http')
  const effective: Transport = transports.includes(mode) ? mode : transports[0] ?? 'http'
  const tokenText = token ?? 'rl_mcp_…'
  const config =
    effective === 'http'
      ? { mcpServers: { relay: { url: info?.endpoint || 'https://relay.example/mcp', headers: { Authorization: `Bearer ${tokenText}` } } } }
      : { mcpServers: { relay: { command: 'docker', args: ['exec', '-i', 'relay', 'relay', 'mcp-stdio', '--token', tokenText] } } }
  const text = JSON.stringify(config, null, 2)

  return (
    <div className="card mcp-side-card">
      <div className="card-title">Connect a client</div>
      <div className="small muted" style={{ lineHeight: 1.5 }}>Paste this into your assistant's MCP config.</div>
      {transports.length > 1 && (
        <Segmented value={effective} onChange={setMode} options={[{ value: 'http', label: 'HTTP' }, { value: 'stdio', label: 'stdio' }]} />
      )}
      <CodeBlock code={text} dark />
      <div className="mcp-copy-block">
        <CopyButton text={text} label="Copy config" size="default" />
      </div>
      {token && <div className="small warn-text">Includes the token you just generated — Relay can't show it again.</div>}
      {info && (
        <div className="small faint">
          {enabled ? `${info.connectedSessions} ${info.connectedSessions === 1 ? 'client' : 'clients'} connected · ${info.toolCount} tools exposed` : 'The MCP server is disabled.'}
        </div>
      )}
    </div>
  )
}

function callTime(iso: string): string {
  const d = new Date(iso)
  const today = new Date()
  const yesterday = new Date(today.getFullYear(), today.getMonth(), today.getDate() - 1)
  const p = (n: number) => String(n).padStart(2, '0')
  if (d.toDateString() === today.toDateString()) return `${p(d.getHours())}:${p(d.getMinutes())}`
  if (d.toDateString() === yesterday.toDateString()) return 'Yest.'
  return `${p(d.getMonth() + 1)}-${p(d.getDate())}`
}

function RecentCalls() {
  const calls = useMCPCalls(8)
  return (
    <div className="card mcp-side-card">
      <div className="row">
        <div className="card-title">Recent tool calls</div>
        <Link to="/logs/audit" className="small mcp-link">Audit log</Link>
      </div>
      {calls.isLoading ? (
        <Skeleton height={80} />
      ) : calls.error ? (
        <div className="small muted">{errorMessage(calls.error)}</div>
      ) : !calls.data?.length ? (
        <div className="small muted">No tool calls yet.</div>
      ) : (
        <div className="mcp-calls">
          {calls.data.map((c) => (
            <div key={c.id} className="mcp-call" title={[c.target, c.detail].filter(Boolean).join(' · ')}>
              <span className="mono faint mcp-call-time">{callTime(c.at)}</span>
              <div className="grow mcp-ellipsis">
                <span className="mono">{c.action}</span> <span className="muted">· {c.actorName}</span>
              </div>
              <span className={resultClass(c.result)}>{c.result}</span>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
