# Relay Balancer — contracts

Relay Balancer (beta) is Relay's own load balancer, built into the `relay`
binary. Users pick HAProxy or Relay Balancer as the load balancer engine;
exactly one of them runs the frontends and backends. It executes a resolved
`balancer.json` with the behaviour of the `haproxy.cfg` Relay renders for the
same snapshot, and speaks the parts of HAProxy's runtime API and log formats
Relay consumes, so `internal/lb` and `internal/logs` work unchanged.

This file is the contract between the parts that build it. Keep it current.

## 1. Packages and ownership

| Part | Files |
|---|---|
| Schema | `internal/balancer/spec/spec.go` (balancer.json), `spec/validate.go` (`(*Config).Validate`, `ParseBind`, `ParsePrefix`, `ParseIPRule`) |
| Data plane | `internal/balancer/**`: `RunCLI`, `Check`, `NewServer` / `Start` / `Reload` / `Shutdown` |
| Health checks | `internal/lbcheck` (shared with the "Check now" probe in `internal/lb/probe.go`) |
| Renderer | `internal/render/balancer/**`: snapshot → `balancer.json` |
| Integration | `internal/agent/engine_balancer.go`, apply, core, lb, logs, `cmd/relay` |

## 2. Process model

- The agent (`relay agent --engine balancer`) supervises
  `relay balancer run --config <root>/current/balancer.json`.
- **Release hash** = name of the directory holding `balancer.json` after
  resolving symlinks (the release behind `current`).
- **Check**: `relay balancer check <dir | file>` loads, validates and compiles
  without binding or resolving DNS. Output (stderr): one `[emerg] <message>`
  per problem and a non-nil error, or `[notice] configuration is valid`.
- **Start**: logs `[NOTICE]   (<pid>) : started hash=<hash>` once every
  listener is bound. Start errors are logged as `[ALERT]` and the process exits
  non-zero.
- **Reload** (`SIGHUP`): re-read through `current`, validate, compile, bind new
  listeners, then switch. Logs `config loaded hash=<hash>` (hash followed by
  end of line) or `[ALERT] … reload failed: <reason>`; on any failure the old
  configuration keeps serving and listeners opened by the attempt are closed.
  - Listeners are keyed by mode + normalised bind; unchanged keys keep their
    socket (no refused connection, keep-alive connections continue with the
    new configuration). Removed listeners stop accepting and drain up to 20 s.
  - When a frontend's client timeout changes, a new `http.Server` takes new
    connections on the same socket; the old one stops accepting but keeps its
    keep-alive connections (on the previous timeout) until they close, so
    nobody is disconnected.
  - In-flight HTTP requests and TCP sessions finish on the configuration they
    started with.
- **Stop** (`SIGTERM`, `SIGQUIT`, `SIGINT`): `shutting down (draining up to
  20s)`, stop accepting, wait for requests and sessions, force-close the rest
  at 20 s, `exited`, exit 0.
- All output goes to **stderr** through a bounded asynchronous queue (lines are
  dropped, never blocking traffic, when it is full; `DroppedLogs` in
  `show info`). Lifecycle markers are flushed synchronously.
- A configuration with no frontends and no backends is valid and runs idle
  (runtime socket only).

## 3. Runtime API (`runtimeSocket`)

- Unix socket, mode 0660, created at load (a stale socket file is removed,
  the parent directory is created), unlinked on stop or when the path changes.
  Default path `/run/relay/balancer-runtime.sock`; never the agent socket.
- HAProxy non-interactive protocol: read one line, run its commands
  (separated by `;`), write the output, close. Clients read until EOF.

| Command | Output |
|---|---|
| `show stat` | CSV, header `# ` + HAProxy 3.0's exact column list (same as `internal/lb/testdata/showstat.csv`), every line ends with `,`, followed by an empty line |
| `show info` | `Key: value` lines + empty line |
| `set server <be>/<srv> state ready\|drain\|maint` | empty on success |
| `set server <be>/<srv> weight <0-256>` / `<n>%` (of the initial weight, capped at 256) | empty on success |
| `set server <be>/<srv> health up\|stopping\|down` | empty on success |
| `set weight <be>/<srv> <w>`, `get weight <be>/<srv>` (`<w> (initial <iw>)`), `enable server`, `disable server`, `help` | HAProxy forms |
| errors | `No such backend.`, `No such server.`, `Require 'backend/server'.`, `'set server <srv> state' expects 'ready', 'drain' and 'maint'.`, `Absolute weight can only be between 0 and 256 inclusive.`, `Relative weight must be positive.`, `Trailing garbage in weight string.`, `Unknown command: '<cmd>'` + help |

`show stat` rows, in order: `stats,FRONTEND` (when the stats listener is
configured; `internal/lb` filters the proxy named `stats`), one `FRONTEND` row
per frontend, then per backend its servers (declaration order) and its
`BACKEND` row. Filled columns:

- FRONTEND: `scur smax slim(maxConn) stot bin bout ereq status=OPEN pid=1 iid
  type=0 rate rate_max conn_rate conn_rate_max conn_tot mode`, and for http
  `hrsp_* req_rate req_rate_max req_tot comp_in comp_out comp_byp comp_rsp`.
- Server: `qcur scur smax stot bin bout econ eresp wretr wredis status weight
  act bck chkfail chkdown lastchg downtime iid sid lbtot type=2 rate rate_max
  check_status check_code check_duration last_chk check_desc check_rise
  check_fall check_health hrsp_* cli_abrt srv_abrt lastsess qtime ctime rtime
  ttime *_max addr cookie mode idle_conn_cur uweight`.
  - `status`: `MAINT`, `DRAIN`, `DRAIN x/y`, `UP`, `UP x/y` (going down),
    `DOWN`, `DOWN x/y` (going up), `no check`.
  - `check_status`: `INI` before the first result, then `L4OK L4TOUT L4CON
    L6OK L6TOUT L6RSP L7OK L7TOUT L7RSP L7STS`; `check_code` is the HTTP status
    for L7 results; `check_desc` HAProxy's text (`Layer7 wrong status`, …);
    `last_chk` the detail (`connection refused`, `503 Service Unavailable
    (expected 2xx)`; commas and quotes replaced).
  - `weight`/`uweight` = current runtime weight; `act`/`bck` = role flags;
    `addr` = the address dialled (resolved IP:port).
- BACKEND: `qcur qmax scur smax slim(fullconn: 10 % of the referencing
  frontends' maxconn, ≥ 1) stot bin bout econ eresp wretr wredis status
  weight act bck chkdown lastchg downtime lbtot type=1 rate rate_max hrsp_*
  req_tot cli_abrt srv_abrt comp_* lastsess qtime ctime rtime ttime mode algo
  uweight`. `status` is `UP` when the servers taking traffic have a total
  weight > 0 (or the backend has no servers), else `DOWN`; `weight` is that
  total; `act`/`bck` count usable active/backup servers.
- `rate` = sessions over the last second (HAProxy freq_ctr); `rtime`, `ctime`,
  `qtime`, `ttime` = averages (ms) over the last 1024 sessions; `lastchg` and
  `downtime` in seconds. Frontend `stot`/`scur` count client connections;
  backend and server `stot`/`scur` count HTTP requests (http) or sessions (tcp).

`show info` keys: `Name` (`Relay Balancer`), `Version`, `Release_date`,
`Nbthread`, `Nbproc`, `Process_num`, `Pid`, `Uptime`, `Uptime_sec`,
`Memmax_MB`, `PoolAlloc_MB`, `PoolUsed_MB`, `PoolFailed`, `Maxconn`,
`Hard_maxconn`, `CurrConns`, `CumConns`, `CumReq`, `ConnRate`,
`ConnRateLimit`, `MaxConnRate`, `SessRate`, `SessRateLimit`, `MaxSessRate`,
`Tasks`, `Run_queue`, `Idle_pct`, `node`, `Stopping`, `Jobs`,
`Unstoppable Jobs`, `Listeners`, `DroppedLogs`, `Start_time_sec`, `Tainted`,
`TotalWarnings`, `MaxconnReached`, `Hash` (active release).

**Pid** is the OS process id plus the number of successful reloads: it changes
on every reload (like a new HAProxy worker), so the sampler's reload detection
(quiet period, `reapplyStates`) behaves as with HAProxy. Counters are *not*
reset by a reload (the sampler's `delta` handles both cases); they reset when
the process restarts.

## 4. Logs (compatibility with internal/logs)

Everything is parsed by `logs.ParseHAProxyLine` (log source `balancer` while
Relay Balancer is the active load balancer engine).

- Process messages: `[NOTICE]   (<pid>) : <message>`, `[WARNING]  (<pid>) : …`,
  `[ALERT]    (<pid>) : …` (levels notice / warn / alert).
  - `Proxy <name> started.` for each new frontend/backend.
  - `Server <be>/<srv> is DOWN, reason: <check_desc>[, code: <n>][, info: "<detail>"], check duration: <n>ms. <a> active and <b> backup servers left. <n> sessions active, 0 requeued, <q> remaining in queue.`
  - `Server <be>/<srv> is UP, reason: Layer7 check passed, code: 200, check duration: 1ms. <a> active and <b> backup servers online. 0 sessions requeued, 0 total in queue.`
  - `Server <be>/<srv> is going DOWN for maintenance. …`, `… is UP/READY (leaving forced maintenance).`, `… enters drain state.`, `… is UP (leaving forced drain).`, `… is UP|DOWN (forced by the runtime API).`
  - `[ALERT] … backend <be> has no server available!`
  - Lifecycle markers of §2; warnings such as a missing `caFile`.
- Traffic lines (raw, no prefix; level info):
  - http (`option httplog`):
    `%ci:%cp [%tr] %ft %b/%s %TR/%Tw/%Tc/%Tr/%Ta %ST %B %CC %CS %tsc %ac/%fc/%bc/%sc/%rc %sq/%bq "%r"`,
    e.g. `10.0.0.9:51234 [15/Sep/2026:14:02:03.123] http-in web-app/web-app-1 0/0/1/12/13 200 1520 - - --VN 3/1/1/1/0 0/0 "GET /api HTTP/1.1"`.
    `%TR` is always 0; `%B` includes response headers (estimated); `%rc` has a
    `+` prefix after a redispatch; `"`, `#` and non-printable bytes in the
    request line are encoded as `#XX`. Without a backend: `<fe>/<NOSRV>`.
  - tcp (`option tcplog`): `%ci:%cp [%t] %ft %b/%s %Tw/%Tc/%Tt %B %ts %ac/%fc/%bc/%sc/%rc %sq/%bq`.
    Like `option dontlognull`, a connected session whose client sent nothing
    is not logged.
  - Termination flags used: `--` normal, `CD`/`SD` client/server abort during
    data, `cD`/`sD` idle timeouts, `CH`/`SH`/`sH` abort, invalid response,
    timeout while waiting for headers, `SC`/`sC` connection failure or no
    server, `CC` client gone while connecting,
    `PR` tcp session without backend. Cookie flags (3rd/4th char, cookie
    modes): `N`/`I`/`D`/`V` and `N`/`I`/`R`/`D`.
  - PROXY header errors: `%ci:%cp [date] <fe>/<bind>: Received something which does not look like a PROXY protocol header`.

## 5. Parity rules (haproxy.cfg behaviour reproduced)

Global and listeners
- `maxConn` is HAProxy's global `maxconn`: listeners stop accepting while the
  number of client connections (all frontends) is at the limit. Frontends
  have no own limit — HAProxy frontends without `maxconn` inherit the global
  value, so the global limit is the effective one; `slim` reports it.
- `acceptProxy`: PROXY v1 or v2 (read within the client timeout; LOCAL and
  UNKNOWN keep the socket addresses; anything else closes the connection).
  The carried client address is used everywhere (rules, X-Forwarded-For,
  logs, source hashing, stick table, send-proxy).
- Defaults: connect 5 s, client 50 s, server 50 s, check interval 2 s, rise 2,
  fall 3. 0 inherits (frontend/backend → global → default). The client timeout
  is an inactivity timeout: it also applies while reading a request body.
  The queue timeout is accepted but unused (servers have no connection limit
  to queue behind).

Rules
- First rule whose conditions all hold; `negate` inverts one condition; a
  condition that doesn't exist in the frontend's mode (host/path/header in
  tcp, sni in http) evaluates false before negation, like an ACL on an
  unavailable fetch. A rule without conditions always matches. Otherwise
  `defaultBackend`; none → http 503 (`SC`), tcp close (`PR`).
- `host`: Host header lowercased, port stripped (`[v6]` keeps brackets),
  `exact` or `suffix`. `path`/`path_beg`/`path_reg`: raw request path, not
  decoded, without query. `header`: every comma-separated value of every
  occurrence (`req.hdr`), names case-insensitive, values case-sensitive,
  `found`. `src`: IPs/CIDRs of both families. `sni`: TLS ClientHello peeked
  (not consumed) for up to `inspectDelayMs` (0 → 5 s) when the frontend has
  SNI rules; non-TLS first bytes decide immediately.

HTTP
- HTTP/1.1 per frontend, keep-alive; client timeout = header read and idle
  keep-alive timeout, and write inactivity towards the client.
- Request forwarded with its Host header and raw target (origin form); hop-by-
  hop headers (`Connection` and the headers it lists, `Keep-Alive`,
  `Proxy-Connection`, `TE`, `Trailer`, `Transfer-Encoding`, `Upgrade` unless
  upgrading) and `Expect` removed; no User-Agent added.
- X-Forwarded-For when the frontend or backend enables it, once, appended to
  an existing value (`a, client`); skipped for clients in the frontend's
  `forwardForExcept`.
- Per-server keep-alive pools (idle connections are watched and dropped when
  the server closes them); send-proxy backends use private connections kept
  per client connection (like HAProxy's non-shareable connections).
- Errors use HAProxy's built-in pages (`Connection: close`): 503 no
  backend/no usable server/connect failure, 504 server timeout before the
  response headers, 502 invalid or missing response, 408 when the client
  stops sending the request body for the client timeout (`cD`). A server failing
  mid-body aborts the client connection (`SD`).
- WebSocket and other `Upgrade`s: 101 is forwarded, then a raw tunnel with the
  client/server idle timeouts.
- Compression (`compression`): gzip (level 1) for `text/html`, `text/plain`,
  `text/css`, `application/json`, `application/javascript` (prefix of
  Content-Type), status 200–203, HTTP/1.1 request and response, client accepts
  gzip (q > 0), no Content-Encoding, no `Cache-Control: no-transform`, not
  HEAD; adds `Vary: Accept-Encoding`, drops Content-Length, weakens ETags.

TCP
- Bidirectional copy with half-close; a side's read times out after its idle
  timeout (client / server) unless the other direction was active meanwhile.

Servers
- Retries: a failed connection (TCP, PROXY header, TLS handshake within the
  connect timeout) is retried `retries` times with HAProxy's turn-around delay
  (min(connect timeout, 1 s) minus the attempt time); with `redispatch` the
  last retry goes to another server chosen by the algorithm. `wretr` per
  retry, `wredis` per redispatch (backend and the server left), `econ` on the
  final failure.
- `sendProxy`: PROXY v2 header (client address → frontend address, IPv6 when
  families differ).
- `tls`: no SNI, no ALPN; `tlsVerify` verifies the chain against `caFile` (or
  the system roots) without checking the host name. A missing `caFile` falls
  back to the system roots with a warning; an unreadable or empty one is an
  error.
- Host name servers are resolved at load (first address); an unresolvable
  server starts DOWN and the load succeeds (`init-addr libc,none`); it is not
  retried until the next reload.

Algorithms (servers usable = health UP, admin ready, weight > 0)
- `roundrobin`, `static-rr`: smooth weighted round robin; runtime weight
  changes apply immediately.
- `leastconn`: fewest current sessions relative to weight; ties rotate.
- `source`: HAProxy map-based hash (XOR of the address words) over a table
  where each server appears weight times. `uri`: sdbm hash of the path without
  the query over the same table.
- `random`: two weighted draws, the less loaded (sessions × weight) wins.
- `first`: first usable server in declaration order.
- Backup servers are used only when no active server is usable, and then the
  first usable backup only.
- Admin states: `drain` (and weight 0) gets no new clients but persistent
  clients (cookie, stick table) still reach it; `maint` gets nothing and its
  checks pause; a DOWN server gets nothing. Leaving `maint` puts a checked
  server UP with health = rise (the first failed check brings it down) and
  checks it at once.
- No usable server: like HAProxy (no server maxconn to queue behind), the
  request gets 503 or the TCP session is closed at once (`SC`).

Sticky sessions
- `insert` (= `insert indirect nocache`): `Set-Cookie: <name>=<server cookie>;
  path=/` plus `Cache-Control: private` when the client had no valid cookie for
  the server that answered; the cookie is removed from requests and a server's
  own Set-Cookie with that name is dropped.
- `prefix` (= `prefix nocache`): response `Set-Cookie: <name>=<v>` becomes
  `<name>=<cookie>~<v>` (+ `Cache-Control: private`); request
  `<name>=<cookie>~<v>` selects the server and is forwarded as `<name>=<v>`.
- `source` (`stick on src`): client address → server name, refreshed on every
  session, expiry `expireMs` (30 min) and size `tableSize` (200 000, least
  recently stored evicted). The table survives reloads.
- Persistence to a DOWN or maint server is ignored (load balanced again, new
  cookie); to a drain server it is honoured.

Health checks (servers with `check` in backends with `check`)
- Interval, rise, fall from the check, else `config.check`, else defaults.
  The whole check (connect, TLS, exchange) times out after the interval. The
  first check runs at a random point of the first interval.
- HAProxy's health counter: servers start UP with health = rise (`UP 1/3`
  until a success), so the first failure brings a fresh server down; later
  `fall` consecutive failures are needed, and `rise` successes bring it back.
- PROXY v2 LOCAL header first for send-proxy backends; TLS for tls backends.
- `tcp`: connect (`L4OK`, TLS: `L6OK`). `http`: `METHOD path HTTP/1.0` without
  Host, or HTTP/1.1 with `Host` and `Connection: close`; `expect` `200`, `2xx`,
  `200-399`, lists (`200-299,404`) → `L7OK`, else `L7STS`; `L7RSP` invalid,
  `L7TOUT` timeout. `pgsql`: StartupMessage (`user`, default `relay`), `R` →
  `L7OK`, `E` → `L7RSP` with the message. `mysql`: server greeting (protocol
  10 → `L7OK`, error packet → `L7RSP`). `redis`: `PING` → `+PONG` (`L7OK`),
  else `L7STS`. Connect errors: `L4CON`, `L4TOUT`.

Stats listener (`stats`)
- `/` HTML page (one table per proxy with HAProxy's row colours, `Refresh: 10`;
  `;norefresh` disables it), `/;csv` the `show stat` CSV, `/metrics` when
  `prometheus` is set. Access rules first: first match wins, unmatched clients
  are allowed, denied → 403 page.
- Prometheus metrics use the HAProxy exporter names and labels
  (`proxy`, `server`, `state`, `code`): `haproxy_process_{nbthread, nbproc,
  relative_process_id, uptime_seconds, start_time_seconds, max_connections,
  current_connections, connections_total, requests_total, dropped_logs_total,
  stopping}`; `haproxy_frontend_{status, current_sessions, max_sessions,
  limit_sessions, sessions_total, bytes_in_total, bytes_out_total,
  requests_denied_total, request_errors_total, current_session_rate,
  max_session_rate, connections_total, http_requests_total,
  http_responses_total{code}, http_comp_*}`; `haproxy_backend_{status{state=UP|DOWN},
  current_queue, max_queue, current_sessions, max_sessions, limit_sessions,
  sessions_total, bytes_*_total, connection_errors_total,
  response_errors_total, retry_warnings_total, redispatch_warnings_total,
  weight, active_servers, backup_servers, check_up_down_total,
  check_last_change_seconds, downtime_seconds_total, loadbalanced_total,
  current_session_rate, max_session_rate, last_session_seconds,
  http_requests_total, client_aborts_total, server_aborts_total,
  *_time_average_seconds, http_responses_total{code}, http_comp_*}`;
  `haproxy_server_{status{state=DOWN|UP|MAINT|DRAIN|NOLB}, current_queue,
  max_queue, current_sessions, max_sessions, sessions_total, bytes_*_total,
  connection_errors_total, response_errors_total, retry_warnings_total,
  redispatch_warnings_total, weight, check_failures_total,
  check_up_down_total, check_last_change_seconds, downtime_seconds_total,
  loadbalanced_total, current_session_rate, max_session_rate,
  last_session_seconds, check_duration_seconds, client_aborts_total,
  server_aborts_total, *_time_average_seconds}`.

## 6. State kept across reloads

HAProxy starts a new worker on reload: counters, health and admin states
reset, and Relay's LB service re-applies drain/maint afterwards. Relay Balancer
keeps, for proxies and servers that still exist:
- frontend/backend counters (by name), server counters, health (counter,
  status, last check) and admin state/weight (by backend name + server name +
  address:port), the source stick table, idle server connections (same dial
  parameters);
- a server's `state` or `weight` from the new config applies only when it
  differs from the previously loaded config; otherwise runtime changes win.
  Re-applying admin states after a reload is therefore harmless.
A server whose address or port changed is a new server (fresh state).
When a frontend's client timeout changes, new connections use the new timeout
while existing keep-alive connections finish on the previous one; nobody is
disconnected by the reload.

## 7. Known differences from HAProxy

| Area | Relay Balancer |
|---|---|
| `path_reg` | Go RE2 syntax (no backreferences/lookaround); the renderer rejects incompatible patterns |
| Reload | seamless always; state and counters kept (§6); `Pid` = process id + reloads |
| Weights | runtime weight changes also accepted for `static-rr`, `source` and `uri` (HAProxy only allows 0 %/100 % there) |
| pgsql check | any authentication request passes (HAProxy ≥ 2.2 requires AuthenticationOk) |
| Protocols | HTTP/1.x only on frontends (Relay never renders h2 binds); no HTTP/2 to servers |
| Timeouts | tcp idle timeouts consider both directions |
| Binds | no SO_REUSEPORT: a wildcard and a specific address on the same port are rejected by validation (HAProxy can bind both on Linux) |
| DNS | resolved once per load, no runtime re-resolution |
| show stat | columns Relay doesn't use are empty (agents, h1/h2, SSL, cache, idle pools except `idle_conn_cur`) |
| Stats page | simplified HTML (same data as the CSV, no admin actions) |
| Logs | `%TR` is 0, `%B` header bytes estimated, no captured headers |
