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
