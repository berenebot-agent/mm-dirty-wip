package store

// migration056 completes the remote (standalone IMAP) index so a local/remote
// mailbox boundary can answer the same read surface as a domain mailbox without
// ever archiving a body.
//
// It is additive and non-destructive. It adds:
//
//   - inbox_remote_messages.is_answered, is_draft, flags_json and internal_date:
//     the header-flag and server-time fields the remote adapter's MessageHeader
//     carries, so a cached remote message exposes the same header-equivalent
//     metadata (read/flagged/answered/draft) as a local message. The body and its
//     MIME structure are still never stored: only the header and flags are.
//   - inbox_remote_labels: free-text labels applied to a cached remote message,
//     scoped by account and inbox. Labels are message metadata and are never a
//     folder; the local search path intersects them with the remote result set.
//   - inbox_folders.remote_indexed_at and inbox_remote_messages.indexed_at: the
//     wall-clock time each folder/message metadata row was last confirmed against
//     the live server, so a scoped read can decide whether an on-demand reconcile
//     is needed and report metadata completeness honestly.
//   - inboxes.remote_index_status/remote_indexed_at/remote_index_error: the
//     per-inbox index-completeness state (never_started, partial, complete,
//     error), independent of provider reachability, so the UI can show whether
//     its local view of a remote mailbox is trustworthy.
//   - idx_inbox_remote_messages_account_thread: threads are account- and
//     inbox-scoped, so the thread grouping path never scans across accounts.
//   - idx_inbox_remote_labels_label: lookup of remote messages by label.
//
// remote_uid_validity remains the per-folder UIDVALIDITY anchor: a cached row is
// only trustworthy while its folder's live UIDVALIDITY still matches, otherwise
// the UID is stale and the message must be re-resolved by Message-ID.
const migration056 = `
ALTER TABLE inbox_remote_messages ADD COLUMN is_answered INTEGER NOT NULL DEFAULT 0;
ALTER TABLE inbox_remote_messages ADD COLUMN is_draft INTEGER NOT NULL DEFAULT 0;
ALTER TABLE inbox_remote_messages ADD COLUMN flags_json TEXT NOT NULL DEFAULT '[]';
ALTER TABLE inbox_remote_messages ADD COLUMN internal_date TEXT;
ALTER TABLE inbox_remote_messages ADD COLUMN indexed_at TEXT NOT NULL DEFAULT '';

ALTER TABLE inbox_folders ADD COLUMN remote_indexed_at TEXT NOT NULL DEFAULT '';

ALTER TABLE inboxes ADD COLUMN remote_index_status TEXT NOT NULL DEFAULT '';
ALTER TABLE inboxes ADD COLUMN remote_indexed_at TEXT NOT NULL DEFAULT '';
ALTER TABLE inboxes ADD COLUMN remote_index_error TEXT NOT NULL DEFAULT '';

CREATE TABLE inbox_remote_labels (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  remote_message_id TEXT NOT NULL REFERENCES inbox_remote_messages(id) ON DELETE CASCADE,
  label TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE(remote_message_id, label)
);
CREATE INDEX idx_inbox_remote_labels_label ON inbox_remote_labels(account_id, inbox_id, label);
CREATE INDEX idx_inbox_remote_labels_message ON inbox_remote_labels(remote_message_id);

CREATE INDEX IF NOT EXISTS idx_inbox_remote_messages_account_thread ON inbox_remote_messages(account_id, inbox_id, thread_key);
CREATE INDEX IF NOT EXISTS idx_inbox_remote_messages_folder ON inbox_remote_messages(account_id, inbox_id, folder_path, remote_uid_validity);

-- remote_sent_copies is the durable, separate state machine for "copy a sent
-- message into the inbox's remote Sent folder". It is deliberately independent
-- of the SMTP send: a send that succeeds commits, and this job is enqueued
-- after. A failing copy never re-runs the SMTP submission (that would risk a
-- duplicate); it retries only the IMAP APPEND, from the frozen raw MIME already
-- stored. States: pending, copied, ambiguous (append outcome unknown; never
-- blindly re-appended), failed (terminal). The message_id links to the local
-- outbound messages row for provenance; it is nullable because the job is
-- independent of the message's own lifecycle.
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
CREATE INDEX idx_remote_sent_copies_pending ON remote_sent_copies(state, next_attempt_at);
CREATE INDEX idx_remote_sent_copies_inbox ON remote_sent_copies(account_id, inbox_id);
`
