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

// migration006 adds a per-attempt outbound delivery log. Each provider send
// (success or failure) appends an immutable row linked to the message, so an
// operator can see the full retry history for a provider. Rows are pruned to
// the newest 5000 per account or 30 days, whichever is more recent.
const migration006 = `CREATE TABLE IF NOT EXISTS outbound_delivery_log (
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
`

// migration007 lets a domain designate its own outbound credential. It
// originally resolved the domain credential first and fell back to the
// account's active credential; migration008 removes that fallback. A domain
// with no credential queues mail until a provider is assigned.
const migration007 = `ALTER TABLE domains ADD COLUMN outbound_credential_id TEXT REFERENCES outbound_credentials(id) ON DELETE SET NULL;
`

// migration008 removes the account-level default provider. A domain may only
// send through its own outbound credential; a domain with none queues mail.
// SQLite cannot DROP a column that carries a foreign key, so the accounts table
// is rebuilt. No data is backfilled: existing domains start with no provider
// and pause sending until one is assigned.
// The transaction and foreign_keys pragma are owned by the migration runner
// (internal/store/migrate.go); this constant contains only the schema work.
const migration008 = `CREATE TABLE accounts_new (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  storage_quota_bytes INTEGER NOT NULL,
  storage_used_bytes INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL
);
INSERT INTO accounts_new(id,name,storage_quota_bytes,storage_used_bytes,created_at)
  SELECT id,name,storage_quota_bytes,storage_used_bytes,created_at FROM accounts;
DROP TABLE accounts;
ALTER TABLE accounts_new RENAME TO accounts;
`

// migration009 moves inbound credentials from process environment into
// account-owned encrypted rows and scopes inbound delivery identity. It:
//   - adds inbound_credentials (multiple per account, one assigned per domain
//     via domains.inbound_credential_id);
//   - rebuilds messages and blocked_messages to store the canonical original
//     envelope recipient and to key deduplication on
//     (account_id, provider, envelope_recipient, provider_delivery_id).
//
// SQLite cannot ALTER a UNIQUE constraint, so both tables are rebuilt. The
// original recipient is backfilled from messages.envelope_to_json where it was
// recorded; legacy blocked rows never stored it and are left empty rather than
// guessing a catch-all address.
const migration009 = `CREATE TABLE IF NOT EXISTS inbound_credentials (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  provider TEXT NOT NULL,
  name TEXT NOT NULL,
  encrypted_config TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_inbound_credentials_account ON inbound_credentials(account_id);
ALTER TABLE domains ADD COLUMN inbound_credential_id TEXT REFERENCES inbound_credentials(id) ON DELETE SET NULL;

CREATE TABLE messages_new (
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
  envelope_recipient TEXT NOT NULL DEFAULT '',
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
  status TEXT NOT NULL DEFAULT 'sent',
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  next_attempt_at TEXT NOT NULL DEFAULT '',
  bcc_json TEXT NOT NULL DEFAULT '[]',
  idem_key TEXT NOT NULL DEFAULT '',
  UNIQUE(account_id, provider, envelope_recipient, provider_delivery_id)
);
INSERT INTO messages_new(id,account_id,inbox_id,thread_id,direction,provider,provider_delivery_id,provider_message_id,rfc_message_id,in_reply_to,references_json,from_name,from_address,to_json,cc_json,envelope_to_json,envelope_recipient,subject,text_body,html_body,raw_path,size_bytes,is_read,is_archived,received_at,sent_at,created_at,status,attempts,last_error,next_attempt_at,bcc_json,idem_key)
  SELECT id,account_id,inbox_id,thread_id,direction,provider,provider_delivery_id,provider_message_id,rfc_message_id,in_reply_to,references_json,from_name,from_address,to_json,cc_json,envelope_to_json,COALESCE(json_extract(envelope_to_json,'$[0]'),''),subject,text_body,html_body,raw_path,size_bytes,is_read,is_archived,received_at,sent_at,created_at,status,attempts,last_error,next_attempt_at,bcc_json,idem_key FROM messages;
DROP TABLE messages;
ALTER TABLE messages_new RENAME TO messages;
CREATE INDEX IF NOT EXISTS idx_messages_inbox_created ON messages(inbox_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_messages_thread_created ON messages(thread_id, created_at);
CREATE INDEX IF NOT EXISTS idx_messages_rfc_thread ON messages(account_id, inbox_id, rfc_message_id);
CREATE INDEX IF NOT EXISTS idx_messages_outbox ON messages(status, next_attempt_at);

CREATE TABLE blocked_messages_new (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  provider TEXT NOT NULL DEFAULT '',
  provider_delivery_id TEXT,
  from_name TEXT NOT NULL DEFAULT '',
  from_address TEXT NOT NULL DEFAULT '',
  to_json TEXT NOT NULL DEFAULT '[]',
  envelope_recipient TEXT NOT NULL DEFAULT '',
  subject TEXT NOT NULL DEFAULT '',
  size_bytes INTEGER NOT NULL DEFAULT 0,
  reason TEXT NOT NULL DEFAULT '',
  received_at TEXT,
  created_at TEXT NOT NULL,
  UNIQUE(account_id, provider, envelope_recipient, provider_delivery_id)
);
INSERT INTO blocked_messages_new(id,account_id,inbox_id,provider,provider_delivery_id,from_name,from_address,to_json,envelope_recipient,subject,size_bytes,reason,received_at,created_at)
  SELECT id,account_id,inbox_id,provider,provider_delivery_id,from_name,from_address,to_json,'',subject,size_bytes,reason,received_at,created_at FROM blocked_messages;
DROP TABLE blocked_messages;
ALTER TABLE blocked_messages_new RENAME TO blocked_messages;
CREATE INDEX IF NOT EXISTS idx_blocked_messages_inbox_created ON blocked_messages(inbox_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_blocked_messages_account_created ON blocked_messages(account_id, created_at DESC);
`

// migration010 scopes outbound idempotency keys to the mailbox they were used
// for, so a key replayed against a different mailbox in the same account is
// rejected. Existing rows are reconciled by reconcileIdempotency.
const migration010 = `ALTER TABLE outbound_idempotency ADD COLUMN inbox_id TEXT NOT NULL DEFAULT '';`

// migration011 separates an outbound claim (a worker's temporary ownership of
// a pending message) from retry scheduling, so a crash no longer parks a
// message for 24 hours. Claims carry an explicit owner and lease expiry.
const migration011 = `ALTER TABLE messages ADD COLUMN claim_owner TEXT NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN claim_expires_at TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_messages_claim ON messages(status, claim_expires_at);
`

// migration013 replaces the shared credential pools with one optional sending
// and one optional receiving configuration per domain. It runs inside the
// runner's foreign_keys=OFF transaction because it rebuilds domains, inboxes
// and the delivery log.
//
//   - domain_sending_configs / domain_receiving_configs each hold one row per
//     domain, keyed by a composite (domain_id, account_id) foreign key that
//     cascades with the domain. The old assigned credential rows are copied
//     per domain so a credential shared by several domains becomes independent
//     copies; unused credentials are dropped with their tables.
//   - domains drops the outbound_credential_id/inbound_credential_id
//     assignment columns; a unique (id, account_id) index backs the composite
//     config foreign keys.
//   - inboxes drops the retired outbound_credential_id foreign key while
//     preserving every other column, including allowed_senders_json.
//   - outbound_delivery_log replaces credential_id with a nullable domain_id
//     (SET NULL, no config foreign key) backfilled from the attempt's
//     message+inbox; attempts with no surviving message keep a NULL domain and
//     every attempt is retained.
//
// The baseline is gated so it cannot recreate the dropped credential tables;
// see bootstrapBaseline in store.go.
const migration013 = `
CREATE TABLE domain_sending_configs (
  id TEXT PRIMARY KEY,
  domain_id TEXT NOT NULL,
  account_id TEXT NOT NULL,
  provider TEXT NOT NULL,
  encrypted_config TEXT NOT NULL,
  revision INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(domain_id),
  FOREIGN KEY(domain_id,account_id) REFERENCES domains(id,account_id) ON DELETE CASCADE
);
CREATE TABLE domain_receiving_configs (
  id TEXT PRIMARY KEY,
  domain_id TEXT NOT NULL,
  account_id TEXT NOT NULL,
  provider TEXT NOT NULL,
  encrypted_config TEXT NOT NULL,
  revision INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(domain_id),
  FOREIGN KEY(domain_id,account_id) REFERENCES domains(id,account_id) ON DELETE CASCADE
);

INSERT INTO domain_sending_configs(id,domain_id,account_id,provider,encrypted_config,revision,created_at,updated_at)
  SELECT 'dsc_'||lower(hex(randomblob(16))), d.id, d.account_id, c.provider, c.encrypted_config, 1, c.created_at, c.updated_at
  FROM domains d JOIN outbound_credentials c ON c.id=d.outbound_credential_id
  WHERE d.outbound_credential_id IS NOT NULL;
INSERT INTO domain_receiving_configs(id,domain_id,account_id,provider,encrypted_config,revision,created_at,updated_at)
  SELECT 'drc_'||lower(hex(randomblob(16))), d.id, d.account_id, c.provider, c.encrypted_config, 1, c.created_at, c.updated_at
  FROM domains d JOIN inbound_credentials c ON c.id=d.inbound_credential_id
  WHERE d.inbound_credential_id IS NOT NULL;

CREATE TABLE outbound_delivery_log_new (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  domain_id TEXT REFERENCES domains(id) ON DELETE SET NULL,
  provider TEXT NOT NULL DEFAULT '',
  message_id TEXT REFERENCES messages(id) ON DELETE SET NULL,
  attempt INTEGER NOT NULL DEFAULT 1,
  status TEXT NOT NULL CHECK(status IN ('sent','failed')),
  provider_message_id TEXT NOT NULL DEFAULT '',
  error_text TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
INSERT INTO outbound_delivery_log_new(id,account_id,domain_id,provider,message_id,attempt,status,provider_message_id,error_text,created_at)
  SELECT l.id, l.account_id,
    (SELECT i.domain_id FROM messages m JOIN inboxes i ON i.id=m.inbox_id WHERE m.id=l.message_id AND m.account_id=l.account_id),
    l.provider, l.message_id, l.attempt, l.status, l.provider_message_id, l.error_text, l.created_at
  FROM outbound_delivery_log l;
DROP TABLE outbound_delivery_log;
ALTER TABLE outbound_delivery_log_new RENAME TO outbound_delivery_log;

CREATE TABLE inboxes_new (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  domain_id TEXT NOT NULL REFERENCES domains(id) ON DELETE CASCADE,
  local_part TEXT NOT NULL COLLATE NOCASE,
  display_name TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1,
  allowed_senders_json TEXT NOT NULL DEFAULT '[]',
  created_at TEXT NOT NULL,
  UNIQUE(domain_id, local_part)
);
INSERT INTO inboxes_new(id,account_id,domain_id,local_part,display_name,enabled,allowed_senders_json,created_at)
  SELECT id,account_id,domain_id,local_part,display_name,enabled,allowed_senders_json,created_at FROM inboxes;
DROP TABLE inboxes;
ALTER TABLE inboxes_new RENAME TO inboxes;

CREATE TABLE domains_new (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  name TEXT NOT NULL COLLATE NOCASE,
  catch_all_inbox_id TEXT,
  created_at TEXT NOT NULL,
  UNIQUE(account_id, name),
  UNIQUE(name)
);
INSERT INTO domains_new(id,account_id,name,catch_all_inbox_id,created_at)
  SELECT id,account_id,name,catch_all_inbox_id,created_at FROM domains;
DROP TABLE domains;
ALTER TABLE domains_new RENAME TO domains;
CREATE UNIQUE INDEX idx_domains_id_account ON domains(id,account_id);

DROP TABLE outbound_credentials;
DROP TABLE inbound_credentials;

CREATE INDEX idx_inboxes_account ON inboxes(account_id);
CREATE INDEX idx_outbound_log_domain ON outbound_delivery_log(account_id, domain_id, id DESC);
CREATE INDEX idx_outbound_log_msg ON outbound_delivery_log(message_id);
`

// migration014 adds the human-in-the-loop draft send workflow. It:
//   - adds drafts.status (draft|pending_approval|rejected) for the live state of
//     an unsent draft;
//   - adds draft_send_requests, the durable record of an assistant's request
//     that a draft be authorized and sent, the human decision, and the delivery
//     outcome.
//
// draft_send_requests.draft_id deliberately has no foreign key: the draft row is
// consumed (deleted) in the same transaction that enqueues the approved send,
// but the request must survive to report the terminal outcome. The account and
// inbox foreign keys cascade, so purging an account or inbox still removes the
// requests. A partial unique index allows only one pending request per draft,
// which is what makes a decision single-use.
const migration014 = `ALTER TABLE drafts ADD COLUMN status TEXT NOT NULL DEFAULT 'draft';

CREATE TABLE IF NOT EXISTS draft_send_requests (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  draft_id TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  delivery_status TEXT NOT NULL DEFAULT 'none',
  content_hash TEXT NOT NULL DEFAULT '',
  requested_at TEXT NOT NULL,
  requested_by TEXT NOT NULL DEFAULT '',
  requested_by_api_key_id TEXT NOT NULL DEFAULT '',
  requested_by_user_id TEXT NOT NULL DEFAULT '',
  decided_at TEXT,
  decision_actor TEXT NOT NULL DEFAULT '',
  decision_actor_id TEXT NOT NULL DEFAULT '',
  decision_method TEXT NOT NULL DEFAULT '',
  feedback TEXT NOT NULL DEFAULT '',
  message_id TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_send_requests_active_draft ON draft_send_requests(draft_id) WHERE status='pending';
CREATE INDEX IF NOT EXISTS idx_send_requests_inbox ON draft_send_requests(inbox_id, status, requested_at DESC);
CREATE INDEX IF NOT EXISTS idx_send_requests_draft ON draft_send_requests(draft_id, requested_at DESC);
CREATE INDEX IF NOT EXISTS idx_send_requests_message ON draft_send_requests(message_id);
`
