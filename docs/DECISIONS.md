# Gatehouse Email — V1 Decision Register

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

**Decision:** V1 inbound internet transport uses Mailgun HTTPS delivery, with Cloudflare Email Routing (via a Worker) as a second adapter.

**Reason:** It removes public SMTP/MX protocol operation from the application while keeping each provider adapter small.

**Boundary:** Provider-specific code remains isolated in `/internal/transport/mailgun` and `/internal/transport/cloudflare`. Adapters authenticate their own webhook and return the explicit binding they verified; the shared mailbox core is transport-neutral.

## D007 — BYO outbound

**Decision:** Users provide outbound credentials.

**V1 adapters:**
- Mailgun HTTP API
- generic SMTP
- Brevo HTTP API

Outbound adapters register through the provider registry, while each credential retains encrypted provider-specific configuration. The Admin UI renders provider-specific fields from a schema each adapter exposes, so users enter an API key and the relevant settings rather than raw JSON.

An account may hold multiple outbound credentials. Each domain designates the credential it sends through (`domains.outbound_credential_id`); there is no account-level default, so mail can only leave through the provider explicitly attached to its domain. Per-inbox credential assignment is removed and no longer accepted by the API.

A domain with no credential is valid: mail is still accepted and queued as `pending` (the outbox worker holds it without consuming retry attempts) until a provider is assigned, rather than being rejected or sent through another domain's provider. On upgrade, existing domains are left with no credential and start paused until an admin assigns one.

**Reason:** Provider credentials are sending-domain-scoped (for example Mailgun's sending domain or an SMTP `from_domain`), and an account-level default risked sending a domain's mail through the wrong provider. Forcing an explicit per-domain choice removes that failure mode while keeping one credential reusable across domains. Queuing instead of rejecting avoids losing mail during setup or a provider outage.

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

**Boundary:** Native Gatehouse Email models remain authoritative for richer features.

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

**Decision:** Inbound delivery idempotency is scoped to `(account_id, provider, canonical original envelope recipient, provider_delivery_id)`. Mailgun uses its authenticated webhook `token`; Cloudflare uses its delivery id or a raw-MIME hash. Thread matching is scoped to the same account and inbox. Unknown recipients use a configured catch-all or receive a terminal transport rejection.

**Reason:** Stable retry behaviour and isolation between mailboxes/accounts. Including the original recipient prevents a catch-all from collapsing distinct deliveries, and excluding the credential row means replacing a credential does not turn a retry into a new delivery.

## D017 — Root encryption key

**Decision:** `APP_ENCRYPTION_KEY` is the root secret for recoverable provider credentials and is backed up separately from `/data`.

**Reason:** Restoring encrypted credentials requires both application data and the root secret.

## D018 — Direct relay credential issuance

**Decision:** The admin UI and REST API issue Hermes relay credentials directly: generate the gateway id, secret, and delivery key, store the encrypted connection, and return a ready-to-paste `.env` block. The one-time enrollment-token flow (`POST /relay/enroll` plus `hermes gateway enroll`) remains for the CLI and hosted provisioning.

**Reason:** A self-hosted operator should be able to create a relay connection and paste the resulting environment variables without a separate token-exchange step or a hosted identity token.

## D019 — Dedicated inbound webhook listener

**Decision:** The application always runs a second HTTP listener on `:8082` that serves only the authenticated inbound webhook routes (`/internal/ingest/mailgun/raw-mime`, `/internal/ingest/cloudflare`, and `/internal/ingest/{provider}`) and `/healthz`. The main listener continues to serve all routes.

**Reason:** This lets an operator expose only the inbound connector to the public internet while keeping the API, web UI, and Relay WebSocket on a private interface or firewall, reducing public attack surface. It remains one process, one container, and one store, so D003 is preserved. Ingest routes stay on the main listener for backward compatibility. The port is fixed rather than configurable so the split works with no extra configuration.

## D020 — Account-owned inbound credentials, domain assignments

**Decision:** Inbound provider secrets are stored as account-owned, encrypted credentials (`inbound_credentials`), and each domain selects one nullable receive credential (`domains.inbound_credential_id`) independent of its outbound credential. Credential reuse is restricted to the same account. Provider identity is immutable on update. The process environment no longer supplies inbound secrets (`MAILGUN_SIGNING_KEY`, `CLOUDFLARE_WEBHOOK_SECRET` are removed).

**Reason:** The concrete BYO requirement is that a self-hosted operator can add a domain, add or select its receive path, and follow the provider's setup steps without editing environment variables or restarting. This supports multiple providers and multiple credentials per account while keeping encryption at rest on the existing `APP_ENCRYPTION_KEY` AES-GCM path. One receive connection per domain is a V1 simplification; provider overlap and failover are deferred. A domain with no receive path is valid to save but cannot accept mail.

**Boundary:** Provider-specific webhook parsing and authentication stay in the transport adapters. The service resolves the account/domain/credential binding and the shared core persists the message.

## D021 — HTML sanitization with bluemonday

**Decision:** Untrusted email HTML is sanitized with `github.com/microcosm-cc/bluemonday` at the rendering boundary (`internal/htmlsanitize`), not escaped at parse time.

**Reason:** Escaping stored markup made HTML messages unreadable in the UI. The parser now stores the raw parsed HTML; the message iframe route and the REST API sanitize it against an allowlist that preserves email formatting (tables, inline styles, images) while stripping scripts, event handlers, forms and embedded objects. Sanitization runs after CID rewriting and before base-target injection.

**Licence:** bluemonday is BSD-3-Clause; attribution is in `THIRD_PARTY_NOTICES.md`.

## D022 — Resend adapter and webhook-triggered pull inbound

**Decision:** Resend is supported as both an inbound and outbound provider in `internal/transport/resend`. Because a Resend `email.received` webhook carries only metadata, the inbound adapter verifies the Svix HMAC signature, resolves the receive binding from the first recipient that maps to a configured domain, then fetches the raw MIME from `GET /emails/receiving/{email_id}` with the account API key before staging it to the bounded temp path. The pull and provider-specific auth stay inside the adapter; the shared core still consumes a staged `InboundMessage`. Delivery dedup uses the Resend `email_id`. Non-`email.received` events return the new `transport.ErrInboundIgnored` sentinel, which the HTTP layer maps to `200` so the provider does not retry an event that requires no ingest.

**Reason:** Resend deliberately excludes bodies, headers, and attachments from webhooks to support large attachments in serverless environments, so a receive webhook must trigger an authenticated fetch. Keeping the fetch inside the adapter preserves the provider-neutral core and the existing dedup/persistence transaction, while the ignore sentinel prevents unrelated event types from becoming retried `500`s.

**Boundary:** Provider auth material, the Svix verification, and the raw-content fetch remain in `internal/transport/resend`. The service resolves the account/domain/credential binding; the core persists the staged message.

## D023 — Same-origin sandbox for the HTML mail frame

**Decision:** The message HTML iframe keeps its sandbox but adds `allow-same-origin`, still without `allow-scripts`.

**Reason:** Without `allow-same-origin` the frame has an opaque origin, so the browser does not send the `SameSite=Lax` session cookie for inline `cid:` images served from `/ui/attachments/{id}/inline`; those images fail authentication and render broken. Scripts remain blocked by the absent `allow-scripts` token, the iframe CSP (`default-src 'none'`, no `script-src`) and the sanitizer, so the residual risk is that sanitized mail can trigger authenticated same-origin GETs. The only state-changing GET is opening a message, which marks it read.

**Scope:** HTML message rendering only.

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
