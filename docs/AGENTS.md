# AGENTS.md — Coding Instructions

## Product

Open Agent Inbox is open email infrastructure for AI agents.

The same application supports:

- a free hosted multi-tenant deployment; and
- a self-hosted Docker deployment.

V1 provides logical inboxes, persistent messages and threads, search, attachments, scoped API keys, realtime events, Hermes Relay integration, a basic human UI, Mailgun inbound transport, and BYO outbound delivery.

## Settled V1 architecture

Use:

- Go
- one compiled application binary
- one application process
- one Docker container
- SQLite in WAL mode
- SQLite FTS5
- filesystem-backed raw MIME storage under `/data`
- embedded web assets
- Mailgun inbound HTTPS transport
- Mailgun HTTP outbound adapter
- generic SMTP outbound adapter
- REST
- durable cursor events
- SSE and long-poll for generic agents
- Hermes Relay as a thin integration adapter

Keep provider-specific code behind narrow transport packages.

## Engineering principles

### Small runtime

Prefer the existing Go process, SQLite, and filesystem for application responsibilities.

Introduce another runtime service only when a measured V1 requirement clearly benefits from it.

### Standard library first

Prefer Go standard library functionality for:

- HTTP
- JSON
- crypto
- MIME primitives
- templates
- embedding
- SQL access

A focused third-party dependency is appropriate where it materially improves security or protocol correctness.

### Explicit SQL

Use `database/sql` plus versioned migrations.

`sqlc` is acceptable as a build-time generator if it improves type safety while keeping runtime dependencies small.

### Streaming I/O

Handle inbound MIME and attachment content with bounded memory.

Large messages should stream to temporary/local files rather than be fully buffered in RAM.

### Durable truth before realtime notification

Persist the message and event transactionally before publishing to SSE, long-poll listeners, or Hermes Relay.

Realtime connections are delivery accelerators. SQLite is the durable source of truth.

### Clean package boundaries

Mailgun-specific structures stay within the Mailgun transport package.

The mailbox core consumes normalized internal types.

Hermes Relay protocol details stay within the Hermes integration package.

### Security and integrity

Treat email content as untrusted input.

- Protect cookie-authenticated state changes with CSRF tokens and secure SameSite cookies.
- Sanitize displayed HTML.
- Serve untrusted attachments as downloads with `Content-Disposition: attachment` and `X-Content-Type-Options: nosniff`.
- Hash verifiable API/session tokens.
- Encrypt recoverable provider credentials with `APP_ENCRYPTION_KEY`; operators back this root secret up separately from `/data`.
- In hosted mode, SMTP connections resolve only to public-routable destinations.
- Scope thread matching to the same account and inbox.
- Deduplicate Mailgun inbound delivery using the authenticated Mailgun delivery token, recorded only after successful persistence.
- Apply mailbox-level roles of Read, Assistant, or Owner, plus an account-level Admin role.

### Tests

Every public API behaviour requires tests.

Every transport adapter requires deterministic fixture tests.

Critical flows require integration tests:

- inbound delivery
- duplicate webhook handling
- event replay
- scoped authorization
- thread grouping
- send/reply
- Hermes Relay delivery
- storage quota enforcement

## Product conventions

Inbox identities are lightweight logical objects.

Account storage is the capacity control for the hosted service.

Generic agents use the canonical REST/event API.

Hermes uses Relay for realtime delivery and the canonical API for mailbox operations where appropriate.

API compatibility with openagent.email should be preserved for overlapping simple operations where semantics match naturally.

Native Open Agent Inbox features retain their own richer models for:

- multi-inbox key scopes
- threads
- search
- replayable event history
- Hermes Relay
- multi-domain operation

## Resource targets

For a quiet self-hosted instance, target:

- low tens of MB to under ~100 MB idle RAM where practical;
- negligible idle CPU;
- one process;
- one container.

Optimise based on measurements. Keep allocations bounded per connection/message.

## Change discipline

Before changing a settled architectural decision:

1. identify the concrete requirement;
2. show why the current design is insufficient;
3. estimate the added runtime/code complexity;
4. document the decision in `DECISIONS.md`.

Prefer a working, understandable V1 over speculative abstraction.
