import type { DocSection } from '../types'
import { C, Cards, Defs, Example, Flow, GoTo, H2, List, Note, P, See, Step, Steps, Table, Tip, UI, Warn } from '../parts'
import { Kbd } from '../../../components/ui'

function Introduction() {
  return (
    <>
      <H2>What Relay does</H2>
      <P>
        Relay is one place to run the front door of your servers. It configures a reverse proxy, <strong>nginx</strong> or Relay’s own{' '}
        <See id="relay-edge">Relay Edge</See> (domains, HTTPS, access control), and <strong>HAProxy</strong> as a load balancer, then validates and reloads them
        for you. You never edit a config file by hand, and a bad change rolls back automatically.
      </P>
      <Defs
        items={[
          ['Proxy hosts', <>Send <C>app.example.com</C> to an app on your network, e.g. <C>192.168.1.20:3000</C>.</>],
          ['Certificates', 'Free HTTPS from Let’s Encrypt (or any ACME CA), renewed automatically. Wildcards via DNS.'],
          ['Access lists', 'Allow or deny IP ranges and ask for a username and password.'],
          ['Redirects', <>Send <C>www.example.com</C> to <C>example.com</C>, old domains to new ones.</>],
          ['Streams', 'Forward raw TCP/UDP ports: databases, game servers, SSH, WireGuard.'],
          ['Load balancer', 'Spread traffic across several servers with health checks, sticky sessions and draining.'],
          ['Docker discovery', 'Finds your containers (local and remote) and turns them into hosts in a few clicks.'],
          ['MCP server', 'Lets AI assistants such as Claude read and manage Relay, with approvals for changes.'],
        ]}
      />

      <H2>How traffic flows</H2>
      <P>A visitor’s request reaches the reverse proxy (nginx or Relay Edge) on port 80/443. It terminates TLS, checks access rules and forwards it to your app.</P>
      <Flow
        steps={[
          { label: 'Browser', sub: 'https://app.example.com' },
          { label: 'Reverse proxy', sub: ':443 · TLS + access' },
          { label: 'Your app', sub: '192.168.1.20:3000' },
        ]}
      />
      <P>For apps with several instances, the reverse proxy hands the request to HAProxy, which picks a healthy server:</P>
      <Flow
        steps={[
          { label: 'Browser' },
          { label: 'Reverse proxy', sub: ':443' },
          { label: 'HAProxy', sub: '127.0.0.1:10080' },
          { label: 'api-1 · api-2 · api-3', sub: 'round robin' },
        ]}
      />

      <H2>The containers</H2>
      <Table
        head={['Container', 'What it does']}
        mono={[0]}
        rows={[
          ['relay', 'This web UI, the REST API, the MCP server, the database, certificates and Docker discovery (port 8181).'],
          ['relay-nginx', 'The official nginx image, supervised by a small Relay agent. Owns ports 80 and 443 while nginx is the proxy engine (the default).'],
          ['relay-edge', 'Relay Edge, run by the same agent. Stopped on standby unless it is chosen in Settings → Proxy engine; only one proxy engine owns ports 80 and 443.'],
          ['relay-haproxy', 'The official HAProxy image, supervised the same way. Starts once you create a backend.'],
        ]}
      />

      <H2>Your first ten minutes</H2>
      <Steps>
        <Step title="Point a domain at this server">
          Create a DNS <C>A</C> record such as <C>app.example.com → your public IP</C>, and forward ports 80 and 443 on your router.
        </Step>
        <Step title="Create a proxy host">
          <UI>Proxy hosts → New host</UI> (or press <Kbd>N</Kbd>). Enter the domain and where the app listens. <See id="proxy-hosts">Walkthrough →</See>
        </Step>
        <Step title="Add HTTPS">
          On the host’s <UI>SSL</UI> tab, request a Let’s Encrypt certificate and turn on <UI>Force HTTPS</UI>.
        </Step>
        <Step title="Apply">
          Click <UI>Apply &amp; reload</UI> in the dark bar at the bottom (<Kbd>⌘</Kbd> <Kbd>⏎</Kbd>). Relay validates, reloads and health-checks. <See id="applying">How applying works →</See>
        </Step>
      </Steps>

      <H2>Where to go next</H2>
      <Cards
        items={[
          { to: '/docs/proxy-hosts', icon: 'hosts', title: 'Proxy a Node app', desc: 'Put an app on port 3000 behind a domain with HTTPS.' },
          { to: '/docs/docker', icon: 'docker', title: 'Import from Docker', desc: 'Create hosts from running and stopped containers.' },
          { to: '/docs/certificates', icon: 'certificates', title: 'Wildcard certificates', desc: 'One certificate for *.example.com using DNS-01.' },
          { to: '/docs/access-lists', icon: 'access', title: 'Lock down an app', desc: 'LAN-only access, passwords, or single sign-on.' },
          { to: '/docs/load-balancer', icon: 'load-balancer', title: 'Load balance', desc: 'Several servers, health checks and zero-downtime deploys.' },
          { to: '/docs/mcp', icon: 'mcp', title: 'Connect Claude', desc: 'Manage Relay from an AI assistant over MCP.' },
        ]}
      />
    </>
  )
}

function Applying() {
  return (
    <>
      <H2>Saving is not the same as going live</H2>
      <P>
        Every edit (a new host, a changed certificate, a deleted access list) is saved immediately as a <strong>pending change</strong>.
        Nothing reaches the reverse proxy or HAProxy yet. A dark bar appears at the bottom of the screen:
      </P>
      <Example title="The pending bar" code={`3 pending changes   app.example.com · api backend · LAN only      [Review diff] [Discard] [Apply & reload]  ⌘⏎`} />
      <List>
        <li><strong>Review diff</strong> opens the exact proxy and HAProxy config lines that will change.</li>
        <li><strong>Discard</strong> throws every pending change away and returns to the live configuration.</li>
        <li><strong>Apply &amp; reload</strong> makes all pending changes live in one go.</li>
      </List>
      <Tip>Batch related edits (host + certificate + access list) and apply once. It is faster and gives you one version to roll back to.</Tip>

      <H2>What happens when you apply</H2>
      <Flow
        steps={[
          { label: 'Render', sub: 'proxy + haproxy.cfg' },
          { label: 'Validate', sub: 'nginx -t or relay edge check · haproxy -c' },
          { label: 'Swap', sub: 'atomic' },
          { label: 'Reload', sub: 'no dropped connections' },
          { label: 'Health check', sub: '10 s' },
          { label: 'Live', sub: 'new version' },
        ]}
      />
      <Steps>
        <Step title="Render">Relay generates the complete reverse proxy (nginx or Relay Edge) and HAProxy configuration from your hosts, backends and settings.</Step>
        <Step title="Validate">The files are tested inside the engine containers with <C>nginx -t</C> (or <C>relay edge check</C> for Relay Edge) and <C>haproxy -c</C>. A syntax error stops here and nothing changes.</Step>
        <Step title="Swap and reload">The new release replaces the old one atomically and the engines reload gracefully.</Step>
        <Step title="Health check">For 10 seconds Relay watches the hosts you changed.</Step>
        <Step title="Done">The result becomes a new numbered version under <UI>Config history</UI>.</Step>
      </Steps>

      <H2>Automatic rollback</H2>
      <P>
        If a host that was healthy before starts returning <C>502</C>/<C>503</C>/<C>504</C>, or an engine fails to start, Relay restores the
        previous release by itself. Your edit is not lost: it stays as a pending change so you can fix it and try again.
      </P>
      <Example
        title="Example: a typo in the upstream port"
        code={`You change grafana.example.com → 192.168.1.20:3001 (should be 3000) and apply.
  ✓ nginx -t passed
  ✓ reloaded
  ✗ grafana.example.com: 502 Bad Gateway for 10 s (was healthy)
  ↺ rolled back to v41 · your change is still pending`}
      />

      <H2>Rolling back by hand</H2>
      <P>
        Open <UI>Config history</UI>, pick an older version and choose <UI>Roll back</UI>. The old version is validated again before it is
        swapped in, so a rollback can’t leave you with a broken proxy. <See id="history">More on config history →</See>
      </P>

      <H2>Changes that skip the apply step</H2>
      <List>
        <li>Draining or putting a load balancer server into maintenance: this takes effect immediately through HAProxy’s runtime API.</li>
        <li>Certificate renewals: renewed files are picked up without you pressing Apply.</li>
      </List>
      <Note title="Who can apply?">Admins and editors can save and apply changes. Viewers see everything read-only.</Note>
    </>
  )
}

function Tour() {
  return (
    <>
      <H2>The menu</H2>
      <Table
        head={['Menu item', 'Shortcut', 'Use it to']}
        rows={[
          [<GoTo to="/" icon="overview">Overview</GoTo>, <Kbd>G O</Kbd>, 'See traffic, health, expiring certificates and recent activity at a glance.'],
          [<GoTo to="/hosts" icon="hosts">Proxy hosts</GoTo>, <Kbd>G H</Kbd>, 'Manage domains, redirects and what happens to unknown domains.'],
          [<GoTo to="/load-balancer" icon="load-balancer">Load balancer</GoTo>, <Kbd>G L</Kbd>, 'Create HAProxy backends and frontends, watch live stats, drain servers.'],
          [<GoTo to="/certificates" icon="certificates">Certificates</GoTo>, <Kbd>G C</Kbd>, 'Request, upload and renew TLS certificates; configure DNS providers.'],
          [<GoTo to="/access" icon="access">Access lists</GoTo>, <Kbd>G A</Kbd>, 'Reusable IP rules and passwords.'],
          [<GoTo to="/streams" icon="streams">Streams</GoTo>, <Kbd>G T</Kbd>, 'Forward TCP/UDP ports.'],
          [<GoTo to="/logs" icon="logs">Logs</GoTo>, <Kbd>G G</Kbd>, 'Search access and error logs, read the audit trail, approve AI requests.'],
          [<GoTo to="/history" icon="history">Config history</GoTo>, <Kbd>G V</Kbd>, 'Compare versions and roll back.'],
          [<GoTo to="/docs" icon="docs">Documentation</GoTo>, <Kbd>G D</Kbd>, 'You are here.'],
          [<GoTo to="/settings" icon="settings">Settings</GoTo>, <Kbd>G S</Kbd>, 'Ports, users, Docker, TLS defaults, engines, MCP, notifications, backups.'],
        ]}
      />
      <P>A red or amber dot on a menu icon means something needs attention (a host is down, a backend is degraded). A number shows pending changes or waiting approvals.</P>

      <H2>Command palette</H2>
      <P>
        Press <Kbd>⌘</Kbd> <Kbd>K</Kbd> (<Kbd>Ctrl</Kbd> <Kbd>K</Kbd> on Windows/Linux) anywhere. Type part of a domain, backend or certificate name to jump
        straight to it, or run actions like <em>New proxy host</em>, <em>Request certificate</em> or <em>Apply pending changes</em>.
      </P>
      <Example title="Try typing" code={`grafana        → open the grafana.example.com host, or show its logs
new            → New proxy host · New backend
apply          → Apply pending changes`} />

      <H2>Keyboard shortcuts</H2>
      <P>Press <Kbd>?</Kbd> to see all of them. The most useful:</P>
      <Table
        head={['Keys', 'Action']}
        rows={[
          [<><Kbd>⌘</Kbd> <Kbd>K</Kbd></>, 'Command palette'],
          [<Kbd>N</Kbd>, 'New proxy host'],
          [<><Kbd>⌘</Kbd> <Kbd>⏎</Kbd></>, 'Apply pending changes'],
          [<><Kbd>⌘</Kbd> <Kbd>S</Kbd></>, 'Save the open drawer'],
          [<Kbd>/</Kbd>, 'Focus search / filter on the current page'],
          [<><Kbd>J</Kbd> <Kbd>K</Kbd> · <Kbd>⏎</Kbd></>, 'Move through a list · open the selected item'],
          [<Kbd>V</Kbd>, 'Toggle grid / table view of hosts'],
          [<Kbd>space</Kbd>, 'Pause or resume the live log tail'],
          [<Kbd>esc</Kbd>, 'Close a drawer or dialog'],
        ]}
      />

      <H2>Health states</H2>
      <Table
        head={['State', 'Meaning']}
        mono={[0]}
        rows={[
          ['healthy', 'The upstream answers normally.'],
          ['degraded', 'It answers, but slowly or with some errors (or some load balancer servers are down).'],
          ['down', 'Connection refused, timeouts or 502/503/504 responses.'],
          ['disabled', 'Switched off in Relay; the proxy doesn’t serve it.'],
          ['unknown', 'Not checked yet, for example right after creating it.'],
        ]}
      />
      <Warn title="Can’t find something?">Use the search box at the top of every page, or the docs search on the left of this page.</Warn>
    </>
  )
}

export const startSections: DocSection[] = [
  {
    id: 'introduction',
    group: 'Getting started',
    title: 'Welcome to Relay',
    icon: 'overview',
    summary: 'What Relay is, how requests flow through it, and the fastest way to put your first app online.',
    keywords: 'intro overview start quickstart what is nginx relay edge haproxy containers architecture',
    Body: Introduction,
  },
  {
    id: 'applying',
    group: 'Getting started',
    title: 'Pending changes & applying',
    icon: 'bolt',
    summary: 'Edits are drafts until you apply them. Relay validates, reloads and rolls back automatically if something breaks.',
    keywords: 'apply reload pending discard diff rollback validate nginx -t relay edge check haproxy -c version draft',
    app: [{ to: '/history?pending=1', label: 'Review pending changes' }],
    Body: Applying,
  },
  {
    id: 'tour',
    group: 'Getting started',
    title: 'Finding your way around',
    icon: 'terminal',
    summary: 'The menu, the command palette, keyboard shortcuts and what the health colours mean.',
    keywords: 'navigation menu shortcuts keyboard command palette cmd k search health status badge',
    Body: Tour,
  },
]
