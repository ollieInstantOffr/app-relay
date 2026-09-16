import type { DocSection } from '../types'
import { C, Defs, Example, Flow, GoTo, H2, List, Note, P, See, Step, Steps, Table, Tip, UI, Warn } from '../parts'

function Tunnels() {
  return (
    <>
      <H2>What tunnels are for</H2>
      <P>
        Relay normally needs ports 80 and 443 to reach it from the internet. Behind carrier-grade NAT, on a rented connection or when you don’t want to expose
        your home IP address, that isn’t possible. A tunnel gateway solves it: a small <C>relay gateway</C> runs on a public server (any VPS), your Relay dials
        out to it, and the gateway sends the traffic for published hosts back through that connection. Nothing is opened on your router.
      </P>
      <Flow
        steps={[
          { label: 'Visitor', sub: 'app.example.com' },
          { label: 'Gateway', sub: 'VPS · ports 80/443' },
          { label: 'Tunnel', sub: 'QUIC or TCP · dialed from home' },
          { label: 'Relay', sub: 'TLS, access lists, login' },
          { label: 'App', sub: 'your network' },
        ]}
      />
      <List>
        <li><strong>HTTPS stays encrypted until it reaches your Relay.</strong> The gateway routes by the requested name (SNI) and never has your certificates or keys.</li>
        <li><strong>Everything else keeps working:</strong> access lists, geo-blocking, rate limits, Relay login and logs see the visitor’s real IP address.</li>
        <li><strong>Only what you publish is reachable.</strong> Relay itself refuses any other name that arrives through the tunnel.</li>
        <li><strong>TCP streams too</strong> (game servers, SSH, databases); UDP isn’t carried.</li>
      </List>

      <H2>Connect a gateway</H2>
      <Steps>
        <Step title="Create the gateway in Relay">
          <UI>Tunnels → Connect gateway</UI>. Enter a name and the server’s public address (host name or IP). Relay creates a key for it and a one-time pairing token.
        </Step>
        <Step title="Start the gateway on the server">
          The wizard shows the command, with the token filled in. It needs Docker with Compose on the server:
          <Example
            lang="shell"
            code={'git clone https://github.com/ollieInstantOffr/app-relay.git relay-gateway && cd relay-gateway\nRELAY_GATEWAY_PAIR_TOKEN=rlypair1_… docker compose -f deploy/gateway/docker-compose.yml up -d --build'}
          />
          Allow <C>80/tcp</C>, <C>443/tcp</C>, <C>7443/tcp</C> and <C>7443/udp</C> in the server’s firewall, plus the TCP ports of streams you publish.
        </Step>
        <Step title="Wait for pairing">
          Relay keeps trying while the wizard is open and pairs as soon as the gateway answers. Both sides then only trust each other’s keys; the token is used up.
        </Step>
        <Step title="Publish hosts and streams">
          Pick them in the last step, or later in a host (<UI>Details → Publish through tunnel</UI>) or a TCP stream. Publishing is a normal pending change: apply it.
        </Step>
        <Step title="Point DNS at the gateway">
          The domains of published hosts must resolve to the gateway. With <See id="public-dns">Public DNS</See>, new records point there automatically; change
          existing records by hand.
        </Step>
      </Steps>
      <GoTo to="/tunnels">Tunnels</GoTo>

      <H2>How it works</H2>
      <Defs
        items={[
          ['Tunnel engine', <>Runs in the <C>relay-tunnel</C> container while something is published through a tunnel, like the load balancer engines. It dials every paired gateway and hands connections to nginx or Relay Edge over local sockets.</>],
          ['Transport', <>QUIC (UDP) by default, falling back to HTTP/2 over TCP when UDP is blocked. Choose <UI>QUIC</UI> or <UI>TCP</UI> on the gateway to force one.</>],
          ['Pairing', <>A one-time token proves both sides to each other over TLS. After that, Relay and the gateway only accept each other’s keys (pinned fingerprints).</>],
          ['Client address', <>The gateway reports the visitor’s address; Relay replaces private or loopback addresses with the gateway’s own address, so a gateway can’t impersonate your LAN.</>],
          ['Apply and rollback', <>The tunnel engine’s configuration is part of every config version: it is validated, applied after the proxy engine and rolled back with it.</>],
          ['Gateway changes', <>Address, transport, enabling and pairing take effect immediately and don’t create pending changes.</>],
        ]}
      />
      <Note title="Status and alerts">
        Each gateway card shows whether it is connected, the transport and round-trip time, and traffic. A gateway that stays down for a minute is logged in
        Activity and sends the <C>tunnel_down</C> notification.
      </Note>

      <H2>Security</H2>
      <P>The gateway can’t read HTTPS traffic, but it isn’t fully trusted either:</P>
      <List>
        <li>Whoever controls the server receives port 80 for your domains, so they could pass an HTTP-01 challenge and get a certificate for them.</li>
        <li>Use <See id="certificates">DNS-01 certificates</See> for published hosts, and a CAA record that only allows your ACME account, e.g. <C>example.com. CAA 0 issue "letsencrypt.org; validationmethods=dns-01"</C>.</li>
        <li>Keep the pairing token secret until the gateway is paired: whoever uses it first pairs their Relay.</li>
      </List>
      <Warn title="The Relay admin UI">
        You can publish the admin UI host through a tunnel. If you manage Relay through that tunnel, a change that breaks it also cuts you off until the
        automatic rollback restores the previous version.
      </Warn>

      <H2>Pair again or remove a gateway</H2>
      <Table
        head={['Task', 'How']}
        rows={[
          ['Pair again (new server or lost state)', <>On the server: <C>docker compose -f deploy/gateway/docker-compose.yml exec gateway relay gateway reset</C>. In Relay: <UI>Pair again…</UI> and start the gateway with the new token.</>],
          ['Check the gateway key', <><C>… exec gateway relay gateway info</C> shows the fingerprint; compare it with the gateway key in Relay.</>],
          ['Update the gateway', <>On the server: <C>git pull</C>, then the same <C>docker compose … up -d --build</C> without a token. Keep it on the same Relay version.</>],
          ['Remove it', <>Stop publishing hosts and streams through it and apply, then delete the gateway in Relay and stop the container on the server.</>],
        ]}
      />
      <Tip>A backup of Relay contains the gateways but not their keys. After restoring on a new machine, pair the gateways again.</Tip>

      <H2>Troubleshooting</H2>
      <Table
        head={['Problem', 'What to check']}
        rows={[
          ['The wizard keeps waiting', <>The gateway container is running (<C>docker logs relay-gateway</C>) and TCP port 7443 is open from the internet.</>],
          ['“The gateway rejected the pairing token”', <>It was already paired or started with another token. Run <C>relay gateway reset</C> on the server and use a new command.</>],
          ['Connected over TCP instead of QUIC', <>UDP port 7443 is blocked by the server’s or your network’s firewall. TCP works; QUIC reconnects faster.</>],
          ['A published site doesn’t load', <>DNS points at the gateway, the host is applied, and the gateway card shows <C>connected</C>. Port 80/443 errors appear on the card.</>],
          ['A stream port shows an error', <>Another process on the server uses the port, or it is reserved (22, 80, 443, 7443).</>],
          ['“version mismatch”', <>Update the gateway to the same Relay version.</>],
        ]}
      />
    </>
  )
}

export const tunnelSections: DocSection[] = [
  {
    id: 'tunnels',
    group: 'Reverse proxy',
    title: 'Tunnels',
    icon: 'tunnel',
    summary: 'Publish hosts and TCP streams through a gateway on a public server, without port forwarding: setup, pairing, security and troubleshooting.',
    keywords: 'tunnel gateway vps cgnat port forwarding cloudflare tunnel alternative relay gateway pairing token quic relay-tunnel publish through tunnel sni',
    app: [{ to: '/tunnels', label: 'Open Tunnels' }],
    Body: Tunnels,
  },
]
