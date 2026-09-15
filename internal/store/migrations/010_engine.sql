-- Engine slice: extra apply metadata on config versions.
ALTER TABLE config_versions ADD COLUMN nginx_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE config_versions ADD COLUMN haproxy_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE config_versions ADD COLUMN haproxy_running INTEGER NOT NULL DEFAULT 0;
ALTER TABLE config_versions ADD COLUMN failed_engine TEXT NOT NULL DEFAULT '';  -- nginx | haproxy
ALTER TABLE config_versions ADD COLUMN failed_stage TEXT NOT NULL DEFAULT '';   -- render | validate | swap | reload | start | health
ALTER TABLE config_versions ADD COLUMN output TEXT NOT NULL DEFAULT '';         -- engine output (validate warnings, ALERT lines)
CREATE INDEX config_versions_status ON config_versions(status);
