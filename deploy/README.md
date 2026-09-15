# Deploying Relay

Relay runs as five containers, all with `network_mode: host`. `relay` is built
from the `Dockerfile`; the engine containers run the official nginx / HAProxy
images and plain `alpine:3.22` for Relay Edge and Relay Balancer, each
executing the relay binary from the `relay-bin` volume:

| Container | What it runs |
|---|---|
| `relay` | API, web UI, MCP server, SQLite database, ACME client, Docker discovery |
| `relay-nginx` | `relay agent --engine nginx` as PID 1, supervising nginx (runs while nginx is the proxy engine) |
| `relay-edge` | `relay agent --engine edge` as PID 1, supervising `relay edge run` (runs while Relay Edge is the proxy engine) |
| `relay-haproxy` | `relay agent --engine haproxy` as PID 1, supervising HAProxy (runs while HAProxy is the load balancer engine and a backend exists) |
| `relay-balancer` | `relay agent --engine balancer` as PID 1, supervising `relay balancer run` (runs while Relay Balancer is the load balancer engine and a backend exists) |

The proxy engine is chosen in Settings → General (nginx by default). Exactly
one of nginx and Relay Edge binds the HTTP/HTTPS ports and streams: the relay
app records the selection in `/run/relay/proxy-engine`, a fresh agent of the
other engine keeps its bootstrap config stopped, and Relay stops the
non-selected engine whenever it finds it running.

The load balancer engine is chosen the same way (Settings → General → Load
balancer engine: HAProxy by default, or Relay Balancer, beta). Both render the
same backends, frontends and load balancer settings and bind the same
frontends, so only the selected one runs; switching is a pending change that
stops the old engine before the new one starts and rolls back on failure.

```sh
docker compose up -d --build
docker compose logs -f relay
```

Open `http://<host>:8181` and finish the setup wizard.

## Ports (host network)

| Port | Bound by | Purpose |
|---|---|---|
| 80/tcp | proxy engine | HTTP, redirects to HTTPS, ACME HTTP-01 challenges (served from the start, before the first apply) |
| 443/tcp | proxy engine | HTTPS for proxy hosts (the default server answers unknown SNI with a self-signed placeholder) |
| 443/udp | proxy engine | HTTP/3 (QUIC), only when enabled globally or per host |
| 8181/tcp | relay | Admin UI and API (`RELAY_LISTEN`) |
| 127.0.0.1:18080 | nginx | `stub_status` for Relay's metrics (`RELAY_NGINX_STATUS_PORT`) |
| 127.0.0.1:18081 | edge | Relay Edge `/healthz`, `/stub_status`, `/metrics` (`RELAY_EDGE_STATUS_PORT`) |
| 127.0.0.1:8404 | load balancer engine | Stats / Prometheus (load balancer settings) |
| 127.0.0.1:10080+ | load balancer engine | Localhost frontends created by the Expose wizard |
| stream ports | proxy engine | Whatever TCP/UDP streams you configure |

"Proxy engine" is nginx or Relay Edge, "load balancer engine" HAProxy or Relay
Balancer, whichever is selected. HTTP and HTTPS
ports can be changed in Settings → General (for example when another proxy
already holds 80/443). Until the first apply, the selected engine runs a
bootstrap config on port 80; set `RELAY_BOOTSTRAP_HTTP_PORT` on the nginx /
edge container to move it.

## Volumes

| Volume | Mounted in | Contents |
|---|---|---|
| `relay-data` | relay (rw), engines (ro) | `relay.db`, `certs/<id>/{fullchain,privkey}.pem`, `acme/` webroot, `geoip/`, `backups/` |
| `relay-run` | all | Agent sockets `nginx.sock`, `edge.sock`, `haproxy.sock`, `balancer.sock`, HAProxy runtime socket `haproxy-runtime.sock` and master socket `haproxy-master.sock`, Relay Balancer runtime socket `balancer-runtime.sock`, the proxy engine selection `proxy-engine` |
| `relay-logs` | relay, nginx, edge, balancer | `access.log`, `stream-access.log`, `error.log` (written by the active proxy engine) |
| `relay-nginx`, `relay-edge`, `relay-haproxy`, `relay-balancer` | engines | Applied config releases (`releases/<hash>/`, `current` symlink). Keeps traffic flowing with the last applied config when an engine container restarts; Relay re-pushes the live version if they ever diverge. |

Back up `relay-data`; everything else is derived from it.

## How applying works

Edits are saved immediately and show up as pending changes. **Apply & reload**
renders the proxy engine's files (nginx config or Relay Edge `edge.json`) and
the load balancer engine's file (`haproxy.cfg` or Relay Balancer
`balancer.json`), validates them inside the engine containers (`nginx -t` /
`relay edge check`, `haproxy -c` / `relay balancer check`), atomically swaps
the `current` symlink, reloads (`SIGHUP`, HAProxy master CLI `reload`), then
health-checks changed hosts for 10 s. Switching the proxy engine stops the old
engine, starts the new one and health-checks every routable host; switching
the load balancer engine does the same for hosts routed through backends and
the frontends that accepted connections before. On failure the new engine is
stopped and the old one started again. If a host that was healthy before starts returning
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
