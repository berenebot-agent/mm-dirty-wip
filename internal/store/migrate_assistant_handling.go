package store

// migration055 adds the per-inbox assistant authoring mode and the RemoteDraft
// handoff record. It is additive and non-destructive.
//
//   - inboxes.authoring_mode selects how an assistant's request to send a draft
//     is handled: 'mailmoose_approval' is the in-product approval workflow (UI/API
//     decisions plus tokenized email approval); 'remote_draft' is a one-way
//     handoff to the connected remote server's Drafts folder. An empty value
//     means "unset": the effective mode is derived from the inbox kind
//     (standalone -> remote_draft, domain -> mailmoose_approval), so no existing
//     row needs rewriting and a domain inbox keeps its current behaviour.
//   - inboxes.notify_default_address is the per-inbox default address a handoff
//     or approval notification is sent to. It is a plain setting, never an
//     approval token. An empty value means "the inbox's own connected address",
//     so notifications still have a destination with no override configured.
//   - assistant_handling_requests records a RemoteDraft handoff: the frozen
//     content hash, the stable handoff correlation id, the RFC5322 Message-ID and
//     the publication/notification state machines. It is separate from
//     draft_send_requests so the existing approval request model is untouched and
//     publication state advances independently of notification state.
//
// Indexes keep the per-inbox list and the pending-publication sweep off a table
// scan.
const migration055 = `
ALTER TABLE inboxes ADD COLUMN authoring_mode TEXT NOT NULL DEFAULT '';
ALTER TABLE inboxes ADD COLUMN notify_default_address TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_inboxes_authoring_mode ON inboxes(account_id, authoring_mode);

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
);
CREATE INDEX idx_assistant_handling_inbox ON assistant_handling_requests(account_id, inbox_id, requested_at DESC);
CREATE INDEX idx_assistant_handling_pending ON assistant_handling_requests(publication, requested_at);
CREATE INDEX idx_assistant_handling_draft ON assistant_handling_requests(account_id, draft_id);
CREATE UNIQUE INDEX idx_assistant_handling_handoff ON assistant_handling_requests(account_id, handoff_id) WHERE handoff_id <> '';
`
