-- Relay login: sessions of the built-in login page that protects apps.
-- Separate from admin UI sessions; the cookie is scoped to the app's domain.
CREATE TABLE IF NOT EXISTS portal_sessions (
    id           TEXT PRIMARY KEY, -- sha256 (hex) of the cookie token
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    domain       TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    expires_at   TEXT NOT NULL,
    last_seen_at TEXT NOT NULL,
    ip           TEXT NOT NULL DEFAULT '',
    user_agent   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS portal_sessions_user ON portal_sessions (user_id);
CREATE INDEX IF NOT EXISTS portal_sessions_expires ON portal_sessions (expires_at);
