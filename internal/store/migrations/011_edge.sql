-- Engine slice: the proxy engine (nginx | edge) a config version was applied
-- with. Its files are stored in nginx_files / nginx_hash.
ALTER TABLE config_versions ADD COLUMN proxy_engine TEXT NOT NULL DEFAULT 'nginx';
