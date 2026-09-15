import type { DocSection } from '../types'
import { C, Example, Flow, H2, H3, List, Note, P, See, Step, Steps, Table, Tip, UI, Warn } from '../parts'

function Users() {
  return (
    <>
      <H2>Roles</H2>
      <Table
        head={['Role', 'Can']}
        mono={[0]}
        rows={[
          ['admin', 'Everything, including users, backups, engine upgrades, MCP and instance settings.'],
          ['editor', 'Create and change hosts, certificates, access lists, streams and backends; apply and roll back; approve AI requests.'],
          ['viewer', 'See everything read-only: dashboards, logs, configs. Good for on-call colleagues.'],
        ]}
      />

      <H2>Adding a user</H2>
      <Steps>
        <Step title="Open Users & access">
          <UI>Settings → Users &amp; access → Add user</UI>.
        </Step>
        <Step title="Fill in the details">Username, optional email, role, and an initial password. Leave <UI>Must change on first sign-in</UI> on.</Step>
        <Step title="Share the password safely">Send the initial password through your password manager or another private channel, not in the same message as the URL.</Step>
      </Steps>

      <H2>Two-factor authentication</H2>
      <List>
        <li>Each user can add an authenticator app (TOTP) or a <strong>passkey</strong> (Touch ID, Windows Hello, a security key) under their account.</li>
        <li><UI>Require 2FA for admins</UI> forces admins to enroll before they can do anything else.</li>
        <li>Recovery: an admin can reset another user’s password, which signs out all of their sessions.</li>
      </List>

      <H2>Protecting the admin UI</H2>
      <Table
        head={['Setting', 'Recommendation']}
        rows={[
          ['Admin UI domain', <>Serve Relay itself on e.g. <C>relay.example.com</C> with HTTPS instead of <C>http://ip:8181</C>.</>],
          ['Restrict admin UI to LAN', 'On, unless you truly need to manage Relay from the internet. Use a VPN instead.'],
          ['Session length', 'Signs out inactive sessions after the chosen time.'],
        ]}
      />

      <H2>Locked out?</H2>
      <P>From a shell on the Docker host, reset any user’s password. A new one-time password is printed:</P>
      <Example lang="bash" code={`docker exec -it relay relay users reset-password admin`} />
      <Warn>Anyone with shell access to the Docker host can do this. Protect SSH access to that machine as carefully as the admin account.</Warn>
    </>
  )
}

function RestApi() {
  return (
    <>
      <H2>Automate Relay from scripts</H2>
      <P>
        Everything the UI does goes through Relay’s REST API, so scripts and CI pipelines can do the same. Authenticate with a personal API token.
      </P>

      <H2>Create a token</H2>
      <Steps>
        <Step title="Generate">
          <UI>Settings → Users &amp; access → REST API tokens</UI>. Give it a name (e.g. <C>deploy pipeline</C>), an expiry, and optionally limit what it can do.
        </Step>
        <Step title="Copy it">The token starts with <C>rl_api_</C> and is shown only once. Store it as a secret in your CI system.</Step>
      </Steps>

      <H2>Examples</H2>
      <Example
        lang="bash"
        title="List proxy hosts"
        code={`export RELAY=https://relay.example.com
export RELAY_TOKEN=rl_api_…

curl -s -H "Authorization: Bearer $RELAY_TOKEN" $RELAY/api/hosts | jq '.[].domains'`}
      />
      <Example
        lang="bash"
        title="See what is pending, then apply"
        code={`curl -s -H "Authorization: Bearer $RELAY_TOKEN" $RELAY/api/pending
curl -s -X POST -H "Authorization: Bearer $RELAY_TOKEN" $RELAY/api/apply`}
      />
      <Example
        lang="bash"
        title="Liveness check (no token needed)"
        code={`curl -fsS http://127.0.0.1:8181/healthz`}
      />
      <Tip>
        To find the request for any action, open your browser’s developer tools (Network tab) while doing it in the UI. The same request works with an API token.
      </Tip>
      <Note>Token actions appear in <UI>Logs → Audit</UI> under the token’s name. Revoke a token from the same settings page at any time.</Note>
    </>
  )
}

function Mcp() {
  return (
    <>
      <H2>What MCP gives you</H2>
      <P>
        The Model Context Protocol lets AI assistants like <strong>Claude</strong> use Relay as a tool. Ask in plain language, and the assistant reads your configuration, searches logs,
        or proposes changes that you approve.
      </P>
      <Example
        title="Things you can ask"
        code={`“Why is grafana.example.com returning 502s since this morning?”
“Which certificates expire in the next 30 days?”
“Create a host for app.example.com pointing at 192.168.1.20:3000 with websockets, then apply.”
“Drain api-2 so I can deploy it.”`}
      />
      <Flow steps={[{ label: 'Claude' }, { label: '/mcp', sub: 'Bearer rl_mcp_…' }, { label: 'Approval', sub: 'for changes' }, { label: 'Relay' }]} />

      <H2>Connect Claude</H2>
      <Steps>
        <Step title="Enable the server">
          <UI>Settings → MCP server</UI>, turn on <UI>MCP server enabled</UI>. Optionally restrict it to an access list so only your machines can reach it.
        </Step>
        <Step title="Generate a token">Click <UI>Generate token</UI>. It starts with <C>rl_mcp_</C> and is shown once.</Step>
        <Step title="Add Relay to your assistant">
          <UI>Connect a client</UI> shows ready-to-paste config with your token filled in. For example:
          <Example
            lang="bash"
            title="Claude Code"
            code={`claude mcp add --transport http relay https://relay.example.com/mcp \\
  --header "Authorization: Bearer rl_mcp_…"`}
          />
          <Example
            lang="json"
            title="Clients that take a JSON config (HTTP)"
            code={`{
  "mcpServers": {
    "relay": {
      "url": "https://relay.example.com/mcp",
      "headers": { "Authorization": "Bearer rl_mcp_…" }
    }
  }
}`}
          />
          <Example
            lang="json"
            title="Local stdio (assistant on the Docker host)"
            code={`{
  "mcpServers": {
    "relay": {
      "command": "docker",
      "args": ["exec", "-i", "relay", "relay", "mcp-stdio", "--token", "rl_mcp_…"]
    }
  }
}`}
          />
        </Step>
      </Steps>

      <H2>Tools</H2>
      <H3>Read tools</H3>
      <Table
        head={['Tool', 'Does']}
        mono={[0]}
        rows={[
          ['list_hosts · get_host', 'Hosts with health, certificate, access list and full configuration.'],
          ['query_logs', 'Search access logs by host, status, IP, text and time window.'],
          ['list_backends · get_backend_status', 'Load balancer backends and live server status.'],
          ['list_certificates', 'Certificates with expiry and errors.'],
          ['list_access_lists · list_streams', 'Access lists and TCP/UDP streams.'],
          ['get_pending_changes', 'What is saved but not live yet.'],
          ['manage_users', 'List users, roles and 2FA status (read-only).'],
        ]}
      />
      <H3>Write tools</H3>
      <Table
        head={['Tool', 'Does']}
        mono={[0]}
        rows={[
          ['create_host · update_host · delete_host', 'Change proxy hosts (saved as pending changes).'],
          ['request_certificate', 'Request a Let’s Encrypt certificate.'],
          ['drain_server', 'Set a server to drain, maint or ready immediately.'],
          ['apply_changes', 'Apply all pending changes, with validation and rollback.'],
        ]}
      />

      <H2>Permissions and approvals</H2>
      <List>
        <li>Under <UI>Exposed tools</UI>, switch any tool off.</li>
        <li>Write tools default to <C>confirm</C>: the request waits in <UI>Logs → Approvals</UI> (and pops up as a toast) until an admin or editor approves or rejects it.</li>
        <li>Set a write tool to <C>allow</C> to let it run without asking. Only do this for tools you’re comfortable automating.</li>
        <li>Unanswered approvals expire after the configured number of minutes.</li>
        <li>Every tool call is listed under <UI>Recent tool calls</UI> and in the audit log.</li>
      </List>
      <Warn>An MCP token acts on Relay with real permissions. Treat it like a password, give each assistant its own token, and revoke tokens you no longer use.</Warn>
      <P>Getting started with a first prompt? Try: <em>“Give me a health report of my Relay: hosts that are down, certificates expiring soon and any 5xx spikes today.”</em> See also <See id="notifications">mcp_write_executed notifications</See>.</P>
    </>
  )
}

export const adminSections: DocSection[] = [
  {
    id: 'users',
    group: 'Users & automation',
    title: 'Users, roles & 2FA',
    icon: 'users',
    summary: 'Invite colleagues with the right role, enforce two-factor authentication and protect the admin UI.',
    keywords: 'users roles admin editor viewer 2fa totp passkey webauthn password reset locked out session lan admin domain',
    app: [{ to: '/settings/users', label: 'Users & access' }],
    Body: Users,
  },
  {
    id: 'rest-api',
    group: 'Users & automation',
    title: 'REST API tokens',
    icon: 'terminal',
    summary: 'Automate Relay from scripts and CI with personal API tokens.',
    keywords: 'rest api token rl_api curl script ci automation bearer healthz',
    app: [{ to: '/settings/users', label: 'Manage tokens' }],
    Body: RestApi,
  },
  {
    id: 'mcp',
    group: 'Users & automation',
    title: 'MCP server (AI assistants)',
    icon: 'mcp',
    summary: 'Connect Claude or another MCP client to read and manage Relay, with approvals for every change.',
    keywords: 'mcp model context protocol claude ai assistant agent token rl_mcp tools approvals permissions stdio claude code desktop',
    app: [{ to: '/settings/mcp', label: 'MCP settings' }, { to: '/logs/approvals', label: 'Approvals' }],
    Body: Mcp,
  },
]
