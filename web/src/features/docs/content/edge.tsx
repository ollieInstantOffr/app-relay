import type { DocSection } from '../types'
import { C, Defs, Example, GoTo, H2, List, Note, P, See, Step, Steps, Table, Tip, UI, Warn } from '../parts'

function RelayEdge() {
  return (
    <>
      <H2>What it is</H2>
      <P>
        Relay Edge is Relay’s own reverse proxy engine. Instead of nginx, it serves your proxy hosts, redirects, the default host and streams on ports 80 and 443.
        It runs in the <C>relay-edge</C> container from the same binary as Relay and is managed exactly like nginx: pending changes, validation, health checks,
        config history and automatic rollback all work the same. HAProxy and the load balancer are not affected.
      </P>
      <Warn title="Beta">
        Relay Edge is in beta. It passes Relay’s full test suite and end-to-end traffic tests, but it hasn’t been proven in as many real-world setups as nginx.
        nginx stays the default, and switching back takes one apply.
      </Warn>
      <P>
        Only one proxy engine serves traffic at a time. While Relay Edge serves, nginx is stopped: it holds no ports and Relay stops it again if it ever
        starts (for example after a container restart). Relay also stops its container and starts it again when you switch back, so switching back still takes one apply. The same goes the other way round.
      </P>
      <GoTo to="/settings/proxy">Settings → Proxy engine</GoTo>

      <H2>Switching engines</H2>
      <Steps>
        <Step title="Pick Relay Edge and save">
          <UI>Settings → Proxy engine → Relay Edge → Switch to Relay Edge</UI>. Nothing changes yet: the switch becomes a pending change
          named <C>Proxy engine: nginx → Relay Edge</C>.
        </Step>
        <Step title="Apply">
          Click <UI>Apply now</UI> in the toast or <UI>Apply &amp; reload</UI> in the pending bar. Relay then:
          <List ordered>
            <li>validates the configuration with <C>relay edge check</C> (and HAProxy as usual),</li>
            <li>stops nginx and starts Relay Edge on the same ports,</li>
            <li>health-checks every enabled host.</li>
          </List>
        </Step>
        <Step title="Automatic rollback">
          If validation fails, Relay Edge doesn’t start, or a healthy host stops answering, Relay stops Relay Edge, starts nginx again and marks the version
          {' '}<C>rolled back</C>. The switch stays pending so you can fix the cause and apply again.
        </Step>
      </Steps>
      <Example
        title="A successful switch in Logs → Activity"
        code={`Switched from nginx to Relay Edge · 840 ms`}
      />
      <Tip>Switching back works the same way: choose nginx, save and apply.</Tip>

      <H2>Moving between engines</H2>
      <P>
        There is nothing to export or import. Your proxy hosts, redirects, the default host, access lists, certificates and streams are stored once in Relay,
        and each engine’s configuration is generated from them when you apply. Switch to Relay Edge and back as often as you like: nginx gets exactly the
        configuration it had before, and every change you made in the meantime.
      </P>
      <List>
        <li><strong>Custom nginx snippets</strong> stay saved on each host. Relay Edge skips them (the generated config notes which hosts have one) and nginx applies them again after switching back.</li>
        <li><strong>Config history</strong> keeps versions from both engines, so you can compare or roll back across a switch.</li>
      </List>

      <H2>What’s better than nginx</H2>
      <P>Relay Edge reproduces nginx’s behaviour for everything Relay configures (server selection, redirects, access lists, forward auth, rate limits, caching, gzip, streams). On top of that:</P>
      <Defs
        items={[
          ['Zero-downtime reloads', 'Applying a change or renewing a certificate never drops open connections. If a new configuration can’t be loaded, the old one keeps serving.'],
          ['Certificates picked up automatically', 'Renewed certificate files are loaded without a reload.'],
          ['Upstream keep-alive', 'Connections to your apps are pooled and reused, which saves a TCP (and TLS) handshake on most requests.'],
          ['Safer forward auth', <>Clients can’t smuggle their own <C>Remote-User</C> or <C>Remote-Groups</C> headers: they are always removed, and replaced with the portal’s values when passing them is on.</>],
          ['Correct sign-in redirects', <>The <C>rd</C> return address sent to the sign-in page is URL-encoded, so return paths with query strings survive.</>],
          ['Faster basic auth', 'A successful username/password check is cached for 60 seconds instead of hashing the password on every request.'],
          ['Built-in metrics', <>A Prometheus <C>/metrics</C> endpoint, no exporter needed.</>],
          ['Post-quantum TLS', 'Modern browsers get a hybrid post-quantum key exchange out of the box.'],
        ]}
      />

      <H2>What isn’t supported</H2>
      <List>
        <li>
          <strong>Custom nginx snippets.</strong> They stay saved and apply again with nginx, but Relay Edge doesn’t run them. Use the host options instead,
          or stay on nginx for hosts that need raw directives. <See id="protection">Host options →</See>
        </li>
      </List>

      <H2>Comparison</H2>
      <Table
        head={['', 'nginx', 'Relay Edge']}
        rows={[
          ['Container', <C>relay-nginx</C>, <C>relay-edge</C>],
          ['Validation', <C>nginx -t</C>, <C>relay edge check</C>],
          ['Updates', 'Official Docker image, upgraded in Settings → Updates', 'Part of Relay, updated with it'],
          ['Reloads', 'Graceful; old workers finish open requests', 'Seamless swap; open connections stay'],
          ['Upstream connections', 'New connection per request', 'Keep-alive pool'],
          ['Custom snippets', 'Yes', 'No'],
          ['Geo-blocking by country', 'Yes', 'Yes'],
          ['Metrics', <><C>stub_status</C> on 127.0.0.1:18080</>, <><C>/metrics</C> (Prometheus) and <C>/stub_status</C> on 127.0.0.1:18081</>],
          ['Config in Config history', <C>nginx.conf</C>, <><C>edge/edge.json</C> (+ <C>edge/htpasswd/…</C>)</>],
        ]}
      />

      <H2>Status and metrics</H2>
      <P>
        Relay Edge answers on a local status listener, <C>127.0.0.1:18081</C> by default (set <C>RELAY_EDGE_STATUS_PORT</C> to change it). It uses host networking,
        so query it from the Docker host itself:
      </P>
      <Table
        head={['Path', 'Returns']}
        mono={[0]}
        rows={[
          ['/healthz', '200 while Relay Edge is serving, 503 while it shuts down.'],
          ['/stub_status', 'Connection and request counters in nginx’s stub_status format.'],
          ['/metrics', 'The same counters and more in Prometheus text format.'],
        ]}
      />
      <Example lang="bash" title="From the Docker host" code={`curl http://127.0.0.1:18081/healthz
curl http://127.0.0.1:18081/metrics`} />
      <Example
        lang="yaml"
        title="prometheus.yml (Prometheus on the same host, host networking)"
        code={`scrape_configs:
  - job_name: relay-edge
    static_configs:
      - targets: ['127.0.0.1:18081']`}
      />
      <Note>The status listener only binds to localhost. To scrape from another machine, run a Prometheus agent on the Docker host or put an access-controlled proxy host in front of it.</Note>

      <H2>Troubleshooting</H2>
      <Defs
        items={[
          [
            'Apply says the Relay Edge engine is unreachable',
            <>The <C>relay-edge</C> container isn’t running. Start it with <C>docker compose up -d edge</C> and apply again. Nothing was changed.</>,
          ],
          [
            'The switch was rolled back',
            <>Open <UI>Config history</UI>: the rolled-back version shows the stage that failed and the engine output. Your switch is still pending. <See id="history">Config history →</See></>,
          ],
          [
            <>“address already in use”</>,
            <>
              Another process holds a port Relay Edge needs, e.g. <C>listen tcp :80: bind: address already in use</C>. The engine banner offers <UI>Show listeners</UI> to find it;
              from a shell run <C>sudo ss -ltnp 'sport = :80'</C>.
            </>,
          ],
          ['Something behaves differently', <>Switch back to nginx (choose it, save, apply) and check <C>docker logs relay-edge</C> for the cause.</>],
        ]}
      />
      <Warn>Don’t start <C>relay-nginx</C> and <C>relay-edge</C> on the same ports by hand. Relay stops the engine that isn’t selected whenever it reconciles.</Warn>
    </>
  )
}

export const edgeSections: DocSection[] = [
  {
    id: 'relay-edge',
    group: 'Reverse proxy',
    title: 'Relay Edge (beta)',
    icon: 'bolt',
    summary: 'Relay’s own reverse proxy engine, in beta: zero-downtime reloads, upstream keep-alive and built-in metrics. How to switch back and forth, and what differs from nginx.',
    keywords: 'relay edge engine proxy engine nginx alternative switch zero downtime reload keep-alive metrics prometheus stub_status healthz 18081 post-quantum relay-edge relay edge check',
    app: [{ to: '/settings/proxy', label: 'Choose proxy engine' }],
    Body: RelayEdge,
  },
]
