import type { DocSection } from '../types'
import { C, Defs, Example, H2, H3, Note, P, See, Step, Steps, Table, Tip, UI, Warn } from '../parts'

function Certificates() {
  return (
    <>
      <H2>Which kind of certificate?</H2>
      <Defs
        items={[
          ['HTTP-01', 'The easiest. Let’s Encrypt checks your domain over port 80. Needs the domain to point at Relay and port 80 open to the internet.'],
          ['DNS-01', <>Proves ownership by creating a DNS record through your DNS provider’s API. Works for <strong>wildcards</strong> (<C>*.example.com</C>) and for servers that aren’t reachable from the internet.</>],
          ['Upload', 'Bring your own certificate and key: a purchased certificate or one from your internal CA.'],
          ['Custom ACME', 'Use another ACME CA instead of Let’s Encrypt: ZeroSSL, Google Trust Services, step-ca or an internal CA.'],
        ]}
      />

      <H2>Request a certificate (HTTP-01)</H2>
      <Steps>
        <Step title="Check the prerequisites">
          <C>app.example.com</C> resolves to your public IP, and port 80 is forwarded to Relay’s host.
        </Step>
        <Step title="Request">
          <UI>Certificates → Request certificate</UI>. Enter the domains (press Enter after each), keep challenge <UI>HTTP-01</UI>.
        </Step>
        <Step title="Test with staging first (optional)">
          Tick <UI>Use staging first</UI>. Staging has no rate limits, but browsers don’t trust its certificates. Once it works, request again without it.
        </Step>
        <Step title="Attach it">
          The certificate shows <C>pending</C> and then <C>valid</C>. Pick it on the host’s <UI>SSL</UI> tab and apply. You can also start the request from the host itself with <UI>Request new</UI>.
        </Step>
      </Steps>

      <H2>Wildcard certificate with DNS-01</H2>
      <P>One certificate for <C>*.example.com</C> covers every sub-domain, so new hosts get HTTPS instantly without a new request.</P>
      <Steps>
        <Step title="Create an API token at your DNS provider">
          For Cloudflare: <UI>My Profile → API Tokens → Create token</UI> using the <em>Edit zone DNS</em> template, limited to your zone.
        </Step>
        <Step title="Add the DNS provider in Relay">
          <UI>Certificates → DNS providers → Add</UI>. Pick the provider, paste the credentials and give it a name. Relay tests the credentials.
        </Step>
        <Step title="Request the certificate">
          <Example code={`Domains     *.example.com, example.com
Challenge   DNS-01
Provider    Cloudflare (main)`} />
          Include the bare domain too: <C>*.example.com</C> doesn’t cover <C>example.com</C> itself.
        </Step>
        <Step title="Use it">New hosts using <C>anything.example.com</C> pick up the wildcard automatically when their certificate is set to <em>auto</em>.</Step>
      </Steps>

      <H3>Supported DNS providers</H3>
      <P>Built in: Cloudflare, AWS Route 53, DigitalOcean, Hetzner, Gandi, GoDaddy, Namecheap and DuckDNS.</P>
      <P>
        Anything else supported by <strong>lego</strong> (over 100 providers) can be picked from the same list. Relay then asks for that provider’s lego environment variables and stores them as secrets.
      </P>
      <Example
        title="Example: Porkbun (lego)"
        code={`Provider               Porkbun (lego)
PORKBUN_API_KEY        pk1_…
PORKBUN_SECRET_API_KEY sk1_…`}
      />

      <H2>Renewal</H2>
      <P>
        Certificates renew automatically well before they expire. Set how early under <UI>Settings → Default TLS → Renew when</UI>. If a renewal fails, the
        certificate shows the error and a <C>cert_renew_failed</C> notification is sent. <See id="notifications">Set up notifications →</See>
      </P>

      <H2>Upload your own certificate</H2>
      <P><UI>Certificates → Upload custom</UI> and paste or drop PEM files:</P>
      <Example code={`Certificate   -----BEGIN CERTIFICATE-----   (fullchain: your certificate followed by intermediates)
Private key   -----BEGIN PRIVATE KEY-----`} />
      <Note>Uploaded certificates don’t renew by themselves. Relay warns you before they expire.</Note>

      <H2>Custom ACME server</H2>
      <P><UI>Settings → Default TLS</UI>, set <UI>Provider</UI> to Custom ACME server:</P>
      <Table
        head={['Field', 'Example']}
        rows={[
          ['ACME directory URL', <C>https://ca.internal:9000/acme/acme/directory</C>],
          ['CA bundle', 'The PEM root certificate of your internal CA, if its HTTPS isn’t publicly trusted.'],
          ['EAB key ID + HMAC key', 'Required by ZeroSSL and Google Trust Services; shown in their dashboards.'],
          ['Contact email', 'Expiry notices from the CA.'],
        ]}
      />
      <Tip>
        Using split-horizon or internal DNS? Set <C>RELAY_ACME_DNS_RESOLVERS=10.0.0.53:53</C> on the <C>relay</C> container so DNS-01 propagation is checked against your own resolvers.
      </Tip>

      <H2>TLS policy for every host</H2>
      <P><UI>Settings → Default TLS</UI> also controls HSTS defaults (max-age, includeSubDomains, preload) and OCSP stapling. Each host can pick a cipher profile on its SSL tab.</P>

      <H2>Troubleshooting</H2>
      <Table
        head={['Error mentions', 'Likely cause', 'Fix']}
        rows={[
          ['Timeout / connection refused during HTTP-01', 'Port 80 isn’t reachable from the internet.', 'Forward TCP 80 to Relay’s host; check the ISP doesn’t block it, or use DNS-01.'],
          ['NXDOMAIN / no valid A records', 'DNS doesn’t point at you (yet).', 'Fix the record and wait for DNS to update.'],
          ['too many certificates / rateLimited', 'Let’s Encrypt rate limit.', 'Wait (limits reset weekly). Test with staging first next time.'],
          ['DNS record not found / propagation', 'Resolvers haven’t seen the TXT record.', <>Retry, check API token permissions, or set <C>RELAY_ACME_DNS_RESOLVERS</C>.</>],
          ['CAA record forbids', 'Your domain’s CAA record allows another CA only.', <>Add <C>0 issue "letsencrypt.org"</C>.</>],
        ]}
      />
      <Warn>Never enable HSTS preload unless every sub-domain serves valid HTTPS. It is very hard to undo.</Warn>
    </>
  )
}

export const tlsSections: DocSection[] = [
  {
    id: 'certificates',
    group: 'Certificates',
    title: 'Certificates & HTTPS',
    icon: 'certificates',
    summary: 'Free Let’s Encrypt certificates, wildcards with DNS-01, uploads, custom ACME servers and automatic renewal.',
    keywords: 'certificate ssl tls https lets encrypt acme http-01 dns-01 wildcard cloudflare route53 hetzner lego zerossl step-ca upload pem renew staging rate limit caa',
    app: [{ to: '/certificates?request=1', label: 'Request certificate' }, { to: '/settings/tls', label: 'Default TLS settings' }],
    Body: Certificates,
  },
]
