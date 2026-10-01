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
  -- internal marks workflow mail (e.g. an approval-request email carrying a
  -- one-time token) that is queued in an inbox but must never be exposed
  -- through the mailbox read surface.
  internal INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  UNIQUE(provider, provider_delivery_id)
);
CREATE INDEX IF NOT EXISTS idx_messages_inbox_created ON messages(inbox_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_messages_thread_created ON messages(thread_id, created_at);
CREATE INDEX IF NOT EXISTS idx_messages_rfc_thread ON messages(account_id, inbox_id, rfc_message_id);
-- internal marks workflow mail hidden from the mailbox read surface; the read
-- paths filter on it, so give that filter an index on fresh databases too (the
-- migration adds it for existing ones).
CREATE INDEX IF NOT EXISTS idx_messages_internal ON messages(inbox_id, internal);

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

// migration015 adds external email approval on top of the Phase 1 draft send
// workflow. It:
//   - adds the nominated approver, a hashed one-time token and an expiry to
//     draft_send_requests;
//   - adds the optional per-inbox approver to inboxes (the approver is always
//     an accepted inbound sender);
//   - adds a content hash to draft_attachments so the frozen fingerprint binds
//     to attachment bytes, not just metadata;
//   - adds inbound_control_messages, the durable record of a consumed approval
//     control email, which both feeds the per-domain receiving log and is the
//     provider-delivery dedup key.
//
// A send request's status may now also be 'expired'. The content-hash backfill
// runs in Go (the runner's transaction) because it must read attachment files
// from disk.
const migration015 = `ALTER TABLE draft_send_requests ADD COLUMN approver_email TEXT NOT NULL DEFAULT '';
ALTER TABLE draft_send_requests ADD COLUMN token_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE draft_send_requests ADD COLUMN token_expires_at TEXT;
ALTER TABLE draft_send_requests ADD COLUMN approval_message_id TEXT NOT NULL DEFAULT '';

ALTER TABLE inboxes ADD COLUMN approver_email TEXT NOT NULL DEFAULT '';

ALTER TABLE draft_attachments ADD COLUMN content_hash TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS inbound_control_messages (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  provider TEXT NOT NULL DEFAULT '',
  provider_delivery_id TEXT,
  envelope_recipient TEXT NOT NULL DEFAULT '',
  from_name TEXT NOT NULL DEFAULT '',
  from_address TEXT NOT NULL DEFAULT '',
  request_id TEXT NOT NULL DEFAULT '',
  action TEXT NOT NULL DEFAULT '',
  outcome TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  UNIQUE(account_id, provider, envelope_recipient, provider_delivery_id)
);
CREATE INDEX IF NOT EXISTS idx_inbound_control_inbox ON inbound_control_messages(inbox_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_inbound_control_request ON inbound_control_messages(request_id);
`

// migration016 makes the inbox sender allow-list an explicit opt-in. Before
// this, an empty allowed_senders list meant "accept all" and a non-empty list
// meant "restrict", so there was no way to distinguish "restricted with no
// senders" from "open". inboxes.sender_restricted captures that choice; existing
// inboxes with a non-empty list are backfilled to restricted so their behaviour
// is unchanged.
const migration016 = `ALTER TABLE inboxes ADD COLUMN sender_restricted INTEGER NOT NULL DEFAULT 0;
UPDATE inboxes SET sender_restricted=1 WHERE allowed_senders_json IS NOT NULL AND allowed_senders_json NOT IN ('','[]');
`

// migration017 records the client that sent an outbound message: the name of
// the API key / Hermes credential (or the UI marker for a session send), stored
// as a denormalized label so history survives a key rename or deletion, plus
// the stable credential id. Inbound mail has no client. Existing rows keep the
// empty default.
const migration017 = `ALTER TABLE messages ADD COLUMN client_label TEXT NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN client_id TEXT NOT NULL DEFAULT '';
`

// migration018 marks internal workflow mail (currently the draft approval-request
// email that carries a one-time approval token) so it is never exposed through
// the mailbox read surface. Approval mail is addressed to the external approver
// but is queued in the requesting inbox, so without this flag any principal that
// can read the inbox -- including the assistant that requested the send -- could
// read the plaintext token and approve its own request.
const migration018 = `ALTER TABLE messages ADD COLUMN internal INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_messages_internal ON messages(inbox_id, internal);
`

// migration019 snapshots the reviewed draft's subject on a consumed approval
// control message. The control record is what the dashboard and domain log
// display, and the draft is deleted when an approval sends it, so the subject
// must be captured at decision time. The raw inbound subject (which carries the
// one-time token) is never stored or shown.
const migration019 = `ALTER TABLE inbound_control_messages ADD COLUMN subject TEXT NOT NULL DEFAULT '';
`

// migration020 adds free-text message labels ("tags"). There is no label
// catalogue: a label exists only while at least one message carries it, so
// there is no definition lifecycle, namespace or orphan state to manage.
// label is COLLATE NOCASE so "Invoice" and "invoice" are one tag; Go
// normalizes whitespace before insert. Rows cascade with their message.
const migration020 = `CREATE TABLE IF NOT EXISTS message_labels (
  message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  label TEXT NOT NULL COLLATE NOCASE,
  created_at TEXT NOT NULL,
  PRIMARY KEY(message_id, label)
);
CREATE INDEX IF NOT EXISTS idx_message_labels_label ON message_labels(label, message_id);
`

// migration021 adds inbox aliases: alternate inbound addresses that deliver to
// an existing inbox instead of a separate mailbox. An alias is an
// address-to-inbox mapping, not a mailbox: it owns no messages, storage or
// settings. domain_id is the alias's own domain and inbox_id is the delivery
// target, which may live on a different domain in the same account (an
// account-internal routing construct). account_id is denormalized for scoped
// listing. UNIQUE(domain_id, local_part) keeps one address from resolving twice;
// collisions with a real inbox local_part are enforced in Go because SQLite
// cannot express a cross-table uniqueness constraint. Rows cascade with the
// inbox, so deleting a mailbox removes its aliases.
const migration021 = `CREATE TABLE IF NOT EXISTS inbox_aliases (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  domain_id TEXT NOT NULL REFERENCES domains(id) ON DELETE CASCADE,
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  local_part TEXT NOT NULL COLLATE NOCASE,
  created_at TEXT NOT NULL,
  UNIQUE(domain_id, local_part)
);
CREATE INDEX IF NOT EXISTS idx_inbox_aliases_inbox ON inbox_aliases(inbox_id);
CREATE INDEX IF NOT EXISTS idx_inbox_aliases_account ON inbox_aliases(account_id);
`

// migration023 adds the optional MX edge's durable state:
//   - is_spam/auth_results_json/spam_reason on messages. Spam is a computed
//     view over messages.is_spam, not a separate table, so Spam still counts
//     toward quota and retains MIME, attachments and recovery. auth_results_json
//     holds the bounded normalized evidence supplied by the authenticated edge.
//   - mx_receipts, the durable per-recipient delivery receipt that records the
//     successful disposition for a delivery fingerprint. Receipts are scoped by
//     account/provider/envelope recipient and survive message deletion for their
//     retention horizon (7 days), so a retry after the message was deleted still
//     deduplicates. message_id is a plain opaque reference, not a foreign key.
const migration023 = `ALTER TABLE messages ADD COLUMN is_spam INTEGER NOT NULL DEFAULT 0;
ALTER TABLE messages ADD COLUMN auth_results_json TEXT NOT NULL DEFAULT '{}';
ALTER TABLE messages ADD COLUMN spam_reason TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_messages_spam ON messages(inbox_id, is_spam);

CREATE TABLE IF NOT EXISTS mx_receipts (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  provider TEXT NOT NULL DEFAULT 'mx',
  envelope_recipient TEXT NOT NULL COLLATE NOCASE,
  delivery_fingerprint TEXT NOT NULL,
  disposition TEXT NOT NULL DEFAULT '',
  message_id TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  UNIQUE(account_id, provider, envelope_recipient, delivery_fingerprint)
);
CREATE INDEX IF NOT EXISTS idx_mx_receipts_expires ON mx_receipts(expires_at);
`

// migration022 moves workflow mail (the approval-request email carrying a
// one-time token) out of the messages table into its own outbound queue. It:
//   - adds outbound_workflow, a durable queue for non-mailbox system mail with
//     its own status/attempts/claim columns. Workflow mail never becomes a
//     mailbox message: it creates no thread, is invisible to the mailbox read
//     surface, and never counts against account storage quota;
//   - adds draft_send_requests.approval_workflow_id and notification_status so
//     the request can report whether the approver was actually notified
//     (none|queued|sent|failed) before the draft is presented as pending;
//   - adds outbound_delivery_log.workflow_id so a workflow attempt is attributed
//     in the per-domain activity log without a messages row.
//
// Raw MIME for workflow mail is stored under $DATA_DIR/workflow/ and retained
// for a fixed 30 days after the job reaches a terminal state, then swept. Token
// markers are redacted from the retained copy on terminal state. The request
// audit row is never swept.
const migration022 = `CREATE TABLE IF NOT EXISTS outbound_workflow (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  request_id TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL DEFAULT '',
  provider TEXT NOT NULL DEFAULT '',
  from_name TEXT NOT NULL DEFAULT '',
  from_address TEXT NOT NULL DEFAULT '',
  to_json TEXT NOT NULL DEFAULT '[]',
  cc_json TEXT NOT NULL DEFAULT '[]',
  bcc_json TEXT NOT NULL DEFAULT '[]',
  subject TEXT NOT NULL DEFAULT '',
  text_body TEXT NOT NULL DEFAULT '',
  html_body TEXT NOT NULL DEFAULT '',
  raw_path TEXT NOT NULL DEFAULT '',
  size_bytes INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'pending',
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  next_attempt_at TEXT NOT NULL DEFAULT '',
  claim_owner TEXT NOT NULL DEFAULT '',
  claim_expires_at TEXT NOT NULL DEFAULT '',
  provider_message_id TEXT NOT NULL DEFAULT '',
  redacted INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  sent_at TEXT,
  terminal_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_outbound_workflow_status ON outbound_workflow(status, next_attempt_at);
CREATE INDEX IF NOT EXISTS idx_outbound_workflow_request ON outbound_workflow(request_id);

ALTER TABLE draft_send_requests ADD COLUMN approval_workflow_id TEXT NOT NULL DEFAULT '';
ALTER TABLE draft_send_requests ADD COLUMN notification_status TEXT NOT NULL DEFAULT 'none';

ALTER TABLE outbound_delivery_log ADD COLUMN workflow_id TEXT;
CREATE INDEX IF NOT EXISTS idx_outbound_log_workflow ON outbound_delivery_log(workflow_id);
`

// migration024 adds send-as-alias (outbound identity). An inbox may send from
// any address in its alias set, not just its primary address:
//   - inboxes.default_sender is the address compose/reply preselects. Empty
//     means the inbox primary. It is validated in Go against the primary and
//     the inbox's aliases, and cleared when the alias set no longer contains it.
//   - drafts.from_address remembers the chosen sender so an approved send uses
//     it, and it is folded into the approval content fingerprint.
//   - messages.sending_domain_id records the domain whose sending configuration
//     was used at enqueue. For an alias sender this is the alias's own domain
//     (which may differ from the inbox's), so delivery, per-domain log
//     attribution and requeue-on-save resolve the correct provider. NULL falls
//     back to the inbox domain, preserving every pre-024 message.
const migration024 = `ALTER TABLE inboxes ADD COLUMN default_sender TEXT NOT NULL DEFAULT '';
ALTER TABLE drafts ADD COLUMN from_address TEXT NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN sending_domain_id TEXT REFERENCES domains(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_messages_sending_domain ON messages(sending_domain_id);
`

// migration025 gives each alias an optional sender display name, so a role
// address can send with its own From name (for example "Acme Billing
// <billing@example.com>") rather than the inbox's name. An empty name falls
// back to the inbox display name. A draft records the resolved name
// (drafts.from_name) so it can be frozen into the approval fingerprint and used
// by the approved send. The name is operator-controlled (Owner/Admin only).
const migration025 = `ALTER TABLE inbox_aliases ADD COLUMN display_name TEXT NOT NULL DEFAULT '';
ALTER TABLE drafts ADD COLUMN from_name TEXT NOT NULL DEFAULT '';
`

// migration026 adds an opt-in authenticated-sender requirement for the optional
// MX edge. The sender allow-list matches the RFC5322.From address, which is
// attacker-controlled and spoofable; only MX delivery carries SPF/DKIM/DMARC
// evidence computed by the authenticated edge. When require_authenticated is
// set, MX mail to the inbox is accepted only when the From domain is
// authenticated (a DMARC pass, or an aligned SPF/DKIM pass). It is off by
// default and has no effect on webhook providers, which carry no such evidence.
const migration026 = `ALTER TABLE inboxes ADD COLUMN require_authenticated INTEGER NOT NULL DEFAULT 0;
`

// migration027 gives a Hermes relay connection an outbound role. Today relay
// outbound always sends with Owner authority; setting outbound_role=assistant
// makes the relay create a draft and request send instead, so a Hermes agent
// can be held to the same human-approval boundary as an Assistant API key.
// Default 'owner' preserves existing connections' behaviour.
const migration027 = `ALTER TABLE hermes_connections ADD COLUMN outbound_role TEXT NOT NULL DEFAULT 'owner';
`

// External aliases have stable IDs and own their optional sending credentials.
// Message/log IDs are immutable attribution snapshots, deliberately not foreign
// keys: deleting an alias must not turn a queued external send into a domain send.
const migration028 = `
CREATE UNIQUE INDEX idx_inboxes_id_account ON inboxes(id,account_id);
CREATE TABLE external_aliases (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL,
  inbox_id TEXT NOT NULL,
  address TEXT NOT NULL COLLATE NOCASE,
  display_name TEXT NOT NULL DEFAULT '',
  provider TEXT NOT NULL DEFAULT '',
  encrypted_config TEXT NOT NULL DEFAULT '',
  revision INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(inbox_id,address),
  FOREIGN KEY(inbox_id,account_id) REFERENCES inboxes(id,account_id) ON DELETE CASCADE
);
CREATE INDEX idx_external_aliases_account ON external_aliases(account_id,inbox_id);
ALTER TABLE messages ADD COLUMN sending_external_alias_id TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_messages_external_alias ON messages(sending_external_alias_id);
ALTER TABLE drafts ADD COLUMN from_external_alias_id TEXT NOT NULL DEFAULT '';
ALTER TABLE outbound_delivery_log ADD COLUMN external_alias_id TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_outbound_log_external_alias ON outbound_delivery_log(account_id,external_alias_id,id DESC);
`

// migration029 makes transport activity independent from mailbox message
// lifetime. Inbound deliveries get an immutable snapshot, while outbound
// attempts retain the message fields needed by the activity log after the
// message is deleted.
const migration029 = `
CREATE TABLE inbound_delivery_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  domain_id TEXT NOT NULL REFERENCES domains(id) ON DELETE CASCADE,
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  provider TEXT NOT NULL DEFAULT '',
  provider_delivery_id TEXT,
  provider_message_id TEXT NOT NULL DEFAULT '',
  message_id TEXT NOT NULL DEFAULT '',
  from_address TEXT NOT NULL DEFAULT '',
  to_json TEXT NOT NULL DEFAULT '[]',
  subject TEXT NOT NULL DEFAULT '',
  size_bytes INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'received',
  created_at TEXT NOT NULL
);
CREATE INDEX idx_inbound_log_domain ON inbound_delivery_log(account_id,domain_id,created_at DESC,id DESC);
CREATE INDEX idx_inbound_log_inbox ON inbound_delivery_log(account_id,inbox_id,created_at DESC,id DESC);
CREATE INDEX idx_inbound_log_message ON inbound_delivery_log(message_id);
CREATE UNIQUE INDEX idx_inbound_log_delivery ON inbound_delivery_log(account_id,provider,inbox_id,provider_delivery_id) WHERE provider_delivery_id IS NOT NULL AND provider_delivery_id<>'';
ALTER TABLE outbound_delivery_log ADD COLUMN inbox_id TEXT NOT NULL DEFAULT '';
ALTER TABLE outbound_delivery_log ADD COLUMN from_address TEXT NOT NULL DEFAULT '';
ALTER TABLE outbound_delivery_log ADD COLUMN to_json TEXT NOT NULL DEFAULT '[]';
ALTER TABLE outbound_delivery_log ADD COLUMN subject TEXT NOT NULL DEFAULT '';
ALTER TABLE outbound_delivery_log ADD COLUMN client_label TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_outbound_log_inbox ON outbound_delivery_log(account_id,inbox_id,id DESC);
INSERT INTO inbound_delivery_log(account_id,domain_id,inbox_id,provider,provider_delivery_id,provider_message_id,message_id,from_address,to_json,subject,size_bytes,status,created_at)
  SELECT m.account_id,i.domain_id,m.inbox_id,m.provider,m.provider_delivery_id,m.provider_message_id,m.id,m.from_address,m.to_json,m.subject,m.size_bytes,'received',m.created_at
  FROM messages m JOIN inboxes i ON i.id=m.inbox_id AND i.account_id=m.account_id
  WHERE m.direction='inbound' AND m.internal=0;
UPDATE outbound_delivery_log SET inbox_id=COALESCE((SELECT m.inbox_id FROM messages m WHERE m.id=outbound_delivery_log.message_id AND m.account_id=outbound_delivery_log.account_id),'');
UPDATE outbound_delivery_log SET from_address=COALESCE((SELECT m.from_address FROM messages m WHERE m.id=outbound_delivery_log.message_id AND m.account_id=outbound_delivery_log.account_id),''),to_json=COALESCE((SELECT m.to_json FROM messages m WHERE m.id=outbound_delivery_log.message_id AND m.account_id=outbound_delivery_log.account_id),'[]'),subject=COALESCE((SELECT m.subject FROM messages m WHERE m.id=outbound_delivery_log.message_id AND m.account_id=outbound_delivery_log.account_id),''),client_label=COALESCE((SELECT m.client_label FROM messages m WHERE m.id=outbound_delivery_log.message_id AND m.account_id=outbound_delivery_log.account_id),'') WHERE from_address='' AND to_json='[]' AND subject='' AND client_label='';
`

// migration030 adds multi-user support. A system administrator is one user on
// the installation (not of any one account) who provisions accounts and
// invitations but gets no automatic access to other accounts' mail. An account
// can additionally have non-admin human members ("mailbox operators") whose
// access is a set of per-inbox Owner grants in user_mailbox_roles. system_settings
// holds installation-wide choices such as the mailbox used to send system mail.
const migration030 = `
ALTER TABLE users ADD COLUMN is_system_admin INTEGER NOT NULL DEFAULT 0;
CREATE TABLE IF NOT EXISTS user_mailbox_roles (
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  role TEXT NOT NULL CHECK(role IN ('read','assistant','owner')),
  PRIMARY KEY(user_id, inbox_id)
);
CREATE INDEX IF NOT EXISTS idx_user_mailbox_roles_user ON user_mailbox_roles(user_id);
CREATE INDEX IF NOT EXISTS idx_user_mailbox_roles_inbox ON user_mailbox_roles(inbox_id);
CREATE TABLE IF NOT EXISTS system_settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL DEFAULT ''
);
-- An invitation is either a new account with an Admin (account_admin, where
-- account_id is the pre-created pending account) or a mailbox operator on an
-- existing account (operator, with owner grants in inbox_ids_json). Only the
-- SHA-256 of the one-time setup token is stored.
CREATE TABLE IF NOT EXISTS invites (
  id TEXT PRIMARY KEY,
  account_id TEXT REFERENCES accounts(id) ON DELETE CASCADE,
  account_name TEXT NOT NULL DEFAULT '',
  email TEXT NOT NULL COLLATE NOCASE,
  kind TEXT NOT NULL CHECK(kind IN ('account_admin','operator')),
  inbox_ids_json TEXT NOT NULL DEFAULT '[]',
  token_hash TEXT NOT NULL UNIQUE,
  expires_at TEXT NOT NULL,
  accepted_at TEXT,
  revoked_at TEXT,
  created_by TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_invites_account ON invites(account_id);
CREATE INDEX IF NOT EXISTS idx_invites_email ON invites(email);
`

// migration031 moves the invitation mailer from an installation-wide setting to
// a per-account choice: each account sends its invitations from one of its own
// mailboxes, so no account ever sends from another's. system_settings held only
// that installation-wide mailer, so it is retired.
const migration031 = `
ALTER TABLE accounts ADD COLUMN mailer_inbox_id TEXT REFERENCES inboxes(id) ON DELETE SET NULL;
DROP TABLE IF EXISTS system_settings;
`

const migration032 = `
CREATE TABLE clients (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  type TEXT NOT NULL CHECK(type IN ('api_key','hermes','webhook')),
  name TEXT NOT NULL,
  created_at TEXT NOT NULL,
  revoked_at TEXT
);
CREATE INDEX idx_clients_account ON clients(account_id, created_at DESC);
INSERT INTO clients(id,account_id,type,name,created_at,revoked_at)
SELECT id,account_id,'api_key',name,created_at,revoked_at FROM api_keys;
INSERT INTO clients(id,account_id,type,name,created_at)
SELECT id,account_id,'hermes',name,created_at FROM hermes_connections;
CREATE TABLE client_api_keys (
  client_id TEXT PRIMARY KEY REFERENCES clients(id) ON DELETE CASCADE,
  key_prefix TEXT NOT NULL,
  key_hash TEXT NOT NULL UNIQUE,
  is_admin INTEGER NOT NULL DEFAULT 0,
  last_used_at TEXT
);
INSERT INTO client_api_keys(client_id,key_prefix,key_hash,is_admin,last_used_at)
SELECT id,key_prefix,key_hash,is_admin,last_used_at FROM api_keys;
CREATE TABLE client_inbox_bindings (
  client_id TEXT NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  role TEXT NOT NULL CHECK(role IN ('read','assistant','owner')),
  PRIMARY KEY(client_id,inbox_id)
);
INSERT INTO client_inbox_bindings(client_id,inbox_id,role)
SELECT api_key_id,inbox_id,role FROM api_key_mailbox_roles;
CREATE TABLE client_push (
  client_id TEXT PRIMARY KEY REFERENCES clients(id) ON DELETE CASCADE,
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  gateway_id TEXT UNIQUE,
  secret_encrypted TEXT NOT NULL DEFAULT '',
  delivery_key_encrypted TEXT NOT NULL DEFAULT '',
  outbound_role TEXT NOT NULL DEFAULT 'owner',
  last_ack_event_id INTEGER NOT NULL DEFAULT 0,
  last_connected_at TEXT,
  url TEXT NOT NULL DEFAULT '',
  payload_mode TEXT NOT NULL DEFAULT '',
  auth_mode TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1,
  last_success_at TEXT,
  last_error TEXT NOT NULL DEFAULT '',
  signing_secret_encrypted TEXT NOT NULL DEFAULT ''
);
INSERT INTO client_push(client_id,inbox_id,gateway_id,secret_encrypted,delivery_key_encrypted,outbound_role,last_ack_event_id,last_connected_at)
SELECT id,inbox_id,gateway_id,secret_encrypted,delivery_key_encrypted,outbound_role,last_ack_event_id,last_connected_at FROM hermes_connections;
CREATE TABLE webhook_deliveries (
  client_id TEXT NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
  event_id INTEGER NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TEXT NOT NULL,
  last_error TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','delivered','failed')),
  created_at TEXT NOT NULL,
  PRIMARY KEY(client_id,event_id)
);
`

// migration033 lets a domain reuse its parent domain's receiving and/or sending
// configuration instead of configuring its own. A subdomain (for example
// agent.example.com under example.com) is still a normal domain row that owns
// its inboxes, addresses and catch-all, but when the matching inherit flag is
// set and it has no configuration of its own, the effective configuration is
// resolved by walking parent_domain_id to the nearest ancestor that has one.
// This lets one provider connector (for example one Cloudflare Worker and its
// shared secret, or one Mailgun route) serve every onboarded subdomain of a
// zone, so a new subdomain address never needs a second receiver. Inheritance
// is resolved at read time, so rotating the parent secret or switching the
// parent provider applies to every descendant automatically. An explicit own
// configuration always wins. Existing domains are roots (NULL parent) and are
// unaffected; ON DELETE CASCADE removes a domain's descendants with it.
const migration033 = `
ALTER TABLE domains ADD COLUMN parent_domain_id TEXT REFERENCES domains(id) ON DELETE CASCADE;
ALTER TABLE domains ADD COLUMN inherit_receiving INTEGER NOT NULL DEFAULT 0;
ALTER TABLE domains ADD COLUMN inherit_sending INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_domains_parent ON domains(parent_domain_id);
`

// migration034 lets a delivery attempt be recorded while it is still in flight
// ("sending") and, if a re-claim happens before an outcome is written, marks the
// abandoned attempt "interrupted". Previously an attempt row was only written
// once there was a terminal outcome, so a send interrupted by a process restart,
// crash or dropped connection left no trace at all: the message silently looped
// as pending with an empty sending log even though the remote may have accepted
// it. SQLite cannot widen a CHECK constraint in place, so the table is rebuilt
// with the expanded status set; every existing row is carried over unchanged.
const migration034 = `
CREATE TABLE outbound_delivery_log_new (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  domain_id TEXT REFERENCES domains(id) ON DELETE SET NULL,
  provider TEXT NOT NULL DEFAULT '',
  message_id TEXT REFERENCES messages(id) ON DELETE SET NULL,
  workflow_id TEXT,
  external_alias_id TEXT NOT NULL DEFAULT '',
  attempt INTEGER NOT NULL DEFAULT 1,
  status TEXT NOT NULL CHECK(status IN ('sent','failed','sending','interrupted')),
  provider_message_id TEXT NOT NULL DEFAULT '',
  error_text TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  inbox_id TEXT NOT NULL DEFAULT '',
  from_address TEXT NOT NULL DEFAULT '',
  to_json TEXT NOT NULL DEFAULT '[]',
  subject TEXT NOT NULL DEFAULT '',
  client_label TEXT NOT NULL DEFAULT ''
);
INSERT INTO outbound_delivery_log_new(id,account_id,domain_id,provider,message_id,workflow_id,external_alias_id,attempt,status,provider_message_id,error_text,created_at,inbox_id,from_address,to_json,subject,client_label)
  SELECT id,account_id,domain_id,provider,message_id,workflow_id,external_alias_id,attempt,status,provider_message_id,error_text,created_at,inbox_id,from_address,to_json,subject,client_label
  FROM outbound_delivery_log;
DROP TABLE outbound_delivery_log;
ALTER TABLE outbound_delivery_log_new RENAME TO outbound_delivery_log;
CREATE INDEX idx_outbound_log_domain ON outbound_delivery_log(account_id, domain_id, id DESC);
CREATE INDEX idx_outbound_log_msg ON outbound_delivery_log(message_id);
CREATE INDEX idx_outbound_log_workflow ON outbound_delivery_log(workflow_id);
CREATE INDEX idx_outbound_log_external_alias ON outbound_delivery_log(account_id, external_alias_id, id DESC);
CREATE INDEX idx_outbound_log_inbox ON outbound_delivery_log(account_id, inbox_id, id DESC);
`

// migration035 persists the transport-supplied envelope sender on inbound
// messages and lets a webhook delivery be terminally skipped.
//
//   - messages.envelope_from holds the SMTP envelope sender (MAIL FROM) the
//     receiving transport observed, exactly as the adapter supplied it. It is
//     bounded metadata, not a second copy of the MIME From header: when the
//     transport supplied no sender (a null return path, or a provider that does
//     not pass one) the column stays empty, and it is never backfilled from the
//     MIME headers. The approval workflow already binds its decision to this
//     value (D029/D058); persisting it makes the same original envelope sender
//     available to durable consumers such as an outbound forward webhook.
//   - webhook_deliveries.status gains 'skipped', a terminal outcome for an
//     event whose message is currently spam, internal or has since been
//     deleted. A skipped delivery advances the client cursor exactly like a
//     delivered or failed one, so such an event cannot block the head of the
//     queue indefinitely. SQLite cannot widen a CHECK constraint in place, so
//     the table is rebuilt and every existing row is carried over unchanged.
const migration035 = `
ALTER TABLE messages ADD COLUMN envelope_from TEXT NOT NULL DEFAULT '';

CREATE TABLE webhook_deliveries_new (
  client_id TEXT NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
  event_id INTEGER NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TEXT NOT NULL,
  last_error TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','delivered','failed','skipped')),
  created_at TEXT NOT NULL,
  PRIMARY KEY(client_id,event_id)
);
INSERT INTO webhook_deliveries_new(client_id,event_id,attempts,next_attempt_at,last_error,status,created_at)
  SELECT client_id,event_id,attempts,next_attempt_at,last_error,status,created_at FROM webhook_deliveries;
DROP TABLE webhook_deliveries;
ALTER TABLE webhook_deliveries_new RENAME TO webhook_deliveries;
`

// migration036 adds client_delivery_log, the per-event delivery history behind
// the Dashboard "Log" button on a Webhook or Hermes relay client. One row is
// appended per (client, event) when a delivery outcome is reached and updated in
// place while a retry or acknowledgement is still outstanding, so an operator
// can see queued, delivered, acknowledged, skipped and failed events without
// re-deriving them from the live queue.
//
//   - status is the transport-neutral outcome ("pending", "delivered",
//     "acknowledged", "skipped" or "failed"); the UI derives the label.
//   - attempts counts delivery or acknowledgement attempts; last_error and
//     next_attempt_at carry the retry context for a pending row.
//   - created_at/updated_at drive ordering and retention. History is bounded:
//     terminal rows older than the retention window are pruned, and a client's
//     delivery cursor plus the outstanding (pending) rows are always kept so a
//     restart still resumes correctly.
//   - ON DELETE CASCADE removes a client's history with the client, and the
//     account index lets retention and the account-scoped read prune and page
//     without scanning every account's rows.
const migration036 = `
CREATE TABLE client_delivery_log (
  client_id TEXT NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
  event_id INTEGER NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('pending','delivered','acknowledged','skipped','failed')),
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  next_attempt_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(client_id,event_id)
);
CREATE INDEX idx_client_delivery_log_client ON client_delivery_log(client_id, event_id DESC, updated_at DESC);
CREATE INDEX idx_client_delivery_log_account ON client_delivery_log(client_id, updated_at);
`

const migration037 = `
CREATE TABLE dialmx_domain_credentials (
  domain_id TEXT PRIMARY KEY REFERENCES domains(id) ON DELETE CASCADE,
  key_id TEXT NOT NULL,
  encrypted_private_seed TEXT NOT NULL,
  public_key TEXT NOT NULL,
  revision INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
`

// migration038 adds mx_settings, the single installation-wide MX receiver
// configuration once owned by environment variables (MX_ENABLE,
// MX_RECEIVER_URL, DIALMX_CORE_KEY). It is editable from the system
// administrator UI/API and reconciled into the running receiver at startup and
// on every change. The table is a singleton: id is always the fixed "mx" row.
// Routing fields (mode, receiver_url) are plaintext so a status/read does not
// need the application key; the bearer credential and any private CA bundle are
// encrypted at rest with APP_ENCRYPTION_KEY, exactly like every other stored
// provider secret. revision is the optimistic-concurrency token a save must
// present; it also orders runtime reconciliation.
const migration038 = `
CREATE TABLE mx_settings (
  id TEXT PRIMARY KEY,
  mode TEXT NOT NULL DEFAULT '',
  receiver_url TEXT NOT NULL DEFAULT '',
  encrypted_secret TEXT NOT NULL DEFAULT '',
  encrypted_config TEXT NOT NULL DEFAULT '',
  revision INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
`

// migration039 adds the Trash: messages.deleted_at marks a message as trashed
// rather than erasing it, and accounts.trash_retention_days is the per-account
// window after which the maintenance worker permanently purges trashed mail
// (0 disables automatic purging).
//
// The dormant messages.is_archived column is dropped in the same rebuild.
// SQLite cannot drop a column in place on every supported version, so messages
// is rebuilt; unlike earlier rebuilds this one has no foreign keys pointing in
// other than attachments (ON DELETE CASCADE, re-established by name) and the
// message_fts contentless table, which is untouched. All rows are carried over
// with deleted_at NULL (nothing was trashed before this change).
const migration039 = `
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
  bcc_json TEXT NOT NULL DEFAULT '[]',
  envelope_to_json TEXT NOT NULL DEFAULT '[]',
  envelope_recipient TEXT NOT NULL DEFAULT '',
  envelope_from TEXT NOT NULL DEFAULT '',
  subject TEXT NOT NULL DEFAULT '',
  text_body TEXT NOT NULL DEFAULT '',
  html_body TEXT NOT NULL DEFAULT '',
  raw_path TEXT NOT NULL DEFAULT '',
  size_bytes INTEGER NOT NULL DEFAULT 0,
  is_read INTEGER NOT NULL DEFAULT 0,
  deleted_at TEXT,
  received_at TEXT,
  sent_at TEXT,
  internal INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'sent',
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  next_attempt_at TEXT NOT NULL DEFAULT '',
  idem_key TEXT NOT NULL DEFAULT '',
  claim_owner TEXT NOT NULL DEFAULT '',
  claim_expires_at TEXT NOT NULL DEFAULT '',
  client_label TEXT NOT NULL DEFAULT '',
  client_id TEXT NOT NULL DEFAULT '',
  sending_domain_id TEXT,
  sending_external_alias_id TEXT,
  is_spam INTEGER NOT NULL DEFAULT 0,
  auth_results_json TEXT NOT NULL DEFAULT '{}',
  spam_reason TEXT NOT NULL DEFAULT '',
  UNIQUE(account_id, provider, envelope_recipient, provider_delivery_id)
);
INSERT INTO messages_new(id,account_id,inbox_id,thread_id,direction,provider,provider_delivery_id,provider_message_id,rfc_message_id,in_reply_to,references_json,from_name,from_address,to_json,cc_json,bcc_json,envelope_to_json,envelope_recipient,envelope_from,subject,text_body,html_body,raw_path,size_bytes,is_read,deleted_at,received_at,sent_at,internal,created_at,status,attempts,last_error,next_attempt_at,idem_key,claim_owner,claim_expires_at,client_label,client_id,sending_domain_id,sending_external_alias_id,is_spam,auth_results_json,spam_reason)
  SELECT id,account_id,inbox_id,thread_id,direction,provider,provider_delivery_id,provider_message_id,rfc_message_id,in_reply_to,references_json,from_name,from_address,to_json,cc_json,bcc_json,envelope_to_json,envelope_recipient,envelope_from,subject,text_body,html_body,raw_path,size_bytes,is_read,NULL,received_at,sent_at,internal,created_at,status,attempts,last_error,next_attempt_at,idem_key,claim_owner,claim_expires_at,client_label,client_id,sending_domain_id,sending_external_alias_id,is_spam,auth_results_json,spam_reason FROM messages;
DROP TABLE messages;
ALTER TABLE messages_new RENAME TO messages;
CREATE INDEX IF NOT EXISTS idx_messages_inbox_created ON messages(inbox_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_messages_thread_created ON messages(thread_id, created_at);
CREATE INDEX IF NOT EXISTS idx_messages_rfc_thread ON messages(account_id, inbox_id, rfc_message_id);
CREATE INDEX IF NOT EXISTS idx_messages_outbox ON messages(status, next_attempt_at);
CREATE INDEX IF NOT EXISTS idx_messages_claim ON messages(status, claim_expires_at);
CREATE INDEX IF NOT EXISTS idx_messages_internal ON messages(inbox_id, internal);
CREATE INDEX IF NOT EXISTS idx_messages_deleted ON messages(account_id, deleted_at);

ALTER TABLE accounts ADD COLUMN trash_retention_days INTEGER NOT NULL DEFAULT 30;
`

// migration040 adds the display-only time zone preferences. An empty value
// means "inherit": a user inherits the account default, and an account default
// of empty falls back to UTC. Timestamps remain stored and served as UTC.
const migration040 = `
ALTER TABLE accounts ADD COLUMN timezone TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN timezone TEXT NOT NULL DEFAULT '';
`
