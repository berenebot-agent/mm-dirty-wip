-- Test-only reconstruction of the exact post-080 schema from the public baseline.

PRAGMA foreign_keys=OFF;

DROP TABLE inboxes;

CREATE TABLE "inboxes" (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  domain_id TEXT REFERENCES domains(id) ON DELETE CASCADE,
  local_part TEXT NOT NULL COLLATE NOCASE,
  address TEXT NOT NULL DEFAULT '',
  display_name TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1,
  kind TEXT NOT NULL DEFAULT 'domain',
  namespace TEXT NOT NULL DEFAULT '',
  allowed_senders_json TEXT NOT NULL DEFAULT '[]',
  sender_restricted INTEGER NOT NULL DEFAULT 0,
  require_authenticated INTEGER NOT NULL DEFAULT 0,
  approver_email TEXT NOT NULL DEFAULT '',
  default_sender TEXT NOT NULL DEFAULT '',
  trash_retention_days INTEGER,
  storage_quota_bytes INTEGER,
  storage_used_bytes INTEGER,
  auto_mark_read_on_delivery INTEGER NOT NULL DEFAULT 0,
  auto_trash_after_delivery_hours INTEGER,
  delivery_trigger TEXT NOT NULL DEFAULT 'all' CHECK(delivery_trigger IN ('any','all')),
  remote_host TEXT NOT NULL DEFAULT '',
  remote_port INTEGER NOT NULL DEFAULT 0,
  remote_username TEXT NOT NULL DEFAULT '',
  remote_security TEXT NOT NULL DEFAULT '',
  smtp_host TEXT NOT NULL DEFAULT '',
  smtp_port INTEGER NOT NULL DEFAULT 0,
  smtp_username TEXT NOT NULL DEFAULT '',
  smtp_security TEXT NOT NULL DEFAULT '',
  remote_configured INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL, authoring_mode TEXT NOT NULL DEFAULT '', notify_default_address TEXT NOT NULL DEFAULT '', remote_index_status TEXT NOT NULL DEFAULT '', remote_indexed_at TEXT NOT NULL DEFAULT '', remote_index_error TEXT NOT NULL DEFAULT '', remote_notify_baseline_set INTEGER NOT NULL DEFAULT 0, remote_sent_copy_enabled INTEGER NOT NULL DEFAULT 1, remote_sent_copy_folder TEXT NOT NULL DEFAULT '', remote_poll_seconds INTEGER, remote_full_sync_minutes INTEGER,
  UNIQUE(domain_id, local_part)
);

CREATE INDEX idx_inboxes_account ON inboxes(account_id);

CREATE INDEX idx_inboxes_authoring_mode ON inboxes(account_id, authoring_mode);

CREATE UNIQUE INDEX idx_inboxes_id_account ON inboxes(id,account_id);

CREATE UNIQUE INDEX idx_inboxes_standalone_address
  ON inboxes(account_id, address COLLATE NOCASE) WHERE kind='standalone' AND address<>'';

DROP TABLE assistant_handling_requests;

CREATE TABLE assistant_handling_requests (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  draft_id TEXT NOT NULL,
  mode TEXT NOT NULL,
  content_hash TEXT NOT NULL DEFAULT '',
  handoff_id TEXT NOT NULL DEFAULT '',
  message_id TEXT NOT NULL DEFAULT '',
  remote_folder TEXT NOT NULL DEFAULT '',
  raw_path TEXT NOT NULL DEFAULT '',
  size_bytes INTEGER NOT NULL DEFAULT 0,
  publication TEXT NOT NULL DEFAULT 'pending',
  remote_uid INTEGER NOT NULL DEFAULT 0,
  notification_status TEXT NOT NULL DEFAULT 'none',
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  requested_at TEXT NOT NULL,
  published_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
, notification_message_id TEXT NOT NULL DEFAULT '');

CREATE INDEX idx_assistant_handling_draft ON assistant_handling_requests(account_id, draft_id);

CREATE UNIQUE INDEX idx_assistant_handling_handoff ON assistant_handling_requests(account_id, handoff_id) WHERE handoff_id <> '';

CREATE INDEX idx_assistant_handling_inbox ON assistant_handling_requests(account_id, inbox_id, requested_at DESC);

CREATE INDEX idx_assistant_handling_pending ON assistant_handling_requests(publication, requested_at);

CREATE TABLE api_keys (
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

CREATE TABLE api_key_mailbox_roles (
  api_key_id TEXT NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  role TEXT NOT NULL CHECK(role IN ('read','assistant','owner')),
  PRIMARY KEY(api_key_id, inbox_id)
);

CREATE TABLE hermes_connections (
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
, outbound_role TEXT NOT NULL DEFAULT 'owner');

CREATE INDEX idx_hermes_inbox ON hermes_connections(inbox_id);

DELETE FROM schema_migrations;

PRAGMA user_version=0;

PRAGMA foreign_keys=ON;
