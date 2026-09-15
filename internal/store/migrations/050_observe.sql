-- Observe slice: indexes for per-host log queries and metrics series, and the
-- HTTP status of the last health probe.
CREATE INDEX IF NOT EXISTS access_log_hostid ON access_log(host_id, ts);
CREATE INDEX IF NOT EXISTS error_log_source ON error_log(source, ts);
CREATE INDEX IF NOT EXISTS metrics_minute_host ON metrics_minute(host, minute);
ALTER TABLE health_checks ADD COLUMN http_status INTEGER NOT NULL DEFAULT 0;
