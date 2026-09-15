-- Off-site copies of backups (S3 and S3-compatible storage).
ALTER TABLE backups ADD COLUMN remote_status TEXT NOT NULL DEFAULT ''; -- '' | uploading | uploaded | failed
ALTER TABLE backups ADD COLUMN remote_key TEXT NOT NULL DEFAULT '';
ALTER TABLE backups ADD COLUMN remote_error TEXT NOT NULL DEFAULT '';
