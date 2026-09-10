# Decisions

Architectural decisions that are not obvious from the code alone. Newest first.

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

Mailgun and Brevo share `netutil.HTTPClient`. In hosted mode it resolves the
destination inside `DialContext`, connects only to an approved public IP
(preserving TLS hostname verification via the URL host), disables proxy
routing, and refuses redirects. Self-hosted mode keeps the default dialer and
honours proxy environment variables.

## In-transaction hydration (BUG-02)

Write paths build their return value from a read inside the same transaction
instead of reading after commit. This removes the failure mode where a
post-commit read error caused the caller to delete MIME that committed rows
already referenced. A commit error is only treated as failure if the row is
genuinely absent.
