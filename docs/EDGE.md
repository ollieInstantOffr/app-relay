# Relay Edge — contracts

Relay Edge is Relay's own reverse proxy engine. Users choose nginx or Relay
Edge in Settings → General (`proxyEngine`). Exactly one of them serves the
HTTP/HTTPS ports and streams; HAProxy is unaffected.

This file is the contract between the parts that build it. Keep it current.

## 1. Packages and ownership

| Part | Files |
|---|---|
| Data plane | `internal/edge/**` (config types in `config.go` are the schema) |
| Renderer | `internal/render/edge/**`: `Render(snap *model.Snapshot, env render.Env) (agent.Files, error)` → `edge.json` + `htpasswd/<safeID(accessListID)>`; plus `RenderHost`/`RenderStream` preview helpers mirroring the nginx package |
| Integration | agent engine, core, model, store, apply, acme, auth, lb, logs, health, engines, api, `cmd/relay`, `docker-compose.yml`, deploy docs |
| UI + docs | `web/src/**`, MCP tool descriptions |

## 2. Process model

- Container `relay-edge` (compose service `edge`): image `alpine:3.22`, host
  network, label `relay.engine=edge`, entrypoint waits for
  `/opt/relay/bin/relay` then runs `relay agent --engine edge`. Volumes like
  nginx: `relay-bin:/opt/relay:ro`, `relay-data:/data:ro`, `relay-run:/run/relay`,
  `relay-logs:/var/log/relay`, `relay-edge:/etc/relay/edge`.
- Agent engine name `edge`, socket `/run/relay/edge.sock`, config root
  `/etc/relay/edge`, main file `edge.json`.
- The agent supervises `relay edge run --config <root>/current/edge.json`.
  - Validate: `relay edge check <dir>` (exit 0 = valid). Output lines use
    nginx-style tags: `[emerg] <message>` on failure, `[notice] configuration
    is valid` on success.
  - Reload: `SIGHUP`. Edge re-reads the config through the `current` symlink,
    compiles it and opens any new listeners **before** swapping. On success it
    logs `[notice] config loaded hash=<release dir name>` to stderr; on
    failure `[emerg] reload failed: <reason>` and keeps serving the old config.
    The agent waits (max 15 s) for one of the two lines.
  - Start: logs `[notice] started hash=<release dir name>` once listening.
  - Stop: `SIGTERM`/`SIGQUIT` → stop accepting, drain up to 20 s, exit 0.
  - Detect (agent status): version = the relay version, modules `stream`,
    `http_v2`, `http_v3`, `auth_request` (+ `ipv6` when the host has IPv6).
- `httpPort` / `httpsPort` of 0 means that listener is not opened.
- Bootstrap config (before the first apply): HTTP port
  `$RELAY_BOOTSTRAP_HTTP_PORT` (80), HTTPS port 443 without hosts (the
  placeholder `/data/certs/_default` is referenced only when its files exist),
  default action `close`, ACME webroot `/data/acme`, log dir `/var/log/relay`,
  status on `127.0.0.1:$RELAY_EDGE_STATUS_PORT` (18081).
- Only the selected proxy engine may bind the ports. The relay app writes the
  selected engine to `/run/relay/proxy-engine` (`nginx` or `edge`; missing =
  `nginx`). On a fresh config root an agent whose engine is not selected
  writes its bootstrap release but does not start it. After that, the
  `stopped` marker set by `Apply{Stop:true}` governs (`/v1/start` and any
  non-stop apply clear it). `Apply{Stop:true}` without files and hash keeps
  the current release and only stops the engine.
- Switching engines (apply of a version whose `proxyEngine` differs from the
  live one): validate on the new agent (503 when unreachable) → HAProxy as
  usual → old engine `Apply{Stop:true}` with the live files → new engine
  `Apply{files}` → health check of every enabled host with an upstream. On
  failure the new engine gets `Apply{Stop:true}` and the old one `/v1/start`;
  the version is `rolled_back` with `failedEngine` `edge`/`nginx`.
- The reconcile loop restores the live release on the selected engine and
  sends `Apply{Stop:true}` to the other proxy engine whenever it runs.

## 3. Logs (compatibility with internal/logs)

- `access.log`: one JSON object per line, all values strings, keys exactly as
  `internal/render/nginx/logformat.go` `relay_json`; `ts` like `$msec`
  (`1726400000.123`), `host_id` empty for the default server and redirect
  groups, `status` 444 when the default server closes a connection, empty
  values as `""`.
- `stream-access.log`: `relay_stream_json` keys.
- `error.log`: `YYYY/MM/DD HH:MM:SS [level] message` in UTC, levels
  `debug info notice warn error crit alert emerg`. Error source in the store:
  `edge`.

## 4. Parity rules (nginx behaviour Edge reproduces)

Server selection, per listener (HTTP port / HTTPS port):
- Host header (lowercased, port stripped) picks the server: exact name, then
  the longest matching `*.` wildcard; otherwise the default server. On TLS the
  certificate is chosen by SNI with the same rule (placeholder for unknown).
- A host with a cert listens on HTTPS; on HTTP it serves the host unless
  ForceHTTPS (then ACME + 301 `https://$host[:port]$request_uri`). A host
  without a cert listens on HTTP only (HTTPS requests for it go to the default
  server). Redirect groups follow the same rule.

Request pipeline for a host server:
1. Blocklist → 403 (also on ACME, redirect groups and the default server).
2. Block exploits → 403 (patterns in `internal/render/nginx/nginx.go`
   `snippets/block-exploits.conf`, matched case-insensitively against the raw
   request URI / raw query / User-Agent).
3. Rate limit → 429 (all locations incl. ACME; exempt keys skip it).
4. Location match: `/.well-known/acme-challenge/` (served from the webroot, no
   access checks) → path redirects → longest prefix location. A request for a
   path equal to a proxy location's path minus its trailing `/` gets 301 to
   the path with `/` (query kept). Location matching uses the decoded path
   with merged slashes.
5. Access: IP rules (403), basic auth (401 + `WWW-Authenticate`), forward auth
   (2xx allow, 401/403 as-is, anything else 500). `satisfy all` = every
   configured check must pass; `SatisfyAny` = one is enough, otherwise the
   last failing check answers. A 401 with forward auth + SignInURL on the
   location becomes `302 SignInURL?rd=<url-encoded scheme://Host-header/request-uri>`.
6. Deny location → 403; otherwise proxy.

Proxying:
- Request headers: `Host: $host`, `X-Real-IP`, `X-Forwarded-For` (appended),
  `X-Forwarded-Proto`, `X-Forwarded-Host: $host`, `X-Forwarded-Port` (listen
  port), `X-Request-ID` (new 32-hex id). Client headers containing `_` are
  dropped. Websocket upgrades only on websocket locations.
- Path: strip prefix → `^<prefix>/?(.*)$` → `<base>/$1`; else `<base>$path`
  when the upstream has a path; else the raw request URI unchanged.
- Upstream TLS: SNI = upstream host; verification per `UpstreamTLSVerify`.
- Timeouts: connect 60 s, read/send idle per host (default 60 s, also for
  websockets). Errors: 502 (connect/protocol), 504 (timeout).
- Response headers added: `Strict-Transport-Security` (TLS, when set),
  `X-Robots-Tag: noindex, nofollow`, `Alt-Svc: h3=":<httpsPort>"; ma=86400`.
- Asset cache (location Cache): `(?i)\.(css|js|mjs|map|png|jpe?g|gif|ico|svg|webp|avif|bmp|woff2?|ttf|otf|eot|mp4|webm|ogg|mp3|wav|pdf)$`,
  GET/HEAD, cache 200/301/302 for 30 d, key scheme+upstream+request URI,
  honour `Cache-Control: private/no-store/no-cache`, `Set-Cookie`, `Vary: *`;
  serve stale on upstream error/timeout/5xx; `Expires` +
  `Cache-Control: max-age=2592000` on those responses.
- gzip for `text/html` and the nginx `gzip_types` list, ≥1024 bytes, when the
  client accepts it and the response isn't already encoded; `Vary: Accept-Encoding`.
- Max body: 413 when exceeded (default rendered as 1 MiB).

Improvements over nginx (intentional differences):
- Configuration and certificate swaps never drop connections; a failed reload
  keeps the old config; renewed certificate files are picked up automatically.
- Upstream keep-alive connection pools.
- When forward auth is active on a location, client-supplied `Remote-User` /
  `Remote-Groups` headers are always removed (and replaced when passed).
- The sign-in `rd` parameter is URL-encoded.
- Basic-auth verifications are cached for 60 s.
- `/metrics` Prometheus endpoint on the status listener.

Not supported (rejected by validation when Relay Edge is selected):
- Custom nginx snippets (`customNginx`).
- Geo-blocking by country (same as the official nginx image: skipped with a
  warning in the rendered config).

## 5. Settings, API and UI

- `model.GeneralSettings.ProxyEngine` (`proxyEngine`): `nginx` (default) |
  `edge`. Part of the snapshot and of the General pending projection, so a
  switch is a pending change applied with validation, health check and
  automatic rollback.
  Saving `edge` is refused while any host has `customNginx` (field error on
  `proxyEngine` listing up to 5 hosts), and a host with `customNginx` can't
  be saved while `edge` is selected (field error on `customNginx`).
- The active proxy engine is the live version's `proxyEngine` (General
  settings before the first apply): `app.ProxyEngine(ctx)` / `app.Proxy(ctx)`.
- `core.EnginesStatus` gains `edge` (EngineState) and `proxy` (active engine
  name). `/api/engines/{engine}/{start|stop|reload|logs|listeners}` accept
  `edge`. `EngineChanged` events carry `engine: nginx|haproxy|edge`.
- Config versions record `proxyEngine` (JSON `proxyEngine`, plus `proxyHash`
  = `nginxHash`); the files of the active proxy engine are stored where nginx
  files were stored (`nginx_files` column, JSON `nginxFiles`) with paths
  prefixed `edge/` in diffs and `edge/` instead of `nginx/` in downloads when
  the engine is edge.
- Previews: `/api/preview/proxy/host` and `/api/preview/proxy/stream`
  (`/api/preview/nginx/*` remain as aliases) return the active engine's
  rendering plus `engine`. `POST /api/lb/expose/preview` keeps its `nginx*`
  fields for the active engine's files and adds `engine`.
- `GET /api/metrics/overview`: `nginx` describes the active proxy engine,
  `proxyEngine` names it. Error log rows from `error.log` have source `edge`
  while Relay Edge is active. `GET /api/ports`: owner `edge` for the proxy
  ports and streams while Relay Edge is active.
- `GET /api/engines/updates`: `proxyEngine`, and `nginx.inactive: true` while
  Relay Edge is selected (no container/image blockers are reported then).
