import type { DocSection } from '../types'
import { C, Defs, Example, Flow, H2, H3, List, Note, P, See, Step, Steps, Table, Tip, UI, Warn } from '../parts'

function LoadBalancer() {
  return (
    <>
      <H2>Proxy host or load balancer?</H2>
      <Table
        head={['You have…', 'Use']}
        rows={[
          ['One app, one address', <See id="proxy-hosts">A proxy host</See>],
          ['Several copies of the same app', 'A load balancer backend, exposed on a domain'],
          ['Zero-downtime deploys (take one server out at a time)', 'A load balancer backend'],
          ['Non-HTTP traffic across several servers (databases, MQTT)', 'A TCP backend plus a frontend or stream'],
        ]}
      />
      <P>The load balancer starts automatically when you create your first backend.</P>

      <H2>Load balancer engine</H2>
      <P>
        Backends and frontends run on <strong>HAProxy</strong> by default. You can switch to <See id="relay-balancer">Relay Balancer</See> (beta), Relay’s own engine,
        in <UI>Settings → Load balancer engine</UI>. Everything on these pages works with both; the examples below show HAProxy’s config.
      </P>

      <H2>The building blocks</H2>
      <Defs
        items={[
          ['Backend', <>A named pool of servers running the same app, e.g. <C>api</C>.</>],
          ['Server', <>One instance: address, port, weight and role (active or backup), e.g. <C>10.0.0.11:3000</C>.</>],
          ['Health check', 'How the load balancer decides a server is up. Failing servers stop receiving traffic.'],
          ['Frontend', <>A listening port with rules that choose a backend. <See id="frontends">Frontends →</See></>],
          ['Expose', <>Puts a backend on a public domain through nginx, with HTTPS. <See id="expose">Expose →</See></>],
        ]}
      />

      <H2>Example: three API instances</H2>
      <Steps>
        <Step title="Create the backend">
          <UI>Load balancer → Backends → New backend</UI>. Name <C>api</C>, mode <UI>HTTP (L7)</UI>, algorithm <C>roundrobin</C>.
        </Step>
        <Step title="Add the servers">
          <Example code={`api-1   10.0.0.11:3000   weight 100   active
api-2   10.0.0.12:3000   weight 100   active
api-3   10.0.0.13:3000   weight 100   backup   (only used if api-1 and api-2 are down)`} />
        </Step>
        <Step title="Configure the health check">
          <Example code={`Check type        HTTP
Method / Path     GET /healthz
Expected status   200
Interval          5 s
Rise / Fall       2 / 3    (2 passes → UP, 3 failures → DOWN)`} />
        </Step>
        <Step title="Save and apply">The load balancer reloads seamlessly: existing connections aren’t dropped.</Step>
        <Step title="Put it online"><See id="expose">Expose</See> the backend on <C>api.example.com</C>.</Step>
      </Steps>
      <Example
        lang="haproxy"
        title="Generated haproxy.cfg (simplified; “View config” shows the real file)"
        code={`backend api
    mode http
    balance roundrobin
    option httpchk GET /healthz
    http-check expect status 200
    default-server inter 5s rise 2 fall 3
    server api-1 10.0.0.11:3000 check weight 100
    server api-2 10.0.0.12:3000 check weight 100
    server api-3 10.0.0.13:3000 check weight 100 backup`}
      />

      <H2>Balancing algorithms</H2>
      <Table
        head={['Algorithm', 'Picks', 'Good for']}
        mono={[0]}
        rows={[
          ['roundrobin', 'Each server in turn, respecting weights', 'Most web apps'],
          ['leastconn', 'The server with the fewest active connections', 'Long requests, websockets, databases'],
          ['source', 'Same client IP → same server', 'Simple stickiness without cookies'],
          ['uri', 'Same path → same server (HTTP only)', 'Caches'],
          ['random', 'A random server, weighted', 'Large pools'],
          ['first', 'Fills the first server before using the next', 'Saving resources on idle servers'],
        ]}
      />

      <H2>Health check types</H2>
      <Table
        head={['Type', 'What it checks']}
        mono={[0]}
        rows={[
          ['none', 'Nothing. Servers are always considered up.'],
          ['tcp', 'The port accepts connections.'],
          ['http', <>A request returns the expected status: <C>200</C>, <C>2xx</C>, <C>200-399</C> or <C>200,204</C>. Set a Host header for virtual-hosted apps.</>],
          ['pgsql · mysql · redis', 'Speaks the database protocol, so a hung database is detected even though the port is open.'],
        ]}
      />

      <H2>More options</H2>
      <Table
        head={['Option', 'What it does']}
        rows={[
          ['Sticky sessions', 'Keep a visitor on the same server: the load balancer inserts a cookie, prefixes your app’s cookie, or uses a source IP stick table.'],
          ['Forward client IP', <>Adds <C>X-Forwarded-For</C> so the app sees the visitor’s IP.</>],
          ['TLS re-encrypt', 'Connects to servers over HTTPS. Turn off Verify server certificates for self-signed ones.'],
          ['Send PROXY protocol', 'Passes client IPs to TCP servers that support it.'],
          ['Timeouts', 'Connect, server and queue timeouts; empty values use Settings → Load balancer.'],
          ['Retries', 'Connection retries; with redispatch a retry may go to another server.'],
        ]}
      />
      <Tip>Renaming a backend is safe: frontends, hosts and streams reference it by ID.</Tip>
    </>
  )
}

function Frontends() {
  return (
    <>
      <H2>What a frontend is</H2>
      <P>
        A frontend is a port the load balancer listens on (the <strong>bind</strong>, for example <C>:8443</C> or <C>127.0.0.1:10081</C>) plus <strong>rules</strong> that decide
        which backend gets each request.
      </P>
      <Note title="Most people don’t need to create frontends by hand">
        nginx owns ports 80 and 443. To put a backend on a domain, use <See id="expose">Expose</See>, which creates the frontend for you. Create your own for extra ports,
        TCP services, or advanced routing.
      </Note>

      <H2>How rules work</H2>
      <List>
        <li>Rules are checked <strong>top to bottom</strong>; the first rule that matches picks the backend. Drag to reorder.</li>
        <li>A rule can have several conditions, and <strong>all of them</strong> must match.</li>
        <li>Tick <UI>Negate</UI> to invert a condition (“path does <em>not</em> begin with /admin”).</li>
      </List>
      <Table
        head={['Condition', 'Matches', 'Example']}
        mono={[0, 2]}
        rows={[
          ['host', 'The Host header (HTTP)', 'api.example.com'],
          ['path_beg', 'Path begins with', '/static'],
          ['path', 'Exact path', '/healthz'],
          ['path_reg', 'Path regular expression (RE2 syntax on Relay Balancer)', '^/v[0-9]+/'],
          ['header', 'A request header value', 'X-Tenant: acme'],
          ['src', 'Client IP or range', '10.0.0.0/8'],
          ['sni', 'TLS server name (TCP mode, no decryption)', 'db.example.com'],
        ]}
      />

      <H2>Example: one port, several apps</H2>
      <Example
        title="HTTP frontend on :8080"
        code={`1. host = api.example.com                       → backend api
2. host = app.example.com  AND  path_beg /static → backend static
3. host = app.example.com                       → backend web`}
      />
      <P>Rule 2 comes before rule 3, so static files are caught first.</P>

      <H2>Example: internal-only admin backend</H2>
      <Example code={`1. path_beg /admin  AND  NOT src 10.0.0.0/8   → backend deny-page
2. path_beg /admin                            → backend admin
3. (everything else)                          → backend web`} />

      <H2>Frontend options</H2>
      <Table
        head={['Option', 'Use']}
        rows={[
          ['Mode', 'HTTP (can read hosts, paths, headers) or TCP (raw, can route by SNI and client IP).'],
          ['Accept PROXY protocol', 'When the traffic comes from a proxy that sends it, such as an nginx stream with PROXY protocol on.'],
          ['Compression', 'gzip text and JSON responses.'],
          ['Enabled', 'Disabled frontends are left out of the load balancer config.'],
        ]}
      />
    </>
  )
}

function Expose() {
  return (
    <>
      <H2>What “Expose online” does</H2>
      <P>
        Exposing a backend gives it a public domain with HTTPS and optional protection, in one wizard. Relay creates a reverse proxy host and a local
        load balancer frontend and wires them together:
      </P>
      <Flow
        steps={[
          { label: 'Internet', sub: 'https://api.example.com' },
          { label: 'nginx', sub: ':443 · TLS · access' },
          { label: 'Load balancer', sub: '127.0.0.1:10080' },
          { label: 'backend api', sub: 'api-1 · api-2 · api-3' },
        ]}
      />
      <P>Local frontends take the first free port from <C>10080</C> upwards. Change the starting port in <UI>Settings → Load balancer</UI>.</P>

      <H2>Walkthrough</H2>
      <Steps>
        <Step title="Start the wizard">
          <UI>Load balancer → Backends</UI>, open the backend’s <UI>⋯</UI> menu and choose <UI>Expose online</UI>.
        </Step>
        <Step title="Domain and route">
          Enter <C>api.example.com</C>. Choose <UI>Reverse proxy</UI> (recommended: the reverse proxy handles HTTPS and security) or <UI>Load balancer</UI> (the load balancer binds the port itself).
        </Step>
        <Step title="Who can reach it">
          <List>
            <li><strong>Anyone on the internet</strong>: a public app. Consider the protections below.</li>
            <li><strong>Access list</strong>: only IPs or users from a list.</li>
            <li><strong>Sign-in required (forward-auth)</strong>: Authelia, Authentik or oauth2-proxy.</li>
          </List>
        </Step>
        <Step title="Protection">
          Block exploits, rate limit (requests per second plus burst), geo-block by country and hide from search engines.
        </Step>
        <Step title="Certificate">
          Use an existing certificate (a wildcard is picked automatically if one matches), request a new one, or choose none (HTTP only; not recommended for anything public).
        </Step>
        <Step title="Review">
          The wizard shows the reverse proxy and load balancer config it will create and validates both (for example <C>nginx -t</C> and <C>haproxy -c</C>, or <C>relay balancer check</C> on Relay Balancer). Confirm, then apply.
        </Step>
      </Steps>
      <Warn>Only HTTP backends can sit behind the reverse proxy. For TCP backends, create a <See id="frontends">frontend</See> or a <See id="streams">stream</See> that points at the backend.</Warn>
    </>
  )
}

function Stats() {
  return (
    <>
      <H2>Live stats</H2>
      <P>
        <UI>Load balancer → Stats</UI> shows every backend and server in real time: status (<C>UP</C>, <C>DOWN</C>, <C>DRAIN</C>, <C>MAINT</C>), sessions per
        second, queue, errors, p95 response time, share of traffic and the last health check result.
      </P>

      <H2>Server states</H2>
      <Table
        head={['State', 'Effect']}
        mono={[0]}
        rows={[
          ['ready', 'Normal: receives traffic when healthy.'],
          ['drain', 'Finishes existing sessions but accepts no new ones.'],
          ['maint', 'Taken out of rotation completely.'],
        ]}
      />
      <P>State changes take effect immediately through the engine’s runtime API (HAProxy’s, or Relay Balancer’s compatible one). No apply is needed, and Relay remembers the state across reloads.</P>

      <H2>Example: zero-downtime deploy</H2>
      <Steps>
        <Step title="Drain api-1">Open the server’s menu and choose <UI>Drain</UI>. Watch its sessions fall to 0.</Step>
        <Step title="Deploy">Update and restart the app on api-1.</Step>
        <Step title="Bring it back">Set it to <UI>Ready</UI>. Wait for the health check to show <C>UP</C>.</Step>
        <Step title="Repeat">Do the same for api-2 and api-3.</Step>
      </Steps>
      <Tip>An AI assistant connected over <See id="mcp">MCP</See> can do this for you with the <C>drain_server</C> tool.</Tip>

      <H2>Prometheus & stats page</H2>
      <P>
        <UI>Settings → Load balancer</UI> enables the stats endpoint (default <C>127.0.0.1:8404</C>) and Prometheus metrics at <C>/metrics</C>. Protect it with a
        stats access list if you bind it to a LAN address.
      </P>
      <Example
        lang="yaml"
        title="prometheus.yml"
        code={`scrape_configs:
  - job_name: haproxy
    metrics_path: /metrics
    static_configs:
      - targets: ['127.0.0.1:8404']`}
      />

      <H3>Global defaults</H3>
      <P>The same settings page holds default timeouts, max connections, health check interval and rise/fall for every backend, plus seamless reloads (HAProxy only; Relay Balancer always reloads seamlessly).</P>
      <Note>A server listed as <C>DOWN</C> while the app works? Check the health check path and expected status, and that the app accepts connections from Relay’s host.</Note>
    </>
  )
}

export const lbSections: DocSection[] = [
  {
    id: 'load-balancer',
    group: 'Load balancer',
    title: 'Backends & servers',
    icon: 'load-balancer',
    summary: 'Spread traffic across several instances of an app with health checks, weights, backups and sticky sessions.',
    keywords: 'haproxy relay balancer engine backend server pool roundrobin leastconn source uri health check sticky session cookie weight backup tls re-encrypt',
    app: [{ to: '/load-balancer/backends?new=1', label: 'New backend' }],
    Body: LoadBalancer,
  },
  {
    id: 'frontends',
    group: 'Load balancer',
    title: 'Frontends & routing rules',
    icon: 'filter',
    summary: 'Listen on a port and route requests to backends by host, path, header, client IP or SNI.',
    keywords: 'frontend bind port acl rule condition host path_beg path_reg header src sni negate tcp http compression proxy protocol',
    app: [{ to: '/load-balancer/frontends', label: 'Open frontends' }],
    Body: Frontends,
  },
  {
    id: 'expose',
    group: 'Load balancer',
    title: 'Expose a backend online',
    icon: 'expose',
    summary: 'Give a backend a public domain with HTTPS, access control and protection in one wizard.',
    keywords: 'expose online wizard public domain https reverse proxy haproxy nginx 10080 forward auth rate limit',
    Body: Expose,
  },
  {
    id: 'lb-stats',
    group: 'Load balancer',
    title: 'Stats, draining & metrics',
    icon: 'stats',
    summary: 'Watch servers live, drain them for zero-downtime deploys, and scrape metrics with Prometheus.',
    keywords: 'stats drain maintenance maint ready zero downtime deploy rolling prometheus metrics 8404 runtime api',
    app: [{ to: '/load-balancer/stats', label: 'Open stats' }, { to: '/settings/load-balancer', label: 'Load balancer settings' }],
    Body: Stats,
  },
]
