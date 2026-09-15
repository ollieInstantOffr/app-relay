-- Configuration documents (one table per kind; data is the JSON entity).
CREATE TABLE hosts         (id TEXT PRIMARY KEY, data TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE redirects     (id TEXT PRIMARY KEY, data TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE streams       (id TEXT PRIMARY KEY, data TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE access_lists  (id TEXT PRIMARY KEY, data TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE certificates  (id TEXT PRIMARY KEY, data TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE dns_providers (id TEXT PRIMARY KEY, data TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE backends      (id TEXT PRIMARY KEY, data TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE frontends     (id TEXT PRIMARY KEY, data TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);

CREATE TABLE settings (key TEXT PRIMARY KEY, data TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE kv (key TEXT PRIMARY KEY, value BLOB NOT NULL);

-- Auth ----------------------------------------------------------------------
CREATE TABLE users (
  id TEXT PRIMARY KEY,
  username TEXT NOT NULL UNIQUE COLLATE NOCASE,
  email TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL,                       -- admin | editor | viewer
  password_hash TEXT NOT NULL,
  totp_secret TEXT NOT NULL DEFAULT '',
  totp_enabled INTEGER NOT NULL DEFAULT 0,
  must_change_password INTEGER NOT NULL DEFAULT 0,
  disabled INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  last_active_at TEXT
);

CREATE TABLE webauthn_credentials (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name TEXT NOT NULL DEFAULT '',
  data TEXT NOT NULL,
  created_at TEXT NOT NULL,
  last_used_at TEXT
);

CREATE TABLE sessions (
  id TEXT PRIMARY KEY,                      -- sha256 of the cookie token
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  last_seen_at TEXT NOT NULL,
  ip TEXT NOT NULL DEFAULT '',
  user_agent TEXT NOT NULL DEFAULT '',
  mfa_pending INTEGER NOT NULL DEFAULT 0,
  remember INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX sessions_user ON sessions(user_id);

CREATE TABLE api_tokens (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  prefix TEXT NOT NULL,                     -- rl_mcp_ / rl_api_
  last4 TEXT NOT NULL,
  hash TEXT NOT NULL UNIQUE,                -- sha256 hex
  scope TEXT NOT NULL,                      -- read | write
  surfaces TEXT NOT NULL DEFAULT '["mcp"]', -- JSON: mcp, rest
  limit_to TEXT NOT NULL DEFAULT '[]',      -- JSON: domain globs / backend names
  expires_at TEXT,
  last_used_at TEXT,
  created_by TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  revoked_at TEXT
);

CREATE TABLE login_attempts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  at TEXT NOT NULL,
  ip TEXT NOT NULL,
  username TEXT NOT NULL,
  success INTEGER NOT NULL
);
CREATE INDEX login_attempts_ip ON login_attempts(ip, at);

-- Audit & activity -----------------------------------------------------------
CREATE TABLE audit_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  at TEXT NOT NULL,
  actor_type TEXT NOT NULL,                 -- user | mcp | token | system | docker
  actor_id TEXT NOT NULL DEFAULT '',
  actor_name TEXT NOT NULL,
  action TEXT NOT NULL,                     -- host.update, auth.login, query_logs …
  target TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT '',
  version INTEGER,
  result TEXT NOT NULL,                     -- ok | applied | pending | denied | blocked | auto | reverted | failed
  ip TEXT NOT NULL DEFAULT ''
);
CREATE INDEX audit_at ON audit_log(at);

CREATE TABLE activity (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  at TEXT NOT NULL,
  kind TEXT NOT NULL,                       -- cert.renewed, upstream.down, host.created, reload …
  level TEXT NOT NULL,                      -- ok | info | warn | error
  title TEXT NOT NULL,
  subject TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT ''
);
CREATE INDEX activity_at ON activity(at);

-- Config versions ------------------------------------------------------------
CREATE TABLE config_versions (
  id INTEGER PRIMARY KEY,                   -- v142
  created_at TEXT NOT NULL,
  actor TEXT NOT NULL,                      -- admin, mcp:claude, docker, system
  summary TEXT NOT NULL,
  status TEXT NOT NULL,                     -- live | superseded | rolled_back | failed | draft
  snapshot TEXT NOT NULL,                   -- JSON model.Snapshot
  nginx_files TEXT NOT NULL,                -- JSON map path → content
  haproxy_cfg TEXT NOT NULL,
  changes TEXT NOT NULL DEFAULT '[]',       -- JSON pending items at apply time
  error TEXT NOT NULL DEFAULT '',
  validate_ms INTEGER NOT NULL DEFAULT 0,
  reload_ms INTEGER NOT NULL DEFAULT 0,
  rolled_back_to INTEGER
);

-- MCP approvals ---------------------------------------------------------------
CREATE TABLE approvals (
  id TEXT PRIMARY KEY,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  token_id TEXT NOT NULL DEFAULT '',
  client_name TEXT NOT NULL,
  tool TEXT NOT NULL,
  args TEXT NOT NULL,
  summary TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT '',
  preview TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,                     -- pending | approved | denied | expired
  decided_by TEXT NOT NULL DEFAULT '',
  decided_at TEXT,
  result TEXT NOT NULL DEFAULT ''
);
CREATE INDEX approvals_status ON approvals(status, created_at);

-- Traffic -------------------------------------------------------------------
CREATE TABLE access_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ts TEXT NOT NULL,
  kind TEXT NOT NULL DEFAULT 'http',        -- http | stream
  host_id TEXT NOT NULL DEFAULT '',
  host TEXT NOT NULL DEFAULT '',
  method TEXT NOT NULL DEFAULT '',
  path TEXT NOT NULL DEFAULT '',
  protocol TEXT NOT NULL DEFAULT '',
  status INTEGER NOT NULL DEFAULT 0,
  client_ip TEXT NOT NULL DEFAULT '',
  upstream_addr TEXT NOT NULL DEFAULT '',
  upstream_status TEXT NOT NULL DEFAULT '',
  request_time REAL NOT NULL DEFAULT 0,
  upstream_connect_time REAL,
  upstream_header_time REAL,
  upstream_response_time REAL,
  bytes_sent INTEGER NOT NULL DEFAULT 0,
  bytes_received INTEGER NOT NULL DEFAULT 0,
  user_agent TEXT NOT NULL DEFAULT '',
  referer TEXT NOT NULL DEFAULT '',
  request_id TEXT NOT NULL DEFAULT '',
  ssl_protocol TEXT NOT NULL DEFAULT '',
  extra TEXT NOT NULL DEFAULT '{}'          -- JSON: selected headers, stream id, …
);
CREATE INDEX access_log_ts ON access_log(ts);
CREATE INDEX access_log_host ON access_log(host, ts);
CREATE INDEX access_log_client ON access_log(client_ip, ts);
CREATE INDEX access_log_status ON access_log(status, ts);

CREATE TABLE error_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ts TEXT NOT NULL,
  source TEXT NOT NULL,                     -- nginx | haproxy | relay | acme
  level TEXT NOT NULL,
  message TEXT NOT NULL
);
CREATE INDEX error_log_ts ON error_log(ts);

CREATE TABLE metrics_minute (
  minute INTEGER NOT NULL,                  -- unix seconds / 60
  host TEXT NOT NULL,                       -- host id, '' = all, 'stream:<id>' for streams
  requests INTEGER NOT NULL DEFAULT 0,
  s2xx INTEGER NOT NULL DEFAULT 0,
  s3xx INTEGER NOT NULL DEFAULT 0,
  s4xx INTEGER NOT NULL DEFAULT 0,
  s5xx INTEGER NOT NULL DEFAULT 0,
  bytes_out INTEGER NOT NULL DEFAULT 0,
  bytes_in INTEGER NOT NULL DEFAULT 0,
  latency_hist TEXT NOT NULL DEFAULT '[]',  -- JSON bucket counts
  PRIMARY KEY (minute, host)
);

CREATE TABLE lb_samples (
  at INTEGER NOT NULL,                      -- unix seconds
  backend TEXT NOT NULL,
  sess_rate INTEGER NOT NULL,
  errors INTEGER NOT NULL,
  queue INTEGER NOT NULL,
  PRIMARY KEY (at, backend)
);

CREATE TABLE health_checks (
  target TEXT PRIMARY KEY,                  -- host:<id> | server:<backend>/<server> | stream:<id>
  status TEXT NOT NULL,                     -- healthy | degraded | down | disabled | unknown
  latency_ms INTEGER NOT NULL DEFAULT 0,
  detail TEXT NOT NULL DEFAULT '',
  checked_at TEXT NOT NULL,
  changed_at TEXT NOT NULL
);

-- Ops ---------------------------------------------------------------------------
CREATE TABLE backups (
  id TEXT PRIMARY KEY,
  created_at TEXT NOT NULL,
  size INTEGER NOT NULL DEFAULT 0,
  contents TEXT NOT NULL DEFAULT '{}',      -- JSON counts
  trigger TEXT NOT NULL,                    -- scheduled | manual | before-upgrade | before-restore
  file TEXT NOT NULL,
  status TEXT NOT NULL,                     -- ok | failed | running
  error TEXT NOT NULL DEFAULT ''
);

CREATE TABLE notification_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  at TEXT NOT NULL,
  event TEXT NOT NULL,
  channel_id TEXT NOT NULL,
  status TEXT NOT NULL,                     -- sent | failed | queued
  title TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT ''
);
