import type { DocSection } from '../types'
import { C, Example, H2, H3, P, See, Table, UI } from '../parts'

function Troubleshooting() {
  return (
    <>
      <H2>I can’t open the Relay UI</H2>
      <Example lang="bash" code={`docker compose ps                 # is relay running and healthy?
docker compose logs --tail 50 relay
curl -fsS http://127.0.0.1:8181/healthz`} />
      <P>
        If it works on the server but not from your laptop, check the server’s firewall for port 8181. If you enabled <UI>Restrict admin UI to LAN</UI>, make sure your address is
        inside the LAN range.
      </P>

      <H2>A host shows 502 Bad Gateway</H2>
      <Table
        head={['Check', 'How']}
        rows={[
          ['Is the app running?', <C>curl -I http://192.168.1.20:3000</C>],
          ['Right port and scheme?', 'An app that serves HTTPS needs upstream scheme https.'],
          ['Reachable from Relay’s host?', <>Run the curl on the Docker host itself; firewalls (ufw, firewalld) often block it.</>],
          ['What does the proxy say?', <><UI>Logs → Error</UI>, e.g. <C>connect() failed (111: Connection refused)</C>.</>],
          ['App bound to localhost only?', <>Apps listening on <C>127.0.0.1</C> inside a container can’t be reached; bind to <C>0.0.0.0</C>.</>],
        ]}
      />

      <H2>Too many redirects</H2>
      <P>
        Usually the app redirects to HTTPS itself while Relay talks to it over HTTP, or Cloudflare is set to <em>Flexible</em> SSL. Set Cloudflare to <em>Full (strict)</em>, and configure
        the app to trust <C>X-Forwarded-Proto</C> (e.g. <C>app.set('trust proxy', 1)</C> in Express).
      </P>

      <H2>My apply was rolled back</H2>
      <P>
        Open <UI>Config history</UI>: the failed attempt shows why, such as a validation error or a host that started failing its health check. Your changes are still pending. Fix them and
        apply again. <See id="applying">How applying works →</See>
      </P>

      <H2>A certificate request fails</H2>
      <P>See the troubleshooting table in <See id="certificates">Certificates</See>. The most common cause is port 80 not being reachable from the internet; DNS-01 avoids that entirely.</P>

      <H2>Docker discovery shows no containers</H2>
      <Table
        head={['Symptom', 'Fix']}
        rows={[
          ['“Docker API unavailable”', <>The socket isn’t mounted. The <C>relay</C> service needs <C>/var/run/docker.sock:/var/run/docker.sock:ro</C>.</>],
          ['Only Relay’s own containers', 'Discovery lists every container on that Docker host; containers on other machines need a remote Docker host.'],
          ['Remote host: “no reachable address”', 'Publish the container’s port on the remote machine.'],
        ]}
      />

      <H2>Websocket connections drop</H2>
      <P>Turn on <UI>Websockets</UI> on the host (or on the location that serves the socket), and raise proxy timeouts if connections idle for more than 60 seconds.</P>

      <H2>Uploads fail with 413</H2>
      <P>Raise <UI>Max upload size</UI> on the host’s Advanced tab, for example to 10 GB for a photo library.</P>

      <H2>The load balancer engine isn’t running</H2>
      <P>
        The load balancer engine (HAProxy or Relay Balancer) only starts once at least one backend exists and <UI>Run HAProxy</UI> or <UI>Run Relay Balancer</UI> is on (<UI>Settings → Load balancer</UI>). The engine banner at the top of the screen links to
        its error log.
      </P>

      <H2>Where are the raw logs?</H2>
      <Example lang="bash" code={`docker logs relay            # Relay app
docker logs relay-nginx      # nginx engine + agent
docker logs relay-edge       # Relay Edge engine + agent
docker logs relay-haproxy    # HAProxy engine + agent
docker logs relay-balancer   # Relay Balancer engine + agent`} />
    </>
  )
}

function Reference() {
  return (
    <>
      <H2>Ports</H2>
      <Table
        head={['Port', 'Used by', 'Purpose']}
        mono={[0]}
        rows={[
          ['80/tcp', 'nginx or edge', 'HTTP, redirects to HTTPS, ACME HTTP-01 challenges'],
          ['443/tcp', 'nginx or edge', 'HTTPS for proxy hosts'],
          ['443/udp', 'nginx or edge', 'HTTP/3 (only when enabled)'],
          ['8181/tcp', 'relay', 'Admin UI, REST API and MCP endpoint (change in Settings → General)'],
          ['127.0.0.1:18080', 'nginx', 'Status for Relay’s metrics'],
          ['127.0.0.1:18081', 'edge', 'Relay Edge status: /healthz, /stub_status, /metrics (Prometheus)'],
          ['127.0.0.1:8404', 'haproxy / balancer', 'Stats and Prometheus metrics'],
          ['127.0.0.1:10080+', 'haproxy / balancer', 'Local frontends created by Expose'],
          ['(your choice)', 'nginx or edge', 'TCP/UDP streams'],
        ]}
      />

      <H2>Volumes</H2>
      <Table
        head={['Volume', 'Contains']}
        mono={[0]}
        rows={[
          ['relay-data', 'Database, certificates, ACME files and backups. The one to back up.'],
          ['relay-run', 'Control sockets between Relay and the engines.'],
          ['relay-logs', 'Access, stream and error logs of the proxy engine.'],
          ['relay-bin', 'The Relay agent binary shared with the engine containers.'],
          ['relay-nginx · relay-edge · relay-haproxy · relay-balancer', 'Applied configuration releases, so engines keep serving after a restart.'],
        ]}
      />

      <H2>Environment variables</H2>
      <Table
        head={['Variable', 'Container', 'Meaning']}
        mono={[0, 1]}
        rows={[
          ['TZ', 'all', 'Timezone.'],
          ['RELAY_LISTEN', 'relay', 'Bind address and first-start port of the admin UI (default :8181). Afterwards the port is set in Settings → General.'],
          ['RELAY_NGINX_IMAGE / RELAY_HAPROXY_IMAGE', 'compose', 'Engine image versions.'],
          ['RELAY_NGINX_STATUS_PORT', 'relay, nginx', 'Local nginx status port (default 18080).'],
          ['RELAY_EDGE_STATUS_PORT', 'relay, edge', 'Local Relay Edge status and metrics port (default 18081).'],
          ['RELAY_BOOTSTRAP_HTTP_PORT', 'nginx, edge', 'Port for the bootstrap config before the first apply.'],
          ['RELAY_ACME_DNS_RESOLVERS', 'relay', 'Resolvers for DNS-01 propagation checks, e.g. 10.0.0.53:53.'],
          ['RELAY_ACME_INSECURE_SKIP_VERIFY', 'relay', 'Testing only: skip TLS verification of the ACME server.'],
          ['RELAY_COMPOSE_PROJECT', 'relay', 'Set automatically; which compose project’s engines Relay manages.'],
          ['RELAY_UPDATE_BRANCH', 'relay', 'Branch the in-app updater follows (default main).'],
          ['RELAY_UPDATER_IMAGE', 'relay', 'Helper image for in-app updates (default docker:29.8.0-cli).'],
          ['RELAY_VERSION', 'compose', 'Build argument with the version from scripts/version.sh; set by make up and in-app upgrades (otherwise the version shows as dev).'],
          ['RELAY_COMMIT', 'compose', 'Build argument recording the commit Relay is built from; set by make up and in-app upgrades.'],
        ]}
      />

      <H2>Command line</H2>
      <Table
        head={['Command', 'Does']}
        mono={[0]}
        rows={[
          ['relay users reset-password <user>', 'Print a new one-time password for a user.'],
          ['relay mcp-stdio --token rl_mcp_…', 'Run the MCP server over stdio for local AI clients.'],
          ['relay version', 'Print the version and build commit.'],
          ['relay healthcheck', 'Exit 0 when the admin UI answers (used by the container healthcheck; follows the admin port).'],
          ['relay serve', 'Run the app (what the relay container does).'],
          ['relay agent --engine nginx|haproxy|edge|balancer', 'Run an engine agent (what the engine containers do).'],
          ['relay edge run --config <file>', 'Run Relay Edge with a config file (the edge agent does this).'],
          ['relay edge check <dir>', 'Validate a Relay Edge config release; exit 0 when valid.'],
        ]}
      />
      <Example lang="bash" title="Run commands inside the container" code={`docker exec -it relay relay users reset-password admin
docker exec -it relay relay version`} />

      <H2>Token prefixes</H2>
      <Table
        head={['Prefix', 'Used for']}
        mono={[0]}
        rows={[
          ['rl_api_', <>REST API. <See id="rest-api">REST API tokens</See></>],
          ['rl_mcp_', <>MCP clients. <See id="mcp">MCP server</See></>],
        ]}
      />

      <H3>Glossary</H3>
      <Table
        head={['Term', 'Meaning']}
        rows={[
          ['Upstream', 'The app a proxy host forwards to.'],
          ['Pending change', 'A saved edit that isn’t live yet.'],
          ['Apply', 'Validate and make pending changes live.'],
          ['Version', 'A configuration that was live at some point; you can roll back to it.'],
          ['ACME', 'The protocol Let’s Encrypt and others use to issue certificates.'],
          ['Drain', 'Let a server finish its sessions without receiving new ones.'],
          ['Forward auth', 'Asking a login service whether a visitor may pass.'],
        ]}
      />
    </>
  )
}

export const referenceSections: DocSection[] = [
  {
    id: 'troubleshooting',
    group: 'Reference',
    title: 'Troubleshooting',
    icon: 'warning',
    summary: 'Fixes for the most common problems: unreachable UI, 502s, redirect loops, failed applies and more.',
    keywords: 'troubleshooting faq problem error 502 bad gateway redirect loop too many redirects rollback 413 websocket not working help debug',
    Body: Troubleshooting,
  },
  {
    id: 'reference',
    group: 'Reference',
    title: 'Ports, volumes & CLI',
    icon: 'info',
    summary: 'Every port, volume, environment variable and command in one place.',
    keywords: 'reference ports volumes environment variables env cli command line tokens glossary',
    Body: Reference,
  },
]
