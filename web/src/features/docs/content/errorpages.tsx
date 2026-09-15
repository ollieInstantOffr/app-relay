import type { DocSection } from '../types'
import { C, Example, GoTo, H2, H3, List, Note, P, See, Step, Steps, Table, Tip, UI } from '../parts'

function ErrorPages() {
  return (
    <>
      <H2>What they are</H2>
      <P>
        When Relay has to answer instead of your app (the app is down, an access list blocks a visitor, someone hits a rate limit), the proxy engine normally
        shows a bare page like <C>502 Bad Gateway</C>. Relay can show a friendly, branded page instead, and a maintenance page while you work on an app.
      </P>
      <GoTo to="/settings/error-pages">Settings → Error pages</GoTo>

      <H2>Turn them on</H2>
      <Steps>
        <Step title="Open the settings">
          <UI>Settings → Error pages</UI>.
        </Step>
        <Step title="Switch on Use Relay’s error pages" />
        <Step title="Add your name and colour (optional)">
          <UI>Brand name</UI> appears at the top of every page. <UI>Accent colour</UI> takes a hex value like <C>#2563eb</C>.
        </Step>
        <Step title="Save and apply">
          Click <UI>Save changes</UI>, then <UI>Apply now</UI>.
        </Step>
      </Steps>

      <H2>Which errors are covered</H2>
      <Table
        head={['Page', 'When visitors see it']}
        mono={[0]}
        rows={[
          ['403', 'An access list or a location rule refuses the visitor.'],
          ['404', 'No host or location matches, e.g. when the default host is set to 404.'],
          ['429', 'A visitor goes over the host’s rate limit.'],
          ['500', 'Something fails inside the proxy itself.'],
          ['502', 'The app can’t be reached: stopped container, wrong port, refused connection.'],
          ['503', 'The app is temporarily unavailable.'],
          ['504', 'The app takes longer than the proxy timeout to answer.'],
          ['Maintenance', 'The host is in maintenance mode. Sent with status 503.'],
        ]}
      />
      <Note title="Your apps’ own pages pass through">
        If your app answers with its own 404 or 500 page, visitors see that page, untouched. Relay only replaces errors it generates itself.
      </Note>

      <H2>Customise a page</H2>
      <P>
        Pick a page under <UI>Pages</UI> and change its title and message. The preview shows exactly what Relay will serve and updates as you type.
      </P>
      <Example
        title="A friendlier 502"
        code={`Title     This app is taking a break
Message   It usually wakes up within a minute. Try reloading the page.`}
      />

      <H3>Custom HTML</H3>
      <P>
        For full control, open <UI>Advanced: custom HTML</UI> and paste a complete page. It replaces the built-in design for that page only. Keep it
        self-contained with inline CSS: the app behind the host may be down, so don’t load files from it. Up to 100 KB per page.
      </P>
      <Example
        lang="html"
        title="Custom 503 page"
        code={`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Back soon</title>
  <style>
    body { font-family: system-ui, sans-serif; background: #0f172a; color: #e2e8f0;
           display: grid; place-items: center; min-height: 100vh; margin: 0; }
    main { text-align: center; padding: 24px; }
    a { color: #93c5fd; }
  </style>
</head>
<body>
  <main>
    <h1>We'll be right back</h1>
    <p>Updates on <a href="https://status.example.com">status.example.com</a></p>
  </main>
</body>
</html>`}
      />
      <P>
        <UI>Reset to built-in design</UI> removes the custom HTML, so the page uses the title and message again.
      </P>

      <H2>Maintenance mode</H2>
      <P>
        Put one host into maintenance while you upgrade or move its app. Visitors get <C>503 Service Unavailable</C> with the maintenance page, which also tells
        search engines the outage is temporary. You can keep using the app from addresses you trust.
      </P>
      <Steps>
        <Step title="Start maintenance">
          In <UI>Proxy hosts</UI>, open the host’s menu and choose <UI>Start maintenance</UI>. Or open the host, go to <UI>Advanced → Maintenance mode</UI> and
          switch it on.
        </Step>
        <Step title="Write a message (optional)">
          A title and message for this host only, e.g. “Upgrading Nextcloud, back by 14:00”. Leave them empty to use the maintenance page from{' '}
          <UI>Settings → Error pages</UI>.
        </Step>
        <Step title="Let yourself in (optional)">
          Under <UI>Who still sees the app</UI>, pick an access list. Addresses it allows see the app as usual. <See id="access-lists">Access lists →</See>
        </Step>
        <Step title="Apply">
          Maintenance starts with the next apply. To finish, choose <UI>End maintenance</UI> from the same menu and apply again.
        </Step>
      </Steps>
      <Example
        title="Keep working from home while everyone else sees the page"
        code={`Access list   home-network
              allow  192.168.1.0/24
              deny   all

Host          cloud.example.com
Maintenance   on
Title         Upgrading Nextcloud
Message       Back by 14:00. Thanks for your patience!
Who still sees the app   home-network

From 192.168.1.20   → Nextcloud, as usual
From anywhere else  → 503 maintenance page`}
      />

      <H3>What keeps working</H3>
      <List>
        <li>
          <strong>Certificate renewals.</strong> ACME challenges under <C>/.well-known/acme-challenge/</C> are still answered, so certificates renew during
          maintenance.
        </li>
        <li>
          <strong>Relay login.</strong> <C>/.relay/login</C> and <C>/.relay/logout</C> still work on protected hosts. <See id="relay-login">Relay login →</See>
        </li>
      </List>
      <Tip>The maintenance page is used even when <UI>Use Relay’s error pages</UI> is off. Hosts in maintenance show a <strong>maintenance</strong> badge in the hosts list.</Tip>
    </>
  )
}

export const errorPageSections: DocSection[] = [
  {
    id: 'error-pages',
    group: 'Reverse proxy',
    title: 'Error & maintenance pages',
    icon: 'warning',
    summary: 'Branded pages for 403, 404, 429 and 5xx errors, and a maintenance mode that shows a 503 page while you work on an app.',
    keywords:
      'error page 403 404 429 500 502 503 504 bad gateway gateway timeout custom html brand colour color maintenance mode downtime bypass access list acme',
    app: [{ to: '/settings/error-pages', label: 'Error pages settings' }],
    Body: ErrorPages,
  },
]
