# Decisions

Architectural decisions that are not obvious from the code alone. Newest first.

## Control-message log label (migration 019)

Migration 019 adds `inbound_control_messages.subject`, the reviewed draft's
subject snapshotted when an approval control message is consumed.

- The dashboard Recent messages list and the per-domain activity log label such
  rows `Approval: <draft subject>` (bare `Approval` when the request could not be
  resolved), so an operator can tell which draft a decision was about.
- The subject is snapshotted at decision time because an approved send deletes
  the draft in the same transaction; a later lookup would find nothing.
- The raw inbound subject is never stored or shown, because it carries the
  one-time approval token. The record keeps only From, request, action, outcome,
  reason and the draft subject.
- Existing control rows keep the empty default and render as bare `Approval`.

## Message client attribution (migration 017)

Migration 017 adds `messages.client_label` and `messages.client_id` so the admin
Recent messages list and the per-domain activity log can show which client sent
an outbound message.

- The columns are denormalized snapshots of the sending credential, resolved at
  enqueue time from the request principal through the same `ActorIdentity`
  helper the draft send-request flow already uses. A rename or deletion of the
  key does not rewrite history.
- A web-UI session send has no credential; it records the literal label `UI`. An
  email-approved send has no client. Inbound, blocked and consumed control mail
  have no client.
- The dashboard renders approval-control rows with the literal label `Control`
  (there is no sending credential to name), and empty client as `—`.
- Existing outbound messages keep the empty default; the information simply did
  not exist before this migration.

## Explicit sender restriction (migration 016)

Migration 016 makes the inbox allow-list an explicit opt-in. Previously an
empty `allowed_senders` list meant "accept all" and a non-empty list meant
"restrict", so there was no way to express "restricted with no senders" and a
non-empty list was the only signal that filtering was on. That also made the
locked approver entry look like it could turn filtering on.

- `inboxes.sender_restricted` (default 0) controls whether the allow-list is
  enforced. When 0, `allowed_senders` is ignored and any sender is accepted;
  when 1, only matching senders (plus the approver) are accepted.
- Existing inboxes with a non-empty list are backfilled to `sender_restricted=1`
  so their behaviour is unchanged.
- `PATCH /v1/inboxes/{id}` accepts `sender_restricted`. For compatibility, a
  request that supplies `allowed_senders` without `sender_restricted` infers
  restriction from a non-empty list, preserving the old API contract.
- Setting an approver never enables restriction. The UI now has an explicit
  "Block senders to this inbox except the allow list below" checkbox and hides
  the allow-list editor when it is off.

## External email approval (migration 015)

Migration 015 adds external email approval on top of the Phase 1 draft
workflow. Rationale is in `docs/DECISIONS.md` D026.

- `draft_send_requests` gains `approver_email`, `token_hash`,
  `token_expires_at` and `approval_message_id`; a request status may now be
  `expired`.
- `inboxes` gains `approver_email`. When set, the approver is always an
  accepted inbound sender and is shown locked in the allowed-senders UI. The
  approver may be changed at any time; an outstanding request keeps the
  approver it was created with and that stored approver's control reply is
  still honoured.
- `draft_attachments.content_hash` stores each attachment's SHA-256. A Go
  backfill hashes existing files; the fingerprint covers these bytes and they
  are re-verified at send.
- `inbound_control_messages` records every consumed approval control email; it
  feeds the per-domain receiving log (kind `approval`) and is the webhook dedup
  key.
- A configured inbox approver makes `request-send` external automatically and
  queues the approval email in the same transaction as the request; the
  optional `{"external": true}` flag is accepted but not required. Approval
  email failure uses the existing outbox/log surfaces; no separate event is
  emitted.
- External approval is a dedicated internal path (not a fabricated Principal);
  it reuses the shared claim/enqueue primitive, so UI and email decisions race
  safely and a decision is single-use.
- Expiry is global (`APPROVAL_EXPIRY_HOURS`, default 48, `0`=never), evaluated
  lazily on request-send and by a sweep in the outbox worker, emitting
  `draft.approval_expired`.
- Sender-authentication (SPF/DKIM/DMARC) capture is deferred.

## Draft send-request migration (migration 014)

Migration 014 adds the human-in-the-loop draft workflow. Rationale is in
`docs/DECISIONS.md` D025.

- `drafts.status` is one of `draft`, `pending_approval`, `rejected`. A pending
  draft is frozen until its request is cancelled.
- `draft_send_requests` is the durable record of a request, the human decision
  and the delivery outcome. It survives the draft being consumed by an approved
  send: `draft_id` is a plain column with no foreign key, while `account_id`
  and `inbox_id` cascade so purging an account or inbox removes requests.
- A partial unique index allows at most one `pending` request per draft, and
  approval updates that row conditionally inside the enqueue transaction, so a
  request can authorize at most one send.
- Decision (`status`) and delivery (`delivery_status`) are tracked separately
  so a provider failure never invalidates an approval.
- Draft bytes stay charged until send consumes them, unchanged by this
  migration.

## Domain-owned provider configuration migration (migration 013)

Migration 013 replaces the account-level connector tables with one optional
sending (`domain_sending_configs`) and one optional receiving
(`domain_receiving_configs`) row per domain, each keyed by a composite
`(domain_id, account_id)` foreign key that cascades with the domain. The
rationale is in `docs/DECISIONS.md` D024.

- Assigned connector rows are copied to per-domain configs, preserving the
  encrypted bytes. A connector shared by several domains becomes independent
  copies, and no re-key is needed because the same `APP_ENCRYPTION_KEY` still
  decrypts them.
- Connectors not assigned to any domain are dropped with the old tables.
- `domains` loses the credential-assignment columns; `inboxes` keeps every
  column, including `allowed_senders_json`, minus the dead outbound foreign key.
- `outbound_delivery_log` replaces `credential_id` with a nullable `domain_id`
  (`ON DELETE SET NULL`, no foreign key to a config row). Attempts are backfilled
  from the attempt's message and inbox; attempts that cannot be attributed keep a
  NULL domain and every attempt is retained. The provider stays an attempt-time
  snapshot.
- Mailbox, auth, message, storage, dedup, and other data are preserved.

The schema baseline (`001`) is gated on the absence of the `schema_migrations`
marker table so a routine boot of an existing database cannot resurrect the
dropped connector tables; the existing marker-recovery and partial-schema
fail-fast behaviour is retained. A partially applied upgrade fails with an
actionable error rather than retrying into a duplicate-column crash loop, so
back up `/data` and `APP_ENCRYPTION_KEY` before running the new binary.

## Atomic migration runner (BUG-04)

Schema migrations and their `schema_migrations` marker are applied in one
transaction on a pinned connection. Migrations that rebuild a table set
`PRAGMA foreign_keys=OFF` outside the transaction and run
`PRAGMA foreign_key_check` before committing. Migrations also declare a
detector so a database whose schema work committed but whose marker was lost
(an interrupted upgrade under the old runner) is reconciled; a partially
applied schema fails fast with an actionable error instead of retrying into a
duplicate-column crash loop.

Runtime migrations are the Go constants in `internal/store/schema.go`. The
duplicate SQL trees (`/migrations`, `internal/store/migrations`) were removed.

## Draft storage accounting (SEC-04)

`accounts.storage_used_bytes` counts message raw MIME plus draft bodies plus
draft attachment files:

```
storage_used_bytes = Σ messages.size_bytes
                   + Σ len(draft.text_body) + len(draft.html_body)
                   + Σ draft_attachments.size_bytes
```

All draft mutations and attachment add/delete adjust the account in the same
transaction as the row change. Sending a draft consumes its rows in the same
transaction as the message insert, so draft bytes are refunded before the
message is charged and are never double-counted. Migration 012 backfills
existing draft usage once.

## Idempotency semantics (SEC-03, SIMP-05)

An idempotency key is scoped to the account but records the mailbox it was
used for. A replay is only returned when the requested mailbox matches the
recorded one and the caller owns it; otherwise the request is rejected with a
conflict. The key-to-message mapping is committed with enqueueing (inside
`CommitOutbound`), not at delivery, so queued and failed messages replay
normally. Replay returns the current message state; `provider_message_id` is
empty until delivery.

## Outbound claim lease (BUG-05)

Claiming a pending message records `claim_owner` and `claim_expires_at` and
leaves `next_attempt_at` for retry scheduling. Startup clears abandoned claims
(safe under the single-process model) and the worker uses a bounded,
cancellation-independent context to record delivery outcomes, so a crash or
cancellation no longer parks a message for 24 hours.

## Revocation cancels live connections (SEC-05)

SSE streams and Relay sockets register the credential scopes they depend on
(`key:`, `sess:`, `user:`, `hrm:`). Revoking, rotating, or rescoping a
credential, deleting a relay connection, logging out, or changing a password
cancels the matching scopes so already-open connections terminate immediately.
Relay outbound operations also revalidate the connection row.

## Hardened outbound HTTP client (SEC-01)

Mailgun, Brevo and Resend share the guarded `netutil` HTTP clients. By default
(and always in hosted mode) they resolve the destination inside `DialContext`,
connect only to an approved public IP (preserving TLS hostname verification via
the URL host), bypass proxy routing, and refuse redirects. Self-hosted mode can
opt out with `ALLOW_PRIVATE_OUTBOUND=true`, which restores the default dialer and
proxy environment; hosted ignores the opt-out. Generic SMTP applies the same
default public-routable check. See `docs/DECISIONS.md` D028.

## In-transaction hydration (BUG-02)

Write paths build their return value from a read inside the same transaction
instead of reading after commit. This removes the failure mode where a
post-commit read error caused the caller to delete MIME that committed rows
already referenced. A commit error is only treated as failure if the row is
genuinely absent.
