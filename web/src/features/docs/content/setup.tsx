import type { DocSection } from '../types'
import { C, Example, H2, List, Note, P, See, Step, Steps, Table, Tip, UI, Warn } from '../parts'

function Installation() {
  return (
    <>
      <H2>Requirements</H2>
      <List>
        <li>A Linux machine (or a Mac with OrbStack/Docker Desktop for testing) with <strong>Docker Engine</strong> and <strong>Docker Compose v2</strong>.</li>
        <li>Ports <C>80</C> and <C>443</C> free: stop any other reverse proxy first.</li>
        <li>Port <C>8181</C> for the admin UI. Change it any time under <UI>Settings → General → Admin UI</UI>; Relay switches ports without a restart and moves your browser along.</li>
        <li>For public HTTPS: domains pointing at your public IP, with ports 80 and 443 forwarded on your router.</li>
      </List>

      <H2>Install</H2>
      <Steps>
        <Step title="Get Relay">
          <Example lang="bash" code={`git clone https://github.com/ollieInstantOffr/app-relay.git relay
cd relay`} />
        </Step>
        <Step title="Start the stack">
          <Example lang="bash" code={`make up`} />
          <C>make up</C> runs <C>docker compose up -d --build</C> with the version and commit taken from git, so the UI shows the right version. This starts <C>relay</C>, <C>relay-nginx</C>, <C>relay-edge</C> (on standby), <C>relay-haproxy</C> and <C>relay-balancer</C> (on standby). Check them with <C>docker compose ps</C>.
        </Step>
        <Step title="Open the UI">
          Browse to <C>http://&lt;server-ip&gt;:8181</C>, e.g. <C>http://192.168.1.10:8181</C>.
        </Step>
        <Step title="Complete the setup wizard">
          <List>
            <li><strong>Admin account</strong>: username and a strong password. Set up 2FA now (recommended).</li>
            <li><strong>Network</strong>: Relay checks ports 80/443 and your LAN range (<C>192.168.1.0/24</C>). Optionally enter an <UI>Admin UI domain</UI> like <C>relay.example.com</C>; Relay creates a host for itself with a LAN-only access list.</li>
            <li><strong>Done</strong>: choose how to start: add a host manually, create hosts from Docker, import from Nginx Proxy Manager, or set up a wildcard certificate.</li>
          </List>
        </Step>
      </Steps>

      <H2>Do I need a .env file?</H2>
      <P>No. Everything has sensible defaults. Create a <C>.env</C> next to <C>docker-compose.yml</C> only to change these:</P>
      <Table
        head={['Variable', 'Default', 'Use']}
        mono={[0, 1]}
        rows={[
          ['TZ', 'UTC', 'Timezone for logs, schedules and quiet hours, e.g. Europe/Oslo.'],
          ['RELAY_NGINX_IMAGE', 'nginx:1.30.4-alpine', 'Pin a different nginx version.'],
          ['RELAY_HAPROXY_IMAGE', 'haproxy:3.4.4-alpine', 'Pin a different HAProxy version.'],
          ['RELAY_NGINX_STATUS_PORT', '18080', 'Local nginx status port, if 18080 is taken.'],
          ['RELAY_EDGE_STATUS_PORT', '18081', 'Local Relay Edge status and metrics port, if 18081 is taken.'],
        ]}
      />
      <Example lang="bash" title=".env (example)" code={`TZ=Europe/Oslo`} />

      <H2>Ports 80/443 already in use?</H2>
      <P>
        Relay uses host networking, so the reverse proxy binds directly to the machine’s ports. If another service must keep 80/443, change the HTTP and HTTPS ports under
        {' '}<UI>Settings → General → Listening ports</UI>. Let’s Encrypt HTTP-01 then only works if something forwards port 80 to Relay; use DNS-01 instead.
      </P>
      <Example lang="bash" title="Find what is using port 80" code={`sudo ss -ltnp 'sport = :80'`} />

      <H2>Updating Relay</H2>
      <P>
        The easiest way is <UI>Settings → Updates → Upgrade</UI>: Relay pulls the newest commits, builds and restarts itself. <See id="engines">How in-app updates work →</See> From a shell:
      </P>
      <Example lang="bash" code={`git pull
make up
# optional: restart the engines so their agents use the new binary (brief interruption)
docker compose restart nginx edge haproxy balancer`} />
      <Note>The engines keep serving the last applied configuration while <C>relay</C> restarts, so updating Relay doesn’t interrupt traffic. Relay Edge and Relay Balancer ship with Relay and run the new version once their containers restart; nginx and HAProxy versions are upgraded separately on the same page.</Note>

      <H2>What to back up</H2>
      <P>Only the <C>relay-data</C> volume matters: database, certificates and backups. Everything else is rebuilt from it. <See id="backups">Backup &amp; restore →</See></P>
      <Tip>Put the admin UI on its own domain with HTTPS early. <C>http://ip:8181</C> sends your password unencrypted over the network.</Tip>
      <Warn>The compose file mounts the Docker socket into <C>relay</C> for discovery and engine upgrades. That is root-equivalent access on the host; see <See id="docker-hosts">Docker security</See> for a socket-proxy alternative.</Warn>
    </>
  )
}

export const setupSections: DocSection[] = [
  {
    id: 'installation',
    group: 'Getting started',
    title: 'Installation & first run',
    icon: 'download',
    summary: 'Install Relay with Docker Compose, finish the setup wizard and learn which settings you may want to change.',
    keywords: 'install setup docker compose requirements wizard first run env .env timezone ports 80 443 8181 update upgrade relay',
    Body: Installation,
  },
]
