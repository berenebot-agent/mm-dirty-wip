package store

// Durable, empty local labels make "New label" additive on native mailboxes,
// rather than inadvertently creating an exclusive folder.
const migration075 = `
CREATE TABLE inbox_defined_labels (
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  name TEXT NOT NULL COLLATE NOCASE,
  PRIMARY KEY(inbox_id, name)
);
CREATE INDEX idx_inbox_defined_labels_account ON inbox_defined_labels(account_id, inbox_id);
`
