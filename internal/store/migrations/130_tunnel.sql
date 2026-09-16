-- Tunnel gateways (runtime state like certificates; not part of config
-- versions). Hosts and streams reference them with tunnelGatewayId.
CREATE TABLE IF NOT EXISTS gateways (id TEXT PRIMARY KEY, data TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);

-- The tunnel engine's release per config version (tunnel.json, no secrets),
-- its hash and whether it runs (something is published through a tunnel).
ALTER TABLE config_versions ADD COLUMN tunnel_files TEXT NOT NULL DEFAULT '';
ALTER TABLE config_versions ADD COLUMN tunnel_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE config_versions ADD COLUMN tunnel_running INTEGER NOT NULL DEFAULT 0;
