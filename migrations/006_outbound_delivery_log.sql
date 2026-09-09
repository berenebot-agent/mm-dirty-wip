-- migration006: per-attempt outbound delivery log
CREATE TABLE IF NOT EXISTS outbound_delivery_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  credential_id TEXT REFERENCES outbound_credentials(id) ON DELETE SET NULL,
  provider TEXT NOT NULL DEFAULT '',
  message_id TEXT REFERENCES messages(id) ON DELETE SET NULL,
  attempt INTEGER NOT NULL DEFAULT 1,
  status TEXT NOT NULL CHECK(status IN ('sent','failed')),
  provider_message_id TEXT NOT NULL DEFAULT '',
  error_text TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_outbound_log_cred ON outbound_delivery_log(account_id, credential_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_outbound_log_msg ON outbound_delivery_log(message_id);
