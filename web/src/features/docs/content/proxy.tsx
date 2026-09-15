import type { DocSection } from '../types'
import { C, Defs, Example, Flow, H2, H3, List, Note, P, See, Step, Steps, Table, Tip, UI, Warn } from '../parts'
import { Kbd } from '../../../components/ui'

function ProxyHosts() {
  return (
    <>
      <H2>Example: put a Node app online</H2>
      <P>
        You run a Next.js or Express app on another machine at <C>192.168.1.20</C>, listening on port <C>3000</C>. You want it at
        {' '}<C>https://app.example.com</C>.
      </P>
      <Steps>
        <Step title="Create the DNS record">
          At your DNS provider add <C>app.example.com A 203.0.113.10</C> (your public IP). For LAN-only apps, add the record in your local DNS
          (Pi-hole, AdGuard, router) pointing at Relay’s LAN IP instead. With <See id="public-dns">Public DNS</See> connected, Relay creates the public record for you.
        </Step>
        <Step title="Open a new host">
          <UI>Proxy hosts → New host</UI>, or press <Kbd>N</Kbd> from anywhere.
        </Step>
        <Step title="Fill in the Details tab">
          <Table
            head={['Field', 'Value']}
            mono={[1]}
            rows={[
              ['Domain names', 'app.example.com'],
              ['Upstream scheme', 'http'],
              ['Upstream host', '192.168.1.20'],
              ['Upstream port', '3000'],
              ['Websockets', 'on (needed for socket.io, Next.js dev, live updates)'],
              ['Block exploits', 'on'],
            ]}
          />
          Press <Kbd>⏎</Kbd> after each domain to add more names, e.g. <C>app.example.com</C> and <C>www.app.example.com</C>.
        </Step>
        <Step title="Add HTTPS on the SSL tab">
          Click <UI>Request new</UI> next to Certificate (Let’s Encrypt), then turn on <UI>Force HTTPS</UI> and <UI>HTTP/2</UI>.
        </Step>
        <Step title="Save and apply">
          <Kbd>⌘</Kbd> <Kbd>S</Kbd> saves it as a pending change. Click <UI>Apply &amp; reload</UI>. Within a few seconds the host card turns green.
        </Step>
      </Steps>
      <P>Relay renders roughly this for you. The drawer’s config preview shows the exact output.</P>
      <Example
        title="Generated nginx (simplified)"
        lang="nginx"
        code={`server {
    listen 443 ssl;
    http2 on;
    server_name app.example.com;
    ssl_certificate     /data/certs/<id>/fullchain.pem;
    ssl_certificate_key /data/certs/<id>/privkey.pem;

    location / {
        proxy_pass http://192.168.1.20:3000;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Upgrade $http_upgrade;      # websockets
        proxy_set_header Connection $connection_upgrade;
    }
}
server { listen 80; server_name app.example.com; return 301 https://$host$request_uri; }`}
      />

      <H2>Choosing the upstream address</H2>
      <P>Relay runs with host networking, so addresses are seen from the Docker host itself:</P>
      <Table
        head={['Your app runs…', 'Upstream host', 'Port']}
        rows={[
          ['On the same machine, port published (-p 3000:3000)', <C>127.0.0.1</C>, 'the published port'],
          ['In a container on the same machine, not published', <>the container IP, e.g. <C>172.18.0.5</C></>, 'the app port (3000)'],
          ['On another machine in your LAN', <C>192.168.1.20</C>, 'the port it listens on'],
          ['Behind HTTPS already (e.g. Proxmox :8006)', <>set scheme to <C>https</C></>, <C>8006</C>],
        ]}
      />
      <Tip>You don’t need to look up container IPs yourself. <See id="docker">Docker discovery</See> fills them in and keeps them updated when containers are recreated.</Tip>

      <H2>The host drawer</H2>
      <Defs
        items={[
          ['Details', 'Domains, upstream, access list and the common toggles.'],
          ['SSL', 'Certificate, Force HTTPS, HTTP/2, HTTP/3, HSTS and upstream TLS verification.'],
          ['Locations', <>Different rules for paths such as <C>/api</C> or <C>/admin</C>. <See id="locations">Locations →</See></>],
          ['Advanced', <>Single sign-on, rate limits, upload size, timeouts and custom nginx. <See id="protection">Protection →</See></>],
        ]}
      />

      <H3>Details tab options</H3>
      <Table
        head={['Option', 'What it does']}
        rows={[
          ['Domain names', <>One or more names. Wildcards like <C>*.example.com</C> are allowed (the certificate must cover them).</>],
          ['Access list', <>Restrict who can reach the host. <See id="access-lists">Access lists →</See></>],
          ['Websockets', 'Passes Upgrade headers so websocket connections work.'],
          ['Block exploits', 'Rejects common attack paths and scanner probes.'],
          ['Cache assets', 'Lets browsers cache static files (images, CSS, JS) for 30 days.'],
          ['HTTP/2', 'Faster multiplexed connections. Requires TLS.'],
        ]}
      />

      <H3>SSL tab options</H3>
      <Table
        head={['Option', 'What it does']}
        rows={[
          ['Certificate', 'Pick an existing certificate, request a new one, or use a wildcard that covers the domain.'],
          ['Force HTTPS', 'Redirects http:// to https:// with a 301.'],
          ['HSTS', 'Tells browsers to always use HTTPS for this domain. Only enable once HTTPS works.'],
          ['HTTP/3 (QUIC)', 'Serves HTTP/3 over UDP 443. Forward UDP 443 on your router too.'],
          ['Upstream TLS', 'When the upstream is https://, choose whether to verify its certificate (turn off for self-signed apps).'],
        ]}
      />

      <H2>Managing many hosts</H2>
      <List>
        <li>Filter by domain or upstream with the search box (<Kbd>/</Kbd>).</li>
        <li>Switch between cards and a table with <Kbd>V</Kbd>.</li>
        <li>Select several hosts with <Kbd>X</Kbd> to enable, disable or delete them together.</li>
        <li>Disabling a host keeps its configuration but stops serving it after the next apply.</li>
      </List>
      <Warn title="Getting 502 Bad Gateway?">
        The proxy can’t reach the upstream. Check that the app is running, that the port is right, and that a firewall on the app’s machine allows
        connections from Relay’s host. The <UI>Logs → Error</UI> tab shows the exact reason (for example <C>connect() failed (111: Connection refused)</C>).
      </Warn>
    </>
  )
}

function Locations() {
  return (
    <>
      <H2>When to use a location</H2>
      <P>
        A location changes what happens for one path on a host. The rest of the host keeps its normal settings. Each location has an
        {' '}<strong>action</strong>:
      </P>
      <Defs
        items={[
          ['Proxy', 'Send this path to a different upstream.'],
          ['Same', 'Keep the host’s upstream but change options (caching, headers, auth) for this path.'],
          ['Deny 403', 'Block the path completely.'],
        ]}
      />
      <P>When several locations match, the longest (most specific) path wins.</P>

      <H2>Example: frontend and API on one domain</H2>
      <P>Your frontend runs on port 3000 and your API on port 4000. Both should live under <C>app.example.com</C>.</P>
      <Table
        head={['Path', 'Action', 'Forward to', 'Options']}
        mono={[0, 2]}
        rows={[
          ['/', '(host upstream)', '192.168.1.20:3000', 'Websockets'],
          ['/api', 'Proxy', '192.168.1.20:4000', 'Strip prefix'],
        ]}
      />
      <Example
        title="What the API receives"
        code={`GET https://app.example.com/api/users/42
  Strip prefix on  → API receives GET /users/42
  Strip prefix off → API receives GET /api/users/42`}
      />

      <H2>Example: public health check on a protected host</H2>
      <P>
        The host uses the <em>LAN only</em> access list, but your uptime monitor on the internet must reach <C>/healthz</C>. Add a location
        {' '}<C>/healthz</C> with action <UI>Same</UI> and turn on <UI>No auth</UI>. It skips the access list and single sign-on for that path only.
      </P>

      <H2>Example: stricter rules for an admin area</H2>
      <P>
        Everyone may use <C>shop.example.com</C>, but <C>/admin</C> should only work from the office. Add a location <C>/admin</C>, action
        {' '}<UI>Same</UI>, turn on <UI>Different access list</UI> and pick <em>Office</em>. To block it outright, use <UI>Deny 403</UI>.
      </P>

      <H2>Adding or removing headers</H2>
      <P>Each location can set response headers or remove headers coming from the upstream.</P>
      <Example
        title="Common headers"
        code={`X-Frame-Options          DENY
Referrer-Policy          strict-origin-when-cross-origin
Cache-Control            no-store          (on /api)
Remove header: X-Powered-By`}
      />

      <H2>Other per-path options</H2>
      <Table
        head={['Option', 'Use it for']}
        rows={[
          ['Strip prefix', 'Apps that don’t know they live under a sub-path.'],
          ['Websockets', <>Only a path such as <C>/socket.io</C> needs websocket upgrades.</>],
          ['Cache', <>Long browser caching for a static path such as <C>/assets</C>.</>],
          ['No auth', 'Webhooks, health checks, public files on an otherwise protected host.'],
          ['Different access list', 'Tighter or looser IP/password rules for this path.'],
        ]}
      />
      <Tip>Many apps misbehave under a sub-path (<C>/app</C>). Where you can, give each app its own sub-domain instead (<C>app.example.com</C>).</Tip>
    </>
  )
}

function Redirects() {
  return (
    <>
      <H2>Redirects</H2>
      <P>
        <UI>Proxy hosts → Redirects</UI> sends a domain, or one path on it, somewhere else. No upstream is needed.
      </P>
      <Table
        head={['Field', 'Meaning']}
        rows={[
          ['Domain names', 'The names to redirect. Wildcards allowed.'],
          ['From path', 'Leave empty to redirect every path, or enter one path such as /blog.'],
          ['Redirect to', <>A full URL. Variables like <C>$host</C> are allowed.</>],
          ['Status code', '301 permanent · 302 temporary · 307/308 keep the method (POST stays POST).'],
          ['Keep path', <><C>/pricing?x=1</C> is appended to the target.</>],
          ['Force HTTPS', 'First upgrade http:// to https:// on the old domain.'],
          ['Certificate', 'Needed so https:// requests to the old domain can be answered before redirecting.'],
        ]}
      />

      <H3>Example: www to the bare domain</H3>
      <Example code={`Domain names   www.example.com
Redirect to    https://example.com
Status code    301
Keep path      on

https://www.example.com/pricing?plan=pro  →  https://example.com/pricing?plan=pro`} />

      <H3>Example: moved to a new domain</H3>
      <Example code={`Domain names   oldbrand.com, www.oldbrand.com
Redirect to    https://newbrand.com
Keep path      on
Certificate    a certificate for oldbrand.com (so https:// links keep working)`} />

      <H3>Example: one path to another site</H3>
      <Example code={`Domain names   example.com
From path      /docs
Redirect to    https://docs.example.com
Status code    302`} />

      <H2>The default host</H2>
      <P>
        <UI>Proxy hosts → Default</UI> decides what happens when a request arrives for a domain Relay doesn’t know, or for your bare IP
        address. Bots scan IPs constantly, so this matters.
      </P>
      <Table
        head={['Choice', 'When to use it']}
        rows={[
          [<strong>Close connection (444)</strong>, 'Recommended on public servers. Nothing is sent back; scanners learn nothing.'],
          ['Relay 404 page', 'A neutral, unbranded “not found” page. Handy on a LAN.'],
          ['Redirect to', 'Send unknown traffic to your main website.'],
          ['Serve a proxy host', 'Treat one existing host as the catch-all.'],
        ]}
      />
      <Note>For HTTPS requests to unknown names, the proxy answers with a self-signed placeholder certificate (CN=localhost) so your real certificates aren’t revealed.</Note>
    </>
  )
}

function AccessLists() {
  return (
    <>
      <H2>What an access list is</H2>
      <P>
        An access list is a reusable set of rules: <strong>which IP addresses may connect</strong> and optionally <strong>a username and password</strong>.
        Create it once under <UI>Access lists</UI>, then attach it to hosts, individual locations, the admin UI, the MCP endpoint or HAProxy stats.
      </P>
      <Flow steps={[{ label: 'Request' }, { label: 'IP rules', sub: 'top to bottom' }, { label: 'Password', sub: 'if enabled' }, { label: 'Your app' }]} />

      <H2>How IP rules are evaluated</H2>
      <List>
        <li>Rules run <strong>top to bottom and the first match wins</strong>. Drag rules to reorder them.</li>
        <li>Each rule is an address (<C>10.0.0.5</C>), a range (<C>192.168.0.0/16</C>) or <C>all</C>.</li>
        <li>Click a rule’s allow/deny badge to flip it.</li>
        <li>End with <C>deny all</C> to block everyone you didn’t list.</li>
      </List>

      <H2>Example: LAN and VPN only</H2>
      <Example
        title="Access list “LAN only”"
        code={`allow  192.168.1.0/24    home network
allow  10.8.0.0/24       WireGuard VPN
deny   all`}
      />
      <P>Attach it to Grafana, Home Assistant or anything you never want reachable from the internet.</P>

      <H2>Example: password-protected staging site</H2>
      <Steps>
        <Step title="Create the list">Name it <em>Staging</em> and turn on <UI>Require username &amp; password</UI>.</Step>
        <Step title="Add users">Add a user such as <C>client</C>. Generate a strong password and copy it before you close the drawer.</Step>
        <Step title="Set the realm">The text shown in the browser’s login prompt, e.g. <C>Staging – ask Ollie for access</C>.</Step>
        <Step title="Attach and apply">Choose it under <UI>Access list</UI> on the <C>staging.example.com</C> host, then apply.</Step>
      </Steps>
      <Tip>Already have an <C>.htpasswd</C> file? Use <UI>Import htpasswd</UI> in the list’s drawer.</Tip>

      <H2>Example: office skips the password</H2>
      <P>Turn on <UI>Satisfy any</UI>. A request is allowed if <em>either</em> the IP matches <em>or</em> the password is correct:</P>
      <Example code={`allow  198.51.100.24     office
deny   all
Basic auth       on  (user: team)
Satisfy any      on

From the office  → straight in
From anywhere else → login prompt`} />

      <Warn title="Behind another proxy or CDN?">
        If traffic passes through Cloudflare or another proxy before Relay, IP rules see that proxy’s address, not the visitor’s. Use passwords or
        single sign-on in that case.
      </Warn>
      <Note>The list’s drawer shows which hosts use it, so you can see the impact of a change before applying.</Note>
    </>
  )
}

function Protection() {
  return (
    <>
      <P>These options live on a host’s <UI>Advanced</UI> tab. The Expose wizard offers the same protections for load-balanced apps.</P>

      <H2>Single sign-on (forward auth)</H2>
      <P>
        Put apps behind a login portal like <strong>Authelia</strong>, <strong>Authentik</strong> or <strong>oauth2-proxy</strong>. Before each request, the reverse proxy
        asks the portal whether the visitor is signed in. If not, the visitor is sent to the sign-in page.
      </P>
      <Flow steps={[{ label: 'Browser' }, { label: 'Reverse proxy', sub: 'forward auth' }, { label: 'Authelia', sub: 'verify URL' }, { label: 'Your app', sub: 'if 2xx' }]} />
      <Tip title="No login portal?">
        Pick <UI>Relay login</UI> as the provider instead: people sign in with their Relay account and there is nothing else to install. <See id="relay-login">Relay login →</See>
      </Tip>
      <Steps>
        <Step title="Pick a provider">Under <UI>Authentication</UI>, choose Authelia, Authentik or oauth2-proxy. Relay fills in typical URLs.</Step>
        <Step title="Adjust the URLs to your setup">
          <Example
            title="Authelia example"
            code={`Verify URL    http://127.0.0.1:9091/api/verify?rd=https://auth.example.com
Sign-in URL   https://auth.example.com`}
          />
          The verify URL must be reachable from Relay’s host. The sign-in URL must be reachable from the visitor’s browser.
        </Step>
        <Step title="Pass the user to the app (optional)">
          <UI>Pass Remote-User</UI> and <UI>Pass Remote-Groups</UI> send the signed-in username and groups as headers, so apps such as Grafana can log the user in automatically.
        </Step>
        <Step title="Keep ACME working">Leave <UI>Skip for /.well-known/</UI> on so certificate renewals aren’t blocked by the login.</Step>
      </Steps>

      <H2>Rate limiting</H2>
      <P>Limits requests per second per client IP. Extra requests get <C>429 Too Many Requests</C>.</P>
      <Example
        title="Protect a login form"
        code={`Requests   10 / s
Burst      20          (short spikes allowed)
Exempt     LAN only    (your own network is never limited)`}
      />

      <H2>Geo-blocking</H2>
      <P>
        Allow only visitors from selected countries using two-letter codes (<C>NO</C>, <C>SE</C>, <C>DE</C>). Press <Kbd>⏎</Kbd> after each code.
        Everyone else gets <C>403</C>. Local and private addresses (your LAN, VPN and <C>127.0.0.1</C>) always get in. Works with nginx and Relay Edge.
      </P>
      <P>
        The first time you apply with geo-blocking on, Relay downloads the free <strong>DB-IP</strong> country database (about 4 MB) and updates it every month.
        Until it’s downloaded, geo-blocking is skipped and the Geo-block section shows why. Prefer MaxMind? Put your <C>GeoLite2-Country.mmdb</C> in <C>/data/geoip</C> and Relay uses it instead.
      </P>
      <Note>IP geolocation by <a href="https://db-ip.com" target="_blank" rel="noreferrer">DB-IP</a>, licensed under CC BY 4.0.</Note>

      <H2>Maintenance mode</H2>
      <P>
        Show a maintenance page (HTTP <C>503</C>) instead of the app while you work on it, with an access list that still lets you in. Relay’s branded error pages
        for 502s, 404s and friends are set up in <UI>Settings → Error pages</UI>. <See id="error-pages">Error &amp; maintenance pages →</See>
      </P>

      <H2>Other options</H2>
      <Table
        head={['Option', 'Example', 'Why']}
        rows={[
          ['Max upload size', '512 MB', <>Uploads larger than the default (1 MB) fail with <C>413</C>. Raise it for Nextcloud, Immich, file shares.</>],
          ['Proxy timeouts', '300 s', 'Long-running requests: exports, AI streaming, large reports. Empty means 60 s.'],
          ['Hide from search engines', 'on', <>Adds <C>X-Robots-Tag: noindex</C> to staging and internal tools.</>],
        ]}
      />

      <H2>Custom nginx snippet</H2>
      <P>
        For anything Relay has no switch for, add raw nginx directives. They are inserted into the host’s <C>server</C> block. Relay checks for balanced
        braces when you save, and <C>nginx -t</C> validates everything before the reload, so a mistake can’t take the proxy down.
      </P>
      <Note title="nginx only">
        Relay Edge can’t run snippets. While it is the proxy engine the field is read-only, and switching to Relay Edge is refused while any host still has one.
        {' '}<See id="relay-edge">Relay Edge →</See>
      </Note>
      <Example
        lang="nginx"
        title="Examples"
        code={`# Security headers
add_header X-Content-Type-Options nosniff always;
add_header Permissions-Policy "camera=(), microphone=()" always;

# Tell the app it lives behind a prefix
proxy_set_header X-Forwarded-Prefix /app;

# Stream responses immediately (server-sent events, AI chat)
proxy_buffering off;`}
      />
    </>
  )
}

function Streams() {
  return (
    <>
      <H2>What streams are for</H2>
      <P>
        A stream forwards raw <strong>TCP or UDP</strong> traffic from a port on Relay’s host to another machine. There are no domains, no TLS termination and no access lists: bytes in,
        bytes out. Use them for databases, game servers, MQTT, SSH or VPNs.
      </P>
      <Flow steps={[{ label: 'Client', sub: 'relay-host:25565' }, { label: 'Reverse proxy', sub: 'tcp stream' }, { label: 'Server', sub: '192.168.1.40:25565' }]} />

      <H2>Creating a stream</H2>
      <Table
        head={['Field', 'Meaning']}
        rows={[
          ['Protocol', 'TCP, UDP or both.'],
          ['Listen', <><C>0.0.0.0</C> for all interfaces, <C>127.0.0.1</C> for this machine only, <C>::</C> for IPv6.</>],
          ['Ports', <>A single port (<C>25565</C>) or a range (<C>2456-2458</C>). Relay checks that the ports are free.</>],
          ['Forward to', 'Host and port of the target. Leave the port empty to keep the same port.'],
          ['or a backend', 'Send the traffic to a load balancer backend instead of a single server.'],
          ['PROXY protocol', 'Passes the real client IP to targets that understand it.'],
          ['Idle timeout', 'Close connections that have been silent this long.'],
        ]}
      />

      <H2>Examples</H2>
      <Table
        head={['Use case', 'Protocol', 'Listen port', 'Forward to']}
        mono={[2, 3]}
        rows={[
          ['Minecraft server', 'TCP', '25565', '192.168.1.40:25565'],
          ['Valheim (port range)', 'UDP', '2456-2458', '192.168.1.41 (same ports)'],
          ['WireGuard VPN', 'UDP', '51820', '192.168.1.2:51820'],
          ['MQTT broker', 'TCP', '1883', '192.168.1.30:1883'],
          ['Postgres for the LAN only', 'TCP', '127.0.0.1:5432', '172.18.0.7:5432'],
          ['DNS server', 'Both', '53', '192.168.1.3:53'],
        ]}
      />
      <Warn title="Don’t expose databases to the internet">
        Streams can’t check passwords or IPs. For databases, listen on <C>127.0.0.1</C> or a LAN address and use your firewall, or connect over a VPN.
      </Warn>
      <Note>Ports 80, 443 (reverse proxy) and 8181 (Relay UI) are taken. Remember to open or forward the stream’s port on your firewall or router.</Note>
      <Tip>Disabled streams keep their configuration but release the port after the next apply.</Tip>
    </>
  )
}

export const proxySections: DocSection[] = [
  {
    id: 'proxy-hosts',
    group: 'Reverse proxy',
    title: 'Proxy hosts',
    icon: 'hosts',
    summary: 'Map a domain to an app on your network, with HTTPS, websockets and sensible security defaults.',
    keywords: 'host domain upstream node 3000 port next express https ssl websockets http2 http3 hsts 502 bad gateway create new',
    app: [{ to: '/hosts?new=1', label: 'New proxy host' }, { to: '/hosts', label: 'Open proxy hosts' }],
    Body: ProxyHosts,
  },
  {
    id: 'locations',
    group: 'Reverse proxy',
    title: 'Locations (per-path rules)',
    icon: 'link',
    summary: 'Route /api to another service, protect /admin, open /healthz, or add headers for a single path.',
    keywords: 'location path api strip prefix sub path headers deny 403 no auth cache route',
    Body: Locations,
  },
  {
    id: 'redirects',
    group: 'Reverse proxy',
    title: 'Redirects & the default host',
    icon: 'redirect',
    summary: 'Redirect www to your bare domain or old domains to new ones, and decide what unknown domains see.',
    keywords: 'redirect www apex 301 302 307 308 default host 444 catch all unknown domain ip scanners',
    app: [{ to: '/hosts/redirects', label: 'Open redirects' }, { to: '/hosts/default', label: 'Default host' }],
    Body: Redirects,
  },
  {
    id: 'access-lists',
    group: 'Reverse proxy',
    title: 'Access lists',
    icon: 'access',
    summary: 'Allow only certain IP ranges, require a password, or combine both. Reusable across hosts.',
    keywords: 'access list ip allow deny cidr lan vpn basic auth password htpasswd satisfy any realm whitelist',
    app: [{ to: '/access', label: 'Open access lists' }],
    Body: AccessLists,
  },
  {
    id: 'protection',
    group: 'Reverse proxy',
    title: 'Single sign-on & protection',
    icon: 'token',
    summary: 'Relay login or forward auth with Authelia/Authentik, rate limits, maintenance mode, geo-blocking, upload limits and custom nginx.',
    keywords: 'forward auth relay login maintenance authelia authentik oauth2-proxy sso login rate limit 429 geo block country upload size 413 timeout snippet custom nginx advanced',
    Body: Protection,
  },
  {
    id: 'streams',
    group: 'Reverse proxy',
    title: 'Streams (TCP/UDP)',
    icon: 'streams',
    summary: 'Forward raw ports for game servers, databases, MQTT or VPNs.',
    keywords: 'stream tcp udp port forward minecraft valheim wireguard mqtt postgres database range proxy protocol',
    app: [{ to: '/streams', label: 'Open streams' }],
    Body: Streams,
  },
]
