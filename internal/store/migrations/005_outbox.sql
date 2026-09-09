-- migration005: outbox + draft attachments
ALTER TABLE messages ADD COLUMN status TEXT NOT NULL DEFAULT 'sent';
ALTER TABLE messages ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE messages ADD COLUMN last_error TEXT NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN next_attempt_at TEXT NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN bcc_json TEXT NOT NULL DEFAULT '[]';
ALTER TABLE messages ADD COLUMN idem_key TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_messages_outbox ON messages(status, next_attempt_at);

CREATE TABLE IF NOT EXISTS draft_attachments (
  id TEXT PRIMARY KEY,
  draft_id TEXT NOT NULL REFERENCES drafts(id) ON DELETE CASCADE,
  filename TEXT NOT NULL DEFAULT '',
  content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
  size_bytes INTEGER NOT NULL DEFAULT 0,
  raw_path TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_draft_attachments_draft ON draft_attachments(draft_id);
