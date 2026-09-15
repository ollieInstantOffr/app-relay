# Relay

Relay is a self-hosted manager for a **reverse proxy** (nginx, or Relay Edge,
Relay's own built-in proxy engine) and an **HAProxy load balancer**. It gives you a web UI and an MCP server so AI assistants can
manage it too. Proxy hosts, TLS certificates (Let's Encrypt HTTP-01 / DNS-01),
access lists, TCP/UDP streams, load-balancer backends and frontends: each edit
becomes a pending change. Applying it creates a validated, versioned config that
rolls back automatically if health checks fail.

## Run it

```bash
make up    # = RELAY_VERSION=$(sh scripts/version.sh) RELAY_COMMIT=$(git rev-parse HEAD) docker compose up -d --build
```

Open `http://<host>:8181`. A first-run wizard creates the admin account,
checks your network and optionally sets up an admin UI domain.

The stack is four containers, all on host networking:

| Container | Role |
|---|---|
| `relay` | Go app: REST API, web UI, MCP endpoint (`/mcp`), SQLite at `/data/relay.db`, ACME, Docker discovery |
| `relay-nginx` | nginx supervised by `relay agent --engine nginx` (ports 80/443 + streams when nginx is the proxy engine) |
| `relay-edge` | Relay Edge supervised by `relay agent --engine edge` (ports 80/443 + streams when Relay Edge is the proxy engine) |
| `relay-haproxy` | HAProxy supervised by `relay agent --engine haproxy` (starts with the first backend) |

Pick the proxy engine in **Settings → General** (`nginx` by default). Only the
selected engine runs; switching is a pending change that is applied with
validation, a health check of every host and automatic rollback. Relay Edge
can't run custom nginx snippets. See `docs/EDGE.md`.

Volumes: `relay-data` (database, certificates, backups), `relay-run` (agent
control sockets), `relay-logs` (proxy engine access and error logs). The Docker socket
is mounted read-only into `relay` for container discovery.

To reset a password from the host:

```bash
docker exec -it relay relay users reset-password <username>
```

## MCP

Enable it under **Settings → MCP server**, generate a token, then point your
client at `https://<admin-domain>/mcp` with `Authorization: Bearer rl_mcp_…`.
For stdio clients:

```bash
docker exec -i relay relay mcp-stdio --token rl_mcp_...
```

Write tools wait in the approvals inbox (**Logs → Approvals**) unless you set
them to run without confirmation.

## Development

```bash
make dev                 # API on :8181 with ./data (engines unavailable without compose)
cd web && npm run dev    # UI on :5173, proxies /api and /mcp to :8181
make test
```

- `internal/model`: configuration entities; `internal/store`: SQLite
- `internal/render/{nginx,edge,haproxy}`: config renderers; `internal/agent`: engine control protocol
- `internal/edge`: Relay Edge, the built-in reverse proxy (`relay edge run`)
- `internal/apply`: pending changes, versions, validation, reload, auto-rollback
- `web/`: React + TypeScript UI (built into `internal/webui/dist`, embedded in the binary)
- `design/`: the Claude Design mockups this UI implements
- `docs/SLICES.md`: architecture contracts between feature areas

## Contributing and security

See [CONTRIBUTING.md](CONTRIBUTING.md) for how to set up a development
environment and send changes, and [SECURITY.md](SECURITY.md) for how to report
a vulnerability privately.

## License

Relay is free software: you can redistribute it and/or modify it under the
terms of the [GNU Affero General Public License v3.0](LICENSE) as published by
the Free Software Foundation.

In short: you can use, self-host, modify and share Relay for free, for any
purpose, including commercially. If you distribute a modified version, or run
one as a service that other people use over a network, you must make the
source code of your version available under the same license.

Copyright (C) 2026 InstantOffr
