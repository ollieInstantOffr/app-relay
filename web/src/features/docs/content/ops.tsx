import type { DocSection } from '../types'
import { C, Defs, Example, Flow, H2, H3, List, Note, P, See, Step, Steps, Table, Tip, UI, Warn } from '../parts'
import { Kbd } from '../../../components/ui'

function Logs() {
  return (
    <>
      <H2>The four log tabs</H2>
      <Defs
        items={[
          ['Access', 'Every request nginx served: time, host, method, path, status, upstream status, client IP, duration, user agent.'],
          ['Error', 'nginx errors, such as upstreams refusing connections, timeouts and TLS problems.'],
          ['Audit', 'Who changed what and when, whether through the UI, a REST API token or an AI assistant over MCP.'],
          ['Approvals', <>Changes requested by AI assistants that are waiting for you. <See id="mcp">MCP →</See></>],
        ]}
      />

      <H2>Searching the access log</H2>
      <P>Type filters and free text into the search box. Filters use <C>name:value</C>:</P>
      <Table
        head={['Filter', 'Example', 'Finds']}
        mono={[0, 1]}
        rows={[
          ['host:', 'host:app.example.com', 'Requests for one host'],
          ['status:', 'status:502', 'An exact status code'],
          ['status:', 'status:5xx', 'Any server error'],
          ['status:', 'status:>=400', 'Every client and server error'],
          ['ip:', 'ip:203.0.113.7', 'One visitor'],
          ['method:', 'method:POST', 'Only POST requests'],
          ['(free text)', 'wp-login', 'Paths, IPs or user agents containing the text'],
        ]}
      />
      <Example title="Combine them" code={`host:shop.example.com status:5xx checkout
host:api.example.com method:POST status:>=400`} />
      <List>
        <li>The log tails live. Press <Kbd>space</Kbd> to pause while you read.</li>
        <li>Click a row for full details, including headers and timing.</li>
        <li><UI>Export</UI> downloads the current filtered view as CSV or NDJSON.</li>
      </List>

      <H2>Reading common problems</H2>
      <Table
        head={['You see', 'Meaning']}
        rows={[
          [<C>502</C>, 'nginx couldn’t connect to the upstream. Check the Error tab for the reason.'],
          [<C>504</C>, 'The upstream took too long. Raise proxy timeouts on the host or fix the slow endpoint.'],
          [<C>413</C>, 'Upload too large. Raise Max upload size on the host’s Advanced tab.'],
          [<C>429</C>, 'The rate limit kicked in.'],
          [<C>403</C>, 'An access list, a Deny location or Block exploits refused it.'],
          [<C>401</C>, 'Basic auth or forward auth asked for a login.'],
        ]}
      />

      <H2>The audit trail</H2>
      <P>
        Every create, update, delete, apply, rollback, sign-in and MCP action is recorded with the actor and details. Filter it and use <UI>Export CSV</UI> for compliance
        or incident reviews.
      </P>
    </>
  )
}

function History() {
  return (
    <>
      <H2>Every apply is a version</H2>
      <P>
        <UI>Config history</UI> lists every configuration that has been live, newest first, with who applied it and what changed. The live one carries a
        {' '}<C>live</C> badge. When there are pending changes, a <strong>Draft</strong> entry at the top shows what the next apply will do.
      </P>

      <H2>Comparing versions</H2>
      <List>
        <li>Select a version to see a per-file diff against the version before it.</li>
        <li>Compare any two versions, for example “what changed between Monday and now?”.</li>
        <li>Download a version’s rendered nginx and HAProxy files.</li>
      </List>

      <H2>Rolling back</H2>
      <Steps>
        <Step title="Find the last good version">Look at the timestamps and diffs, e.g. <C>v41</C> from before this morning’s change.</Step>
        <Step title="Roll back">Choose <UI>Roll back</UI> and confirm.</Step>
        <Step title="Relay validates it again">The old version runs through the full apply pipeline (validate, reload, health check), so a rollback can’t break things either.</Step>
      </Steps>
      <Flow steps={[{ label: 'v41', sub: 'good' }, { label: 'v42', sub: 'broke login' }, { label: 'Roll back to v41' }, { label: 'v43', sub: '= v41, live' }]} />
      <Note>A rollback creates a new version with the old content, so history is never rewritten.</Note>

      <H2>Discarding pending changes</H2>
      <P>Changed your mind before applying? <UI>Discard</UI> in the pending bar drops every pending change and restores the editable state to the live version.</P>
      <Tip>Automatic rollbacks (when a health check fails after apply) also appear here, with the reason, so you can see exactly what went wrong.</Tip>
    </>
  )
}

function Notifications() {
  return (
    <>
      <H2>Channels</H2>
      <P>A channel is somewhere Relay can send alerts. Add one under <UI>Settings → Notifications</UI>, then use <UI>Send test</UI>.</P>
      <Defs
        items={[
          ['Email (Resend)', 'Send through the Resend API. The easiest way to get reliable email.'],
          ['Email (SMTP)', 'Any mail server: your provider, Mailgun, SES, Postmark, Gmail with an app password.'],
          ['Webhook', 'POST JSON to any URL. Slack and Discord webhook URLs are formatted automatically.'],
        ]}
      />

      <H2>Events</H2>
      <Table
        head={['Event', 'Sent when', 'Critical']}
        mono={[0]}
        rows={[
          ['upstream_down', 'A host or backend stops responding', 'yes'],
          ['cert_renew_failed', 'A certificate could not be renewed', 'yes'],
          ['reload_failed', 'An apply or reload failed and was rolled back', 'yes'],
          ['cert_expiring', 'A certificate is close to expiry', ''],
          ['unknown_sign_in', 'Someone signs in from a new device or location', ''],
          ['mcp_write_executed', 'An AI assistant changed something', ''],
          ['weekly_summary', 'Weekly traffic and health summary', ''],
          ['engine_update_available', 'A new nginx or HAProxy version is out', ''],
        ]}
      />
      <P>For each event, tick the channels that should receive it.</P>

      <H2>Quiet hours and duplicates</H2>
      <List>
        <li><strong>Quiet hours</strong> (default 23:00–07:00, in your instance’s timezone) hold back non-critical alerts. When quiet hours end, each channel gets one digest of what was held.</li>
        <li>Critical events (<C>upstream_down</C>, <C>cert_renew_failed</C>, <C>reload_failed</C>) are always sent immediately.</li>
        <li>The same alert repeating within 5 minutes is sent only once.</li>
      </List>

      <H2>Set up Resend</H2>
      <Steps>
        <Step title="Verify your domain">In the Resend dashboard, add your domain (e.g. <C>example.com</C>) and create the DNS records it shows.</Step>
        <Step title="Create an API key">Create a key with <em>Sending access</em>. It starts with <C>re_</C>.</Step>
        <Step title="Add the channel">
          <Example code={`Type       Email (Resend)
API key    re_…
From       Relay <relay@example.com>      (must be on the verified domain)
To         ops@example.com, you@example.com
Reply-to   support@example.com            (optional)`} />
        </Step>
        <Step title="Send a test">If the key or domain is wrong, the error from Resend is shown, e.g. <C>The example.com domain is not verified</C>.</Step>
      </Steps>

      <H2>Set up SMTP</H2>
      <Example code={`Host       smtp.example.com
Port       587
Security   starttls        (465 → tls · 25 → none)
Username   relay@example.com
Password   ••••••••
From       relay@example.com
To         ops@example.com`} />

      <H2>Webhooks</H2>
      <H3>Slack or Discord</H3>
      <P>Paste an incoming webhook URL (<C>https://hooks.slack.com/services/…</C> or <C>https://discord.com/api/webhooks/…</C>). Messages are formatted for the app.</P>
      <H3>Your own endpoint</H3>
      <P>Any other URL receives this JSON:</P>
      <Example
        lang="json"
        code={`{
  "event": "upstream_down",
  "level": "error",
  "title": "grafana.example.com is down",
  "message": "502 from 192.168.1.20:3000",
  "url": "https://relay.example.com/hosts?edit=…",
  "at": "2026-09-15T08:12:03Z"
}`}
      />
      <P>
        Add a secret and Relay sends it in the <C>X-Relay-Secret</C> header (you can choose another header name). Reject requests without it.
      </P>
      <Example
        lang="js"
        title="Minimal receiver (Node/Express)"
        code={`app.post('/relay-alerts', express.json(), (req, res) => {
  if (req.get('X-Relay-Secret') !== process.env.RELAY_SECRET) return res.sendStatus(401)
  const { event, title, message } = req.body
  console.log(\`[\${event}] \${title}: \${message}\`)
  res.sendStatus(204)
})`}
      />
      <Note>Secrets such as API keys and SMTP passwords are write-only: once saved they are never shown again, only replaced.</Note>
    </>
  )
}

function Backups() {
  return (
    <>
      <H2>What a backup contains</H2>
      <P>
        One encrypted archive with your hosts, backends, access lists, certificates, users and settings: what you need to rebuild Relay on a new machine.
      </P>
      <Warn title="Keep the passphrase safe">
        Archives are encrypted with <strong>age</strong> using your passphrase. Without it a backup cannot be restored, not even by support. Store it in your password manager.
      </Warn>

      <H2>Set it up</H2>
      <Steps>
        <Step title="Set a passphrase">
          <UI>Settings → Backup &amp; restore → Set passphrase</UI> (at least 8 characters). Changing it later only affects new backups.
        </Step>
        <Step title="Choose what to include">
          <UI>Include private keys</UI> keeps certificate keys in the archive. Without them, restored certificates must be issued again.
        </Step>
        <Step title="Schedule">
          <UI>Edit schedule</UI>: a time of day and how many backups to keep, e.g. daily at 03:00, keep newest 14.
        </Step>
        <Step title="Test">Click <UI>Back up now</UI> and download the archive once to check it works.</Step>
      </Steps>

      <H2>Copy backups off the machine</H2>
      <P>Backups are stored in the <C>relay-data</C> volume. A backup on the same disk won’t help if that disk dies, so copy them elsewhere:</P>
      <Example
        lang="bash"
        title="Copy archives to the host, then sync them anywhere"
        code={`docker cp relay:/data/backups ./relay-backups
rsync -a ./relay-backups/ nas:/backups/relay/`}
      />
      <Example
        lang="bash"
        title="Or archive the whole data volume (stop relay first for a consistent copy)"
        code={`docker compose stop relay
docker run --rm -v relay_relay-data:/data -v "$PWD":/out alpine tar czf /out/relay-data.tgz -C /data .
docker compose start relay`}
      />

      <H2>Restore</H2>
      <Steps>
        <Step title="Install Relay">On the new machine, follow <See id="installation">Installation</See> and finish the setup wizard.</Step>
        <Step title="Restore from file">
          <UI>Settings → Backup &amp; restore → Restore from file</UI>, drop the archive and enter its passphrase. Or pick one from the snapshot list and choose <UI>Restore</UI>.
        </Step>
        <Step title="Apply">Review the pending changes and apply.</Step>
      </Steps>
      <Tip>Relay also makes a <C>before-upgrade</C> backup automatically before upgrading nginx or HAProxy.</Tip>
    </>
  )
}

function NpmImport() {
  return (
    <>
      <H2>Moving from Nginx Proxy Manager</H2>
      <P>
        Relay reads NPM’s data folder and converts proxy hosts, redirection hosts, streams, certificates and access lists. Nothing changes until you review and confirm,
        and NPM can keep running during the import.
      </P>

      <H2>Step by step</H2>
      <Steps>
        <Step title="Make NPM’s data visible to Relay">
          Mount NPM’s <C>data</C> and <C>letsencrypt</C> folders read-only into the <C>relay</C> container, e.g. in a <C>docker-compose.override.yml</C>:
          <Example
            lang="yaml"
            code={`services:
  relay:
    volumes:
      - /opt/npm/data:/import/npm/data:ro
      - /opt/npm/letsencrypt:/import/npm/letsencrypt:ro`}
          />
          <Example lang="bash" code={`docker compose up -d relay`} />
        </Step>
        <Step title="Start the import">
          <UI>Settings → Backup &amp; restore → Import from Nginx Proxy Manager → Start import</UI> (also offered on an empty Proxy hosts page).
        </Step>
        <Step title="Point at the data">
          Folder inside the Relay container: <C>/import/npm/data</C>. Relay finds <C>database.sqlite</C>, custom certificates and the Let’s Encrypt certificates next to it.
          If NPM used MySQL/MariaDB, export it or point at the database as offered.
        </Step>
        <Step title="Review">
          See what will be created and what is skipped. Tick <UI>Overwrite items that already exist</UI> only if you want NPM’s version to win.
        </Step>
        <Step title="Cut over">
          Confirm the import. Stop NPM to free ports 80 and 443 (<C>docker stop npm</C>), then <UI>Apply &amp; reload</UI> in Relay.
        </Step>
      </Steps>
      <Note>Imported Let’s Encrypt certificates keep working, and Relay renews them from then on.</Note>
      <Tip>Keep NPM’s container (stopped) for a few days. If anything is missing, start it again while you fix the host in Relay.</Tip>
    </>
  )
}

function Engines() {
  return (
    <>
      <H2>Update Relay itself</H2>
      <P>
        Relay updates itself from its GitHub repository. <UI>Settings → Updates</UI> shows the commit Relay runs, the newest commit on <C>main</C> and the commits
        you don’t have yet, with links to GitHub.
      </P>
      <Flow
        steps={[
          { label: 'Fetch', sub: 'git fetch' },
          { label: 'Pull', sub: 'fast-forward' },
          { label: 'Build', sub: 'docker compose build' },
          { label: 'Restart', sub: 'docker compose up -d' },
          { label: 'Health check', sub: 'rollback if unhealthy' },
        ]}
      />
      <Steps>
        <Step title="Check for updates">
          Relay checks automatically at the interval below, or when you click <UI>Check now</UI>. It runs <C>git fetch</C> in the folder you started Relay from and lists the new commits.
          You also get an <C>engine_update_available</C> notification for each new version.
        </Step>
        <Step title="Update">
          Click <UI>Upgrade to 0.4.2</UI> on the Relay card. Optionally tick <UI>Also restart nginx and HAProxy</UI> so the engines pick up the new agent (a 1–3 second pause in traffic).
        </Step>
        <Step title="Watch it run">
          The progress panel shows pull, build and restart. <UI>Show build output</UI> streams the docker build log. Building takes a few minutes the first time and is much faster afterwards thanks to the build cache.
        </Step>
        <Step title="Done">The admin UI disconnects for a few seconds while Relay restarts, then the page reloads on the new version by itself.</Step>
      </Steps>

      <H3>Requirements</H3>
      <List>
        <li>Relay was started with <C>docker compose</C> from a <C>git clone</C> of the repository (the normal install).</li>
        <li>The checkout is on <C>main</C> with no local commits. Local edits to tracked files make the update stop if git can’t fast-forward over them.</li>
        <li>The Docker socket is mounted into the <C>relay</C> container (the default compose file does this).</li>
        <li>The Docker host can reach the git remote. Public <C>https://</C> remotes work out of the box; SSH remotes and private repositories need credentials the updater doesn’t have.</li>
      </List>

      <H3>What keeps it safe</H3>
      <List>
        <li>A backup is created before anything changes.</li>
        <li>git only fast-forwards and runs as the owner of the folder, so your checkout is never rewritten or left owned by root.</li>
        <li>If the build fails, Relay keeps running the current version.</li>
        <li>If the new version doesn’t become healthy after the restart, the previous image is restored automatically.</li>
        <li>nginx and HAProxy keep serving traffic throughout (unless you choose to restart them).</li>
        <li>The work runs in a short-lived helper container (<C>docker:29.8.0-cli</C>) that survives Relay’s own restart; the restarted Relay reports the result.</li>
      </List>
      <H3>Version numbers</H3>
      <P>
        Relay’s version (<C>MAJOR.MINOR.PATCH</C>, shown on the Relay card, the About page and the sign-in screen) is worked out from the git history, so it moves with every
        commit on <C>main</C>:
      </P>
      <Table
        head={['Commit', 'Example', 'Version change']}
        mono={[1, 2]}
        rows={[
          ['Anything else', 'Fix the tabs on the hosts page', '0.4.2 → 0.4.3'],
          [<>Subject starts with <C>feat:</C> or contains <C>[minor]</C></>, 'feat: Resend email channel', '0.4.3 → 0.5.0'],
          [<>A <C>type!:</C> subject, <C>BREAKING CHANGE</C> in the body, or <C>[major]</C></>, 'feat!: new config format', '0.5.0 → 1.0.0'],
          [<>A tag <C>vX.Y.Z</C></>, 'git tag v2.0.0', 'jumps to 2.0.0'],
        ]}
      />
      <P>Run <C>sh scripts/version.sh</C> in the checkout to see the current number. Builds from <C>make up</C> and in-app upgrades record it; a plain <C>docker compose up --build</C> shows <C>dev</C>.</P>

      <Note title="Prefer the shell?">
        The manual way still works: <C>git pull &amp;&amp; make up</C> in the Relay folder. Set <C>RELAY_UPDATE_BRANCH</C> on the <C>relay</C> service to follow another branch.
      </Note>

      <H2>nginx and HAProxy</H2>
      <P>
        nginx and HAProxy run from the official Docker Hub images. <UI>Settings → Updates</UI> shows the running version of each, checks Docker Hub for new releases,
        and upgrades the containers in place with automatic rollback.
      </P>

      <H2>Update checks</H2>
      <List>
        <li><strong>Channels</strong>: nginx <C>stable</C> (even minor, e.g. 1.30.x) or <C>mainline</C>; HAProxy <C>lts</C> (e.g. 3.4.x) or <C>latest</C>.</li>
        <li>Checks run shortly after start, then every <UI>Check interval</UI> (default 12 h), or when you click <UI>Check now</UI>. Failed checks retry hourly.</li>
        <li>Each new version creates one <C>engine_update_available</C> notification and a badge on Settings.</li>
      </List>

      <H2>What an upgrade does</H2>
      <Flow
        steps={[
          { label: 'Preflight' },
          { label: 'Pull image' },
          { label: 'Validate', sub: 'config on new version' },
          { label: 'Backup' },
          { label: 'Swap container' },
          { label: 'Health check' },
        ]}
      />
      <Steps>
        <Step title="Validate first">Your live configuration is tested against the new version in a throwaway container. If it fails, nothing changes and you see the output.</Step>
        <Step title="Swap">The container is recreated with the same settings and the new image. nginx is briefly unavailable, typically 1–3 seconds.</Step>
        <Step title="Watch">Relay confirms the engine runs and that hosts which were healthy stay healthy.</Step>
        <Step title="Roll back if needed">If anything fails, the old container is restored automatically and you get the last log lines.</Step>
      </Steps>

      <H2>Making an upgrade permanent</H2>
      <P>
        An in-UI upgrade doesn’t edit your <C>docker-compose.yml</C>. If Compose later recreates the container, it may start the older image again. Relay detects this
        drift and offers <UI>Upgrade again</UI> or <UI>Keep compose version</UI>. To pin the version, add it to a <C>.env</C> file next to <C>docker-compose.yml</C>:
      </P>
      <Example lang="bash" title=".env" code={`RELAY_NGINX_IMAGE=nginx:1.31.5-alpine
RELAY_HAPROXY_IMAGE=haproxy:3.4.4-alpine`} />
      <Example lang="bash" code={`docker compose up -d`} />
      <Warn>Upgrades need write access to the Docker socket (the default compose file mounts it). A read-only socket proxy is enough for discovery but not for upgrades.</Warn>
    </>
  )
}

export const opsSections: DocSection[] = [
  {
    id: 'logs',
    group: 'Operations',
    title: 'Logs & audit trail',
    icon: 'logs',
    summary: 'Search live access and error logs, find the cause of 502s, and see who changed what.',
    keywords: 'logs access error audit search filter status 5xx 502 504 413 ip host tail export csv ndjson troubleshooting',
    app: [{ to: '/logs/access', label: 'Open logs' }],
    Body: Logs,
  },
  {
    id: 'history',
    group: 'Operations',
    title: 'Config history & rollback',
    icon: 'history',
    summary: 'Compare configuration versions, download them, and roll back safely in two clicks.',
    keywords: 'history version diff rollback roll back compare discard draft undo revert',
    app: [{ to: '/history', label: 'Open config history' }],
    Body: History,
  },
  {
    id: 'notifications',
    group: 'Operations',
    title: 'Notifications',
    icon: 'bolt',
    summary: 'Get alerts by email (Resend or SMTP), Slack, Discord or your own webhook when something needs attention.',
    keywords: 'notifications alerts email resend smtp webhook slack discord quiet hours events upstream down certificate expiring secret',
    app: [{ to: '/settings/notifications', label: 'Notification settings' }],
    Body: Notifications,
  },
  {
    id: 'backups',
    group: 'Operations',
    title: 'Backup & restore',
    icon: 'download',
    summary: 'Encrypted, scheduled backups of everything, and how to restore on a new machine.',
    keywords: 'backup restore encrypted age passphrase schedule snapshot migrate disaster recovery volume',
    app: [{ to: '/settings/backup', label: 'Backup settings' }],
    Body: Backups,
  },
  {
    id: 'npm-import',
    group: 'Operations',
    title: 'Import from Nginx Proxy Manager',
    icon: 'upload',
    summary: 'Bring hosts, certificates and access lists over from NPM and switch with minimal downtime.',
    keywords: 'nginx proxy manager npm import migrate migration database sqlite letsencrypt',
    app: [{ to: '/settings/backup?import=npm', label: 'Start NPM import' }],
    Body: NpmImport,
  },
  {
    id: 'engines',
    group: 'Operations',
    title: 'Updates',
    icon: 'reload',
    summary: 'Update Relay from GitHub in one click, and upgrade nginx and HAProxy to new releases with automatic rollback.',
    keywords: 'upgrade update self update relay git pull github rebuild main branch nginx haproxy version docker hub stable mainline lts pin image env drift',
    app: [{ to: '/settings/engines', label: 'Open updates' }],
    Body: Engines,
  },
]
