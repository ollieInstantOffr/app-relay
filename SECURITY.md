# Security policy

Relay sits in front of your applications and handles certificates, access
control and logins, so security reports are taken seriously. Thank you for
helping keep Relay and its users safe.

## Supported versions

Relay is installed from this repository and updated with **Settings → Updates →
Upgrade**, which follows the `main` branch. Security fixes are made on `main`
only; there are no maintained older release branches.

| Version | Supported |
|---|---|
| Latest `main` | Yes |
| Anything older | No, upgrade to the latest `main` |

## Reporting a vulnerability

**Please do not open a public issue, pull request or discussion for a security
problem.**

Report it privately through GitHub instead:

1. Open the repository's **Security** tab.
2. Click **Report a vulnerability**.
3. Describe the issue.

Please include:

- the Relay version (Settings → About) and proxy engine (nginx or Relay Edge);
- what an attacker can do, and what access they need first (none, a network
  position, a Relay account and which role, an MCP or API token…);
- steps to reproduce, or a proof of concept;
- any relevant logs or configuration, with secrets, tokens and passwords removed.

## What to expect

- An acknowledgement within **5 working days**.
- An initial assessment (confirmed, needs more information, or not a
  vulnerability) within **14 days**.
- A fix on `main` as soon as practical, depending on severity. You'll be kept
  informed of progress.
- Credit in the release notes or advisory once a fix is available, unless you
  would rather stay anonymous.

Please give a reasonable amount of time to fix the issue before disclosing it
publicly. Relay is maintained by a small team, so a 90-day window is suggested.

## Scope

In scope:

- the Relay application: web UI, REST API, MCP server, authentication,
  sessions, roles, API and MCP tokens;
- Relay login (`/.relay/*`), the built-in login page for protected apps;
- Relay Edge and the engine agents (`relay agent`, `relay edge`);
- configuration rendering (nginx, Relay Edge, HAProxy), for example injection
  into generated config;
- the self-update, backup and restore features;
- integrations such as Docker discovery, DNS providers and Public DNS,
  including how their credentials are stored.

Out of scope:

- vulnerabilities in nginx, HAProxy, Docker or other third-party software
  itself (please report those to their projects), unless Relay makes them
  exploitable;
- issues that need an admin account, or shell access to the Docker host, to
  exploit, when that access already grants the same power;
- missing hardening on setups that ignore the documentation, for example
  exposing the admin UI to the internet without HTTPS or an access list;
- denial of service through traffic volume alone.

## Safe harbour

Good-faith security research is welcome. Don't access, change or delete other
people's data. Test only against installations you own or have permission to
test, and don't degrade other people's services. Research done this way won't
lead to legal action from the maintainers.
