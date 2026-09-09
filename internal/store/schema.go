package store

const migration001 = `PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS accounts (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  storage_quota_bytes INTEGER NOT NULL,
  storage_used_bytes INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS users (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  email TEXT NOT NULL COLLATE NOCASE,
  password_hash TEXT NOT NULL,
  is_admin INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  UNIQUE(account_id, email),
  UNIQUE(email)
);

CREATE TABLE IF NOT EXISTS sessions (
  id_hash TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  csrf_token TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);

CREATE TABLE IF NOT EXISTS domains (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  name TEXT NOT NULL COLLATE NOCASE,
  catch_all_inbox_id TEXT,
  created_at TEXT NOT NULL,
  UNIQUE(account_id, name),
  UNIQUE(name)
);

CREATE TABLE IF NOT EXISTS outbound_credentials (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  provider TEXT NOT NULL,
  encrypted_config TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS inboxes (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  domain_id TEXT NOT NULL REFERENCES domains(id) ON DELETE CASCADE,
  local_part TEXT NOT NULL COLLATE NOCASE,
  display_name TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1,
  outbound_credential_id TEXT REFERENCES outbound_credentials(id) ON DELETE SET NULL,
  created_at TEXT NOT NULL,
  UNIQUE(domain_id, local_part)
);
CREATE INDEX IF NOT EXISTS idx_inboxes_account ON inboxes(account_id);


CREATE TABLE IF NOT EXISTS threads (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  subject TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_threads_inbox_updated ON threads(inbox_id, updated_at DESC);

CREATE TABLE IF NOT EXISTS messages (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  thread_id TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
  direction TEXT NOT NULL CHECK(direction IN ('inbound','outbound')),
  provider TEXT NOT NULL DEFAULT '',
  provider_delivery_id TEXT,
  provider_message_id TEXT NOT NULL DEFAULT '',
  rfc_message_id TEXT NOT NULL DEFAULT '',
  in_reply_to TEXT NOT NULL DEFAULT '',
  references_json TEXT NOT NULL DEFAULT '[]',
  from_name TEXT NOT NULL DEFAULT '',
  from_address TEXT NOT NULL DEFAULT '',
  to_json TEXT NOT NULL DEFAULT '[]',
  cc_json TEXT NOT NULL DEFAULT '[]',
  envelope_to_json TEXT NOT NULL DEFAULT '[]',
  subject TEXT NOT NULL DEFAULT '',
  text_body TEXT NOT NULL DEFAULT '',
  html_body TEXT NOT NULL DEFAULT '',
  raw_path TEXT NOT NULL DEFAULT '',
  size_bytes INTEGER NOT NULL DEFAULT 0,
  is_read INTEGER NOT NULL DEFAULT 0,
  is_archived INTEGER NOT NULL DEFAULT 0,
  received_at TEXT,
  sent_at TEXT,
  created_at TEXT NOT NULL,
  UNIQUE(provider, provider_delivery_id)
);
CREATE INDEX IF NOT EXISTS idx_messages_inbox_created ON messages(inbox_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_messages_thread_created ON messages(thread_id, created_at);
CREATE INDEX IF NOT EXISTS idx_messages_rfc_thread ON messages(account_id, inbox_id, rfc_message_id);

CREATE TABLE IF NOT EXISTS attachments (
  id TEXT PRIMARY KEY,
  message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  filename TEXT NOT NULL DEFAULT '',
  content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
  size_bytes INTEGER NOT NULL DEFAULT 0,
  part_index INTEGER NOT NULL,
  content_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_attachments_message ON attachments(message_id);

CREATE VIRTUAL TABLE IF NOT EXISTS message_fts USING fts5(
  message_id UNINDEXED,
  account_id UNINDEXED,
  inbox_id UNINDEXED,
  subject,
  from_address,
  recipients,
  body,
  attachment_names
);

CREATE TABLE IF NOT EXISTS drafts (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  reply_to_message_id TEXT NOT NULL DEFAULT '',
  to_json TEXT NOT NULL DEFAULT '[]',
  cc_json TEXT NOT NULL DEFAULT '[]',
  bcc_json TEXT NOT NULL DEFAULT '[]',
  subject TEXT NOT NULL DEFAULT '',
  text_body TEXT NOT NULL DEFAULT '',
  html_body TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_drafts_inbox ON drafts(inbox_id, updated_at DESC);

CREATE TABLE IF NOT EXISTS api_keys (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  key_prefix TEXT NOT NULL,
  key_hash TEXT NOT NULL UNIQUE,
  is_admin INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  last_used_at TEXT,
  revoked_at TEXT
);

CREATE TABLE IF NOT EXISTS api_key_mailbox_roles (
  api_key_id TEXT NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  role TEXT NOT NULL CHECK(role IN ('read','assistant','owner')),
  PRIMARY KEY(api_key_id, inbox_id)
);

CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  inbox_id TEXT,
  type TEXT NOT NULL,
  entity_id TEXT NOT NULL DEFAULT '',
  payload_json TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_account_id ON events(account_id, id);
CREATE INDEX IF NOT EXISTS idx_events_inbox_id ON events(inbox_id, id);

CREATE TABLE IF NOT EXISTS outbound_idempotency (
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  idem_key TEXT NOT NULL,
  message_id TEXT NOT NULL,
  result_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY(account_id, idem_key)
);

CREATE TABLE IF NOT EXISTS hermes_connections (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  gateway_id TEXT NOT NULL UNIQUE,
  secret_encrypted TEXT NOT NULL,
  delivery_key_encrypted TEXT NOT NULL,
  last_ack_event_id INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  last_connected_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_hermes_inbox ON hermes_connections(inbox_id);

CREATE TABLE IF NOT EXISTS hermes_enroll_tokens (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  token_hash TEXT NOT NULL UNIQUE,
  expires_at TEXT NOT NULL,
  used_at TEXT,
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS audit_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id TEXT,
  kind TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS schema_migrations (
  version TEXT PRIMARY KEY,
  applied_at TEXT NOT NULL
);
`

const migration002 = `ALTER TABLE accounts ADD COLUMN active_outbound_credential_id TEXT REFERENCES outbound_credentials(id) ON DELETE SET NULL;`

const migration003 = `ALTER TABLE inboxes ADD COLUMN allowed_senders_json TEXT NOT NULL DEFAULT '[]';

CREATE TABLE IF NOT EXISTS blocked_messages (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  provider TEXT NOT NULL DEFAULT '',
  provider_delivery_id TEXT,
  from_name TEXT NOT NULL DEFAULT '',
  from_address TEXT NOT NULL DEFAULT '',
  to_json TEXT NOT NULL DEFAULT '[]',
  subject TEXT NOT NULL DEFAULT '',
  size_bytes INTEGER NOT NULL DEFAULT 0,
  reason TEXT NOT NULL DEFAULT '',
  received_at TEXT,
  created_at TEXT NOT NULL,
  UNIQUE(provider, provider_delivery_id)
);
CREATE INDEX IF NOT EXISTS idx_blocked_messages_inbox_created ON blocked_messages(inbox_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_blocked_messages_account_created ON blocked_messages(account_id, created_at DESC);
`

// migration004 adds a status column to outbound_idempotency so a reservation
// can be claimed atomically before the provider send, closing the race where
// two concurrent requests with the same key both send.
const migration004 = `ALTER TABLE outbound_idempotency ADD COLUMN status TEXT NOT NULL DEFAULT 'done';
CREATE INDEX IF NOT EXISTS idx_outbound_idem_status ON outbound_idempotency(account_id, idem_key, status);
`

// migration005 adds an outbox to messages: a status column (pending/sent/failed)
// plus retry bookkeeping, and a draft_attachments table so drafts can hold
// attachments that carry over to the sent message. Existing messages are
// backfilled to 'sent' (they were delivered synchronously before the outbox).
const migration005 = `ALTER TABLE messages ADD COLUMN status TEXT NOT NULL DEFAULT 'sent';
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
`
