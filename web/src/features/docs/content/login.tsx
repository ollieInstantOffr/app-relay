import type { DocSection } from '../types'
import { C, Example, Flow, GoTo, H2, List, Note, P, See, Step, Steps, Table, Tip, UI, Warn } from '../parts'

function RelayLogin() {
  return (
    <>
      <H2>What it is</H2>
      <P>
        Relay login is a sign-in page built into Relay. Turn it on for a proxy host and visitors have to sign in with a Relay account before they reach the app.
        There is no extra container, database or SSO portal to run.
      </P>
      <Flow steps={[{ label: 'Browser' }, { label: 'Reverse proxy', sub: 'signed in?' }, { label: '/.relay/login', sub: 'if not' }, { label: 'Your app', sub: 'if yes' }]} />
      <P>
        It suits apps without a login of their own (dashboards, home automation, admin tools), and apps you want an extra lock in front of.
      </P>

      <H2>Protect a host</H2>
      <Steps>
        <Step title="Open the host">
          In <UI>Proxy hosts</UI>, click the host (for example <C>grafana.example.com</C>) and open the <UI>Advanced</UI> tab.
        </Step>
        <Step title="Turn on Authentication">
          Switch on <UI>Authentication</UI> and pick <UI>Relay login</UI> as the provider. There are no URLs to fill in.
        </Step>
        <Step title="Choose who can sign in">
          <UI>Every Relay user</UI> lets any enabled account in. <UI>Only selected users</UI> shows a list of people to tick.
        </Step>
        <Step title="Save and apply">
          Click <UI>Save to pending</UI>, then <UI>Apply now</UI>.
        </Step>
        <Step title="Try it">
          Open <C>https://grafana.example.com</C> in a private window. You land on <C>https://grafana.example.com/.relay/login</C>. Sign in and you’re sent back
          to the page you asked for.
        </Step>
      </Steps>
      <Tip>Leave <UI>Skip for /.well-known/</UI> on so certificate renewals aren’t blocked by the login.</Tip>
      <GoTo to="/hosts">Open proxy hosts</GoTo>

      <H2>Accounts that only use apps</H2>
      <P>
        Family, friends or colleagues who should use your apps but not manage Relay get the <strong>App access only</strong> role. They can sign in on protected
        hosts, but not to the Relay admin UI.
      </P>
      <Steps>
        <Step title="Invite them">
          <UI>Settings → Users &amp; access → Invite</UI>.
        </Step>
        <Step title="Pick the role">
          Choose <UI>App access only</UI>, set an initial password and share it privately.
        </Step>
      </Steps>
      <Note>Admins, editors and viewers can sign in on protected hosts too, unless you limit the host to selected users. Disabled users can’t sign in anywhere.</Note>

      <H2>Allowed users</H2>
      <P>Limit a host to a few people with <UI>Only selected users</UI>. Anyone else can’t get in, even with a valid Relay account.</P>
      <Example
        title="Only two people for the family photo app"
        code={`Host              photos.example.com
Provider          Relay login
Who can sign in   Only selected users
                  [x] anna    app access only
                  [x] ben     app access only
                  [ ] ollie   admin`}
      />
      <P>Changes to the list are a normal host edit: they take effect on the next apply.</P>

      <H2>Headers sent to your app</H2>
      <P>Apps that trust a proxy header can sign the person in automatically. Turn the headers on with the toggles under <UI>Authentication</UI>.</P>
      <Table
        head={['Header', 'Contains', 'Sent with']}
        mono={[0]}
        rows={[
          ['Remote-User', <>Username, e.g. <C>anna</C></>, 'Pass Remote-User'],
          ['Remote-Email', 'Email address of the Relay account', 'Pass Remote-User'],
          ['Remote-Name', 'Name to show in the app', 'Pass Remote-User'],
          ['Remote-Groups', <>The Relay role: <C>admin</C>, <C>editor</C>, <C>viewer</C> or <C>member</C> (App access only)</>, 'Pass Remote-Groups'],
        ]}
      />
      <Example
        lang="ini"
        title="grafana.ini: sign people in from Relay’s headers"
        code={`[auth.proxy]
enabled = true
header_name = Remote-User
header_property = username
auto_sign_up = true
headers = Email:Remote-Email Name:Remote-Name`}
      />
      <Warn title="Only trust the headers behind Relay">
        If the app can also be reached directly (for example on a published port), anyone could send these headers themselves. Publish the app only on{' '}
        <C>127.0.0.1</C> or a Docker network that Relay uses.
      </Warn>

      <H2>Signing out</H2>
      <P>
        Send people to <C>/.relay/logout</C> on the same domain, e.g. <C>https://grafana.example.com/.relay/logout</C>. Many apps let you set this as their
        sign-out address:
      </P>
      <Example
        lang="ini"
        title="grafana.ini"
        code={`[auth]
signout_redirect_url = https://grafana.example.com/.relay/logout`}
      />
      <P>
        A sign-in lasts as long as <UI>Session length</UI> in <UI>Settings → Users &amp; access → Sign-in</UI>.
      </P>

      <H2>nginx and Relay Edge</H2>
      <P>
        Relay login works the same with both proxy engines, and switching engines keeps every setting. <See id="relay-edge">Relay Edge →</See>
      </P>

      <H2>Limitations</H2>
      <List>
        <li>
          <strong>One sign-in per domain.</strong> Signing in on <C>grafana.example.com</C> doesn’t sign you in on <C>photos.example.com</C>: each domain asks
          once. For one sign-in across many apps, use Authelia or Authentik. <See id="protection">Forward auth →</See>
        </li>
        <li>
          <strong>No passkeys on the login page.</strong> It asks for username, password and, with 2FA, a 6-digit authenticator code. People whose only second
          factor is a passkey need to add an authenticator app under <UI>My account</UI> to sign in to protected apps.
        </li>
      </List>
    </>
  )
}

export const loginSections: DocSection[] = [
  {
    id: 'relay-login',
    group: 'Reverse proxy',
    title: 'Relay login',
    icon: 'token',
    summary: 'Protect any app with a built-in sign-in page. People use their Relay account and there is nothing else to install.',
    keywords:
      'relay login sign in signin authentication protect app password forward auth sso member app access only allowed users remote-user remote-email remote-name remote-groups logout .relay/login grafana',
    app: [
      { to: '/hosts', label: 'Open proxy hosts' },
      { to: '/settings/users', label: 'Users & access' },
    ],
    Body: RelayLogin,
  },
]
