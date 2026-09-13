# Decisions

Architectural decisions that are not obvious from the code alone. Newest first.

## Embedded MX: single-container mode with a separate-uid child

`MX_RECEIVE_ENABLED=true` with `MX_EMBEDDED=true` makes `cmd/server` spawn
`gatehouse-mx` as a child process in the same container, under a different
uid/gid (`MX_UID`/`MX_GID`, default 65533) with a scrubbed environment, then
drop its own privileges to the app runtime user (default 65532). The edge has
no `/data` access, no `APP_ENCRYPTION_KEY`, no `DATA_DIR` and no `MX_EDGE_KEYS`,
and stages messages in memory. This is the default compose mode.

- **Why:** operators want `docker compose up -d` to receive mail directly with
  no second service, while still keeping the edge out of the app's data and
  secrets. A separate process under a separate uid gives that with DAC, without
  a new binary or a supervisor package.
- **No separate supervisor, no `GATEHOUSE_ROLE`:** the existing app process is
  already PID 1, so it spawns the child before its privilege drop and keeps
  running to coordinate shutdown. There is no `cmd/gatehouse`.
- **Shutdown is a pipe, not a signal:** after the parent drops uid it cannot
  signal a child owned by a different uid, so the parent passes an inherited
  write pipe and closing it (EOF) tells the edge to stop (`MX_SHUTDOWN_FD`).
- **Refuses rather than degrading:** if the container cannot start as root to
  spawn the child (strict `user:` or `cap_drop: [ALL]`), startup fails with the
  two remedies (remove the hardening, or disable embedded MX and use the
  sidecar). Silent same-uid degradation would defeat the isolation.
- **Isolation is weaker than the sidecar:** DAC + separate uid, no mount or
  network namespaces. `docker-compose.mx-sidecar.yml` remains the recommended
  mode when two containers are acceptable; the embedded mode trades namespace
  isolation for a one-container deployment.
- **In-memory staging, capped:** the edge holds the original bytes in RAM for
  one transaction and releases them after the core ingest, bounded by
  `MX_STAGING_BYTES` (default 256 MiB) across concurrent transactions. A
  reservation that would exceed the budget fails the transaction temporarily
  (SMTP `451`) rather than risking an OOM kill; a single oversize message is a
  permanent `552`, with the configured limit advertised to senders as the ESMTP
  `SIZE` value at `EHLO` and echoed in the rejection text. This is a deliberate
  reversal of the earlier disk-staging design, and it means the
  sidecar/embedded edge needs no writable filesystem and no privilege at all.
- **Credential:** the operator normally sets no secret; the embedded mode
  generates one and shares it with its own core in-process. Supplying
  `MX_EDGE_KEYS` overrides it (the lexicographically smallest key id is used,
  deterministically). The sidecar/remote modes still require `MX_EDGE_KEYS`,
  since the core cannot generate a secret the operator's edge would know.

## Minimal default Compose, opt-in hardening (docker-compose.advanced.yml)

`docker-compose.yml` is the minimal default: one service running the app with
the MX edge embedded (or a plain app when MX is off), no hardening keys. A
separate, self-contained `docker-compose.advanced.yml` adds the hardened stack
(`read_only`, `/tmp` tmpfs, `no-new-privileges`, the optional `cap_drop`/`user`
block, `GATEHOUSE_RUN_UID`/`GID`). `docker-compose.mx-sidecar.yml` is the
two-container sidecar deployment.

- **Why:** the hardening was the default but is not required to run. Defaults
  that restate code defaults (the app's privilege drop already defaults to
  `65532:65532`) and MX keys whose values matched `internal/config` and
  `internal/mxagent` defaults made the shipped compose look far more complex
  than the actual minimum. The default now shows the real minimum; operators who
  want the hardening opt in with one `-f`.
- **MX minimum is one variable:** `MX_RECEIVE_ENABLED=true`. The embedded edge
  credential is generated automatically, so no secret is required.
- **Embedded privilege model:** the app chowns `/data`, spawns the edge under
  `MX_UID`/`MX_GID`, then self-drops. `cap_drop: [ALL]`/`user:` disable this;
  embedded MX refuses to start and points at the sidecar.


## Workflow mail has its own outbound queue (migration 022)

The draft approval-request email (and any future system mail) is **not mailbox
content**. It now lives in `outbound_workflow`, a small queue with its own
status/attempts/claim columns, instead of a `messages` row flagged `internal`.

- **Why:** the `internal` flag leaked through the mailbox model. Workflow mail
  consumed account storage quota, created a user-visible thread when it was the
  only message in one (a "ghost thread"), and its delivery was never reflected
  on the send request — so a draft could be shown as "awaiting approval" even
  when the notification was never delivered. A separate queue removes that whole
  class of bugs by construction.
- **No thread, no quota, no read surface:** workflow mail creates no `threads`
  row, never touches `accounts.storage_used_bytes`, and is invisible to every
  mailbox read path. It carries no `messages.id`, so it cannot be used as a
  forward/reply source. Raw MIME is stored under `$DATA_DIR/workflow/`.
- **Truthful notification state:** `draft_send_requests.notification_status`
  moves `none` → `queued` → `sent`/`failed`. The approval token's expiry clock
  starts only on `sent` (the provider accepted the handoff), so a request whose
  notification failed does not silently lapse; the failure is surfaced to the
  API/UI and the agent.
- **Retention and redaction:** terminal workflow jobs are kept for a fixed 30
  days (matching the outbound delivery log), then swept row + raw file. On
  terminal state the worker redacts `[GH-REQUEST|APPROVE|REJECT:<token>]`
  markers from the retained copy; the token is already dead, this is
  defense-in-depth. The `draft_send_requests` audit row is never swept.
- **Delivery log:** `outbound_delivery_log.workflow_id` attributes a workflow
  attempt in the per-domain log without a `messages` row.
- The `messages.internal` column and its filters remain for backward
  compatibility with already-migrated rows, but new code never writes it.

## In-process privilege drop (no gosu, bind-mounted ./data)

The container image has no `USER` and installs no `gosu`. It boots as root so
`internal/privdrop` can recursively `Lchown` a fresh, root-owned `./data` bind
mount to the runtime UID/GID, then `Setgroups`/`Setgid`/`Setuid` before the
database is opened. `GATEHOUSE_RUN_UID`/`GATEHOUSE_RUN_GID` (default
65532:65532) select the runtime user; the drop is a no-op when the process is
already non-root, so the opt-in hardened compose (`user:`, `cap_drop: [ALL]`)
needs no setuid capability. This mirrors the sibling router service: no host `chown` step,
and the hardened `docker-compose.advanced.yml` ships `read_only`, `tmpfs /tmp`
and `no-new-privileges`.

## Free-text message labels (migration 020)

Migration 020 adds `message_labels(message_id, label, created_at)`, a many-to-many
tag on messages, plus an index on `(label, message_id)`.

- There is deliberately **no label catalogue**. A label exists only while at
  least one message carries it, so there is no definition lifecycle, namespace,
  ownership, slug or orphan state to manage. "Creating" a label is just
  assigning it; "deleting" every occurrence makes it disappear.
- `label` is `TEXT COLLATE NOCASE` and Go (`model.NormalizeLabel`) trims and
  collapses whitespace first, so `Invoices`, `invoices` and `" Invoices "` are
  the same tag. Display casing is the first-assigned form. NOCASE folds ASCII
  only, which is acceptable for typical tags.
- Assignment is a replace-set: `PATCH /v1/messages/{id}` with
  `{"labels":[...]}` sets the exact set, `[]` clears, omission leaves it
  unchanged. It requires Assistant or Owner on the message's inbox, matching
  `UpdateMessageState`. Reads require no label-specific role.
- `GET /v1/messages?label=a&label=b` and the same on `/v1/search` combine labels
  with AND (every listed label must be present). Labels are not indexed in
  `message_fts`; the filter is a join, so it composes with full-text search.
- `GET /v1/labels` derives the distinct in-use labels from `message_labels`,
  scoped to the caller's authorized inboxes.
- A label change emits one durable `message.labels_changed` event carrying the
  resulting set, written in the same transaction as the rows.
- Account-wide rename and delete are deferred: with no catalogue they are bulk
  operations rather than object lifecycle, and no V1 requirement needs them yet.
  Labels carry no color in V1.
- The assistant UI renders chips on message rows (as `<span>`, not a nested
  anchor inside the clickable row) and on the message detail, filters the
  mailbox with `?label=`, and offers message-level and bulk add/remove.

## Control-message log label (migration 019)

Migration 019 adds `inbound_control_messages.subject`, the reviewed draft's
subject snapshotted when an approval control message is consumed.

- The dashboard Recent messages list and the per-domain activity log label such
  rows `Approval: <draft subject>` for approved decisions and
  `Rejected: <draft subject>` for rejected decisions (bare `Approval`/`Rejected`
  when the request could not be resolved), so an operator can tell which draft a
  decision was about and how it went.
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
