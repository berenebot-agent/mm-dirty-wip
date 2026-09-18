# MailMoose — V1 Implementation Plan

Each phase should leave the application runnable and tested.

## Phase 0 — Repository foundation

### Deliverables

- Go module
- directory/package structure
- config loader
- structured logging
- SQLite driver with WAL, busy timeout, serialized writer and bounded read pool
- migrations runner (versioned Go constants in `internal/store`)
- `/healthz`
- reverse-proxy-aware HTTP configuration
- Dockerfile
- multi-arch build workflow
- minimal Compose example
- CI for format, vet/static checks, tests, build

### Gate

```text
docker compose up
→ application starts
→ SQLite created under /data
→ GET /healthz returns healthy
```

---

## Phase 1 — Accounts, auth, domains, inboxes

### Deliverables

- account/user schema
- password/session auth for human UI with SameSite cookies and CSRF protection
- one-shot initial Admin configuration (`INITIAL_ADMIN_*`) and configurable registration mode
- API key issuance and hashing
- mailbox-level roles: Read, Assistant, Owner
- per-inbox role assignments for each API key
- account-level Admin role
- domains table/state
- logical inbox CRUD
- basic admin UI for accounts/domains/inboxes
- audit events for credential/inbox changes

### Gate

A user can log in, create three inboxes, and verify:

- one key can be Owner of one inbox, Assistant on another, and Read on a third;
- Read can read/search/download attachments only;
- Assistant can also delete messages and create/edit drafts;
- Owner can also send/reply and manage that mailbox;
- mailbox roles apply only to their assigned inboxes;
- an Admin key can create/delete inboxes and manage the full account.

---

## Phase 2 — Canonical message store and MIME ingestion

### Deliverables

- canonical `InboundMessage`
- raw MIME streaming to `/data/messages`
- MIME metadata parser
- messages/recipients schema
- attachment metadata extraction
- read/archive state
- deterministic provider idempotency
- bounded message-size handling
- HTML sanitization
- attachment download hardening (`Content-Disposition` + `nosniff`)

### Gate

A fixture MIME message with HTML and an attachment can be ingested, persisted, retrieved, and safely rendered after restart.

Duplicate ingest produces one logical message.

---

## Phase 3 — Mailgun inbound

### Deliverables

- Mailgun webhook authentication
- Mailgun multipart/raw-MIME extraction
- route into canonical ingest
- Mailgun-token idempotency
- catch-all/unknown-recipient behaviour
- error/status mapping
- fixture tests from realistic Mailgun webhook payloads
- setup documentation

### Gate

A real external email sent to the configured Mailgun receiving domain appears in the correct logical inbox.

---

## Phase 4 — Threads, search, attachments

### Deliverables

- thread schema
- standard header-based grouping scoped to account + inbox
- `/v1/threads`
- FTS5 index and `/v1/search`
- attachment listing/download
- cross-inbox search respecting key scopes
- UI thread/search views

### Gate

A three-message reply chain appears as one thread.

Search returns expected messages across only the key's authorized inboxes.

---

## Phase 5 — Durable events and generic realtime API

### Deliverables

- events table
- event append within mailbox transactions
- cursor semantics
- `/v1/events`
- `/v1/events/wait`
- `/v1/events/stream`
- in-process subscriber hub
- reconnect/replay tests
- connection limits/timeouts

### Gate

A client disconnects after event N, receives events N+1..N+K after reconnect, and then resumes live delivery.

---

## Phase 6 — BYO outbound

### Deliverables

- encrypted provider credential storage
- outbound transport registry and interface
- Mailgun HTTP sender
- Brevo HTTP sender
- generic SMTP sender with default public-destination validation
- outbound attachment support with per-provider translation
- outbound-send rate limiting
- draft CRUD
- send endpoint
- reply endpoint
- RFC reply/thread headers
- idempotency keys
- provider status/error recording
- outbound UI setup/test function

### Gate

A user configures their own provider, sends a message, receives a reply, and sees both sides in one thread.

Retrying the same idempotent send resolves to the original send result.

---

## Phase 7 — Hermes Relay

### Deliverables

- Relay enrollment token model
- `/relay/enroll`
- gateway credential storage
- Relay WebSocket endpoint
- Hermes capability handshake
- inbound email → normalized Hermes message event
- thread → stable Hermes conversation mapping
- Hermes outgoing reply/action → canonical send/reply
- reconnect/buffering behaviour required by the Hermes Relay contract
- UI "Connect Hermes" flow with copy-ready CLI command

### Gate

From a Hermes host behind NAT:

```text
create inbox
→ enroll Hermes using one CLI command
→ send external email
→ Hermes receives prompt immediately
→ Hermes reply is delivered through user's outbound provider
```

The Hermes host uses its outbound Relay connection for both directions.

---

## Phase 8 — Agent discovery and compatibility

### Deliverables

- `/.well-known/mailmoose`
- `/agent`
- `/v1/bootstrap`
- `/openapi.json`
- Python examples
- curl examples
- selected openagent.email-compatible endpoints/fields
- compatibility contract tests

### Gate

A fresh capable agent given only URL + key can discover how to list inboxes, read a message, search, and send/reply.

A selected simple openagent.email client fixture can execute compatible list/read/wait/send operations.

---

## Phase 9 — Hosted hardening

### Deliverables

- account-level storage accounting
- retention/cleanup policy
- fair-use/rate controls
- secure-cookie/base-URL production settings
- request/body limits
- operational metrics endpoint/log metrics
- quiesced/snapshot backup and restore procedure
- encryption-key backup/recovery guidance
- migration upgrade tests
- ARM64 deployment validation
- admin operational view

### Gate

The hosted instance can be upgraded with preserved `/data`, restore from backup, enforce account storage, and sustain the agreed load-test profile.

---

## V1 release gate

Release V1 when:

- all acceptance tests pass;
- prebuilt amd64 and arm64 images publish automatically;
- hosted and self-hosted deployments use the same binary;
- core API documentation matches actual behaviour;
- Mailgun inbound and BYO outbound have real end-to-end tests;
- Hermes Relay works from an outbound-only Hermes host;
- backup and restore are demonstrated.
