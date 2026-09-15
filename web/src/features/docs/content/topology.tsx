import type { DocSection } from '../types'
import { C, Defs, H2, List, Note, P, Table, Tip, UI } from '../parts'

function Topology() {
  return (
    <>
      <P>
        <UI>Topology</UI> draws everything Relay routes as one live tree: clients at the top, the reverse proxy, then your proxy hosts and the load balancer with its
        backends and servers. Pipes carry the traffic: the wider the pipe and the faster its dots, the more requests take that path.
      </P>

      <H2>Reading the tree</H2>
      <Defs
        items={[
          ['Clients', 'Everyone who reached Relay in the selected window, split into LAN, VPN, Internet and blocked requests.'],
          ['Relay · reverse proxy', <>The active proxy engine (nginx or Relay Edge) with its ports, request rate, p95 latency and share of 5xx responses.</>],
          ['Proxy hosts', <>One card per host, labelled with its domain. A lock means the host has a certificate. The dot is the host’s health check.</>],
          ['Load balancer', <>HAProxy or Relay Balancer. The label on its link shows the frontend address proxy hosts use, e.g. <C>127.0.0.1:10080</C>.</>],
          ['Backends & servers', 'Black pills are backends (name, algorithm, sessions per second); the cards below them are their servers.'],
        ]}
      />
      <Table
        head={['Pipe', 'Means']}
        rows={[
          ['Grey with moving dots', 'Traffic; width and speed follow the metric chosen in the toolbar'],
          ['Pink with red dots', 'Errors: the node is down or 5% or more of its responses are 5xx'],
          ['Thin dashed line', 'No traffic in the window'],
        ]}
      />

      <H2>Working with it</H2>
      <List>
        <li>Click a node for details and actions: open it, jump to its logs, test a host’s upstream or retry a server’s health check.</li>
        <li><UI>⊖</UI> collapses a branch; <UI>+N hosts</UI> expands the quietest hosts, which are grouped when there are many.</li>
        <li>Scroll to pan and pinch (or ⌘/Ctrl + scroll) to zoom. In <UI>✋ pan</UI> mode dragging pans and scrolling zooms; hold Space to pan in select mode.</li>
        <li><UI>Selection spotlight</UI> fades everything that isn’t upstream or downstream of the selected node.</li>
        <li>The toolbar switches the metric (requests, bandwidth, errors), the window (5 min to 24 h) and pauses live updates.</li>
        <li><UI>Sankey</UI> shows the same traffic as bands from sources through Relay to hosts, backends and servers.</li>
        <li></li>
      </List>

      <H2>Filters</H2>
      <P>
        Status, access list and traffic source filters narrow the tree; parents stay visible while any child matches. Layers turn whole parts on or off, including TCP/UDP
        streams. Your filters and view are remembered in this browser.
      </P>
      <Note>
        Traffic sources are worked out from client addresses: private, loopback and link-local ranges count as <C>LAN</C>, <C>100.64.0.0/10</C> and Tailscale’s IPv6
        range as <C>VPN</C>, everything else as <C>Internet</C>. Requests answered with 403 or 444 (access lists, geo-blocking, the blocklist) count as blocked.
      </Note>

      <H2>Traffic flow for one host</H2>
      <P>
        <UI>View flow</UI> on a host (from its node or the host’s <UI>⋯</UI> menu) follows one request path hop by hop: clients, proxy, frontend and backend when the host
        uses the load balancer, then the containers or servers. It shows where time goes, error and rate-limit shares, and each server’s share of traffic.
      </P>
      <Tip>
        Load balancer figures are sessions per second from the engine’s stats; proxy figures are requests per second from the access log. Both are needed to see a
        whole path, so keep access logging on.
      </Tip>
    </>
  )
}

export const topologySections: DocSection[] = [
  {
    id: 'topology',
    group: 'Operations',
    title: 'Topology & traffic flow',
    icon: 'topology',
    summary: 'A live map of clients, Relay, hosts, the load balancer and servers, with traffic on every link.',
    keywords: 'topology map tree traffic flow sankey pipes live requests bandwidth errors lan vpn internet blocked spotlight hop latency',
    app: [{ to: '/topology', label: 'Open topology' }],
    Body: Topology,
  },
]
