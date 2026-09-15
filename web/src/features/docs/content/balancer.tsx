import type { DocSection } from '../types'
import { C, Defs, Example, GoTo, H2, List, Note, P, See, Step, Steps, Table, Tip, UI, Warn } from '../parts'

function RelayBalancer() {
  return (
    <>
      <H2>What it is</H2>
      <P>
        Relay Balancer is Relay’s own load balancer engine. Instead of HAProxy, it runs your backends and frontends: health checks, sticky sessions, weights,
        draining and routing rules. It runs in the <C>relay-balancer</C> container from the same binary as Relay and is managed exactly like HAProxy: pending
        changes, validation, config history and the Expose wizard all work the same. The reverse proxy (nginx or Relay Edge) is not affected.
      </P>
      <Warn title="Beta">
        Relay Balancer is in beta. It is built to behave like the HAProxy config Relay generates, but it hasn’t been proven in as many real-world setups as HAProxy.
        HAProxy stays the default, and switching back takes one apply.
      </Warn>
      <P>
        Only one load balancer engine runs at a time. While Relay Balancer runs, HAProxy is stopped and holds no ports, and its container is stopped too.
        Relay starts it again when you switch back. The same goes the other way round.
      </P>
      <GoTo to="/settings/lb-engine">Settings → Load balancer engine</GoTo>

      <H2>Switching engines</H2>
      <Steps>
        <Step title="Pick Relay Balancer and save">
          <UI>Settings → Load balancer engine → Relay Balancer → Switch to Relay Balancer</UI>. Nothing changes yet: the switch becomes a pending change
          named <C>Load balancer engine: HAProxy → Relay Balancer</C>.
        </Step>
        <Step title="Apply">
          Click <UI>Apply now</UI> in the toast or <UI>Apply &amp; reload</UI> in the pending bar. Relay then:
          <List ordered>
            <li>validates the configuration with <C>relay balancer check</C> (and the proxy engine as usual),</li>
            <li>stops HAProxy and starts Relay Balancer with the same frontends and ports,</li>
            <li>checks that the engine is running.</li>
          </List>
        </Step>
        <Step title="If something fails">
          A failed apply is marked <C>rolled back</C> or <C>failed</C> in <See id="history">Config history</See>, with the stage and engine output. The switch stays
          pending so you can fix the cause and apply again.
        </Step>
      </Steps>
      <Tip>Switching back works the same way: choose HAProxy, save and apply.</Tip>

      <H2>Moving between engines</H2>
      <P>
        There is nothing to export or import. Backends, servers, frontends, rules and the load balancer settings are stored once in Relay, and each engine’s
        configuration is generated from them when you apply. HAProxy gets <C>haproxy.cfg</C>, Relay Balancer gets <C>balancer.json</C>. Switch as often as you like.
      </P>
      <List>
        <li><strong>Server states</strong> (drain, maintenance) and weights you set are kept across a switch.</li>
        <li><strong>Config history</strong> keeps versions from both engines, so you can compare or roll back across a switch.</li>
        <li><strong>Settings → Load balancer</strong> (timeouts, health check defaults, stats, Expose ports) is shared by both engines.</li>
      </List>

      <H2>What’s the same</H2>
      <Table
        head={['Feature', 'HAProxy', 'Relay Balancer']}
        rows={[
          ['HTTP and TCP backends', 'Yes', 'Yes'],
          ['Algorithms (roundrobin, leastconn, source, uri, random, first)', 'Yes', 'Yes'],
          ['Health checks (tcp, http, pgsql, mysql, redis) with rise/fall', 'Yes', 'Yes'],
          ['Sticky sessions (insert cookie, prefix cookie, source IP)', 'Yes', 'Yes'],
          ['Weights, backup servers', 'Yes', 'Yes'],
          ['Drain and maintenance without an apply', 'Yes', 'Yes'],
          ['Frontends with host, path, header, source IP and SNI rules', 'Yes', 'Yes'],
          ['PROXY protocol, X-Forwarded-For, compression, TLS to servers', 'Yes', 'Yes'],
          ['Expose wizard', 'Yes', 'Yes'],
          ['Live stats in Load balancer → Stats, Prometheus metrics', 'Yes', 'Yes'],
        ]}
      />

      <H2>What’s different</H2>
      <Defs
        items={[
          [
            'Path regex rules use RE2',
            <>
              <C>path_reg</C> conditions are Go (RE2) regular expressions. Everyday patterns like <C>^/v[0-9]+/</C> work the same; lookarounds (<C>(?=…)</C>, <C>(?!…)</C>)
              and backreferences (<C>\1</C>) aren’t supported and are rejected when the config is checked.
            </>,
          ],
          ['Reloads are always seamless', <>Open connections carry on through every reload. The <UI>Seamless reloads</UI> switch in Settings → Load balancer only applies to HAProxy.</>],
          ['The stats page looks different', 'The built-in stats page shows the same backends, servers and counters, in Relay Balancer’s own layout.'],
          ['No image to update', <>Relay Balancer is part of Relay and updates with it. <UI>Settings → Updates</UI> shows the HAProxy card as <C>Not in use</C> while Relay Balancer is the engine.</>],
        ]}
      />

      <H2>Comparison</H2>
      <Table
        head={['', 'HAProxy', 'Relay Balancer']}
        rows={[
          ['Container', <C>relay-haproxy</C>, <C>relay-balancer</C>],
          ['Validation', <C>haproxy -c</C>, <C>relay balancer check</C>],
          ['Config file', <C>haproxy.cfg</C>, <><C>balancer.json</C> (JSON)</>],
          ['Updates', 'Official Docker image, upgraded in Settings → Updates', 'Part of Relay, updated with it'],
          ['Reloads', 'Seamless when the setting is on', 'Always seamless'],
          ['Path regex syntax', 'PCRE', 'RE2'],
          ['Maturity', 'Rock solid', 'Beta'],
        ]}
      />

      <H2>Stats and metrics</H2>
      <P>
        Both engines use the stats endpoint from <UI>Settings → Load balancer</UI> (default <C>127.0.0.1:8404</C>), including its access list. Relay Balancer serves:
      </P>
      <Table
        head={['Path', 'Returns']}
        mono={[0]}
        rows={[
          ['/', 'The stats page.'],
          ['/;csv', 'The same data as CSV, in HAProxy’s column format.'],
          ['/metrics', 'Prometheus metrics, when Prometheus metrics is on.'],
        ]}
      />
      <Example lang="bash" title="From the Docker host" code={`curl http://127.0.0.1:8404/metrics`} />
      <Example
        lang="yaml"
        title="prometheus.yml"
        code={`scrape_configs:
  - job_name: relay-balancer
    metrics_path: /metrics
    static_configs:
      - targets: ['127.0.0.1:8404']`}
      />
      <Note>
        Relay talks to Relay Balancer over a runtime socket that speaks the part of HAProxy’s runtime API Relay needs (<C>show stat</C>, <C>show info</C>, setting
        server state and weight). That is why live stats, draining and weight changes work the same with both engines.
      </Note>

      <H2>Troubleshooting</H2>
      <Defs
        items={[
          [
            'Apply says the Relay Balancer engine is unreachable',
            <>The <C>relay-balancer</C> container isn’t running. Start it with <C>docker compose up -d balancer</C> and apply again. Nothing was changed.</>,
          ],
          [
            'The config doesn’t validate',
            <>
              The error comes from <C>relay balancer check</C> and is shown in the apply toast and in Config history. A rule that works on HAProxy but fails here is
              usually a <C>path_reg</C> pattern using lookarounds or backreferences. <UI>Load balancer → View config → Validate</UI> runs the same check without applying.
            </>,
          ],
          [
            'The switch was rolled back',
            <>Open <UI>Config history</UI>: the rolled-back version shows the stage that failed and the engine output. Your switch is still pending. <See id="history">Config history →</See></>,
          ],
          ['Something behaves differently', <>Switch back to HAProxy (choose it, save, apply) and check <C>docker logs relay-balancer</C> for the cause.</>],
        ]}
      />
      <Warn>Don’t start <C>relay-haproxy</C> and <C>relay-balancer</C> by hand at the same time. Relay stops the engine that isn’t selected whenever it reconciles.</Warn>
    </>
  )
}

export const balancerSections: DocSection[] = [
  {
    id: 'relay-balancer',
    group: 'Load balancer',
    title: 'Relay Balancer (beta)',
    icon: 'load-balancer',
    summary: 'Relay’s own load balancer engine, in beta: the same backends, frontends and stats as HAProxy, built into Relay. How to switch, and what differs.',
    keywords: 'relay balancer engine load balancer engine haproxy alternative switch seamless reload re2 regex stats prometheus 8404 runtime api show stat relay-balancer relay balancer check balancer.json',
    app: [{ to: '/settings/lb-engine', label: 'Choose load balancer engine' }],
    Body: RelayBalancer,
  },
]
