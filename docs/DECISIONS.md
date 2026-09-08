# Open Agent Inbox — V1 Decision Register

This file records architectural decisions the implementation should treat as settled unless a concrete requirement justifies revision.

## D001 — One codebase for hosted and self-hosted

**Decision:** Hosted and self-hosted deployments share the same core binary, API, mailbox model, and UI.

**Reason:** Dogfooding, simple maintenance, user trust, and easy portability.

## D002 — Go runtime

**Decision:** Implement V1 in Go.

**Reason:** Compact deployment, low idle resource use, strong networking/concurrency, and suitable standard-library primitives.

## D003 — Single-container runtime

**Decision:** Package V1 as one prebuilt Docker image containing one application process.

**Reason:** Simple self-hosting, backup, upgrade, and ARM64/amd64 deployment.

## D004 — SQLite + FTS5

**Decision:** Use SQLite in WAL mode for hosted and self-hosted V1.

**Reason:** Workload is initially modest, inbox records are lightweight, and SQLite keeps the operational footprint very small.

**Extension point:** Add a PostgreSQL store when measured hosted load makes it beneficial.

## D005 — Local filesystem MIME store

**Decision:** Store canonical raw MIME under `/data/messages`.

**Reason:** Simple persistence and backup, efficient streaming, and minimal runtime dependencies.

**Extension point:** Add object storage when hosted storage scale warrants it.

## D006 — Mailgun reference inbound transport

**Decision:** V1 inbound internet transport uses Mailgun HTTPS delivery.

**Reason:** It removes public SMTP/MX protocol operation from the application while keeping the provider adapter small.

**Boundary:** Mailgun-specific code remains isolated in `/internal/transport/mailgun`.

## D007 — BYO outbound

**Decision:** Users provide outbound credentials.

**V1 adapters:**
- Mailgun HTTP API
- generic SMTP
- Brevo HTTP API

Outbound adapters register through the provider registry, while each credential retains encrypted provider-specific JSON configuration.

**Reason:** Users retain provider choice, pricing, and sender reputation while hosted operation remains simple.

## D008 — Replayable event history

**Decision:** SQLite events are the persistent realtime history.

**Interfaces:**
- incremental REST
- long-poll
- SSE

**Reason:** One recovery model supports both realtime and disconnected clients.

## D009 — Hermes Relay integration

**Decision:** Hermes Relay is the preferred realtime Hermes integration.

**Reason:** Hermes can maintain an outbound-only authenticated connection and receive new mail through native gateway messaging semantics.

**Boundary:** Relay remains an adapter over the mailbox/event core because its protocol is externally versioned.

## D010 — REST is canonical

**Decision:** REST/event APIs are the canonical generic agent interface.

**Reason:** Small token footprint, direct debugging, self-documentation, and client independence.

**Extension point:** MCP can be provided as an optional adapter.

## D011 — openagent.email compatibility

**Decision:** Preserve compatible endpoint names/fields where semantics align cleanly.

**Reason:** Familiar integration and easier migration.

**Boundary:** Native Open Agent Inbox models remain authoritative for richer features.

## D012 — Lightweight web UI

**Decision:** Use embedded server-rendered or similarly compact web assets served by the Go binary.

**Reason:** Human administration requires forms, lists, search, and message inspection rather than a separate application runtime.

## D013 — Account storage as hosted capacity control

**Decision:** Hosted V1 uses account-level storage/fair-use controls while logical inbox identities remain cheap to create.

**Reason:** Storage reflects real infrastructure consumption more closely than inbox count.

## D014 — Mailbox roles plus account Admin

**Decision:** Permissions use three mailbox-level roles plus one account-level administrative role.

Mailbox roles are assigned independently per inbox:

```text
Read       → read messages/threads, search, attachments
Assistant  → Read + delete messages + create/edit drafts
Owner      → Assistant + send/reply + mailbox settings
```

A single key can have different roles on different inboxes.

Account administration is separate:

```text
Admin      → full account access
```

Admin can create/delete inboxes, manage domains, keys/users, outbound providers, Hermes connections, and account-wide settings.

**Reason:** This matches agent delegation: mailbox Owner grants full mailbox operation, while Admin grants account-wide administration.

## D015 — SQLite concurrency

**Decision:** Use one serialized SQLite writer connection plus a small bounded read pool, with WAL and a busy timeout.

**Reason:** Predictable concurrent behaviour with minimal runtime complexity.

## D016 — Mail integrity boundaries

**Decision:** Mailgun webhook tokens provide inbound delivery idempotency. Thread matching is scoped to the same account and inbox. Unknown recipients use a configured catch-all or receive a terminal transport rejection.

**Reason:** Stable retry behaviour and isolation between mailboxes/accounts.

## D017 — Root encryption key

**Decision:** `APP_ENCRYPTION_KEY` is the root secret for recoverable provider credentials and is backed up separately from `/data`.

**Reason:** Restoring encrypted credentials requires both application data and the root secret.

## Future extension register

Potential future additions include:

- additional inbound transport adapters
- custom-domain automation improvements
- optional managed outbound
- object storage
- PostgreSQL
- optional MCP bridge
- broader agent platform adapters
- richer team/organisation administration

Additions should preserve the canonical mailbox/event model.
