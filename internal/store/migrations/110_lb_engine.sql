-- Relay Balancer: the load balancer engine a config version was applied with
-- (haproxy | balancer). haproxy_cfg / haproxy_hash / haproxy_running keep
-- holding the active load balancer engine's main file, hash and run state.
ALTER TABLE config_versions ADD COLUMN lb_engine TEXT NOT NULL DEFAULT 'haproxy';
