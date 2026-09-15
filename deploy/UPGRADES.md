# Engine versions & upgrades

Relay runs nginx and HAProxy from the **official Docker Hub images**
(`nginx:<version>-alpine`, `haproxy:<version>-alpine`). It checks Docker Hub for
new releases and can upgrade either engine from the UI:
**Settings → Updates**.

## How the containers are wired

```
relay           relay:latest            copies /usr/local/bin/relay → relay-bin volume (/opt/relay/bin/relay)
relay-nginx     nginx:1.30.4-alpine     entrypoint: wait for /opt/relay/bin/relay, exec `relay agent --engine nginx`
relay-edge      alpine:3.22             same, `relay agent --engine edge` (Relay Edge is part of the relay binary)
relay-haproxy   haproxy:3.4.4-alpine    same, `relay agent --engine haproxy`, user root
```

Relay Edge has no image to upgrade: it is updated together with Relay. While
Relay Edge is the selected proxy engine the nginx card is marked inactive
(`inactive: true` in `GET /api/engines/updates`); nginx can still be upgraded,
but its container stays stopped and no health check waits for it.

- The relay binary is static, so the agent runs unchanged on any official
  image. The agent is PID 1 and supervises nginx/HAProxy exactly as before.
- Engine containers carry the label `relay.engine=nginx|haproxy`. Relay only
  touches containers with that label **in its own compose project**
  (`RELAY_COMPOSE_PROJECT`, set from `${COMPOSE_PROJECT_NAME}` in
  docker-compose.yml).
- The loopback `stub_status` listener defaults to `127.0.0.1:18080`. Change it
  with `RELAY_NGINX_STATUS_PORT` in `.env` (used by relay when rendering
  `nginx.conf`, by the nginx agent's bootstrap config and by the nginx health
  check).
- Modules: the official nginx image compiles `stream`, `http_v2`, `http_v3`,
  `auth_request` and `stub_status` in, so the rendered `nginx.conf` has no
  `load_module` lines. If you point `RELAY_NGINX_IMAGE` at a build where a
  module is dynamic, the agent reports its `.so` path and Relay adds
  `load_module`. The official image has **no geoip2 module**: country-based
  geo-blocking is skipped when rendering (the Engines page says so).

## Version checks

- Relay lists the tags of `library/nginx` and `library/haproxy` and only
  considers exact release tags `X.Y.Z-alpine`.
- Channels: **nginx** `stable` (even minor, e.g. 1.30.x) or `mainline` (odd
  minor); **HAProxy** `lts` (even minor, e.g. 3.4.x) or `latest` (any branch).
- Checks run 15 s after start, then every *check interval* (default 12 h),
  and on **Check now**. A failed check (offline, Docker Hub rate limit) is
  shown as an error — Relay never pretends you're up to date — and retried
  hourly.
- Each new version produces one activity entry and one
  `engine_update_available` notification (route it in Settings →
  Notifications).

API: `GET /api/engines/updates`, `POST /api/engines/updates/check` (admin),
settings key `engines` (`GET/PUT /api/settings/engines`).

## What an upgrade does

`POST /api/engines/{nginx|haproxy}/upgrade {"version":"1.31.5"}` (admin) starts a
background job; progress is published on the `engine.upgrade` event topic and
at `GET /api/engines/upgrade-status`.

1. **Preflight** — Docker API reachable, engine container found, it runs an
   official image, no config apply in progress (applies are blocked until the
   upgrade finishes).
2. **Pull** `nginx:<version>-alpine` with progress.
3. **Validate** — the live config version is copied into a throwaway container
   of the new image (same mounts and network) and checked with `nginx -t` /
   `haproxy -c`. If it fails, nothing changed; the output is shown.
4. **Backup** — a `before-upgrade` backup (best effort).
5. **Swap** — the container is recreated with the same Config, HostConfig
   (volumes, restart policy, `network_mode: host`, `container:`/`service:`
   namespaces, bridge networks and aliases), labels and name, but the new
   image. The old container is renamed `<name>-old-<timestamp>` and stopped
   (nginx: a brief interruption, typically 1–3 s), then the new one starts.
6. **Health check** — wait for the agent to report the new version, push the
   live config if needed, confirm the engine runs and watch hosts that were
   healthy before for 10 s.
7. **Success** — the old container is removed and the new image is stored as
   the desired image.

**Automatic rollback:** if the new container doesn't start, the agent doesn't
come up, the engine isn't running or a healthy host starts failing, Relay
removes the new container, renames the old one back and starts it. The job
ends as `rolled_back` with the new container's last log lines, and an
activity entry, audit entry and notification are recorded. If Relay itself
restarts in the middle of a swap, it restores (or cleans up) the
`-old-` container on the next start.

Requirements: the Docker socket mounted into `relay` (the default compose
file does this; `:ro` on a socket does not block API writes). A read-only
socket proxy (`POST: 0`) is enough for discovery but **not** for upgrades.

## Compose recreations & drift

An in-UI upgrade replaces the container but not your `docker-compose.yml`.
Compose only recreates the container when its service definition changes
(for example you edit the file or `.env`, or run
`docker compose up -d --force-recreate`). It then starts the image from the
compose file again, which may be older than what Relay installed.

Relay detects this **drift** (running image ≠ image Relay last installed) and
shows a callout on the engine card:

- **Upgrade again** — reinstall the version Relay had upgraded to.
- **Keep compose version** — accept the running image as the desired one.

To make an upgrade permanent, pin it in `.env` next to `docker-compose.yml`:

```sh
RELAY_NGINX_IMAGE=nginx:1.31.5-alpine
RELAY_HAPROXY_IMAGE=haproxy:3.4.4-alpine
```

## Pinning and manual upgrades

Set `RELAY_NGINX_IMAGE` / `RELAY_HAPROXY_IMAGE` in `.env` and run
`docker compose up -d`. Any official tag works (`nginx:1.31.5-alpine`,
`haproxy:3.2.23-alpine`). Non-official images (your own builds) run fine but
the UI can't upgrade them; it shows why.

After upgrading **Relay itself** (`docker compose up -d --build` or
`pull`), the new relay binary is copied to `relay-bin` on start. Running engine
agents keep the previous binary until their containers restart; to switch them
immediately run `docker compose restart nginx haproxy` (brief interruption).

## Migrating from the image-based compose file

Older compose files built `relay-nginx:latest` and `relay-haproxy:latest`
from this repository's Dockerfile. The volume names are unchanged, so your
database, certificates, logs and applied config releases carry over.

```sh
git pull                      # new docker-compose.yml + Dockerfile
docker compose up -d --build  # pulls nginx/haproxy, recreates all three containers
docker compose ps             # relay-nginx and relay-haproxy become healthy
docker image rm relay-nginx:latest relay-haproxy:latest   # optional cleanup
```

Notes:

- nginx is briefly unavailable while compose recreates `relay-nginx`.
- The agents start with the last applied release from the `relay-nginx` /
  `relay-haproxy` volumes. If you used **geo-blocking by country**, that release
  contains `geoip2` directives the official image can't load; open Relay and
  **Apply** once (the renderer now skips geo-blocking) or remove the
  country rules first.
- HAProxy moves from 3.0 to the current LTS image. Relay's generated
  `haproxy.cfg` validates on 3.4; to stay on 3.0, set
  `RELAY_HAPROXY_IMAGE=haproxy:3.0.27-alpine`.
- Using a custom project name (`docker compose -p myrelay`)? Nothing to do —
  `RELAY_COMPOSE_PROJECT` follows it.
