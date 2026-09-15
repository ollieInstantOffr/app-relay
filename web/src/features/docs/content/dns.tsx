import type { DocSection } from '../types'
import { C, Defs, Example, Flow, H2, H3, List, Note, P, See, Step, Steps, Table, Tip, UI, Warn } from '../parts'

function PublicDNS() {
  return (
    <>
      <P>
        Public DNS connects Relay to the company that hosts your domain’s DNS (GoDaddy or Cloudflare). You can see and edit your records in Relay, and Relay can
        create the missing record when you add a proxy host, so a new domain works without visiting your DNS provider.
      </P>
      <Flow
        steps={[
          { label: 'Save a host', sub: 'test.example.com' },
          { label: 'Apply', sub: 'config goes live' },
          { label: 'Relay checks DNS', sub: 'is there a record?' },
          { label: 'Creates it', sub: 'only if missing' },
        ]}
      />

      <H2>Which providers work</H2>
      <Defs
        items={[
          ['Cloudflare', <>Works on the free plan. Create an API token with <C>Zone → DNS → Edit</C> and <C>Zone → Zone → Read</C>.</>],
          ['GoDaddy', <>Only accounts with <strong>10 or more domains</strong> or a <strong>Discount Domain Club</strong> plan get API access. Other accounts get “Access denied”.</>],
        ]}
      />
      <Warn title="GoDaddy says “Access denied”?">
        That’s GoDaddy’s API restriction, not a wrong key. Keep your domain registered at GoDaddy and let Cloudflare (free) host its DNS instead, as described below.
      </Warn>

      <H3>Use Cloudflare DNS for a domain registered at GoDaddy</H3>
      <Steps>
        <Step title="Add the domain to Cloudflare">
          Sign up at Cloudflare, choose <UI>Add a domain</UI>, enter it (e.g. <C>example.com</C>) and pick the <UI>Free</UI> plan.
        </Step>
        <Step title="Check the imported records">
          Cloudflare copies your existing records. Compare them with GoDaddy’s DNS page, especially mail (<C>MX</C>, <C>TXT</C>) records, and add anything missing.
        </Step>
        <Step title="Change the nameservers at GoDaddy">
          Cloudflare shows two nameservers such as <C>ada.ns.cloudflare.com</C>. In GoDaddy open <UI>My Products → your domain → DNS → Nameservers → Change</UI>,
          choose <UI>I’ll use my own nameservers</UI> and paste both.
        </Step>
        <Step title="Wait for Cloudflare to confirm">
          Usually minutes, sometimes up to 24 hours. Cloudflare emails you when the domain is <C>Active</C>. The domain stays registered (and renewed) at GoDaddy.
        </Step>
        <Step title="Create an API token">
          In Cloudflare: <UI>My Profile → API Tokens → Create token</UI>, start from <UI>Edit zone DNS</UI>, add the permission <C>Zone → Zone → Read</C>, and
          limit it to your domains (or all zones).
        </Step>
      </Steps>

      <H2>Set it up</H2>
      <Steps>
        <Step title="Add the DNS provider">
          <UI>Settings → Public DNS → Add DNS provider</UI> (the same list as <UI>Settings → Default TLS → DNS providers</UI>, so one provider also works for
          {' '}<See id="certificates">DNS-01 certificates</See>). Pick Cloudflare or GoDaddy and paste the credentials.
        </Step>
        <Step title="Turn it on">Switch on <UI>Use Public DNS</UI> and tick the providers whose domains Relay may manage. Use <UI>Test</UI> to see their domains.</Step>
        <Step title="Choose automation">Turn on <UI>Create missing records</UI> and pick the record type, target and TTL. Exclude domains Relay should never touch.</Step>
        <Step title="Save">Settings apply right away. <UI>Public DNS</UI> appears in the sidebar.</Step>
      </Steps>

      <H2>Example: a new host gets its record</H2>
      <P>
        You own <C>instantoffr.com</C> at Cloudflare and add a proxy host for <C>test.instantoffr.com</C> → <C>192.168.1.20:3000</C>. Automation is on, record type
        {' '}<C>A</C>, target empty (Relay’s public IP).
      </P>
      <Example
        title="After you apply"
        code={`Host      test.instantoffr.com → http://192.168.1.20:3000
Domain    instantoffr.com (Cloudflare)
Check     no record for "test" → missing
Created   test  A  203.0.113.10  TTL 600`}
      />
      <P>The host drawer shows this before you apply: under the domains, <UI>Public DNS</UI> says <C>missing · Created after you apply</C>.</P>
      <Tip>Automation off? The drawer offers a <UI>Create record</UI> button instead, and <UI>Settings → Public DNS → Sync now</UI> checks every host at once.</Tip>

      <H2>A or CNAME?</H2>
      <Defs
        items={[
          ['A', 'Points a name at an IP address. Simple. If your IP changes, every A record needs the new address.'],
          ['CNAME', 'Points a name at another hostname. Keep one record up to date (for example with dynamic DNS) and every CNAME follows it.'],
        ]}
      />
      <Example
        title="The same three hosts, both ways"
        code={`# A: every record holds the IP
app.example.com    A      203.0.113.10
blog.example.com   A      203.0.113.10
shop.example.com   A      203.0.113.10

# CNAME: one A record, the rest point at it
home.example.com   A      203.0.113.10   ← the only place to change
app.example.com    CNAME  home.example.com
blog.example.com   CNAME  home.example.com
shop.example.com   CNAME  home.example.com`}
      />
      <Note>For CNAME, set the target to a hostname you manage, like <C>home.example.com</C>. Most providers don’t allow a CNAME on the bare domain (<C>example.com</C>) itself.</Note>

      <H2>What the statuses mean</H2>
      <Table
        head={['Status', 'Means', 'Relay does']}
        rows={[
          [<C>ok</C>, 'A record already points to Relay.', 'Nothing.'],
          [<C>wildcard</C>, <>A wildcard like <C>*.example.com</C> covers the name.</>, 'Nothing.'],
          [<C>missing</C>, 'No record for the name.', 'Creates it after apply (automation on), or offers Create record.'],
          [<C>conflict</C>, 'A record exists but points somewhere else.', <strong>Never changes it.</strong>],
          [<C>excluded</C>, 'The domain is in Excluded domains.', 'Nothing automatic.'],
          [<C>not managed</C>, <>The domain isn’t in a connected provider, e.g. <C>grafana.home.lan</C>.</>, 'Nothing.'],
          [<C>error</C>, 'The provider returned an error.', 'Shows the message; try again later.'],
        ]}
      />

      <H2>Manage records by hand</H2>
      <P>
        <UI>Public DNS</UI> in the sidebar lists the records of each domain. Pick a domain, filter by name, value or type, and add, edit or delete records.
        Supported types: <C>A</C>, <C>AAAA</C>, <C>CNAME</C>, <C>MX</C>, <C>TXT</C>, <C>CAA</C> and <C>NS</C> (not on <C>@</C>).
      </P>
      <List>
        <li>Use <C>@</C> as the name for the domain itself, <C>www</C> for <C>www.example.com</C>, <C>*.dev</C> for a wildcard.</li>
        <li><UI>Used by</UI> shows the proxy hosts a record serves. Click one to open the host.</li>
        <li>On Cloudflare you can turn <UI>Proxy through Cloudflare</UI> on or off per record.</li>
        <li>Records Relay can’t edit safely (the domain’s own <C>NS</C>, <C>SOA</C>, <C>SRV</C>) are shown as read-only.</li>
      </List>

      <H2>Deleting a host</H2>
      <P>
        When Public DNS is on, the delete dialog has <UI>Also delete its DNS records</UI> (off by default). Relay then removes records for the host’s domains
        that point to Relay. Records that point elsewhere, wildcards and records other hosts still use are kept.
      </P>

      <H2>Safety</H2>
      <List>
        <li>Automation only creates missing records. It never overwrites or deletes a record.</li>
        <li>Excluded domains are never changed automatically.</li>
        <li>Changes go to your provider right away, not through pending changes, and every change is in <UI>Logs → Audit</UI>.</li>
        <li>Admins change the settings; editors can edit records; viewers can only look.</li>
      </List>

      <H2>Troubleshooting</H2>
      <Table
        head={['You see', 'Why', 'Fix']}
        rows={[
          ['GoDaddy: Access denied', 'GoDaddy limits its API to large accounts.', 'Move the DNS to Cloudflare (steps above).'],
          ['Cloudflare: authentication error or no domains', 'The token lacks permissions or isn’t limited to the right zones.', <>Give it <C>Zone → DNS → Edit</C> and <C>Zone → Zone → Read</C>.</>],
          ['conflict', 'An old record points to another server.', 'Edit or delete it on the Public DNS page if it’s no longer needed.'],
          ['Record created but the site doesn’t load', 'Resolvers still cache the old answer.', 'Wait for the TTL to pass; check with dig test.example.com.'],
        ]}
      />
    </>
  )
}

export const dnsSections: DocSection[] = [
  {
    id: 'public-dns',
    group: 'Public DNS',
    title: 'Public DNS',
    icon: 'expose',
    summary: 'Edit your GoDaddy or Cloudflare DNS records from Relay and create records for new proxy hosts automatically.',
    keywords: 'public dns record a cname txt mx caa ns zone domain godaddy cloudflare nameservers api token access denied automatic create sync ttl proxied',
    app: [{ to: '/settings/public-dns', label: 'Public DNS settings' }, { to: '/dns', label: 'Open Public DNS' }],
    Body: PublicDNS,
  },
]
