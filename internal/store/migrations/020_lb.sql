-- Load balancer: per-minute aggregates of HAProxy runtime counters.
-- backend = haproxy backend name; '' = all backends combined.
-- Counters are deltas within the minute (reset-safe), sessions are rates.
CREATE TABLE lb_minute (
  at INTEGER NOT NULL,                      -- unix seconds, start of the minute
  backend TEXT NOT NULL,
  sess_avg INTEGER NOT NULL DEFAULT 0,
  sess_max INTEGER NOT NULL DEFAULT 0,
  econ INTEGER NOT NULL DEFAULT 0,
  eresp INTEGER NOT NULL DEFAULT 0,
  ereq INTEGER NOT NULL DEFAULT 0,
  wretr INTEGER NOT NULL DEFAULT 0,
  wredis INTEGER NOT NULL DEFAULT 0,
  queue_max INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (at, backend)
);
CREATE INDEX lb_minute_backend ON lb_minute(backend, at);
