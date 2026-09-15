# Relay — implementation slices & contracts

Relay is built in vertical **slices** (backend + UI per area), developed in
parallel. This file is the contract between slices. Read it fully before
touching code. The design lives in `design/`:

- `design/Relay Proxy Manager.dc.html` — screens 01–16d (HTML mockups; exact
  spacing/colors/copy). It is truncated inside 16d.
- `design/screens-17-30.md` — text of screens 17–30 (same visual system).
- `design/outline.txt` — text outline of screens 01–16d.
- Tokens: `web/src/styles/global.css` (from `brand/tokens.md`).

Match the design's copy, layout and density closely. Everything must be real and
working end-to-end (no fake data in the UI): if a value can't be known, show
"—" or an empty state.

## Architecture

```
docker compose (network_mode: host for all five)
├─ relay          Go app: REST API /api, SPA, MCP /mcp, SQLite /data/relay.db, ACME, docker discovery
├─ relay-nginx    nginx + `relay agent --engine nginx`   (PID 1 supervises nginx)
├─ relay-edge     alpine + `relay agent --engine edge`   (PID 1 supervises `relay edge run`, Relay Edge)
├─ relay-haproxy  haproxy + `relay agent --engine haproxy`
└─ relay-balancer alpine + `relay agent --engine balancer` (PID 1 supervises `relay balancer run`, Relay Balancer)
shared volumes: relay-data:/data (ro in engines), relay-run:/run/relay (agent sockets, proxy-engine),
                relay-logs:/var/log/relay (proxy engine access/error logs)
                relay-bin:/opt/relay (relay copies its static binary here; engines run it)
```

- Desired config = SQLite documents (`internal/model`, `internal/store`).
- The **proxy engine** (`general.proxyEngine`: `nginx` | `edge`) serves the
  HTTP/HTTPS ports and streams; only the selected one runs (`docs/EDGE.md`).
  `app.Proxy(ctx)` returns its agent client, `app.ProxyEngine(ctx)` its name.
- The **load balancer engine** (`general.lbEngine`: `haproxy` | `balancer`) runs
  backends and frontends; only the selected one runs (`docs/BALANCER.md`).
  `app.LBClient(ctx)` returns its agent client, `app.LBEngine(ctx)` its name;
  both engines answer the same runtime API subset (`/v1/runtime`), so
  `internal/lb` stats, drain and weights work unchanged. Renderers are
  registered in `internal/lb/lbengine`. Config versions record `lb_engine`;
  `haproxy_cfg` / `haproxy_hash` / `haproxy_running` hold the active load
  balancer engine's main file, hash and run state.
- Edits are saved immediately and become **pending changes**. **Apply** renders
  the proxy engine's files + the load balancer engine's file (haproxy.cfg | balancer.json), sends them to the agents (validate → atomic swap →
  reload), health-checks for 10 s and rolls back automatically on failure.
  Each apply is a config **version** with a full snapshot (rollback restores it).
- Agents speak `internal/agent/protocol.go` over unix sockets
  `/run/relay/nginx.sock`, `/run/relay/edge.sock`, `/run/relay/haproxy.sock` and `/run/relay/balancer.sock`.
- Because all containers use host networking, load balancer frontends bound to
  `127.0.0.1:10080` are reachable from nginx (Expose wizard).

## Repository layout & ownership

Shared (owned by the integrator — do **not** edit; if you truly need a change,
make the smallest additive edit and mention it in your final report):
`internal/model/model.go`, `internal/model/interfaces.go`, `internal/store/{store,repo,snapshot,defaults,audit}.go`,
`internal/store/migrations/001_init.sql`, `internal/core/{app,actor,services}.go`, `internal/events`,
`internal/agent/{protocol,client}.go`, `internal/render/env.go`, `internal/render/nginx/logformat.go`,
`internal/httpx/*`, `internal/api/*` (it only delegates to slice `Routes` functions), `cmd/relay/main.go`,
`web/src/lib/*`, `web/src/components/ui/*`, `web/src/components/shell/*`, `web/src/app/*`, `web/src/main.tsx`,
`web/src/styles/global.css`, `go.mod` / `go.sum` (deps are pinned via `internal/deps`; do not add modules —
the standard library plus pinned modules must suffice), `web/package.json` (no new npm packages).

Several slices work **in the same working tree at the same time**. Never run
`git` commands that change state (commit, checkout, stash, reset). Verify your
own packages (`go build ./internal/<pkg>/... && go vet ./internal/<pkg>/...`);
if `go build ./...` fails only in files you don't own, ignore it.

You may **add** files anywhere in your own areas. New shared UI helpers go in
your feature folder. Extra CSS: put it in `web/src/features/<yours>/<name>.css`
and import it from your component. New store queries: `internal/store/<slice>_*.go`.
New migrations: `internal/store/migrations/NNN_<slice>.sql` using your number
range. New core methods beyond the contract: `internal/core/<slice>_ext.go`.

| Slice | Backend (own) | UI (own) | Migration range |
|---|---|---|---|
| **engine** | `internal/agent/server.go` (+ new files in agent), `internal/render/nginx/*` (except logformat.go), `internal/apply/*` (routes: `apply.Routes`), `deploy/`, `Dockerfile`, `docker-compose.yml`, `.dockerignore` | `features/history/*` (HistoryPage, PendingBar, EngineBanners) | 010–019 |
| **lb** | `internal/render/haproxy/*`, `internal/lb/*` (routes: `lb.Routes`), `internal/model/validate_lb.go` | `features/loadbalancer/*`, `features/settings/HAProxySettings.tsx` | 020–029 |
| **hosts** | `internal/hostsapi/*` (routes: `hostsapi.Routes`), `internal/model/validate_host.go` (hosts + redirects + default host) | `features/hosts/*` | 030–039 |
| **certs** | `internal/acme/*` (routes: `acme.Routes`, also access lists + streams extras), `internal/model/validate_certs.go` (certs, DNS providers, access lists, streams, TLS settings + their secrets) | `features/certificates/*`, `features/access/*`, `features/streams/*`, `features/settings/TLSSettings.tsx` | 040–049 |
| **observe** | `internal/logs/*` (routes: `logs.Routes`, incl. health/metrics/audit endpoints), `internal/health/*` | `features/overview/*`, `features/logs/*` | 050–059 |
| **auth** | `internal/auth/*` (routes: `auth.PublicRoutes`, `auth.Routes`), `internal/store/auth_*.go` | `features/auth/*`, `features/settings/{General,Users,About}Settings.tsx` | 060–069 |
| **ops** | `internal/docker/*`, `internal/notify/*`, `internal/backup/*`, `internal/npmimport/*`, `internal/opsapi/*` (routes: `opsapi.Routes`) | `features/docker/*`, `features/settings/{Docker,Notifications,Backup}Settings.tsx` | 070–079 |
| **mcp** | `internal/mcp/*` (routes: `mcp.AdminRoutes`) | `features/mcp/*`, `features/settings/MCPSettings.tsx` | 080–089 |

Route functions have the signature `func Routes(app *core.App, r chi.Router)` and
are called once at startup (inside the authenticated route group, except
`auth.PublicRoutes`). Register CRUD hooks (`httpx.HostHooks.BeforeSave = …`)
and settings hooks (`httpx.SettingsHooks[key] = …`) from there.

Stub files already exist for every owned entry point; replace their contents
but keep the exported names/signatures.

## Backend conventions

- Service packages implement the interfaces in `internal/core/services.go`
  with a `Service` struct from `New(app *core.App)`. `Start(ctx)` must return
  quickly; run loops in goroutines bound to ctx. Services may be unavailable
  (agents not running in dev): degrade gracefully, never crash.
- Other slices' services are reached through `app.X` (interfaces only).
  Never import another slice's implementation package, except that the
  engine slice imports `internal/render/haproxy` (`Render`, `HasBackends`)
  and the MCP/hosts slices may import `internal/render/nginx` for previews.
- API: add routes in your `Routes(app, r)` with **flat patterns** relative to
  `/api` (`r.Post("/hosts/{id}/test", …)`), never `r.Route`/`r.Mount`.
  Handlers use `httpx.WriteJSON`, `httpx.Decode`, `httpx.Fail(w, r, err)`,
  `httpx.Errorf(status, code, msg)`, `httpx.Actor(r)`, `httpx.RequireAdmin(h)`,
  `httpx.InUse(what, names)`. Viewers are blocked from non-GET automatically
  in the protected group.
- Generic CRUD already exists for `/api/{hosts,redirects,streams,access-lists,certificates,dns-providers,backends,frontends}`
  (GET list, GET one, POST create, PUT update, DELETE; certificates have no
  POST). Order: KeepSecrets → BeforeSave hook → Validate → store → audit →
  `Changed` → AfterSave. Customise via `httpx.HostHooks` etc.
  Validation: implement `Validate() error` (model.Validator) on the entity in
  your `internal/model/validate_*.go`; secrets via `Redact()` and
  `KeepSecrets(prev any) error`. Cross-entity checks (unique domains, port
  clashes, references) belong in BeforeSave/BeforeDelete hooks.
- Settings: `GET/PUT /api/settings/{key}` exist for keys general, security,
  docker, tls, default_host, haproxy, mcp, notifications, backup, blocklist.
  Customise with `httpx.SettingsHooks[key] = &httpx.SettingsHook{…}`.
- After any config write outside generic CRUD call
  `app.Changed(ctx, kind, id, name, core.ActionX)`; record user-visible actions
  with `app.Audit(ctx, core.AuditEntry{…})`; dashboard items with
  `app.Activity(ctx, kind, level, title, subject, detail)`; notifications with
  `app.Notify.Notify(ctx, core.Notification{…})`; publish UI events on
  `app.Bus` using topics in `internal/events/bus.go`.
- Use `store.LoadSettings[model.XSettings](ctx, st, key)` for settings with defaults.
- Tests: add focused Go tests for renderers/parsers/pure logic (`go test ./internal/<pkg>/...`).
- Verify with `go build ./... && go vet ./internal/<your pkgs>/...`.

## Cross-slice REST contract

All JSON, camelCase, types in `web/src/lib/types.ts`. Errors:
`{"error":{"code","message","fields?"}}` (422 `invalid` has `fields`).

| Endpoint | Owner | Notes |
|---|---|---|
| `GET /api/auth/session` → `Session` | auth | public; `{authenticated, setupRequired, user?, version, instanceName}` |
| `POST /api/auth/login` `{username,password,totp?,remember}` | auth | public; sets `relay_session` cookie; 401 `totp_required` when a code is needed |
| `POST /api/auth/logout` | auth | |
| `GET /api/pending` → `Pending` | engine | |
| `POST /api/apply` `{summary?}` → `Version` | engine | synchronous; progress via `apply.progress` events |
| `POST /api/pending/discard` | engine | restores live snapshot |
| `GET /api/versions` → `Version[]`, `GET /api/versions/{id}`, `GET /api/versions/{id}/diff?against=` , `POST /api/versions/{id}/rollback` | engine | |
| `GET /api/engines` → `EnginesStatus` (`nginx`, `edge`, `haproxy`, `balancer`, `proxy`, `lb`); `POST /api/engines/{engine}/{start|stop|reload}`; `GET /api/engines/{engine}/logs`; `GET /api/engines/{engine}/listeners` | engine | engine = nginx \| edge \| haproxy \| balancer; starting the non-selected proxy / load balancer engine is 409 `not_selected` |
| `GET /api/engines/updates` → `EngineUpdates`; `POST /api/engines/updates/check` (admin); `POST /api/engines/{engine}/upgrade` `{version}` (admin, 202 → `UpgradeJob`); `GET /api/engines/upgrade-status` → `{job}`; `POST /api/engines/{engine}/keep-image` (admin) | engine | image version check + in-place upgrade (`internal/engines`); progress on `engine.upgrade`; settings key `engines`; see `deploy/UPGRADES.md` |
| `POST /api/preview/proxy/host` `{host}` → `ConfigPreview` | engine | active proxy engine: renders one host (+ validates the full config with the draft); `engine` in the response; `/api/preview/nginx/host` is an alias |
| `POST /api/preview/proxy/stream` `{stream}` → `ConfigPreview` | engine | alias `/api/preview/nginx/stream` |
| `POST /api/preview/lb/backend` `{backend}`, `/frontend` `{frontend}` → `ConfigPreview` | lb | active load balancer engine; `engine` in the response; `/api/preview/haproxy/*` are aliases |
| `GET /api/lb/config` → `{config, engine, …}`; `POST /api/lb/validate` → `{valid, checked}` | lb | the active engine's main file (haproxy.cfg or balancer.json, live); `checked` = haproxy \| balancer \| local; `/api/haproxy/config` and `/api/haproxy/validate` are aliases |
| `GET /api/lb/stats` → `LBStats` | lb | |
| `POST /api/backends/{id}/servers/{serverId}/state` `{state}` | lb | ready/drain/maint at runtime |
| `POST /api/lb/expose` → `{host, frontend, version?}` | lb | Expose wizard |
| `GET /api/health` → `Record<target, HealthStatus>` | observe | targets `host:<id>`, `stream:<id>` |
| `POST /api/health/probe` `{upstream}` → `HealthStatus` | observe | "Upstream reachable · 200 OK · 12 ms" |
| `GET /api/logs/access?host=&status=&ip=&q=&since=&before=&limit=` → `AccessPage` | observe | |
| `GET /api/audit?q=&actor=&since=&before=` → `AuditRow[]`, `GET /api/activity` → `ActivityRow[]` | observe | |
| `GET /api/metrics/overview?range=1h\|24h\|7d` | observe | dashboard numbers + traffic series (shape owned by observe) |
| `GET /api/metrics/hosts` → `HostMetrics` | observe | host cards "42k req" + bars |
| `GET /api/metrics/streams` → `StreamMetrics` | observe | streams table connections/throughput |
| `POST /api/blocklist` `{cidr, note}` / `DELETE /api/blocklist/{cidr}` | observe | global deny list (settings key blocklist) |
| `GET /api/ports` → `{port, proto, address, owner, kind}[]` | certs | port usage card (streams) and port-clash checks |
| `GET /api/tokens?surface=mcp\|rest` → `ApiToken[]`; `POST /api/tokens` `{name, scope, surfaces, expiresInDays, limitTo}` → `ApiToken` (with `token`); `DELETE /api/tokens/{id}` | auth | |
| `GET /api/docker/status` → `{enabled, connected, endpoint, version, containers, error}` | ops | |
| `GET /api/docker/containers` → `Container[]` | ops | |
| `GET /api/approvals?status=` → `Approval[]`; `POST /api/approvals/{id}/approve|deny` | mcp | |
| `POST /api/certificates/request` `CertRequest` → `Certificate` | certs | |

UI deep links every slice must honour: `/hosts?new=1`, `/hosts?edit=<id>`,
`/load-balancer/backends?new=1`, `/load-balancer/backends?edit=<id>`,
`/certificates?cert=<id>`, `/certificates?request=1`,
`/logs/access?host=<domain>&status=<expr>&ip=<ip>`.

Shared UI components other slices import (keep props):
`features/certificates/RequestCertificateDialog.tsx`,
`features/docker/DockerSuggestionsDialog.tsx`, `features/mcp/ApprovalsPanel.tsx`,
`features/auth/NotAllowed.tsx`, `features/auth/CreateTokenDialog.tsx`.

## Engine ↔ LB contract (Expose)

The Expose wizard (lb) creates a HAProxy frontend bound to
`127.0.0.1:<port>` (first free port ≥ `haproxy.exposePortStart`) with
`hostId` set, and a proxy host whose upstream is
`{scheme: "http", host: "127.0.0.1", port: <port>, backendId: <backend id>}`.
The proxy engine renderers simply proxy to host:port; `backendId` is informational.
A stream with `backendId` proxies to the first enabled TCP frontend bound to
127.0.0.1 whose default backend is that backend (validation error otherwise).

## Admin UI host

The auth slice (setup wizard / General settings) maintains one proxy host
with `system: true` for `general.adminDomain` → `http://127.0.0.1:<adminPort>`
(websockets on, access list per security settings). The renderer treats it like
any other host; the hosts UI shows it with a "system" badge and blocks delete.

Window events: `relay:apply` (⌘⏎ — PendingBar applies), `relay:focus-search`
(`/` — focus page filter), `relay:palette`, `relay:shortcuts`,
`relay:unauthenticated` (SessionExpiredDialog).

## UI conventions

- Pages: `<TopBar title … actions … />` then `<div className="page">…`.
  Settings sections render inside `SettingsLayout` (no TopBar).
- Components from `components/ui`: Button, IconButton, Input, Select,
  Textarea, PasswordInput, Field, Toggle, ToggleCard, ToggleRow, Checkbox,
  RadioCard, Segmented, ChipsInput, CopyButton, Kbd, Spinner, Badge, Dot,
  Status, StateBadge, AIChip, Avatar, Card, StatCard, SectionHeader,
  EmptyState, Callout, Tabs, CodeBlock, DiffView, Bars, Sparkline, Meter,
  Stepper, Skeleton, Drawer (tabs, footer, 560/640/820), Dialog,
  ConfirmDialog (typeToConfirm), Menu, Tooltip, useToast, Icon (sprite names
  in `Icon.tsx`).
- Data: `useEntities/useEntity/useSaveEntity/useDeleteEntity`,
  `useSettings/useSaveSettings`, `usePending`, `useEngines`, `useHealth`,
  `useLBStats`, `useContainers`, `usePendingApprovals`, `useRole` from
  `lib/queries`; `api.get/post/put/del` from `lib/api`; live updates via
  `useBusEvent` / `useTopicStream` from `lib/events`; formatting in `lib/format`.
- Saving config from a drawer = "Save to pending" (toast "Host saved ·
  … added to pending changes · Apply now" where Apply now dispatches
  `relay:apply`).
- Viewers: hide/disable write actions (`useRole().canWrite`).
- Verify with `cd web && npx tsc -b` (fix errors in your files; ignore
  in-progress errors in files you don't own) and `npx vite build`.
