# Decisions

Architectural decisions that are not obvious from the code alone. Newest first.

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
