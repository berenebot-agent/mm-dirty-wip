package store

// A RemoteDraft handoff no longer queues a notification email: a draft-mode
// request to send just places the draft in the connected mailbox's Drafts
// folder. Any handoff notification job still queued from a previous build is
// settled terminally here, so the outbox worker never delivers one. The job's
// retained raw MIME is left for the normal terminal-content sweep; the status is
// the terminal 'failed' value with an explicit reason, so it is never claimed.
const migration076 = `
UPDATE outbound_workflow
   SET status='failed',
       last_error='handoff notification retired: a RemoteDraft handoff no longer notifies',
       next_attempt_at='',
       claim_owner='',
       claim_expires_at='',
       terminal_at=COALESCE(NULLIF(terminal_at,''), created_at)
 WHERE kind='draft_handoff' AND status='pending';
`
