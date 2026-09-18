# Gatehouse Mail — V1 Decision Register

This file records architectural decisions the implementation should treat as settled unless a concrete requirement justifies revision. It is the single decision register for the project: the former root `DECISIONS.md` implementation notes were merged here as D038–D051.

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

> **Superseded in part by [D024](#d024--domain-owned-sending-and-receiving-configuration).**
> The account-level outbound credential pool, named connector reuse, and
> `domains.outbound_credential_id` assignment below were replaced by one optional
> sending configuration owned by each domain. The BYO rationale, provider
> registry, and adapter-schema boundary remain. Text below is retained as the
> historical decision.

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

**Boundary:** Native Gatehouse Mail models remain authoritative for richer features.

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

**Decision:** The application always runs a second HTTP listener on `:8082` that serves the authenticated inbound webhook routes (`/internal/ingest/mailgun/raw-mime`, `/internal/ingest/cloudflare`, and `/internal/ingest/{provider}`), the optional MX routes (`/internal/mx/resolve` and `/internal/mx/ingest`, present only when `MX_ENABLE=true|remote`), and `/healthz`. The main listener continues to serve all routes.

**Reason:** This lets an operator expose only the inbound connector to the public internet while keeping the API, web UI, and Relay WebSocket on a private interface or firewall, reducing public attack surface. It remains one process, one container, and one store, so D003 is preserved. Ingest routes stay on the main listener for backward compatibility. The port is fixed rather than configurable so the split works with no extra configuration.

## D020 — Account-owned inbound credentials, domain assignments

> **Superseded in part by [D024](#d024--domain-owned-sending-and-receiving-configuration).**
> Inbound secrets are no longer account-owned credentials selected by a nullable
> assignment; each domain owns at most one receiving configuration. The
> provider-boundary and encryption-at-rest rationale below remain. Text below is
> retained as the historical decision.

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

## D024 — Domain-owned sending and receiving configuration

**Decision:** Each domain owns at most one optional sending configuration and at
most one optional receiving configuration. There is no account-level connector
pool, no named or reusable connector, no shared assignment, and no standalone
connector API. Sending and receiving are managed directly on the domain:

```http
GET    /v1/admin/domains/{id}/sending
PUT    /v1/admin/domains/{id}/sending
DELETE /v1/admin/domains/{id}/sending
GET    /v1/admin/domains/{id}/receiving
PUT    /v1/admin/domains/{id}/receiving
DELETE /v1/admin/domains/{id}/receiving
GET    /v1/admin/domains/{id}/sending/deliveries
GET    /v1/admin/domains/{id}/receiving/deliveries
```

`PUT` takes `{provider, config}` (receiving also takes `regenerate_secret`). A
generated Cloudflare Worker secret is created only when missing, preserved by a
normal same-provider save, and replaced only by an explicit regenerate. Domain
creation accepts `name` only; `PATCH /v1/admin/domains/{id}` accepts
`catch_all_inbox_id` only. Supersedes the affected portions of
[D007](#d007--byo-outbound) and [D020](#d020--account-owned-inbound-credentials-domain-assignments).

**Requirement:** The old model exposed an account-level pool of named
credentials plus explicit per-domain assignment selectors in both the API and
the UI. It was insufficient because the underlying provider configuration is
already sending/receiving-domain-scoped (for example Mailgun's sending domain or
an SMTP `from` domain), so a reusable named connector invited sending a domain's
mail through another domain's provider. The separate connector lifecycle also
required orphan handling and an assignment step that had no product value, and
the UI split configuration across a connector screen and a domain screen. A
domain-first model makes ownership, the pause-on-missing-provider behaviour, and
the per-domain two-way activity log (outbound attempts plus delivered and
blocked inbound mail) explicit.

**Complexity:** Qualitative, not measured. The change removes the standalone
connector CRUD, assignment fields, and orphan handling while adding two
domain-keyed tables, one forward migration, and domain-scoped handlers. It
introduces no new runtime service and no new dependency: it reuses the existing
SQLite store, AES-GCM encryption with `APP_ENCRYPTION_KEY`, provider registry,
and adapters. Net surface area is smaller than the connector model.

**Migration:** Migration 013 copies assigned connector bytes into independent
per-domain configs without re-keying, drops connectors not assigned to any
domain, and preserves mailbox, auth, message, storage, and delivery-log data.
The `domains` table loses the credential-assignment columns;
`outbound_delivery_log.credential_id` becomes a nullable `domain_id`
(`ON DELETE SET NULL`, no foreign key to a config row), with attempts backfilled
from the attempt's message and inbox and unattributable attempts retained with a
NULL domain. The schema baseline (`001`) is gated on the absence of the
`schema_migrations` marker table, so a routine boot of an existing database
cannot resurrect the dropped connector tables; a partially applied upgrade fails
with an actionable error rather than retrying into a duplicate-column crash
loop. Back up `/data` and `APP_ENCRYPTION_KEY` before running the new binary.

## D025 — Draft send requests with human approval

**Decision:** An Assistant may draft and request send; an Owner authorizes. A
send request is recorded as a durable `draft_send_requests` row independent of
the draft, because a successful send consumes (deletes) the draft. The request
stores the requester, a fingerprint of the reviewed content, the decision
(actor, method, optional feedback), and the delivery outcome separately from the
decision. A draft is frozen (`drafts.status = pending_approval`) while a request
is outstanding; edits are refused until the request is cancelled. Approving
claims the pending request in the same transaction that enqueues the outbound
message, so a request can never authorize two sends. UI approval and eventual
external approval converge on the existing draft-send/outbound path.

**Requirement:** The product rule is that an agent expresses intent to send but
cannot self-authorize. The existing draft model had no workflow state, and
sending deleted the draft atomically, so approval could neither be recorded nor
reported after the fact. Separating the decision from delivery also satisfies
the rule that a provider failure must not require a second approval.

**Complexity:** Qualitative. Adds one column and one table (migration 014), a
store workflow module, service methods, API endpoints, durable `draft.*` events,
and UI for review and decisions. No new runtime service, dependency, or
infrastructure: it reuses the existing draft, outbound, event, and storage
subsystems. External email approval is deliberately out of scope for this phase;
the request table is shaped so approver identity, a hashed token, and an expiry
can be added by a later migration.

**Migration (014):** `drafts.status` is one of `draft`, `pending_approval` or
`rejected`. `draft_send_requests` is the durable record of the request, decision
and delivery outcome; it survives the draft being consumed by an approved send
(`draft_id` is a plain column with no foreign key, while `account_id` and
`inbox_id` cascade so purging an account or inbox removes requests). A partial
unique index allows at most one `pending` request per draft, and approval updates
that row conditionally inside the enqueue transaction. Decision (`status`) and
delivery (`delivery_status`) are tracked separately so a provider failure never
invalidates an approval. Draft bytes stay charged until send consumes them.

## D026 — External email approval for draft sends

**Decision:** Phase 2 lets an external person authorize a draft send entirely
by email, with no Gatehouse account. An inbox may configure one optional
`approver_email` (Owner/Admin via `PATCH /v1/inboxes/{id}`). A configured
approver makes `request-send` external automatically: Gatehouse freezes the
draft, records a request carrying the approver, a hashed one-time token and an
expiry, and queues an approval email from the agent inbox through the existing
outbound path in the same transaction. The optional `{"external": true}` flag
is accepted for clarity but is not required and only errors when the inbox has
no approver. The email carries the reviewed draft and its real attachment
bytes, plus `mailto:` Approve/Reject actions that pre-address a control reply
to the inbox; nothing happens until the approver sends that reply.

**Trigger:** External approval is a property of the inbox, not the request. An
agent that simply calls `request-send` gets approval by email whenever the
inbox has an approver configured; it never has to know the transport. An inbox
with an approver cannot create a Gatehouse-only request (the owner can still
approve or reject in the UI).

An inbound message whose decoded subject contains exactly one strict
`[GH-APPROVE:<token>]` or `[GH-REJECT:<token>]` is consumed as workflow input.
The token is 96-bit (12 random bytes, 16 base64url characters), stored only as a
SHA-256 hash, and the reply subject appends the draft subject so the reply links
back to the draft; the parser tolerates surrounding text.
An approver may also decide by replying to the approval email directly, without
using the buttons. The email body carries a visible `[GH-REQUEST:<token>]`
reference line; a plain-text or HTML reply quotes it, binding the reply to the
same one-time request whether or not the client honours `mailto:`. The action is
read only from the first non-empty line of the approver's own text, after
cutting at the first quoted-history boundary: a first token of
`Approve`/`Approved` or a common affirmative (`yes`, `yep`, `yeah`, `ok`,
`okay`, `accept`/`accepted`, `confirm`/`confirmed`, `authorize`/`authorized`/
`authorised`, `lgtm`, `y`) in any case approves, and any other readable line
rejects (an empty or unreadable reply rejects too). A first affirmative is
honoured even when the rest of the line qualifies it ("Yes, don't approve"),
because no negation is parsed; the emailed instruction names the exact word and
the gate remains the nominated approver's reply. This is the deliberate, narrow
exception to the rule that a parser must not look for natural-language words
like "approve": the word is only the action selector, while the token, the
stored request and the sender binding remain the authentication, and it is read
solely from the approver's new text so the quoted original — which always
contains the word "Approve" and the control tokens — can never select it. The
`mailto:` buttons are retained for clients that open them.
Control-format mail is handed to the control handler regardless of the sender
allow-list, because the handler validates the live token and the exact stored
approver and because the inbox's approver setting may have changed after the
request was created; non-control mail still passes the allow-list. The
configured approver is always an accepted sender (shown as a locked,
non-removable entry in the allowed-senders UI).
Consumed control mail is never stored as a message, FTS-indexed, relayed or
marked unread; it is recorded in `inbound_control_messages` for the per-domain
activity log and webhook dedup, labelled `Approval: <draft subject>` for an
approval or `Rejected: <draft subject>` for a rejection (the raw
inbound subject, which carries the token, is never stored or shown). A decision
is valid only when the token is live
(not expired/decided), the request is pending, the RFC From equals the stored
approver, and the frozen content fingerprint still matches. A valid approve
claims the request and enqueues the frozen draft through the shared outbound
primitive; a valid reject stores optional feedback and returns the draft to
`rejected`. The inbox approver may be changed at any time; an outstanding
request keeps the approver it was created with.

**Requirement:** The product rule is that an agent expresses intent but cannot
self-authorize, and that a nominated human approves without needing a Gatehouse
account. Email is the lowest-friction transport, but a bare subject token is
weak: it can be guessed, replayed, forwarded or forged. The design therefore
pairs the secret token with a nominated sender address, enforces single use at
the same store primitive as UI approval, and keeps expiry (default 48h, global
`APPROVAL_EXPIRY_HOURS`, `0`=never) actively releasing the draft.

**Complexity:** Qualitative. Adds migration 015 (send-request approver/token/
expiry columns, inbox approver columns, `draft_attachments.content_hash`, and
`inbound_control_messages`), a strict control-subject parser and feedback
extractor, a dedicated internal external-approval entry point, an expiry sweep
in the outbox worker, an approval-email builder, API/UI approver settings, and
the `draft.approval_expired` event. No new dependency or runtime service; it
reuses the outbound, event, storage and SQLite subsystems.

**Sender binding:** the decision is bound to the provider-attested envelope
sender (`transport.InboundMessage.EnvelopeFrom`) rather than the attacker-
controlled MIME `From:` header; both must equal the stored approver and a
missing envelope is rejected (see D029). Full SPF/DKIM/DMARC evidence capture
remains deferred. Per-request arbitrary approvers and approval-token resend are not
supported; the approver is an inbox setting and a new request issues a new
token. Section 20 of `roadmap/roadmap_assistant.md` (arbitrary per-request
approver) is superseded by the inbox-level setting.

**Integrity fix:** the frozen fingerprint now covers each attachment's SHA-256
bytes (not just filename/type/size), and the bytes are re-verified at send, so
an approval binds to the exact content reviewed. This closes a Phase 1 gap.

**Migration (015):** `draft_send_requests` gains `approver_email`, `token_hash`,
`token_expires_at` and `approval_message_id`, and a request status may now be
`expired`. `inboxes` gains `approver_email`; when set, the approver is always an
accepted inbound sender and is shown locked in the allowed-senders UI, and the
approver may be changed at any time while an outstanding request keeps the
approver it was created with. `draft_attachments.content_hash` stores each
attachment's SHA-256 (a Go backfill hashes existing files; the fingerprint covers
these bytes and they are re-verified at send). `inbound_control_messages` records
every consumed approval control email; it feeds the per-domain receiving log
(kind `approval`) and is the webhook dedup key. A configured inbox approver makes
`request-send` external automatically and queues the approval email in the same
transaction as the request; external approval is a dedicated internal path (not a
fabricated Principal) that reuses the shared claim/enqueue primitive, so UI and
email decisions race safely and a decision is single-use. Expiry is global
(`APPROVAL_EXPIRY_HOURS`, default 48, `0`=never), evaluated lazily on
request-send and by a sweep in the outbox worker, emitting
`draft.approval_expired`. Sender-authentication (SPF/DKIM/DMARC) capture is
deferred.

## D027 — Explicit sender restriction toggle (migration 016)

**Decision:** An inbox's sender allow-list is only enforced when an explicit
`sender_restricted` flag is set. When it is off, `allowed_senders` is ignored
and any sender is accepted; when on, only matching senders (plus the configured
approver) are accepted. The web inbox editor exposes this as a "Block senders
to this inbox except the allow list below" checkbox and hides the allow-list
editor while it is off. `PATCH /v1/inboxes/{id}` accepts `sender_restricted`;
supplying `allowed_senders` without it infers restriction from a non-empty list
so existing API clients keep working. Migration 016 backfills existing inboxes
with a non-empty list to restricted.

**Reason:** The previous rule (empty list = open, non-empty = restricted) could
not distinguish "restricted with no senders" from "open", and made the locked
approver entry look like it enabled filtering. Setting an approver must never
restrict general receiving — it did not in practice, but the UI implied it
could. An explicit flag makes the intent unambiguous in the API, the UI and the
inbound check.

**Complexity:** One column, one backfill, a store setter, an API field and a UI
checkbox; no new dependency or runtime service.

## D028 — Public-routable outbound destinations by default

**Decision:** Every outbound transport (HTTP provider clients and generic SMTP)
must resolve its destination to a public-routable address before connecting,
in all modes. `MODE=hosted` always enforces this. A self-hosted operator can opt
out with `ALLOW_PRIVATE_OUTBOUND=true` for a private gateway or local relay; the
opt-out is ignored in hosted mode. Provider HTTP API bases must also be HTTPS
and must not name a loopback, private or link-local IP literal when enforcement
is on. The shared `netutil` client validates inside `DialContext` (so DNS cannot
rebind between validation and connection), refuses redirects, and bypasses the
proxy environment while enforcement is active; enforcement off restores the
default transport and proxy behaviour.

**Reason:** The previous guard was gated behind `MODE=hosted`, so the default
self-hosted deployment skipped it and an account Administrator could point a
provider's `api_base` at loopback, RFC1918 or the cloud metadata address and
read the upstream response through the delivery log. The control belongs on by
default; the operator, not the mode default, should decide to weaken it. Turning
the proxy off while enforcing closes the proxy escape hatch.

**Complexity:** One config flag, a shared `netutil` gate, and adapter wiring; no
new dependency or runtime service.

## D029 — Approval sender bound to the provider envelope sender

**Decision:** An email approval is accepted only when the provider-attested
envelope sender presented in the inbound webhook equals the stored approver
address, and the message's MIME `From:` header equals it too. A missing or
mismatched envelope is consumed and recorded as invalid, never as a decision.

**Reason:** The MIME `From:` header is attacker-controlled, so binding a
one-time-token decision to it let a token holder spoof the approver. The
envelope sender is what the receiving provider observed and what SPF covers.
The token is additionally hidden from the mailbox read surface (the workflow-mail
`internal` flag); this decision keeps the human-in-the-loop boundary intact even
if a token leaks by another channel.

**Boundary:** The envelope sender is already normalized across the Mailgun,
Cloudflare and Resend adapters (`transport.InboundMessage.EnvelopeFrom`).
Providers that do not supply it fail closed; the generated Cloudflare Worker
sends it. Full SPF/DKIM/DMARC evidence capture remains a future extension.

## D030 — Inbox aliases: inbound address routing (migration 021)

**Decision:** An inbox may carry aliases: alternate inbound addresses
(`local@domain`) that deliver to that inbox instead of creating a separate
mailbox. An alias owns no messages, storage or settings; it is a pure
address-to-inbox mapping. Aliases may live on any domain the account owns, so an
alias on one domain can deliver to an inbox on another domain of the same
account. Resolution precedence per envelope recipient is: exact inbox, then
alias on the recipient's domain, then the domain catch-all. Resolution returns
the matched route, and the ingest core applies a route-aware binding check:
exact and catch-all matches must stay on the authenticated domain and account
(unchanged), while an alias only must belong to the authenticated account. The
alias set is replaced wholesale via `PATCH /v1/inboxes/{id}` (`aliases`) and the
inbox add/edit dialog's Aliases tab; collisions with a real mailbox local part
are rejected in Go because SQLite cannot express cross-table uniqueness. Aliases
are inbound-only: replies still send from the inbox's primary address.

**Reason:** Cross-domain routing and address consolidation are common agent
needs (many public addresses → one working inbox). Modelling them as a
mailbox-to-mailbox forward would have required a second delivery path, loop
detection, and a fan-out/dedup model change, and would have split mailbox
settings between source and target. An address-to-inbox alias reuses the
existing resolver, threads, events, quota, FTS and Relay unchanged, and lets the
target inbox's allow-list/approver rules apply uniformly. Cross-domain is safe
because the mail still authenticates against the recipient domain's receiving
provider and both domains belong to the same account; relaxing only the alias
route keeps the stricter domain binding on the exact and catch-all paths.

**Complexity:** Qualitative. Adds migration 021 (`inbox_aliases`), a
route-aware `ResolveRecipient`, transactional alias set/reconcile, an Aliases
tab with an alias editor and a flag-column indicator, and the `aliases` API
field. No new dependency or runtime service. Reply-as-alias (send-as) is
deferred; aliases are inbound only.

## D031 — Optional Go SMTP (MX) edge with signed core handoff

**Decision:** Direct internet mail on TCP port 25 is received by an optional Go
sidecar, `cmd/mx`, built from this repo into the same image as `cmd/server` and
run as a separate non-root process (an explicit exception to
[D003](#d003--single-container-runtime); one image is not one process). The
edge owns SMTP framing, bounded staging of the original bytes, and
SPF/DKIM/DMARC computation; it is policy-free. All routing authorization,
policy, quota and durable storage stay in the core behind two authenticated
endpoints on the inbound connector:

```http
POST /internal/mx/resolve   # bounded recipient list -> per-recipient routing
POST /internal/mx/ingest    # one recipient + original MIME -> durable disposition
```

The edge holds no domain/policy snapshot, no database mount and no
`APP_ENCRYPTION_KEY`. Authentication failures are a durable Spam delivery, not
an SMTP rejection. Authentication-based SMTP rejection and `on_auth_fail=delete`
are deferred. Per-domain `enforcement=moderate|hard` selects the local Spam
disposition; the published DMARC policy is preserved and displayed, not
pretended to be enforced.

**Reason:** The provider-webhook architecture (D006) has no way to receive mail
for a domain that does not already route through Mailgun/Cloudflare/Resend, and
those providers are the wrong trust and cost model for a self-hosted operator
who owns the domain. An in-repo Go edge reuses the existing module, build and
image, keeps one artifact to ship, and stays small (standard library plus three
narrow, MIT-licensed protocol libraries). Keeping policy and durable state in
the core means the edge cannot authorize a domain, grant quota or forge durable
truth, and a compromised edge credential is separately revocable.

**Dependencies:** `github.com/emersion/go-smtp` v0.25.0 (MIT),
`github.com/emersion/go-msgauth` v0.7.0 (MIT) and `blitiri.com.ar/go/spf`
v1.6.0 (MIT), with transitive `go-sasl`, `go-message`, `go-milter` and the
test-only `yaml.v3`. Recorded in `THIRD_PARTY_NOTICES.md`; approved in the root
`AGENTS.md`.

**Wire contract:** HMAC-SHA256 over the SHA-256 digests of the request metadata
and body (protocol version, timestamp, request ID, key ID, envelope sender,
client IP, HELO, normalized auth evidence, content digest, size, accepted
recipient set) with constant-time verification and overlapping accepted keys
for rotation. Signing digests lets both ends stream a large body through its
hash with bounded memory. The ingest body is a raw two-part stream (a 4-byte
metadata length, the metadata JSON, then the original MIME), so the message is
never base64-buffered or JSON-escaped; resolve, being tiny, uses the same framing
for one code path. The core parses the MIME once and fans out internally to the
accepted recipient set, so neither side holds a copy per recipient. Key IDs map
to configured operator credentials; remote links require verified TLS. Timestamp
skew bounds replay duration, not replay itself; request IDs are bound to the
authenticated fingerprint and conflicting reuse is rejected. Replayed retries
return the recorded durable disposition.

**Retry identity (refines [D016](#d016--mail-integrity-boundaries)):** the
edge delivery fingerprint is a versioned digest over canonical envelope sender,
canonical recipient and SHA-256 of the original incoming MIME, computed before
any local trace/header change. RFC Message-ID is descriptive metadata, never the
delivery token. Core deduplicates with durable, bounded delivery receipts scoped
by account, provider and envelope recipient, serialized with message/quota/event
persistence so concurrent MX nodes cannot duplicate side effects. Receipts are
retained **7 days**, which covers the supported sender retry window, HTTP replay
window and expected outage recovery, survive message deletion for that horizon,
and are swept by the existing worker.

**Spam:** `is_spam` is a computed view over `messages` (not a separate table),
with bounded `auth_results_json` and a classification reason. Spam counts toward
quota and retains MIME, attachments, identity and recovery. Every read/action
path (lists, unread counts, default search, threads, replies, message waits,
Relay payload rebuilds) excludes or revalidates Spam consistently, and release
commits a durable `message.spam_state_changed` state-change event with old/new
state.

**Complexity:** Qualitative. Adds `internal/mxwire`, a pure policy engine and
normalized auth types, a `mx` receiving provider, an explicit authenticated MX
service entry point, migrations for receipts/`is_spam`/`auth_results_json`, the
`cmd/mx` edge with `MX_VERIFY_SPF|DKIM|DMARC` toggles, and Spam UI/API/Relay
handling. It stays within the existing SQLite store, event bus and filesystem
MIME store. No new runtime service beyond the optional edge; no caching, ARC,
BIMI, durable edge queue or SMTP rejection in V1. A separate embedded
single-container mode later superseded the "all-in-one supervisor" deferral:
`cmd/server` spawns the edge as a separate-uid child before dropping, and the
shipped `docker-compose.yml` enables it by default; see D038 and D039 below.

**Deferrals:** authentication-based SMTP rejection / `on_auth_fail=delete`; ARC
verification and trusted-forwarder policy; BIMI; a Haraka/mailauth alternate
edge; policy snapshots and local rejection; durable edge queue and end-to-end
HA; scoped credential-to-domain binding; reputation and content filtering; DMARC
report generation; authenticated submission/relay.

## D032 — Send-as-alias: outbound identity (migration 024)

**Decision:** An inbox may send from any address in its alias set, not only its
primary address. The sender is chosen per message (`sender` on `POST /v1/send`
and on reply; a `From` select in the UI compose/reply form), with a per-inbox
`default_sender` that preselects it. A draft stores its chosen sender
(`drafts.from_address`) so an approved send uses it; the sender is folded into
the draft's approval content fingerprint, and the approval-request notification
continues to send from the inbox primary while stating the intended From in its
body. The sending provider is resolved from the **chosen address's own domain**:
for an alias this is the alias's domain, which may differ from the inbox's.
`messages.sending_domain_id` records that domain at enqueue so delivery,
per-domain log attribution and requeue-on-save resolve the correct config
without re-parsing MIME (NULL falls back to the inbox domain for pre-024 rows).
Assistants may set a draft's sender (they can draft), but executing a send —
and therefore any non-primary send — remains Owner-only under the existing send
authorization path. This revisits D030's "aliases are inbound only; reply-as-
alias (send-as) is deferred".

**Reason:** Aliases already exist as account-controlled addresses; letting them
send completes the identity, so a consolidated inbox can correspond as `sales@`
or `billing@` rather than only its primary. Resolving the provider by the From
domain is what makes cross-domain aliases deliverable: the provider credentials
and DKIM identity must match the domain on the envelope, not the inbox's. A
recorded `sending_domain_id` keeps the durable log, quota and retry semantics
correct when the sender differs from the inbox domain, and keeps
`RequeuePendingForDomain` working when the alias domain's config is (re)saved.

**Complexity:** Qualitative. Adds migration 024
(`inboxes.default_sender`, `drafts.from_address`,
`messages.sending_domain_id`), sender resolution in the store, sender plumbing
through `SendInput`/draft approval, the `sender`/`default_sender` API fields, and
the UI From select plus default-sender cascade. No new dependency, no new
runtime service, no new transport adapter. Hermes Relay send-as remains
deferred (the Relay send protocol has no sender field yet).

## D033 — Outbox worker panic containment

**Decision:** The background outbox worker contains panics per unit of work
rather than letting them exit the process. Each maintenance/delivery pass
(`tick`) is wrapped in a per-step recover, and each individual message and
workflow delivery is wrapped again so a panic in one item is recorded as a
failed attempt (with backoff) and the loop continues. The panic **value** is
never logged, only its type and the goroutine stack — matching the HTTP
recoverer's secret-safety rule; stack frames carry no request or credential
values. Recording a recovered failure runs under a second, absorbing recover so
a fault while persisting the outcome cannot itself terminate the process.

**Reason:** Under the single-process, `restart: unless-stopped` deployment, a
deterministically panicking message previously exited the process, was
reclaimed on restart, and panicked again — a crash loop that could take mail
down for every inbox on a shared instance. Containing the fault at the message
granularity converts it into one failed message plus a healthy queue. A single
recover around the whole worker loop would be worse: it would silently kill the
worker goroutine with the process still running, stalling all mail.

**Complexity:** Local. Adds `tick`/`recoverUnit`/`safeFail` in
`internal/app/worker.go` and `failMessagePanic`/`failWorkflowPanic` helpers; no
schema change, no new dependency, no new runtime service.

## D034 — Alias sender display names (migration 025)

**Decision:** Each inbox alias may carry an optional sender display name.
Sending from an alias uses `Name <alias@domain>` when a name is set, and falls
back to the inbox's `display_name` when it is not; the primary address always
uses the inbox display name. The inbox UI requires a name when adding or editing
an alias (the API still accepts an empty name and falls back, for compatibility
with older clients and imports). A draft records the resolved name
(`drafts.from_name`), it is part of the draft's frozen approval fingerprint, and
the approval-request body shows the intended `Name <address>`. Names are
operator-controlled: alias management is already Owner/Admin-only (the inbox
`PATCH` requires Owner or Admin, the UI is Admin), so an agent cannot set a
display name. Names may not contain commas, newlines or control characters and
are length-capped at 128. The API adds a `alias_names` object map (address →
name) alongside the existing `aliases` array, so older clients that send only
`aliases` are unaffected.

**Reason:** The display name is a property of the sending identity, not the
mailbox: every major provider (Gmail "send mail as", Fastmail identities)
supports a per-address name. Without it, a role alias like `sales@` sends as the
inbox's personal name, which reads wrong and is a common cause of misfiled mail.
Routing and SPF/DKIM/DMARC key off the address/domain, so the name is purely
cosmetic — but it is still a phishing surface, which is why it stays
operator-only and is frozen into the approval so an approver reviews the exact
name that will be shown.

**Complexity:** Qualitative. Adds migration 025
(`inbox_aliases.display_name`, `drafts.from_name`), name plumbing through alias
set/draft storage and the send path, the `alias_names` API field, and an
alias editor where each row shows the sender name with its address beneath and
an add/edit popup collects both. The Aliases tab lists aliases first (with the
add button above them), then a Primary / Default Address section whose dropdown
lists the main address and each alias as `Name (email)` — the primary name being
the inbox display name. No new dependency or runtime service.

## D035 — External review remediation (security, correctness, UX)

**Decision:** A consolidated review of the V1 tree produced a batch of fixes
across the approval boundary, MX edge, outbound queue, crypto and UI:

- **Approval preview is the exact sendable message.** The approval-request
  email and the UI review page now show Bcc (which the generated MIME never
  carries) and both the text and HTML body alternatives. The HTML alternative
  is included as escaped source in the email and never rendered inside
  Gatehouse's own approval mail; only the sandboxed UI view renders it.
- **Approval/rejection processing is retry-safe.** A transient store failure
  while recording a decision is returned, so the webhook provider re-delivers
  and the MX edge answers a temporary SMTP failure; only terminal outcomes
  (already decided, expired, forbidden, not found) are acknowledged and
  consumed.
- **MX ingest authenticates before staging.** The declared `content_digest` is
  part of the signed canonical string, so the core verifies the HMAC from the
  bounded metadata prelude before writing any body byte; the streamed body is
  then checked against that digest. No wire change was needed.
- **DMARC organizational domain uses the full Public Suffix List.**
  `golang.org/x/net/publicsuffix` replaces the hand-maintained approximation, so
  multi-label, wildcard and private suffixes (and therefore two tenants of a
  hosted suffix such as `*.github.io`) are handled correctly.
- **Sender allow-listing is described honestly.** It matches the spoofable
  RFC5322.From address and the UI/API copy says so. An opt-in MX-only
  `require_authenticated` inbox flag additionally requires a DMARC pass or an
  aligned SPF/DKIM pass; it has no effect on webhook providers.
- **MX supervisor shutdown is non-blocking and idempotent.** The child exit is a
  closed broadcast channel plus a stored error, so the monitor and `Stop` can
  both observe it; the forced-kill path waits with a bounded deadline.
- **MX staging is cancellable.** A DATA timeout cancels the reader so the
  staging goroutine cannot outlive the transaction and the RAM-budget release
  remains accurate; the buffer is zeroed before release.
- **MX protects per-source IPs** with a concurrency cap, and can require
  STARTTLS (`MX_REQUIRE_TLS`).
- **MX alias fan-out deduplicates by resolved inbox**, matching the webhook
  path, while still returning one result per envelope recipient.
- **Outbox cancellation vs in-flight delivery** is handled by tolerating a row
  deleted mid-send: the provider may still have accepted the mail (which no
  local action can recall), but the message is not resurrected and the outcome
  is logged as ambiguous rather than retried.
- **Resend sends an `Idempotency-Key`** so a retry after a lost success
  response does not duplicate; SMTP/Mailgun/Brevo have no provider idempotency,
  and that residual ambiguity is documented.
- **Tooling hygiene:** a 16-byte approval token, fail-closed RNG for ids and
  the embedded edge credential, a versioned PBKDF2 key derivation with legacy
  fallback, special-use SSRF ranges, lazy startup chown, an authenticated
  MX-only Hermes outbound role, per-mailbox UI role checks, a bounded
  attachment-upload body, remote-image opt-in, and a slower maintenance ticker
  than the delivery poll.

**Reason:** The review identified the approval preview and decision durability,
the unauthenticated MX staging write, the public-suffix approximation and the
supervisor deadlock as release-gating, and the remainder as material hygiene or
UX risks. Each fix preserves the existing security defaults (fail-closed
approval, token hygiene, public-routable outbound) or makes them opt-in.

**Deliberately unchanged:** Control-mail replies whose token is stale/invalid
are still consumed rather than delivered, and a reply whose first line is not an
explicit approval still rejects. These are intentional fail-closed/token-hygiene
choices; changing them was judged riskier than the UX they would improve.

**Dependencies:** `golang.org/x/net` (publicsuffix) and `golang.org/x/crypto`
(pbkdf2) are promoted from indirect to direct. Both are BSD-3-Clause Go
sub-repositories already present in the module graph; notices updated in
`THIRD_PARTY_NOTICES.md`.

**Complexity:** Qualitative; migrations 026 (inbox `require_authenticated`) and
027 (Hermes `outbound_role`). No new runtime service.

## D036 — One-shot draft writes and consistent draft API

**Decision:** Draft writes accept the same base64 JSON `attachments` array as
send/reply, plus an `action` of `draft` (default), `request-send` or `send`.
This lets an agent create a draft, attach files and submit it for approval (or
send it, as an owner) in a single request, instead of the previous mandatory
create → multipart upload → request-send sequence. The same optional body is
accepted by `POST /v1/drafts/{id}/send` and `/request-send`, so an owner or
assistant can edit and act in one call.

Supporting consistency fixes on the same surface:

- `PATCH /v1/drafts/{id}` is now a true partial update: only fields present in
  the body are changed. Previously an omitted field was written empty, silently
  wiping a draft.
- Draft read responses include an `attachments` array, and
  `GET /v1/drafts/{id}/attachments/{attId}` downloads one attachment's bytes.
- `sender` is accepted as an alias for `from_address` on draft writes.
- `POST /v1/drafts/{id}/send` returns `provider_message_id`, matching `/v1/send`.
- `GET /v1/drafts` supports `limit` and `before` keyset paging.
- `PATCH /v1/messages/{id}` carries explicit JSON tags.
- `POST /v1/inboxes` accepts `localpart` as well as `local_part`.

Attachments added inline are appended to any already uploaded, and inline bytes
are covered by the existing per-attachment and total size checks and by the
frozen approval fingerprint (SHA-256 per attachment), so an approved send still
applies to the exact reviewed bytes.

**Reason:** The draft workflow was the only write path that required a
pre-existing draft and a separate multipart upload, while send/reply already
accepted inline attachments. This was a real integration hazard for agents and
the direct cause of repeated failed client sends. The PATCH semantics change is
the only backward-incompatible behaviour change; it prevents silent data loss
and there are minimal live clients to protect.

**Complexity:** No new migrations, services or dependencies. A shared
`persistDraftAttachments` helper replaces the file-write loop duplicated across
the API and UI paths, with cleanup on partial failure.

**Deliberately unchanged:** `{"external": true}` on `request-send` is retained
for compatibility; it remains optional because a configured inbox approver makes
the request external automatically. `/v1/drafts/{id}/attachments` multipart
upload is kept for incremental uploads.

## D037 — External sending aliases (migration 028)

**Decision:** An inbox may carry one or more **external sending aliases**: full
email addresses on domains Gatehouse does not manage, used only as outbound
identities. Each external alias owns its own sending connector (any existing
outbound provider, encrypted with `APP_ENCRYPTION_KEY`) and an optional sender
display name. External aliases are strictly send-only: they never participate in
inbound recipient resolution (`ResolveRecipient` is unchanged), add no receiving
connector, no mailbox sync, and no external-domain ownership claim. They are
self-hosted Admin-only, in both management and read paths.

Key invariants:

- **Stable ids.** `external_aliases.id` is the immutable identity. A sender is
  resolved to either a managed domain or an external alias id; `messages`
  records `sending_external_alias_id` and `drafts` records
  `from_external_alias_id` so a queued message or a reviewed draft stays pinned
  to the alias it was created with. Deleting an alias clears any `default_sender`
  that referenced it, and subsequent delivery attempts for its queued messages
  fail permanently with a clear missing-alias error — they never fall back to a
  domain connector or a recreated alias with the same address.
- **No replace-set for external aliases.** Unlike managed aliases (which keep
  D030's replacement semantics), external aliases are created, edited and deleted
  only through their admin endpoints, one at a time. An ordinary inbox save can
  never drop an external alias or its connector. This is why `SetExternalAliases`
  does not exist.
- **Connector lifecycle.** A send whose selected/default sender is an external
  alias with no connector is held pending (not failed) with a sender-specific
  reason; saving that alias's connector requeues only pending messages targeting
  that alias. Removing the connector clears its credentials and requeues the same
  set, which then hold again.
- **Sender identity is frozen at draft time.** `ResolveSendingTargetByID`
  resolves a frozen external-alias id and returns `ErrExternalAliasDeleted` when
  it is gone; an empty/unchanged sender keeps the frozen id through owner sends,
  approval and retries. The approval-request notification itself stays on the
  inbox primary managed address and its domain connector.
- **Hermes Relay** sends with the inbox's configured `default_sender`, falling
  back to the primary, for both direct sends and Assistant-mode drafts. When the
  default is an external alias, so is the relay send.
- **Delivery attribution.** `outbound_delivery_log.external_alias_id` records
  the alias for each attempt; per-alias outbound history reuses the domain
  delivery renderer's query (`ListExternalAliasDeliveryAttempts`).
- **API/UI.** Inbox responses gain an `external_aliases` metadata collection
  (ids, addresses, names, connector status; never secrets). Admin endpoints live
  under `/v1/admin/inboxes/{id}/external-aliases` (create/metadata/delete,
  `/sending` GET/PUT/DELETE, `/sending/deliveries`). An alias address is
  immutable after creation; display name and connector remain editable.

**Reason:** The motivating case is an address you control but cannot attach a
domain connector to (for example a Gmail address, or a provider that offers no
webhook). Domain connectors remain the primary model and are untouched; this adds
a narrow outbound-only identity for exactly that gap. Reusing the existing
provider schema, encryption, CAS/revision, requeue-on-save and delivery-log code
means no new runtime service, transport adapter or dependency. Keeping external
aliases out of inbound routing avoids any "why didn't my mail arrive" ambiguity:
a managed domain is authoritative for its own namespace, and an external alias
never claims one.

**Complexity:** Qualitative. Migration 028 adds `external_aliases`,
`messages.sending_external_alias_id`, `drafts.from_external_alias_id` and
`outbound_delivery_log.external_alias_id`. New store module
(`internal/store/external_aliases.go`), service admin surface
(`internal/app/external_aliases.go`) and admin API
(`internal/httpapp/external_alias_api.go`). No new dependency or runtime service.

**Deferrals:** an external alias may not be used for inbound or for a per-alias
DKIM/SPF identity Gatehouse cannot prove; address immutability avoids migration
of queued attribution.

**UI:** the inbox Aliases tab presents managed and external aliases as two
separate lists; external rows are display-only ("External · sending only",
connector status) and link to a dedicated server-rendered page per alias holding
its editable sender name, connector editor and full outbound activity. External
aliases are added only to a saved inbox (the Add-inbox dialog points to this
step). The compose/reply From select and the default-sender selector include
external aliases, and sending readiness follows the selected/default sender's
connector, so a missing external connector shows a sender-specific pause banner.

## D038 — Embedded MX: single-container mode with a separate-uid child

**Decision:** `MX_ENABLE=true` makes `cmd/server` spawn `gatehouse-mx` as a child
process in the same container, under a different uid/gid (`MX_UID`/`MX_GID`,
default 65533) with a scrubbed environment, then drop its own privileges to the
app runtime user (default 65532). The edge has no `/data` access, no
`APP_ENCRYPTION_KEY`, no `DATA_DIR` and no `MX_EDGE_KEYS`, and stages messages in
memory. The single switch `MX_ENABLE` has three values: `false` (no MX), `true`
(embedded, above), and `remote` (edge runs as its own container/image,
`gatehouse-mx`, or on another host, sharing `MX_EDGE_KEYS`). The shipped
`docker-compose.yml` selects `true` by default; see D039.

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
  two remedies (remove the hardening, or use `MX_ENABLE=remote` with the
  separate edge image). Silent same-uid degradation would defeat the isolation.
- **Isolation is weaker than `remote`:** DAC + separate uid, no mount or
  network namespaces. `docker-compose.mx-sidecar.yml` (the `gatehouse-mx` image)
  remains the recommended mode when two containers are acceptable; the embedded
  mode trades namespace isolation for a one-container deployment.
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
  deterministically). `MX_ENABLE=remote` requires `MX_EDGE_KEYS`, since the core
  cannot generate a secret the operator's separate edge would know.

## D039 — Minimal default Compose enables embedded MX; webhook-only is the opt-out

**Decision:** `docker-compose.yml` is the minimal default and enables the
embedded MX edge (`MX_ENABLE=true`, host port 25 published) so a plain
`docker compose up -d` can receive internet mail directly. An operator who
receives mail only through a webhook provider (Mailgun, Cloudflare Email
Routing, Resend) sets `MX_ENABLE=false` in `.env` to run webhook-only; the port
is then published but has no listener. Compose supplies the `true` default via
`MX_ENABLE: "${MX_ENABLE:-true}"`, so `.env` still overrides it; the server
binary itself defaults to `false` when `MX_ENABLE` is unset. A separate,
self-contained `docker-compose.advanced.yml` mirrors the same MX default and adds
the hardened stack (`read_only`, `/tmp` tmpfs, `no-new-privileges`, the optional
`cap_drop`/`user` block, `GATEHOUSE_RUN_UID`/`GID`).
`docker-compose.mx-sidecar.yml` is the two-container sidecar deployment and
forces `MX_ENABLE=remote`.

**Requirement:** the earlier minimal Compose set no `MX_ENABLE` yet published
host port 25, while the docs claimed embedded MX was the default. That was the
worst of both: an exposed port with MX actually off, and prose that did not
match the artifact. The deployment default is now stated in one place and the
artifact implements it.

**Reason:** a self-hoster who owns the domain wants `docker compose up -d` to
receive mail directly without a second service, and leaving MX enabled is safe
because a domain is not an MX receiver until its receiving provider is set to
`MX` in the Admin UI: unconfigured recipients are rejected. Operators who only
want webhooks opt out with one variable. Keeping the server default `false`
preserves the provider-webhook baseline for anyone running the binary directly.

- **Hardening is opt-in:** the default now ships the real minimum; operators who
  want `read_only`/`tmpfs`/`no-new-privileges` opt in with one `-f`. Defaults
  that restate code defaults (the app's privilege drop already defaults to
  `65532:65532`) made the shipped compose look far more complex than the
  actual minimum.
- **Embedded privilege model:** the app chowns `/data`, spawns the edge under
  `MX_UID`/`MX_GID`, then self-drops. `cap_drop: [ALL]`/`user:` disable this;
  embedded (`true`) MX refuses to start and points at `MX_ENABLE=remote`.

**Complexity:** one `environment` line in each of the two single-container
compose files and the matching documentation; no Go code, no new dependency, no
new runtime service. Changing the settled default was made under the
change-discipline rule for a settled decision (this entry).

## D040 — Separate edge image for remote/sidecar MX (Dockerfile.mx)

The sidecar/remote edge runs from its own image, `gatehouse-mx` (`Dockerfile.mx`),
not the app image with an entrypoint override. The app image still builds the
edge binary for `MX_ENABLE=true`; the separate image is edge-only.

- **Why:** "which container is this?" should be obvious. A standalone MX host or
  sidecar previously ran the full app image (including `gatehouse-mail` and
  `libsqlite3`) with a different entrypoint. The edge links no SQLite and opens
  no files, so the edge image builds `CGO_ENABLED=0` static, ships only
  ca-certificates/tzdata, and runs as a non-root user with no `/data` volume.
- **Debuggable base:** `debian:bookworm-slim` (not distroless) so the container
  has a shell and tools when diagnosing DNS/TLS issues.
- **Credential unchanged:** `remote` still requires `MX_EDGE_KEYS` shared with
  the edge (`MX_EDGE_SECRET` in the sidecar compose).

## D041 — Workflow mail has its own outbound queue (migration 022)

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

## D042 — In-process privilege drop (no gosu, bind-mounted ./data)

The container image has no `USER` and installs no `gosu`. It boots as root so
`internal/privdrop` can recursively `Lchown` a fresh, root-owned `./data` bind
mount to the runtime UID/GID, then `Setgroups`/`Setgid`/`Setuid` before the
database is opened. `GATEHOUSE_RUN_UID`/`GATEHOUSE_RUN_GID` (default
65532:65532) select the runtime user; the drop is a no-op when the process is
already non-root, so the opt-in hardened compose (`user:`, `cap_drop: [ALL]`)
needs no setuid capability. This mirrors the sibling router service: no host `chown` step,
and the hardened `docker-compose.advanced.yml` ships `read_only`, `tmpfs /tmp`
and `no-new-privileges`.

## D043 — Free-text message labels (migration 020)

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

## D044 — Control-message log label (migration 019)

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

## D045 — Message client attribution (migration 017)

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

## D046 — Atomic migration runner (BUG-04)

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

## D047 — Draft storage accounting (SEC-04)

`accounts.storage_used_bytes` counts message raw MIME plus draft bodies plus
draft attachment files:

```text
storage_used_bytes = Σ messages.size_bytes
                   + Σ len(draft.text_body) + len(draft.html_body)
                   + Σ draft_attachments.size_bytes
```

All draft mutations and attachment add/delete adjust the account in the same
transaction as the row change. Sending a draft consumes its rows in the same
transaction as the message insert, so draft bytes are refunded before the
message is charged and are never double-counted. Migration 012 backfills
existing draft usage once.

## D048 — Idempotency semantics (SEC-03, SIMP-05)

An idempotency key is scoped to the account but records the mailbox it was
used for. A replay is only returned when the requested mailbox matches the
recorded one and the caller owns it; otherwise the request is rejected with a
conflict. The key-to-message mapping is committed with enqueueing (inside
`CommitOutbound`), not at delivery, so queued and failed messages replay
normally. Replay returns the current message state; `provider_message_id` is
empty until delivery.

## D049 — Outbound claim lease (BUG-05)

Claiming a pending message records `claim_owner` and `claim_expires_at` and
leaves `next_attempt_at` for retry scheduling. Startup clears abandoned claims
(safe under the single-process model) and the worker uses a bounded,
cancellation-independent context to record delivery outcomes, so a crash or
cancellation no longer parks a message for 24 hours.

## D050 — Revocation cancels live connections (SEC-05)

SSE streams and Relay sockets register the credential scopes they depend on
(`key:`, `sess:`, `user:`, `hrm:`). Revoking, rotating, or rescoping a
credential, deleting a relay connection, logging out, or changing a password
cancels the matching scopes so already-open connections terminate immediately.
Relay outbound operations also revalidate the connection row.

## D051 — In-transaction hydration (BUG-02)

Write paths build their return value from a read inside the same transaction
instead of reading after commit. This removes the failure mode where a
post-commit read error caused the caller to delete MIME that committed rows
already referenced. A commit error is only treated as failure if the row is
genuinely absent.

## D052 — One source of truth for API discovery

`internal/apispec` is a stdlib-only leaf package holding the canonical `Route`
table for the authenticated `/v1` surface. Every discovery artifact renders from
that one table:

- `/agent` — the served Markdown guide;
- `/openapi.json` — an OpenAPI 3.0.3 document covering the complete operation
  list;
- `docs/API-REFERENCE.md` — the generated reference, checked in and verified by
  a staleness test;
- `/examples/python`, `/examples/bash` and `/examples/curl` — the embedded
  Python client, Bash client and curl cookbook.

The authenticated `/v1` registrations are a single registration table in
`internal/httpapp/server.go`, so the documented surface and the live mux cannot
drift apart in isolation. `tests/unit/httpapp/route_coverage_test.go` fails when
a registered `/v1` route is missing from the table, or when a table entry has no
registration.

`/openapi.json` is valid OpenAPI 3.0.3 because every operation carries a
non-empty `responses` object (the primary success response plus `401` and
`default`). Per-operation request and response JSON Schemas remain deferred:
they need handler-level annotations and are a separate project.

## D053 — Proxy-chain trust and canonical HTTPS redirect

**Decision:** `X-Forwarded-For` is walked from the right (closest to us):
trusted `TRUSTED_PROXIES` entries are stripped and the first untrusted address
is the client identity used for rate limiting; a single trusted hop yields the
last entry. Under legacy `TRUST_PROXY_HEADERS` trust-all the last entry is
used, which requires the proxy to overwrite (not append to) `X-Forwarded-For`.
`X-Forwarded-Proto` follows the same rightmost rule. `FORCE_HTTPS` redirects
target only the canonical `BASE_URL` host (validated as a bare origin at
startup; `FORCE_HTTPS=true` requires an `https://` base), never the request
`Host`. Discovery documents (`/openapi.json`, `/examples/*`) still advertise
the request origin; only the 308 is canonical.

**Reason:** An appending proxy preserves attacker-supplied leading XFF/XFP
entries, so leftmost parsing lets a caller pick its own rate-limit key and
spoof `https` to bypass the redirect and `Secure` cookies. Request `Host`
headers are attacker-controlled, so echoing them in a 308 enables phishing
redirects and cache poisoning.

## D054 — Operator MX edge secrets require 256 bits

**Decision:** Every operator-supplied MX edge HMAC secret (`MX_EDGE_KEYS`
entries, `MX_EDGE_SECRET`) must carry 32 bytes / 256 bits of entropy as hex,
base64 or 32+ raw bytes, enforced fail-closed at startup by both
`config.Load` and `mxagent.Load` via `mxwire.CheckEdgeSecret`. Embedded mode
already generates 32 random bytes; only operator values are gated. Rotation
uses overlapping `MX_EDGE_KEYS` entries.

**Reason:** The secret authenticates every edge->core request and the core
trusts the edge's SPF/DKIM/DMARC evidence on a valid signature, so a guessable
secret lets anyone inject mail, forge auth evidence and burn quota. Length is
an enforceable proxy for unguessability; it cannot prove randomness.

## D055 — One-shot initial admin and operator password reset

**Decision:** Remove `ADMIN_BOOTSTRAP_TOKEN` and the unauthenticated `/setup`
claim flow. A fresh self-hosted database is initialised from one-shot
`INITIAL_ADMIN_EMAIL` / `INITIAL_ADMIN_PASSWORD` (either may be supplied via a
`*_FILE` secret) at startup, creating the first administrator inside a single
`BEGIN IMMEDIATE` transaction so two processes cannot both initialise the
database. Once any user exists the bootstrap settings have no effect. If no
credentials are supplied the service starts and serves a static
"not configured" page; there is no HTTP path that can claim the instance.
Password recovery is operator-driven through
`gatehouse admin reset-password <email>`, which reuses the normal password
rules, revokes all browser sessions, leaves API keys intact, and writes an
audit event; `gatehouse admin revoke-api-keys <email>` is a separate command.

**Reason:** A bootstrap token that must be read from logs and re-entered on a
web form is easy to mishandle, and an unauthenticated setup form is a standing
claim risk on a fresh instance. Deployment-time credentials fit Docker secrets
and match how the rest of the configuration is supplied, while the guaranteed
server-side reset covers lost access without depending on outbound email. API
keys are treated as independent machine integrations, so they are not torn down
by a human password reset.

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
