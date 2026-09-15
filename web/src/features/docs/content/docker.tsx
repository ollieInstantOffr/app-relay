import type { DocSection } from '../types'
import { C, Example, H2, H3, List, Note, P, See, Step, Steps, Table, Tip, UI, Warn } from '../parts'

function Discovery() {
  return (
    <>
      <H2>What discovery does</H2>
      <P>
        Relay watches your Docker hosts and lists <strong>every container</strong>, running or stopped, together with the address and port it would proxy to. You can
        turn containers into proxy hosts in a few clicks, and Relay keeps their upstreams correct when containers are recreated.
      </P>

      <H2>Create hosts from containers</H2>
      <Steps>
        <Step title="Open the dialog">
          On <UI>Proxy hosts</UI> click <UI>From Docker · N found</UI>. The same list is on the Overview page and under <UI>Settings → Docker discovery</UI>.
        </Step>
        <Step title="Pick containers">
          Tick the containers you want. Relay’s own containers are marked <C>relay</C> and never pre-selected.
        </Step>
        <Step title="Check domain and port">
          Each row suggests a domain from your <UI>Domain pattern</UI> and the detected app port. Change either inline, for example set port <C>3000</C> for a Node app.
        </Step>
        <Step title="Create and apply">The hosts appear as pending changes. Apply to put them live.</Step>
      </Steps>
      <Example
        title="Example rows"
        code={`☑ web        node:22-alpine       app.example.com       172.18.0.4:3000   (PORT env)
☑ grafana    grafana/grafana      grafana.example.com   172.18.0.6:3000   (exposed port)
☐ postgres   postgres:17          —                     5432              ⚠ database port
☐ relay      relay:latest         —                                       relay`}
      />

      <H2>How the port is detected</H2>
      <P>Relay picks the app port in this order:</P>
      <List ordered>
        <li>The <C>relay.port</C> label.</li>
        <li><C>PORT</C>-style environment variables: <C>PORT</C>, <C>APP_PORT</C>, <C>HTTP_PORT</C>, <C>SERVER_PORT</C>, <C>LISTEN_PORT</C>, <C>NUXT_PORT</C>, <C>VITE_PORT</C>.</li>
        <li>Ports the image exposes (<C>EXPOSE</C>), then published ports.</li>
        <li>Image and command hints: Node/Next/Nuxt/Vite/Bun/Deno → <C>3000</C>, Django/gunicorn/uvicorn → <C>8000</C>, Flask → <C>5000</C>.</li>
      </List>
      <Tip>Your app doesn’t need to publish a port. On the local Docker host, Relay connects straight to the container IP.</Tip>

      <H2>Stopped containers</H2>
      <P>
        A stopped container can still get a host. Relay creates it <strong>disabled and linked</strong> to the container. When the container starts, Relay fills in its IP and port
        and enables the host automatically.
      </P>

      <H2>Keeping upstreams in sync</H2>
      <P>
        Containers get a new IP each time they are recreated (<C>docker compose up</C> after an image update). With <UI>Keep upstreams in sync</UI> on, Relay updates the upstream
        address and port of linked hosts for you.
      </P>

      <H2>Defaults for new hosts</H2>
      <Table
        head={['Setting', 'Example', 'Effect']}
        rows={[
          ['Domain pattern', <C>{'{name}.example.com'}</C>, 'Suggested domain for each container.'],
          ['Certificate for new hosts', 'Auto (matching wildcard)', <>Uses a valid certificate that covers the domain, such as <C>*.example.com</C>.</>],
          ['Access list for new hosts', 'LAN only', <>Applied unless a container overrides it with <C>relay.access</C>.</>],
        ]}
      />
      <Note>Relay uses host networking. For containers on the same machine, <C>127.0.0.1:&lt;published port&gt;</C> and the container IP both work.</Note>
      <P>Want hosts created without clicking? Use <See id="docker-labels">Docker labels</See>. Containers on other machines: <See id="docker-hosts">Remote Docker hosts</See>.</P>
    </>
  )
}

function Labels() {
  return (
    <>
      <H2>Configure hosts in docker-compose</H2>
      <P>
        Add <C>relay.*</C> labels to a container and Relay creates and updates the proxy host for you. This works when <UI>Auto-create hosts from labels</UI> is on for that
        Docker host (<UI>Settings → Docker discovery → Docker hosts</UI>).
      </P>
      <Example
        lang="yaml"
        title="docker-compose.yml: a Node app"
        code={`services:
  web:
    image: ghcr.io/acme/web:latest
    restart: unless-stopped
    environment:
      PORT: 3000
    labels:
      relay.host: app.example.com,www.app.example.com
      relay.port: "3000"
      relay.tls: letsencrypt
      relay.websockets: "true"`}
      />

      <H2>All labels</H2>
      <Table
        head={['Label', 'Values', 'Meaning']}
        mono={[0, 1]}
        rows={[
          ['relay.host', 'a.example.com,b.example.com', 'Domains for the host. Comma-separated.'],
          ['relay.port', '3000', 'App port inside the container.'],
          ['relay.scheme', 'http | https', 'Use https if the app itself serves TLS.'],
          ['relay.tls', 'auto | letsencrypt | off', 'auto uses a matching certificate (e.g. a wildcard); letsencrypt requests one; off serves plain HTTP.'],
          ['relay.access', 'LAN only', 'Name of an access list to attach.'],
          ['relay.websockets', 'true | false', 'Pass websocket upgrades.'],
          ['relay.backend', 'api', 'Instead of a host, add this container as a server to the existing load balancer backend with that name.'],
          ['relay.enable', 'false', 'Ignore this container completely.'],
        ]}
      />

      <H2>More examples</H2>
      <H3>Internal tool behind the LAN-only list with a wildcard certificate</H3>
      <Example lang="yaml" code={`services:
  grafana:
    image: grafana/grafana:latest
    labels:
      relay.host: grafana.example.com
      relay.tls: auto          # uses *.example.com
      relay.access: LAN only`} />

      <H3>Scale an API behind the load balancer</H3>
      <P>Create a backend named <C>api</C> first. Every replica then joins it as a server:</P>
      <Example lang="yaml" code={`services:
  api:
    image: ghcr.io/acme/api:latest
    deploy:
      replicas: 3
    labels:
      relay.backend: api
      relay.port: "8080"`} />

      <H3>App that already speaks HTTPS</H3>
      <Example lang="yaml" code={`services:
  unifi:
    image: lscr.io/linuxserver/unifi-network-application
    labels:
      relay.host: unifi.example.com
      relay.port: "8443"
      relay.scheme: https`} />

      <H2>Removing containers</H2>
      <P>
        With <UI>Remove with container</UI> on, hosts that labels created are removed when their container is deleted. Hosts you created by hand are never removed automatically.
      </P>
      <Warn title="Label typos">
        Invalid values (a bad domain, <C>relay.tls: yes</C>) are reported in Relay’s activity feed with the exact problem, e.g. <C>relay.tls: use auto, letsencrypt or off</C>.
      </Warn>
      <Tip>Changing a label updates the host on the next sync. Edits you make in the UI to a label-managed host may be overwritten when labels change, so keep the labels as the source of truth.</Tip>
    </>
  )
}

function RemoteHosts() {
  return (
    <>
      <H2>Watch more than one machine</H2>
      <P>
        <UI>Settings → Docker discovery → Docker hosts → Add Docker host</UI>. Each host has a name (containers show as <C>nas · grafana</C>), a connection, an address used for
        upstreams, and its own <UI>Auto-create hosts from labels</UI> and <UI>Remove with container</UI> switches. Use <UI>Test connection</UI> before saving.
      </P>

      <H2>Option 1: socket proxy over TCP (recommended on a LAN)</H2>
      <P>Run this on the remote machine, bound to its LAN IP only:</P>
      <Example
        lang="yaml"
        title="docker-compose.yml on the remote host"
        code={`services:
  docker-socket-proxy:
    image: tecnativa/docker-socket-proxy:latest
    restart: unless-stopped
    environment:
      CONTAINERS: 1
      EVENTS: 1
      NETWORKS: 1
      INFO: 1
      VERSION: 1
      POST: 0
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    ports:
      - "192.168.1.20:2375:2375"   # the host's LAN IP, never 0.0.0.0`}
      />
      <P>Then add <C>tcp://192.168.1.20:2375</C> as a <UI>TCP</UI> host. Firewall port 2375 so only Relay’s machine can reach it.</P>

      <H2>Option 2: Docker daemon with TLS</H2>
      <P>If the daemon runs with <C>--tlsverify</C> on port 2376, choose <UI>TCP + TLS</UI> and paste the CA, client certificate and client key (PEM). The key is stored and never shown again.</P>

      <H2>Option 3: SSH</H2>
      <Steps>
        <Step title="Prepare a user on the remote machine">
          <Example lang="bash" code={`sudo adduser --disabled-password relay
sudo usermod -aG docker relay
# add Relay's public key to /home/relay/.ssh/authorized_keys`} />
        </Step>
        <Step title="Add the host">Choose <UI>SSH</UI>, enter <C>relay@192.168.1.20:22</C> and paste a private key without passphrase.</Step>
        <Step title="Trust the host key">The SSH host key is trusted on first connect and shown in the UI. If it changes later, Relay refuses to connect until you trust the new key.</Step>
      </Steps>

      <H2>How remote upstreams are addressed</H2>
      <List>
        <li>Container IPs on another machine aren’t reachable, so upstreams use <C>&lt;address for upstreams&gt;:&lt;published port&gt;</C>, e.g. <C>192.168.1.20:8080</C>.</li>
        <li>Containers with <C>network_mode: host</C> use the address plus the container port.</li>
        <li>A remote container without a published port (or published only on <C>127.0.0.1</C>) is listed but can’t be proxied. Publish it first: <C>ports: ["8080:3000"]</C>.</li>
      </List>

      <Warn title="Security">
        Read access to the Docker API reveals every container’s environment variables (often secrets), and write access is root on that machine. Use the socket proxy with
        {' '}<C>POST: 0</C>, bind it to a private interface, and use a dedicated SSH user and key.
      </Warn>
      <Note>A read-only socket proxy is enough for discovery. <See id="engines">Engine upgrades</See> need write access to the local Docker socket.</Note>
    </>
  )
}

export const dockerSections: DocSection[] = [
  {
    id: 'docker',
    group: 'Docker',
    title: 'Docker discovery',
    icon: 'docker',
    summary: 'Turn running and stopped containers into proxy hosts, with ports detected and upstreams kept up to date.',
    keywords: 'docker container discovery compose create hosts from docker port detection 3000 stopped container ip sync domain pattern',
    app: [{ to: '/settings/docker', label: 'Docker discovery settings' }],
    Body: Discovery,
  },
  {
    id: 'docker-labels',
    group: 'Docker',
    title: 'Docker labels',
    icon: 'token',
    summary: 'Declare domains, ports, TLS and access right in docker-compose.yml and let Relay create the hosts.',
    keywords: 'labels relay.host relay.port relay.tls relay.access relay.websockets relay.backend relay.enable relay.scheme compose auto create',
    Body: Labels,
  },
  {
    id: 'docker-hosts',
    group: 'Docker',
    title: 'Remote Docker hosts',
    icon: 'link',
    summary: 'Discover containers on other machines over a socket proxy, TLS or SSH.',
    keywords: 'remote docker host ssh tls tcp socket proxy tecnativa 2375 2376 nas multiple machines',
    Body: RemoteHosts,
  },
]
