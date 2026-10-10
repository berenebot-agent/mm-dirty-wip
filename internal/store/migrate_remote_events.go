package store

// migration057 adds the durable remote event/detection surface for standalone
// (IMAP) inboxes. It is additive and non-destructive. The remote watcher detects
// new INBOX arrivals by comparing the live UID set against a durable per-folder
// cursor, emits exactly one durable event per genuinely new message, and never
// floods on the first observation of an existing mailbox.
//
// It adds:
//
//   - inbox_remote_cursors: the per-(inbox, folder) detection anchor. last_uid is
//     the highest UID already accounted for; baseline_uid records the UID that was
//     present when the cursor was first established. A cursor with baseline_done=1
//     means the first observation already happened, so only UIDs strictly greater
//     than last_uid are "new" and an existing mailbox never floods on first run.
//     uid_validity scopes last_uid: when the server resets UIDVALIDITY the whole
//     cursor is re-baselined so a stale UID is never treated as new mail.
//   - inbox_remote_arrivals: the durable, idempotent record of a detected arrival.
//     Keyed by (inbox, folder, uid_validity, uid) so a repeated detection is a
//     no-op. It carries the header metadata needed to route proactive delivery and
//     the delivery state of the proactive (webhook/Hermes) fan-out. It is
//     deliberately NOT the inbox_remote_messages metadata index: a notification is
//     never a live-read side effect and the two advance independently. A row is
//     never created for a message that is only seen by a read path.
//   - inbox_remote_notifications: the per-(inbox, folder, uid_validity, uid)
//     notification baseline. It records that a proactive notification decision was
//     made for an arrival, so a restart never re-notifies the same message and a
//     read of the mailbox never advances the notification baseline.
//   - inbox_remote_actions: the durable auto-action state for a remote arrival
//     (mark-read on ACK, delayed move-to-Trash after N hours). It is keyed by the
//     arrival's message identity and advanced by the worker; it is independent of
//     inbox_remote_messages because a remote body is never archived.
//   - inboxes.remote_notify_baseline_set: a per-inbox flag recording that the very
//     first detection pass established the notification baseline, so enabling
//     remote delivery on an existing mailbox does not notify its whole backlog.
//
// Provider Trash auto-expiry is intentionally out of scope here: a remote Trash
// folder's server-side expiry is the provider's concern and is never swept by the
// local retention sweeper (which only touches locally-persisted messages).
const migration057 = `
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
CREATE INDEX idx_inbox_remote_cursors_account ON inbox_remote_cursors(account_id, inbox_id);

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
  settled_at TEXT,
  UNIQUE(inbox_id, folder_path, remote_uid_validity, remote_uid)
);
CREATE INDEX idx_inbox_remote_arrivals_pending ON inbox_remote_arrivals(delivery_state, next_attempt_at);
CREATE INDEX idx_inbox_remote_arrivals_inbox ON inbox_remote_arrivals(account_id, inbox_id, detected_at DESC);

CREATE TABLE inbox_remote_notifications (
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  folder_path TEXT NOT NULL,
  remote_uid_validity INTEGER NOT NULL DEFAULT 0,
  remote_uid INTEGER NOT NULL DEFAULT 0,
  notified_at TEXT NOT NULL,
  PRIMARY KEY(inbox_id, folder_path, remote_uid_validity, remote_uid)
);
CREATE INDEX idx_inbox_remote_notifications_account ON inbox_remote_notifications(account_id, inbox_id);

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
CREATE INDEX idx_inbox_remote_actions_due ON inbox_remote_actions(trash_done, trash_due_at);

ALTER TABLE inboxes ADD COLUMN remote_notify_baseline_set INTEGER NOT NULL DEFAULT 0;
`
