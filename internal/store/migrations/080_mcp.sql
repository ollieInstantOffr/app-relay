-- MCP approvals: the requesting actor (so an approval can be executed after the
-- client disconnected or Relay restarted), a short target label for toasts, and
-- the tool outcome (JSON {text, structured, isError}) once executed.
ALTER TABLE approvals ADD COLUMN actor TEXT NOT NULL DEFAULT '{}';
ALTER TABLE approvals ADD COLUMN target TEXT NOT NULL DEFAULT '';
ALTER TABLE approvals ADD COLUMN output TEXT NOT NULL DEFAULT '';
CREATE INDEX approvals_expiry ON approvals(status, expires_at);
