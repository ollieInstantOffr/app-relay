-- Auth slice: lookup indexes for sessions, login throttling, tokens and passkeys.
CREATE INDEX IF NOT EXISTS sessions_expires ON sessions(expires_at);
CREATE INDEX IF NOT EXISTS login_attempts_pair ON login_attempts(username, ip, at);
CREATE INDEX IF NOT EXISTS login_attempts_at ON login_attempts(at);
CREATE INDEX IF NOT EXISTS api_tokens_created_by ON api_tokens(created_by);
CREATE INDEX IF NOT EXISTS webauthn_credentials_user ON webauthn_credentials(user_id);
