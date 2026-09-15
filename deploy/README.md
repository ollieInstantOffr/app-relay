# Deploying Relay

Relay runs as three containers from one image build (`Dockerfile` targets
`relay`, `nginx`, `haproxy`), all with `network_mode: host`:

| Container | What it runs |
|---|---|
| `relay` | API, web UI, MCP server, SQLite database, ACME client, Docker discovery |
| `relay-nginx` | `relay agent --engine nginx` as PID 1, supervising nginx |
| `relay-haproxy` | `relay agent --engine haproxy` as PID 1, supervising HAProxy (only runs once a backend exists) |

```sh
docker compose up -d --build
docker compose logs -f relay
```

Open `http://<host>:8181` and finish the setup wizard.

## Ports (host network)

| Port | Bound by | Purpose |
|---|---|---|
| 80/tcp | nginx | HTTP, redirects to HTTPS, ACME HTTP-01 challenges (served from the start, before the first apply) |
| 443/tcp | nginx | HTTPS for proxy hosts (the default server answers unknown SNI with a self-signed placeholder) |
| 443/udp | nginx | HTTP/3 (QUIC), only when enabled globally or per host |
| 8181/tcp | relay | Admin UI and API (`RELAY_LISTEN`) |
| 127.0.0.1:18080 | nginx | `stub_status` for Relay's metrics |
| 127.0.0.1:8404 | haproxy | Stats / Prometheus (HAProxy settings) |
| 127.0.0.1:10080+ | haproxy | Localhost frontends created by the Expose wizard |
| stream ports | nginx | Whatever TCP/UDP streams you configure |

HTTP and HTTPS ports can be changed in Settings → General (for example when
another proxy already holds 80/443). Until the first apply, nginx runs a
bootstrap config on port 80; set `RELAY_BOOTSTRAP_HTTP_PORT` on the nginx
container to move it.

## Volumes

| Volume | Mounted in | Contents |
|---|---|---|
| `relay-data` | relay (rw), engines (ro) | `relay.db`, `certs/<id>/{fullchain,privkey}.pem`, `acme/` webroot, `geoip/`, `backups/` |
| `relay-run` | all | Agent sockets `nginx.sock`, `haproxy.sock`, HAProxy runtime socket `haproxy-runtime.sock` and master socket `haproxy-master.sock` |
| `relay-logs` | relay, nginx | `access.log`, `stream-access.log`, `error.log` |
| `relay-nginx`, `relay-haproxy` | engines | Applied config releases (`releases/<hash>/`, `current` symlink). Keeps traffic flowing with the last applied config when an engine container restarts; Relay re-pushes the live version if they ever diverge. |

Back up `relay-data`; everything else is derived from it.

## How applying works

Edits are saved immediately and show up as pending changes. **Apply & reload**
renders the nginx files and `haproxy.cfg`, validates them inside the engine
containers (`nginx -t`, `haproxy -c`), atomically swaps the `current` symlink,
reloads (nginx `SIGHUP`, HAProxy master CLI `reload`), then health-checks
changed hosts for 10 s. If a host that was healthy before starts returning
502/503/504, or an engine fails to start, the previous release is restored
automatically and the edit stays as a draft. Every apply is a config version
under **Config history**, with diffs, downloads and rollback.

## Docker socket

The compose file mounts `/var/run/docker.sock` read-only into `relay` for
container discovery. Access to the Docker socket is equivalent to root on the
host. To limit it, run a socket proxy that only allows reading containers and
point Relay at it (Settings → Docker discovery → endpoint):

```yaml
services:
  docker-proxy:
    image: tecnativa/docker-socket-proxy:latest
    container_name: relay-docker-proxy
    restart: unless-stopped
    environment:
      CONTAINERS: 1     # GET /containers/* only
      EVENTS: 1
      POST: 0
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    ports:
      - "127.0.0.1:2375:2375"

  relay:
    # remove the docker.sock volume from the relay service and set the
    # endpoint to tcp://127.0.0.1:2375 in Settings → Docker discovery
```

Discovery lists every container, including stopped ones (shown greyed with a
reason, e.g. `stopped · starts on 127.0.0.1:8080`).

## Remote Docker hosts

Relay can watch any number of Docker hosts (Settings → Docker discovery →
**Docker hosts → Add Docker host**). Each host has its own name (shown as
`nas · grafana`), connection, address for upstreams and auto-create /
auto-remove switches. Label-created hosts get `sourceRef` `<host>/<container>`.
Pick one of three connection types:

**1. docker-socket-proxy over TCP (recommended on a LAN).** Run this on the
remote host, bound to its LAN IP only:

```yaml
services:
  docker-socket-proxy:
    image: tecnativa/docker-socket-proxy:latest
    restart: unless-stopped
    environment:
      CONTAINERS: 1
      EVENTS: 1
      NETWORKS: 1
      INFO: 1
      VERSION: 1
      POST: 0
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    ports:
      - "192.168.1.20:2375:2375"   # the host's LAN IP, never 0.0.0.0 on an internet-facing host
```

Then add `tcp://192.168.1.20:2375` as a **TCP** host. The API is unencrypted
and unauthenticated: firewall port 2375 so only Relay's host can reach it.

**2. Docker daemon with TLS.** If the daemon runs with `--tlsverify` (port
2376), choose **TCP + TLS** and paste the CA, client certificate and client
key (PEM). The client key is write-only: it is stored and never returned by
the API.

**3. SSH.** Choose **SSH** and enter `user@host:22` plus a dedicated private
key without passphrase. Requirements on the remote host: the user can log in
with that key (`~/.ssh/authorized_keys`) and is in the `docker` group
(`sudo usermod -aG docker <user>`), and `/var/run/docker.sock` exists. The
SSH host key is trusted on first connect (trust-on-first-use) and shown in the
UI; if it later changes, Relay refuses to connect until you trust the new key.

### Upstream addressing

- **Local socket** (or a socket proxy on `127.0.0.1`): running containers are
  reached by their container IP (Relay uses host networking); stopped containers
  and host-network containers by `127.0.0.1:<port>`.
- **Remote hosts**: container IPs are not routable from Relay, so upstreams
  are `<address for upstreams>:<published port>` (the address defaults to the
  host part of the URL). Host-network containers use `<address>:<container port>`.
- Containers on remote hosts without a published port (or published only on
  `127.0.0.1`) are listed but can't be proxied — publish the port
  (`ports: ["8080:80"]`) first.
- The app port is detected from the `relay.port` label, then `PORT`-style
  environment variables (`PORT`, `APP_PORT`, `HTTP_PORT`, `SERVER_PORT`,
  `LISTEN_PORT`, `NUXT_PORT`, `VITE_PORT`), exposed ports, published ports and
  finally image/command hints (Node/Next/Nuxt/Vite/Bun/Deno → 3000, Django/
  gunicorn/uvicorn → 8000, Flask → 5000). Any port can be entered in the
  "Create hosts from Docker" dialog; database ports only show a warning.
- A **stopped local container** without a published port can still get a host:
  it is created disabled, linked to the container, and filled in
  (`container IP:port`) and enabled automatically when the container starts.

### Security

Read access to the Docker API reveals every container's environment variables
(often including secrets), and write access is root on that host. Prefer the
socket proxy with `POST: 0`, bind it to a private interface, use a dedicated
SSH user/key per Relay instance, and remove hosts you no longer need.

## Certificates (ACME)

Let's Encrypt (production or staging) is the default. Settings → Default TLS →
"Custom ACME server" points Relay at any ACME directory (step-ca, ZeroSSL,
Google, internal CAs) with an optional CA bundle and External Account Binding.

Environment variables on the `relay` service:

| Variable | Purpose |
|---|---|
| `RELAY_ACME_DNS_RESOLVERS` | Comma-separated resolvers (e.g. `10.0.0.53:53,10.0.0.54`) used to check DNS-01 propagation instead of public resolvers — needed for split-horizon/internal DNS. |
| `RELAY_ACME_INSECURE_SKIP_VERIFY` | Test-only: skip TLS verification of the ACME directory. Never set in production; use the CA bundle field instead. |

HTTP-01 needs port 80 reachable from the ACME server; wildcard domains need a
DNS-01 provider (Certificates → DNS providers).

## Operations

```sh
# Reset a user's password (prints a new one-time password)
docker exec -it relay relay users reset-password <username>

# Liveness of the relay app (unauthenticated)
docker exec relay wget -qO- http://127.0.0.1:8181/healthz

# Inspect an engine agent directly over its unix socket
# (busybox wget can't use unix sockets, so use a throwaway curl container)
docker run --rm -v relay_relay-run:/run/relay alpine:3.22 \
  sh -c 'apk add -q curl && curl -s --unix-socket /run/relay/nginx.sock http://agent/v1/status'

# Engine output (also in the UI: engine banner → Error log)
docker logs relay-nginx
```

Upgrading: `docker compose pull && docker compose up -d` (or `--build` when
building locally). The engines keep serving the last applied release while
`relay` restarts.
