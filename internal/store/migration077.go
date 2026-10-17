package store

// migration077 adds a durable pre-send marker to both outbound queues.
//
// The delivery log already records that an attempt is in flight
// ("sending" in outbound_delivery_log), but that row is a per-domain activity
// log, not the queue's own state: it cannot gate the queue, it is retained and
// pruned by its own rules, and a failed outcome write never clears it. So an
// attempt that reached the provider but crashed before its outcome was
// persisted left the message looking like an ordinary pending message, and a
// later attempt resent mail the provider had already accepted.
//
// send_attempt_state fixes that by making the pre-send marker part of the
// queued row itself:
//
//   - ” (empty) means no provider send has been started, or the last one
//     reached a definitive outcome that was durably persisted. An empty marker
//     is the only value that may be claimed and resent.
//   - a non-empty value is the claim token of the attempt that started the
//     provider send. It is written in the same transaction that resolves the
//     sending configuration and immediately before the provider call, so from
//     that instant the queue refuses to re-claim the row. Nothing else may
//     move the row back to a claimable state: LeaseNextPending,
//     RecoverAbandonedClaims/WorkflowClaims (including startup recovery) and
//     the domain requeue all leave a non-empty marker untouched.
//   - Resolving the marker (returning it to ”) is done only by a definitive
//     outcome (MarkSent / MarkFailed / MarkWorkflowSent / MarkWorkflowFailed)
//     or by an explicit operator action (RequeueFailed / recovery quarantine),
//     so an ambiguous send is never automatically resent.
//
// The default ” therefore backfills every existing row as "no send in flight",
// which is correct for rows queued before this version: they have no in-flight
// provider send from the new code.
const migration077 = `
ALTER TABLE messages ADD COLUMN send_attempt_state TEXT NOT NULL DEFAULT '';
ALTER TABLE outbound_workflow ADD COLUMN send_attempt_state TEXT NOT NULL DEFAULT '';
`
