-- Public database baseline 001. Append versioned migrations; do not rewrite this schema.
CREATE TABLE account_mx_receivers (
  account_id TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
  receiver_url TEXT NOT NULL DEFAULT '',
  encrypted_secret TEXT NOT NULL DEFAULT '',
  encrypted_config TEXT NOT NULL DEFAULT '',
  revision INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE "accounts" (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  storage_quota_bytes INTEGER NOT NULL,
  storage_used_bytes INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  mailer_inbox_id TEXT REFERENCES inboxes(id) ON DELETE SET NULL,
  trash_retention_days INTEGER NOT NULL DEFAULT 0,
  timezone TEXT NOT NULL DEFAULT ''
);

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
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  requested_at TEXT NOT NULL,
  published_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE attachments (
  id TEXT PRIMARY KEY,
  message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  filename TEXT NOT NULL DEFAULT '',
  content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
  size_bytes INTEGER NOT NULL DEFAULT 0,
  part_index INTEGER NOT NULL,
  content_id TEXT NOT NULL DEFAULT ''
);

CREATE TABLE audit_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id TEXT,
  kind TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);

CREATE TABLE "blocked_messages" (
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
  created_at TEXT NOT NULL, source TEXT NOT NULL DEFAULT '',
  UNIQUE(account_id, provider, envelope_recipient, provider_delivery_id)
);

CREATE TABLE client_api_keys (
  client_id TEXT PRIMARY KEY REFERENCES clients(id) ON DELETE CASCADE,
  key_prefix TEXT NOT NULL,
  key_hash TEXT NOT NULL UNIQUE,
  is_admin INTEGER NOT NULL DEFAULT 0,
  last_used_at TEXT
);

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

CREATE TABLE client_inbox_bindings (
  client_id TEXT NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  role TEXT NOT NULL CHECK(role IN ('read','assistant','owner')), send_any_in_domain INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(client_id,inbox_id)
);

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

CREATE TABLE "clients" (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  type TEXT NOT NULL CHECK(type IN ('api_key','hermes','webhook','openclaw')),
  name TEXT NOT NULL,
  created_at TEXT NOT NULL,
  revoked_at TEXT
);

CREATE TABLE dialmx_domain_credentials (
  domain_id TEXT PRIMARY KEY REFERENCES domains(id) ON DELETE CASCADE,
  key_id TEXT NOT NULL,
  encrypted_private_seed TEXT NOT NULL,
  public_key TEXT NOT NULL,
  revision INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
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

CREATE TABLE "domains" (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  name TEXT NOT NULL COLLATE NOCASE,
  catch_all_inbox_id TEXT,
  created_at TEXT NOT NULL, parent_domain_id TEXT REFERENCES domains(id) ON DELETE CASCADE, inherit_receiving INTEGER NOT NULL DEFAULT 0, inherit_sending INTEGER NOT NULL DEFAULT 0, dkim_private_key TEXT NOT NULL DEFAULT '', dkim_public_key TEXT NOT NULL DEFAULT '', dkim_selector TEXT NOT NULL DEFAULT '', dkim_domain TEXT NOT NULL DEFAULT '',
  UNIQUE(account_id, name),
  UNIQUE(name)
);

CREATE TABLE draft_attachments (
  id TEXT PRIMARY KEY,
  draft_id TEXT NOT NULL REFERENCES drafts(id) ON DELETE CASCADE,
  filename TEXT NOT NULL DEFAULT '',
  content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
  size_bytes INTEGER NOT NULL DEFAULT 0,
  raw_path TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
, content_hash TEXT NOT NULL DEFAULT '');

CREATE TABLE draft_send_requests (
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
, approver_email TEXT NOT NULL DEFAULT '', token_hash TEXT NOT NULL DEFAULT '', token_expires_at TEXT, approval_message_id TEXT NOT NULL DEFAULT '', approval_workflow_id TEXT NOT NULL DEFAULT '', notification_status TEXT NOT NULL DEFAULT 'none');

CREATE TABLE drafts (
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
, status TEXT NOT NULL DEFAULT 'draft', from_address TEXT NOT NULL DEFAULT '', from_name TEXT NOT NULL DEFAULT '');

CREATE TABLE empty_trash_operations (
 action_id TEXT PRIMARY KEY REFERENCES inbox_message_actions(id) ON DELETE CASCADE,
 phase TEXT NOT NULL DEFAULT 'discovering', folder TEXT NOT NULL DEFAULT '',
 cursor TEXT NOT NULL DEFAULT '', uid_validity INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE empty_trash_targets (
 operation_id TEXT NOT NULL REFERENCES empty_trash_operations(action_id) ON DELETE CASCADE,
 target_id TEXT NOT NULL, target_json TEXT NOT NULL, child_id TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(operation_id,target_id)
);

CREATE TABLE events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  inbox_id TEXT,
  type TEXT NOT NULL,
  entity_id TEXT NOT NULL DEFAULT '',
  payload_json TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL
);

CREATE TABLE gmail_history_pages (
 inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
 purpose TEXT NOT NULL CHECK(purpose IN ('sync','detection')),
 anchor TEXT NOT NULL, page_token TEXT NOT NULL DEFAULT '', payload TEXT NOT NULL DEFAULT '',
 item_offset INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(inbox_id,purpose)
);

CREATE TABLE google_detection (inbox_id TEXT PRIMARY KEY REFERENCES inboxes(id) ON DELETE CASCADE,history_id TEXT NOT NULL);

CREATE TABLE google_label_memberships (
 message_id TEXT NOT NULL REFERENCES inbox_remote_messages(id) ON DELETE CASCADE,
 inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
 label_id TEXT NOT NULL,
 PRIMARY KEY(message_id,label_id)
);

CREATE TABLE google_message_ids (
 inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
 message_id TEXT NOT NULL REFERENCES inbox_remote_messages(id) ON DELETE CASCADE,
 provider_id TEXT NOT NULL, thread_id TEXT NOT NULL, labels_json TEXT NOT NULL DEFAULT '[]',
 sync_generation TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(inbox_id,provider_id), UNIQUE(message_id)
);

CREATE TABLE google_oauth_attempts (
 state_hash TEXT PRIMARY KEY, account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 user_id TEXT NOT NULL, inbox_id TEXT NOT NULL DEFAULT '', display_name TEXT NOT NULL,
 client_id TEXT NOT NULL, redirect_uri TEXT NOT NULL, encrypted_pending TEXT NOT NULL,
 expires_at TEXT NOT NULL
);

CREATE TABLE hermes_enroll_tokens (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  token_hash TEXT NOT NULL UNIQUE,
  expires_at TEXT NOT NULL,
  used_at TEXT,
  created_at TEXT NOT NULL
, kind TEXT NOT NULL DEFAULT 'hermes' CHECK (kind IN ('hermes','openclaw')));

CREATE TABLE inbound_control_messages (
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
  created_at TEXT NOT NULL, subject TEXT NOT NULL DEFAULT '', source TEXT NOT NULL DEFAULT '',
  UNIQUE(account_id, provider, envelope_recipient, provider_delivery_id)
);

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
, source TEXT NOT NULL DEFAULT '');

CREATE TABLE inbox_aliases (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  domain_id TEXT NOT NULL REFERENCES domains(id) ON DELETE CASCADE,
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  local_part TEXT NOT NULL COLLATE NOCASE,
  created_at TEXT NOT NULL, display_name TEXT NOT NULL DEFAULT '',
  UNIQUE(domain_id, local_part)
);

CREATE TABLE inbox_defined_labels (
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  name TEXT NOT NULL COLLATE NOCASE,
  PRIMARY KEY(inbox_id, name)
);

CREATE TABLE inbox_folders (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  path TEXT NOT NULL,
  name TEXT NOT NULL DEFAULT '',
  parent_path TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL DEFAULT 'folder',
  selectable INTEGER NOT NULL DEFAULT 1,
  remote_uid_validity INTEGER NOT NULL DEFAULT 0,
  message_count INTEGER NOT NULL DEFAULT 0,
  unread_count INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL, is_system INTEGER NOT NULL DEFAULT 0, origin TEXT NOT NULL DEFAULT 'local', remote_metadata_json TEXT NOT NULL DEFAULT '{}', remote_indexed_at TEXT NOT NULL DEFAULT '', role_locked INTEGER NOT NULL DEFAULT 0, backfill_before_uid INTEGER NOT NULL DEFAULT 0, backfill_complete INTEGER NOT NULL DEFAULT 0, remote_highest_modseq INTEGER NOT NULL DEFAULT 0,
  UNIQUE(inbox_id, path)
);

CREATE TABLE inbox_gmail_sync (
 inbox_id TEXT PRIMARY KEY REFERENCES inboxes(id) ON DELETE CASCADE,
 account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 history_id TEXT NOT NULL DEFAULT '', backfill_page TEXT NOT NULL DEFAULT '',
 backfill_done INTEGER NOT NULL DEFAULT 0, sync_generation TEXT NOT NULL DEFAULT '',
 backfill_offset INTEGER NOT NULL DEFAULT 0, backfill_snapshot TEXT NOT NULL DEFAULT ''
, backfill_after TEXT NOT NULL DEFAULT '');

CREATE TABLE inbox_google_connections (
 inbox_id TEXT PRIMARY KEY REFERENCES inboxes(id) ON DELETE CASCADE,
 account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 client_id TEXT NOT NULL, encrypted_secret TEXT NOT NULL, encrypted_token TEXT NOT NULL,
 expires_at TEXT NOT NULL, history_id TEXT NOT NULL DEFAULT '', backfill_page TEXT NOT NULL DEFAULT '',
 backfill_done INTEGER NOT NULL DEFAULT 0
 ,sync_generation TEXT NOT NULL DEFAULT ''
);

CREATE TABLE inbox_message_actions (
 id TEXT PRIMARY KEY, account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
 message_id TEXT NOT NULL, input_json TEXT NOT NULL, state TEXT NOT NULL DEFAULT 'pending',
 attempts INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '',
 next_attempt_at TEXT NOT NULL, claim_owner TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL, settled_at TEXT NOT NULL DEFAULT '', dismissed INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE inbox_metadata_sync (
 inbox_id TEXT PRIMARY KEY REFERENCES inboxes(id) ON DELETE CASCADE,
 account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 state TEXT NOT NULL DEFAULT 'waiting', phase TEXT NOT NULL DEFAULT 'initial',
 total INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL
);

CREATE TABLE inbox_microsoft_connections (
 inbox_id TEXT PRIMARY KEY REFERENCES inboxes(id) ON DELETE CASCADE,
 account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 client_id TEXT NOT NULL, tenant TEXT NOT NULL, identity_id TEXT NOT NULL,
 mailbox TEXT NOT NULL, shared INTEGER NOT NULL, encrypted_token TEXT NOT NULL, expires_at TEXT NOT NULL
);

CREATE TABLE "inbox_one_bindings" (
 inbox_id TEXT PRIMARY KEY REFERENCES inboxes(id) ON DELETE CASCADE,
 account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 connection_id TEXT NOT NULL, encrypted_connection_key TEXT NOT NULL,
 encrypted_key TEXT NOT NULL, key_aad TEXT NOT NULL, fingerprint TEXT NOT NULL,
 rate_group TEXT NOT NULL, revision INTEGER NOT NULL DEFAULT 1,
 gmail_email TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'active', last_error TEXT NOT NULL DEFAULT '',
 last_checked_at TEXT NOT NULL DEFAULT '', failure_count INTEGER NOT NULL DEFAULT 0, UNIQUE(account_id,connection_id)
);

CREATE TABLE inbox_remote_actions (
  arrival_id TEXT PRIMARY KEY REFERENCES inbox_remote_arrivals(id) ON DELETE CASCADE,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  folder_path TEXT NOT NULL,
  remote_uid_validity INTEGER NOT NULL DEFAULT 0,
  remote_uid INTEGER NOT NULL DEFAULT 0,
  mark_read_done INTEGER NOT NULL DEFAULT 0,
  trash_due_at TEXT,
  trash_done INTEGER NOT NULL DEFAULT 0,
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL
);

CREATE TABLE inbox_remote_arrivals (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  folder_path TEXT NOT NULL,
  remote_uid_validity INTEGER NOT NULL DEFAULT 0,
  remote_uid INTEGER NOT NULL DEFAULT 0,
  rfc_message_id TEXT NOT NULL DEFAULT '',
  from_name TEXT NOT NULL DEFAULT '',
  from_address TEXT NOT NULL DEFAULT '',
  subject TEXT NOT NULL DEFAULT '',
  size_bytes INTEGER NOT NULL DEFAULT 0,
  internal_date TEXT,
  -- delivery_state is the proactive fan-out state: pending, delivered, failed,
  -- skipped (a control message or handoff notification that is never a live-read).
  delivery_state TEXT NOT NULL DEFAULT 'pending',
  delivery_attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TEXT NOT NULL DEFAULT '',
  claim_owner TEXT NOT NULL DEFAULT '',
  claim_expires_at TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  control INTEGER NOT NULL DEFAULT 0,
  event_recorded INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  detected_at TEXT NOT NULL,
  settled_at TEXT, source TEXT NOT NULL DEFAULT '',
  UNIQUE(inbox_id, folder_path, remote_uid_validity, remote_uid)
);

CREATE TABLE inbox_remote_credentials (
  inbox_id TEXT PRIMARY KEY REFERENCES inboxes(id) ON DELETE CASCADE,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  encrypted_imap TEXT NOT NULL DEFAULT '',
  encrypted_smtp TEXT NOT NULL DEFAULT '',
  revision INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE inbox_remote_cursors (
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  folder_path TEXT NOT NULL,
  remote_uid_validity INTEGER NOT NULL DEFAULT 0,
  last_uid INTEGER NOT NULL DEFAULT 0,
  baseline_done INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(inbox_id, folder_path)
);

CREATE TABLE inbox_remote_labels (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  remote_message_id TEXT NOT NULL REFERENCES inbox_remote_messages(id) ON DELETE CASCADE,
  label TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE(remote_message_id, label)
);

CREATE TABLE inbox_remote_messages (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  folder_path TEXT NOT NULL,
  remote_uid_validity INTEGER NOT NULL DEFAULT 0,
  remote_uid INTEGER NOT NULL DEFAULT 0,
  rfc_message_id TEXT NOT NULL DEFAULT '',
  in_reply_to TEXT NOT NULL DEFAULT '',
  references_json TEXT NOT NULL DEFAULT '[]',
  thread_key TEXT NOT NULL DEFAULT '',
  from_name TEXT NOT NULL DEFAULT '',
  from_address TEXT NOT NULL DEFAULT '',
  to_json TEXT NOT NULL DEFAULT '[]',
  cc_json TEXT NOT NULL DEFAULT '[]',
  subject TEXT NOT NULL DEFAULT '',
  snippet TEXT NOT NULL DEFAULT '',
  size_bytes INTEGER NOT NULL DEFAULT 0,
  has_attachments INTEGER NOT NULL DEFAULT 0,
  is_read INTEGER NOT NULL DEFAULT 0,
  is_flagged INTEGER NOT NULL DEFAULT 0,
  received_at TEXT,
  sent_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL, is_answered INTEGER NOT NULL DEFAULT 0, is_draft INTEGER NOT NULL DEFAULT 0, flags_json TEXT NOT NULL DEFAULT '[]', internal_date TEXT, indexed_at TEXT NOT NULL DEFAULT '', snippet_pending INTEGER NOT NULL DEFAULT 0,
  UNIQUE(inbox_id, folder_path, remote_uid_validity, remote_uid)
);

CREATE TABLE inbox_remote_notifications (
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  folder_path TEXT NOT NULL,
  remote_uid_validity INTEGER NOT NULL DEFAULT 0,
  remote_uid INTEGER NOT NULL DEFAULT 0,
  notified_at TEXT NOT NULL,
  PRIMARY KEY(inbox_id, folder_path, remote_uid_validity, remote_uid)
);

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
  storage_used_bytes INTEGER NOT NULL DEFAULT 0,
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

CREATE TABLE invites (
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

CREATE TABLE key_sessions (
  id_hash TEXT PRIMARY KEY,
  client_id TEXT NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
  csrf_token TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE message_deliveries (
  message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  client_id TEXT NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
  delivered_at TEXT NOT NULL,
  PRIMARY KEY(message_id, client_id)
);

CREATE VIRTUAL TABLE message_fts USING fts5(
  message_id UNINDEXED,
  account_id UNINDEXED,
  inbox_id UNINDEXED,
  subject,
  from_address,
  recipients,
  body,
  attachment_names
);

CREATE TABLE message_labels (
  message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  label TEXT NOT NULL COLLATE NOCASE,
  created_at TEXT NOT NULL,
  PRIMARY KEY(message_id, label)
);

CREATE TABLE "messages" (
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
  is_spam INTEGER NOT NULL DEFAULT 0,
  auth_results_json TEXT NOT NULL DEFAULT '{}',
  spam_reason TEXT NOT NULL DEFAULT '', delivery_action_due_at TEXT, mailbox_id TEXT REFERENCES inbox_folders(id) ON DELETE SET NULL, snippet TEXT NOT NULL DEFAULT '', send_attempt_state TEXT NOT NULL DEFAULT '',
  UNIQUE(account_id, provider, envelope_recipient, provider_delivery_id)
);

CREATE TABLE microsoft_delta (
 inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
 folder_id TEXT NOT NULL, cursor TEXT NOT NULL DEFAULT '', initialized INTEGER NOT NULL DEFAULT 0, generation TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(inbox_id,folder_id)
);

CREATE TABLE microsoft_message_ids (
 inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
 message_id TEXT NOT NULL REFERENCES inbox_remote_messages(id) ON DELETE CASCADE,
 provider_id TEXT NOT NULL, folder_id TEXT NOT NULL, generation TEXT NOT NULL DEFAULT '', detected INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(inbox_id,provider_id), UNIQUE(message_id)
);

CREATE TABLE microsoft_oauth_attempts (
 state_hash TEXT PRIMARY KEY, account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 user_id TEXT NOT NULL, inbox_id TEXT NOT NULL DEFAULT '', display_name TEXT NOT NULL,
 client_id TEXT NOT NULL, tenant TEXT NOT NULL, mailbox TEXT NOT NULL, shared INTEGER NOT NULL,
 encrypted_pending TEXT NOT NULL, expires_at TEXT NOT NULL
, encrypted_token TEXT NOT NULL DEFAULT '');

CREATE TABLE microsoft_push (
 inbox_id TEXT PRIMARY KEY, account_id TEXT NOT NULL,
 subscription_id TEXT NOT NULL DEFAULT '', encrypted_state TEXT NOT NULL,
 state_hash TEXT NOT NULL, callback_url TEXT NOT NULL,
 expires_at TEXT NOT NULL DEFAULT '', retry_at TEXT NOT NULL DEFAULT '',
 status TEXT NOT NULL DEFAULT 'setting up',
 requested INTEGER NOT NULL DEFAULT 0, completed INTEGER NOT NULL DEFAULT 0,
 lifecycle TEXT NOT NULL DEFAULT '',
 client_id TEXT NOT NULL, tenant TEXT NOT NULL, encrypted_token TEXT NOT NULL
, requested_source TEXT NOT NULL DEFAULT '');

CREATE TABLE mx_receipts (
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

CREATE TABLE one_import_setups (
 id TEXT PRIMARY KEY, account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 encrypted_key TEXT NOT NULL, fingerprint TEXT NOT NULL, expires_at TEXT NOT NULL
);

CREATE TABLE "outbound_delivery_log" (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  domain_id TEXT REFERENCES domains(id) ON DELETE SET NULL,
  provider TEXT NOT NULL DEFAULT '',
  message_id TEXT REFERENCES messages(id) ON DELETE SET NULL,
  workflow_id TEXT,
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

CREATE TABLE outbound_idempotency (
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  idem_key TEXT NOT NULL,
  message_id TEXT NOT NULL,
  result_json TEXT NOT NULL,
  created_at TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'done', inbox_id TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(account_id, idem_key)
);

CREATE TABLE outbound_workflow (
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
, send_attempt_state TEXT NOT NULL DEFAULT '');

CREATE TABLE pending_file_cleanup (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  rel_path TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE remote_sent_copies (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  message_id TEXT,
  folder_path TEXT NOT NULL DEFAULT '',
  rfc_message_id TEXT NOT NULL DEFAULT '',
  message_id_header TEXT NOT NULL DEFAULT '',
  raw_path TEXT NOT NULL DEFAULT '',
  size_bytes INTEGER NOT NULL DEFAULT 0,
  state TEXT NOT NULL DEFAULT 'pending',
  remote_uid_validity INTEGER NOT NULL DEFAULT 0,
  remote_uid INTEGER NOT NULL DEFAULT 0,
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  next_attempt_at TEXT NOT NULL DEFAULT '',
  claim_owner TEXT NOT NULL DEFAULT '',
  claim_expires_at TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  copied_at TEXT,
  terminal_at TEXT
);

CREATE TABLE schema_migrations (
  version TEXT PRIMARY KEY,
  applied_at TEXT NOT NULL
);

CREATE TABLE sessions (
  id_hash TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  csrf_token TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE threads (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  subject TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE user_mailbox_roles (
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  role TEXT NOT NULL CHECK(role IN ('read','assistant','owner')),
  PRIMARY KEY(user_id, inbox_id)
);

CREATE TABLE users (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  email TEXT NOT NULL COLLATE NOCASE,
  password_hash TEXT NOT NULL,
  is_admin INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL, is_system_admin INTEGER NOT NULL DEFAULT 0, timezone TEXT NOT NULL DEFAULT '', password_auth_enabled INTEGER NOT NULL DEFAULT 1 CHECK (password_auth_enabled IN (0,1)), dashboard_preferences TEXT NOT NULL DEFAULT '{}',
  UNIQUE(account_id, email),
  UNIQUE(email)
);

CREATE TABLE webauthn_credentials (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  credential_id BLOB NOT NULL,
  public_key BLOB NOT NULL,
  attestation_type TEXT NOT NULL DEFAULT '',
  aaguid TEXT NOT NULL DEFAULT '',
  sign_count INTEGER NOT NULL DEFAULT 0,
  transports TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL DEFAULT '',
  backup_eligible INTEGER NOT NULL DEFAULT 0,
  backup_state INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  last_used_at TEXT
);

CREATE TABLE "webhook_deliveries" (
  client_id TEXT NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
  event_id INTEGER NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TEXT NOT NULL,
  last_error TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','delivered','failed','skipped')),
  created_at TEXT NOT NULL,
  PRIMARY KEY(client_id,event_id)
);

CREATE UNIQUE INDEX idx_account_mx_receivers_url
  ON account_mx_receivers(receiver_url) WHERE receiver_url<>'';

CREATE UNIQUE INDEX idx_account_mx_receivers_url_nocase
  ON account_mx_receivers(receiver_url COLLATE NOCASE) WHERE receiver_url<>'';

CREATE INDEX idx_assistant_handling_draft ON assistant_handling_requests(account_id, draft_id);

CREATE UNIQUE INDEX idx_assistant_handling_handoff ON assistant_handling_requests(account_id, handoff_id) WHERE handoff_id <> '';

CREATE INDEX idx_assistant_handling_inbox ON assistant_handling_requests(account_id, inbox_id, requested_at DESC);

CREATE INDEX idx_assistant_handling_pending ON assistant_handling_requests(publication, requested_at);

CREATE INDEX idx_attachments_message ON attachments(message_id);

CREATE INDEX idx_blocked_messages_account_created ON blocked_messages(account_id, created_at DESC);

CREATE INDEX idx_blocked_messages_inbox_created ON blocked_messages(inbox_id, created_at DESC);

CREATE INDEX idx_client_delivery_log_account ON client_delivery_log(client_id, updated_at);

CREATE INDEX idx_client_delivery_log_client ON client_delivery_log(client_id, event_id DESC, updated_at DESC);

CREATE INDEX idx_clients_account ON clients(account_id, created_at DESC);

CREATE UNIQUE INDEX idx_domains_id_account ON domains(id,account_id);

CREATE INDEX idx_domains_parent ON domains(parent_domain_id);

CREATE INDEX idx_draft_attachments_draft ON draft_attachments(draft_id);

CREATE INDEX idx_draft_send_requests_pending_token
  ON draft_send_requests(inbox_id, token_hash) WHERE status='pending';

CREATE INDEX idx_drafts_inbox ON drafts(inbox_id, updated_at DESC);

CREATE INDEX idx_empty_trash_children ON empty_trash_targets(child_id);

CREATE INDEX idx_events_account_id ON events(account_id, id);

CREATE INDEX idx_events_inbox_id ON events(inbox_id, id);

CREATE INDEX idx_google_label_memberships ON google_label_memberships(inbox_id,label_id,message_id);

CREATE INDEX idx_inbound_control_inbox ON inbound_control_messages(inbox_id, created_at DESC);

CREATE INDEX idx_inbound_control_request ON inbound_control_messages(request_id);

CREATE UNIQUE INDEX idx_inbound_log_delivery ON inbound_delivery_log(account_id,provider,inbox_id,provider_delivery_id) WHERE provider_delivery_id IS NOT NULL AND provider_delivery_id<>'';

CREATE INDEX idx_inbound_log_domain ON inbound_delivery_log(account_id,domain_id,created_at DESC,id DESC);

CREATE INDEX idx_inbound_log_inbox ON inbound_delivery_log(account_id,inbox_id,created_at DESC,id DESC);

CREATE INDEX idx_inbound_log_message ON inbound_delivery_log(message_id);

CREATE INDEX idx_inbox_aliases_account ON inbox_aliases(account_id);

CREATE INDEX idx_inbox_aliases_inbox ON inbox_aliases(inbox_id);

CREATE INDEX idx_inbox_defined_labels_account ON inbox_defined_labels(account_id, inbox_id);

CREATE INDEX idx_inbox_folders_inbox ON inbox_folders(inbox_id, path);

CREATE INDEX idx_inbox_folders_role ON inbox_folders(inbox_id, role);

CREATE INDEX idx_inbox_remote_actions_due ON inbox_remote_actions(trash_done, trash_due_at);

CREATE INDEX idx_inbox_remote_arrivals_inbox ON inbox_remote_arrivals(account_id, inbox_id, detected_at DESC);

CREATE INDEX idx_inbox_remote_arrivals_pending ON inbox_remote_arrivals(delivery_state, next_attempt_at);

CREATE INDEX idx_inbox_remote_credentials_account ON inbox_remote_credentials(account_id);

CREATE INDEX idx_inbox_remote_cursors_account ON inbox_remote_cursors(account_id, inbox_id);

CREATE INDEX idx_inbox_remote_labels_label ON inbox_remote_labels(account_id, inbox_id, label);

CREATE INDEX idx_inbox_remote_labels_message ON inbox_remote_labels(remote_message_id);

CREATE INDEX idx_inbox_remote_messages_account_thread ON inbox_remote_messages(account_id, inbox_id, thread_key);

CREATE INDEX idx_inbox_remote_messages_folder ON inbox_remote_messages(account_id, inbox_id, folder_path, remote_uid_validity);

CREATE INDEX idx_inbox_remote_messages_inbox ON inbox_remote_messages(inbox_id, received_at DESC);

CREATE INDEX idx_inbox_remote_messages_rfc ON inbox_remote_messages(account_id, inbox_id, rfc_message_id);

CREATE INDEX idx_inbox_remote_messages_thread ON inbox_remote_messages(inbox_id, thread_key);

CREATE INDEX idx_inbox_remote_notifications_account ON inbox_remote_notifications(account_id, inbox_id);

CREATE INDEX idx_inboxes_account ON inboxes(account_id);

CREATE INDEX idx_inboxes_authoring_mode ON inboxes(account_id, authoring_mode);

CREATE UNIQUE INDEX idx_inboxes_standalone_address
  ON inboxes(account_id, address COLLATE NOCASE) WHERE kind='standalone' AND address<>'';

CREATE INDEX idx_invites_account ON invites(account_id);

CREATE INDEX idx_invites_email ON invites(email);

CREATE INDEX idx_key_sessions_client ON key_sessions(client_id);

CREATE INDEX idx_message_actions_due ON inbox_message_actions(state,next_attempt_at);

CREATE INDEX idx_message_actions_order ON inbox_message_actions(account_id,inbox_id,message_id,created_at);

CREATE INDEX idx_message_deliveries_message ON message_deliveries(message_id);

CREATE INDEX idx_message_labels_label ON message_labels(label, message_id);

CREATE INDEX idx_messages_claim ON messages(status, claim_expires_at);

CREATE INDEX idx_messages_deleted ON messages(account_id, deleted_at);

CREATE INDEX idx_messages_delivery_due ON messages(delivery_action_due_at) WHERE delivery_action_due_at IS NOT NULL;

CREATE INDEX idx_messages_inbox_created ON messages(inbox_id, created_at DESC);

CREATE INDEX idx_messages_internal ON messages(inbox_id, internal);

CREATE INDEX idx_messages_mailbox ON messages(account_id, inbox_id, mailbox_id);

CREATE INDEX idx_messages_outbox ON messages(status, next_attempt_at);

CREATE INDEX idx_messages_rfc_thread ON messages(account_id, inbox_id, rfc_message_id);

CREATE INDEX idx_messages_thread_created ON messages(thread_id, created_at);

CREATE INDEX idx_mx_receipts_expires ON mx_receipts(expires_at);

CREATE INDEX idx_outbound_idem_status ON outbound_idempotency(account_id, idem_key, status);

CREATE INDEX idx_outbound_log_domain ON outbound_delivery_log(account_id, domain_id, id DESC);

CREATE INDEX idx_outbound_log_inbox ON outbound_delivery_log(account_id, inbox_id, id DESC);

CREATE INDEX idx_outbound_log_msg ON outbound_delivery_log(message_id);

CREATE INDEX idx_outbound_log_workflow ON outbound_delivery_log(workflow_id);

CREATE INDEX idx_outbound_workflow_request ON outbound_workflow(request_id);

CREATE INDEX idx_outbound_workflow_status ON outbound_workflow(status, next_attempt_at);

CREATE INDEX idx_remote_sent_copies_inbox ON remote_sent_copies(account_id, inbox_id);

CREATE INDEX idx_remote_sent_copies_pending ON remote_sent_copies(state, next_attempt_at);

CREATE UNIQUE INDEX idx_send_requests_active_draft ON draft_send_requests(draft_id) WHERE status='pending';

CREATE INDEX idx_send_requests_draft ON draft_send_requests(draft_id, requested_at DESC);

CREATE INDEX idx_send_requests_inbox ON draft_send_requests(inbox_id, status, requested_at DESC);

CREATE INDEX idx_send_requests_issued_token ON draft_send_requests(token_hash) WHERE token_hash<>'';

CREATE INDEX idx_send_requests_message ON draft_send_requests(message_id);

CREATE INDEX idx_sessions_user ON sessions(user_id);

CREATE INDEX idx_threads_inbox_updated ON threads(inbox_id, updated_at DESC);

CREATE INDEX idx_user_mailbox_roles_inbox ON user_mailbox_roles(inbox_id);

CREATE INDEX idx_user_mailbox_roles_user ON user_mailbox_roles(user_id);

CREATE UNIQUE INDEX idx_webauthn_cred_id ON webauthn_credentials(credential_id);

CREATE INDEX idx_webauthn_user ON webauthn_credentials(user_id);

CREATE UNIQUE INDEX microsoft_push_subscription ON microsoft_push(subscription_id) WHERE subscription_id<>'';

CREATE INDEX remote_snippet_pending ON inbox_remote_messages(snippet_pending,created_at,id) WHERE snippet_pending=1;
