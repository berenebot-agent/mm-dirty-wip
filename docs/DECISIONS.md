# MailMoose — V1 Decision Register

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

**Extension point:** Add a PostgreSQL store when measured load makes it beneficial.

## D005 — Local filesystem MIME store

**Decision:** Store canonical raw MIME under `/data/messages`.

**Reason:** Simple persistence and backup, efficient streaming, and minimal runtime dependencies.

**Extension point:** Add object storage when storage scale warrants it.

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

**Boundary:** Native MailMoose models remain authoritative for richer features.

## D012 — Lightweight web UI

**Decision:** Use embedded server-rendered or similarly compact web assets served by the Go binary.

**Reason:** Human administration requires forms, lists, search, and message inspection rather than a separate application runtime.

## D013 — Account storage as hosted capacity control

**Decision:** Account-level storage is the capacity control while logical inbox
identities remain cheap to create.

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

**Decision:** The admin UI and REST API issue Hermes relay credentials directly: generate the gateway id, secret, and delivery key, store the encrypted connection, and return a ready-to-paste `.env` block.

**Reason:** A self-hosted operator should be able to create a relay connection and paste the resulting environment variables without a separate token-exchange step or a hosted identity token.

**Status note:** the "remains for CLI provisioning" half of this decision was never implemented for Hermes. `POST /relay/enroll` works, but only the OpenClaw kind can mint a code for it (`CreateRelayEnrollCode` rejects every other kind), so a Hermes `hermes gateway enroll` cannot complete. The store-level `CreateHermesEnrollToken` is reachable from tests only. Either wire a Hermes mint path or treat the token flow as OpenClaw-only; until then, the direct block is the only Hermes path. See `docs/CONNECTORS.md` §3.

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
by email, with no MailMoose account. An inbox may configure one optional
`approver_email` (Owner/Admin via `PATCH /v1/inboxes/{id}`). A configured
approver makes `request-send` external automatically: MailMoose freezes the
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
with an approver cannot create a MailMoose-only request (the owner can still
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
self-authorize, and that a nominated human approves without needing a MailMoose
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
must resolve its destination to a public-routable address before connecting.
An operator can opt out with `ALLOW_PRIVATE_OUTBOUND=true` for a private gateway
or local relay. Provider HTTP API bases must also be HTTPS
and must not name a loopback, private or link-local IP literal when enforcement
is on. The shared `netutil` client validates inside `DialContext` (so DNS cannot
rebind between validation and connection), refuses redirects, and bypasses the
proxy environment while enforcement is active; enforcement off restores the
default transport and proxy behaviour.

**Reason:** The previous guard was gated behind the hosted-mode flag, so the
default deployment skipped it and an account Administrator could point a
provider's `api_base` at loopback, RFC1918 or the cloud metadata address and
read the upstream response through the delivery log. The control belongs on by
default; the operator should decide to weaken it. Turning
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

Normalization is **not** the same as attestation, and the difference matters for
the approval decision (`D058`, `D027`): the value is normalized on every adapter
but only *attested* on Resend, whose envelope sender comes from the Svix-signed
payload. Mailgun passes an unsigned form field and Cloudflare passes an unsigned
request header (`X-MailMoose-Envelope-From`), so on both the transport is
authenticated while the sender **content** is caller-controlled. A consumer that
needs an attested sender must require a provenance flag, not the provider name
(round-2 retest, finding A).

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
  MailMoose's own approval mail; only the sandboxed UI view renders it.
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
email addresses on domains MailMoose does not manage, used only as outbound
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
DKIM/SPF identity MailMoose cannot prove; address immutability avoids migration
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

**Decision:** `MX_ENABLE=true` makes `cmd/server` spawn `mailmoose-mx` as a child
process in the same container, under a different uid/gid (`MX_UID`/`MX_GID`,
default 65533) with a scrubbed environment, then drop its own privileges to the
app runtime user (default 65532). The edge has no `/data` access, no
`APP_ENCRYPTION_KEY`, no `DATA_DIR` and no `MX_EDGE_KEYS`, and stages messages in
memory. The single switch `MX_ENABLE` has three values: `false` (no MX), `true`
(embedded, above), and `remote` (edge runs as its own container/image,
`mailmoose-mx`, or on another host, sharing `MX_EDGE_KEYS`). The shipped
`docker-compose.yml` selects `true` by default; see D039.

- **Why:** operators want `docker compose up -d` to receive mail directly with
  no second service, while still keeping the edge out of the app's data and
  secrets. A separate process under a separate uid gives that with DAC, without
  a new binary or a supervisor package.
- **No separate supervisor, no `MAILMOOSE_ROLE`:** the existing app process is
  already PID 1, so it spawns the child before its privilege drop and keeps
  running to coordinate shutdown. There is no `cmd/mailmoose`.
- **Shutdown is a pipe, not a signal:** after the parent drops uid it cannot
  signal a child owned by a different uid, so the parent passes an inherited
  write pipe and closing it (EOF) tells the edge to stop (`MX_SHUTDOWN_FD`).
- **Refuses rather than degrading:** if the container cannot start as root to
  spawn the child (strict `user:` or `cap_drop: [ALL]`), startup fails with the
  two remedies (remove the hardening, or use `MX_ENABLE=remote` with the
  separate edge image). Silent same-uid degradation would defeat the isolation.
- **Isolation is weaker than `remote`:** DAC + separate uid, no mount or
  network namespaces. `docker-compose.mx-sidecar.yml` (the `mailmoose-mx` image)
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
`cap_drop`/`user` block, `MAILMOOSE_RUN_UID`/`GID`).
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

The sidecar/remote edge runs from its own image, `mailmoose-mx` (`Dockerfile.mx`),
not the app image with an entrypoint override. The app image still builds the
edge binary for `MX_ENABLE=true`; the separate image is edge-only.

- **Why:** "which container is this?" should be obvious. A standalone MX host or
  sidecar previously ran the full app image (including `mailmoose` and
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
database is opened. `MAILMOOSE_RUN_UID`/`MAILMOOSE_RUN_GID` (default
65532:65532) select the runtime user; the drop is a no-op when the process is
already non-root, so the opt-in hardened compose (`user:`, `cap_drop: [ALL]`)
needs no setuid capability: there is no host `chown` step, and the hardened
`docker-compose.advanced.yml` ships `read_only`, `tmpfs /tmp`
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
  only, which is acceptable for typical tags. Labels may not contain control
  characters, `/` or `\`; the path separators are rejected so a label can be
  addressed by its own URL query parameter without path-encoding ambiguity.
- The web UI renders the inbox's in-use labels in the mailbox sidebar under a
  Labels heading; selecting one filters the list via
  `GET /ui/inboxes/{id}/label?name=...`.
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
`default`). Per-operation request, response and query schemas were subsequently
added (`internal/apispec/schemas.go` and `route_io.go`; wired in `openapi.go`,
covered by `tests/unit/apispec/openapi_test.go`), superseding the original
deferral. Every operation now references named `components.schemas`.

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

## D055 — Operator password reset (superseded in part by D060)

> **Superseded in part by D060:** the one-shot `INITIAL_ADMIN_*` bootstrap was
> replaced by the deployment-authoritative `ADMIN_*` system administrator, and
> `admin reset-password` now refuses the system administrator (the configured
> secret owns that login). The remainder of this decision still stands.

**Decision:** Remove `ADMIN_BOOTSTRAP_TOKEN` and the unauthenticated `/setup`
claim flow. A fresh self-hosted database is initialised from one-shot
`INITIAL_ADMIN_EMAIL` / `INITIAL_ADMIN_PASSWORD` (either may be supplied via a
`*_FILE` secret) at startup, creating the first administrator inside a single
`BEGIN IMMEDIATE` transaction so two processes cannot both initialise the
database. Once any user exists the bootstrap settings have no effect. If no
credentials are supplied the service starts and serves a static
"not configured" page; there is no HTTP path that can claim the instance.
Password recovery is operator-driven through
`mailmoose admin reset-password <email>`, which reuses the normal password
rules, revokes all browser sessions, leaves API keys intact, and writes an
audit event; `mailmoose admin revoke-api-keys <email>` is a separate command.

**Reason:** A bootstrap token that must be read from logs and re-entered on a
web form is easy to mishandle, and an unauthenticated setup form is a standing
claim risk on a fresh instance. Deployment-time credentials fit Docker secrets
and match how the rest of the configuration is supplied, while the guaranteed
server-side reset covers lost access without depending on outbound email. API
keys are treated as independent machine integrations, so they are not torn down
by a human password reset.

## D056 — Product rename to MailMoose

**Decision:** Rename the product, Go module (`github.com/dellarb/mailmoose`),
binaries (`mailmoose`, `mailmoose-mx`), env prefix (`MAILMOOSE_*`), HTTP
headers (`X-Mailmoose-*`), discovery path (`/.well-known/mailmoose`), API key
prefix (`mmm_`), cookies (`mmm_csrf`, `mmm_session`), and embedded brand
assets to MailMoose. No compatibility shims for the old names.

**Reason:** The product launches under the MailMoose name with its own logo
pack. A breaking rename before first deployment avoids carrying two brands.

## D057 — Self-hosted Direct MX outbound delivery

**Decision:** Add a `mx` outbound provider. It resolves the recipient domain's
MX records and delivers raw MIME directly to port 25, using the configured HELO
hostname and opportunistic STARTTLS without SMTP credentials. Each queued
message is limited to one unique envelope recipient, so the existing atomic
outbox state remains correct. All resolved destinations continue to pass the
public-routable outbound guard.

**Reason:** Operators who own their sending IP and DNS should be able to send
without paying for an SMTP relay. The single-recipient constraint avoids
claiming success for recipients that were not accepted when one SMTP
transaction partially succeeds.

**Operational requirements:** Operators are responsible for port 25 egress,
forward and reverse DNS for the HELO identity, SPF, DKIM if desired, DMARC
alignment, and IP reputation. A future extension may add durable per-recipient
delivery state and signing support.

## D058 — Strix security review remediation

**Decision:** Apply the confirmed and hardening fixes from an external
security review of this codebase:

- **Events feed role parity (vuln-0004).** `ListEvents` withholds the
  assistant-scoped payload fields `approver_email`, `decision_actor`,
  `decision_method` and `feedback` from a principal that cannot assist the
  event's inbox, matching the `CanAssist` gate `ListSendRequests` already
  applies. Admin principals see every field.
- **Durable control receipts (vuln-0001).** Consumed control mail on the MX
  path records an MX receipt with disposition `control`, so a byte-identical
  signed replay is deduplicated by the existing receipt lookup even though no
  message row exists.
- **Authenticated evidence for MX approvals (vuln-0002).** The MX edge's
  trusted SPF/DKIM/DMARC evidence is required before a control message may act
  on the sender's identity, regardless of the inbox's `require_authenticated`
  setting. Webhook providers carry no edge evidence and are unaffected.
- **Trusted-proxy startup guard (vuln-0005).** `config.Load` refuses
  `TRUST_PROXY_HEADERS=true` and any `/0` `TRUSTED_PROXIES` entry, both of which
  make the peer check meaningless. This supersedes the legacy trust-all fallback
  described in D053; `TRUSTED_PROXIES` is now the only way to trust forwarded
  headers.
- **Path containment (hardening).** `internal/safepath.Join` resolves a stored
  relative path beneath `DataDir` and rejects absolute paths, `..`, NUL and
  backslash, and is used at every raw-MIME/attachment open, read and remove.
- **Per-credential long-lived bounds (hardening).** SSE streams and long-polls
  are capped per credential so one key cannot park unbounded connections. The
  MX replay cache remains in-process and is documented as single-instance,
  consistent with the one-process/one-container architecture.
- **Error hygiene and HSTS (hardening).** Unrecognised storage-engine and
  filesystem errors answer a generic 500 instead of echoing schema, query or
  path text; `Strict-Transport-Security` is sent when `FORCE_HTTPS=true`.

**Accepted risk — Mailgun unattested envelope sender (vuln-0003).** The Mailgun
webhook HMAC covers only `timestamp+token`, so the `sender` form field is not
provider-attested. The approval path continues to treat it as the envelope
sender to preserve email-based approvals for Mailgun deployments. Exploitation
requires *both* valid Mailgun signing material (the signing key, or one captured
triple replayable within the 24-hour window) and the 128-bit single-use
approval token, so the practical bar is high. A future change should either
fail closed on unattested envelopes (breaking Mailgun email approvals) or
authenticate the reply locally (for example DKIM verification of the staged
MIME) before revisiting this.

**Correction (round-2 retest):** the acceptance originally rested in part on the
claim that the Cloudflare adapter carried a genuinely attested envelope sender and
was therefore unaffected. **That claim was false** — Cloudflare reads the envelope
sender from an unsigned request header, so it is a second unattested entry point to
the same decision (finding A, `LOW`). Resend remains genuinely attested. The
"failing closed breaks Mailgun" argument is correspondingly weaker: a single
provenance flag on `transport.InboundMessage` closes Mailgun and Cloudflare together.

**Reason:** The review found no privilege escalation to `owner`/`admin`, no
cross-inbox rights bleed and no MX HMAC bypass. The confirmed issues are a
role-parity gap on the events surface, a missing control-mail receipt, and an
approval-identity gap on the MX path; the remainder are hardening. The Mailgun
gap is a deliberate product trade-off recorded here rather than silently
resolved.

## D059 — Unattested inbound envelope senders: extended acceptance and containment

**Context:** A second review round (Strix run `<review-run>`, live-app
assessment) found that the accepted risk `D058` rested partly on a claim that was
false. `D058` asserted the Cloudflare inbound adapter carried a "genuinely
attested" envelope sender and was therefore unaffected by the envelope-sender
risk. It does not: `internal/transport/cloudflare/inbound.go` reads the envelope
sender from the `X-MailMoose-Envelope-From` request header, and the shared
per-domain bearer authenticates the caller without attesting the value. Cloudflare
is therefore a second unattested entry to the approval decision. Resend **is**
attested (its envelope sender comes from the Svix-signed payload).

**Decision:** Accept the Cloudflare exposure under the same reasoning as `D058`
rather than changing approval behaviour at this time. The maintainer's judgement:
the practical bar remains possession of the domain's receiving secret *and* the
128-bit single-use approval token, and the approver identity is server-fixed, so
the attacker's gain is limited to forcing approval of the specific draft whose
token they hold.

**Consequences:**

- The accepted risk now covers **two** providers (Mailgun and Cloudflare) and is
  documented for reporters in `SECURITY.md`.
- The "failing closed would break Mailgun" rationale is weaker than it appeared.
  A single provenance flag on `transport.InboundMessage` would close Mailgun and
  Cloudflare together, which is the preferred eventual fix — the blocker is that
  fail-closed loses email approvals for every provider that passes an unattested
  sender, not Mailgun alone.
- **Attestation is a property of what a signature covers, never of a provider's
  name.** Any future adapter is attested only if its verified signature or secret
  covers the sender value the adapter reports. Recorded as a general rule in
  `SECURITY.md`.

**Also in this change (retest follow-ups, not part of the acceptance):**

- `internal/store`: a NUL or control byte in a search query no longer reaches the
  FTS5 engine. A NUL is a typed client-input error (`ErrInvalidSearchQuery`) and a
  control byte is stripped; an emptied expression short-circuits to "no matches".
  Previously the malformed expression surfaced as an unmapped store error and an
  HTTP 500 (retest finding B, informational).
- `internal/httpapp`: `ErrInvalidSearchQuery` maps to a 400 rather than falling
  through to the generic 500 branch.
- `internal/config`: a `TRUSTED_PROXIES` entry wider than `/8` is now refused at
  load, closing the "equivalent-but-not-literal /0" gap recorded as a retest open
  item; a peer found inside the trust set is logged at WARN so a covering-set
  misconfiguration is visible at runtime, which startup cannot detect. (The second
  clause is superseded by D061; the load-time `/8` floor stands.)

## D060 — System administrator, account Admins, and mailbox operators

**Context:** The app already modelled separate accounts and users, but only ever
created one human user per account and every human login was an account Admin
(the web UI and `/v1` admin surface required `p.Admin`; non-admin sessions failed
closed). Provisioning another person meant public registration, which is closed
by default and creates a fresh account each time.

**Decision:** Add three explicit human roles and an installation-level admin
plane:

- **System administrator** — one configured login (`ADMIN_EMAIL` /
  `ADMIN_PASSWORD`, `*_FILE` supported), marked by `users.is_system_admin`. The
  configured credentials are authoritative while present: they create the login
  on first start, or **adopt an existing user with that email** (forcing it to
  account Admin and system Admin, and resetting its password), and rotate the
  stored email/password (revoking its sessions) on later starts; when both are
  absent the stored login is preserved. The login is never editable in the UI
  and `admin reset-password` refuses it, both pointing at the deployment
  secret. The system administrator has an ordinary account of its own but does
  **not** automatically gain access to other accounts' mail.
- **Account Admin** — unchanged `users.is_admin`; full control of one account's
  domains, clients and mailboxes. Each account has one Admin.
- **Mailbox operator** — a non-admin member of an account whose access is a set
  of per-inbox Owner grants in `user_mailbox_roles`. Operators are Owners of the
  mailboxes assigned to them and nothing else; they get no domain, client or
  account controls.

Provisioning is by **invitation** (`invites`), not stored initial passwords. An
invite is either a new, separate account with its own Admin or a mailbox
operator on an existing account. The invitee chooses their password from a
single-use, expiring (7 day) link; only the token hash is stored. Sending the
invitation email goes through the **ordinary outbound queue** from the
inviter's account **mailer** (`accounts.mailer_inbox_id`) — a mailbox that
account owns, so no account ever sends from another's — and the same setup link
can be copied and shared out of band instead.

**UI:** the system administrator's **Admin** page lists accounts (with their
Admin or a pending invitation) and offers a `Create invitation` dialog. An
account Admin manages the account's operators and its mailer on the **Account**
page, using the same list-plus-dialog pattern. There is no separate account
"Members" page, so an account Admin keeps a single nav button.

**Consequences:**

- `SessionPrincipal` now resolves per-inbox roles for non-admin users from
  `user_mailbox_roles`, so the previously unimplemented
  `userMailboxRoles` path is live.
- The account-level UI and `/v1` admin routes still require `Admin`; the
  mailbox-level UI routes check the per-inbox role, so an operator can read,
  draft, send/reply, approve and manage only their assigned mailboxes.
- `INITIAL_ADMIN_EMAIL` / `INITIAL_ADMIN_PASSWORD` (and `INITIAL_ACCOUNT_NAME`)
  are renamed to `ADMIN_*` in place; the project is not yet deployed, so no
  compatibility shim is carried.
- `users`, `user_mailbox_roles`, `invites`, and the `is_system_admin` column are
  added by migration `030`; `accounts.mailer_inbox_id` replaces the discarded
  installation-wide `system_settings` in migration `031`.

**Reason:** Separate accounts keep each person's domains and settings isolated,
which the existing account boundary already supports. Keeping the system
administrator out of other accounts' mail preserves the account boundary rather
than making system administration a superuser. Reusing existing Owner semantics
for operators avoids inventing another permission tier, and routing invitations
through the normal outbound queue avoids a second mail path. A per-account
mailer keeps invitations inside the account's own sending credentials.

## D061 — The trusted-proxy per-request WARN is removed (D059 correction)

**Context:** D059 hardened the trusted-proxy path in two ways: `config.Load` refuses
a `TRUSTED_PROXIES` entry wider than `/8` (`minTrustedProxyBits`), and
`Config.IsTrustedProxy` logged at WARN on every request whose socket peer fell inside
the trust set, on the reasoning that a covering-set misconfiguration cannot be
detected at startup. In practice the second half reports the healthy case. With the
proxy actually in front, every proxied request wrote

    WARN request peer is inside the trusted-proxy set; forwarded headers will be honoured
        peer=203.0.113.10 prefix=203.0.113.10/32

and the same line appeared in the test-run logs for every trusted fixture peer, so the
WARN carried no information beyond "the configured proxy is proxying", and buried real
warnings.

**Decision:** `IsTrustedProxy` returns `true` for an in-set peer without logging.
The load-time `/8` floor remains and is the control that keeps the trust set from
covering ordinary clients; the runtime log line is not part of the control, so
removing it removes no protection. The matching prefix is still resolved the same
way, and `clientIP`, `cookieSecure` and the request-scoped trust checks in
`internal/httpapp` are unchanged.

**Reason:** A warning that fires on correct configuration trains an operator to
ignore warnings. The signal it was meant to carry is already enforced at load, where
it is enforced before the first request rather than after it.

**Consequences:**

- No behaviour change: trust decisions, rate-limit identity and the `Secure`-cookie
  decision are byte-for-byte the same.
- `log/slog` is no longer imported by `internal/config`; the file's only other
  logging call was this one.
- The reviewed round's report recorded the WARN as a delivered remediation. That
  record is superseded by this decision on the behaviour, not the history of the
  review round.
- D059's closing paragraph in this file is superseded on its second clause ("a peer
  found inside the trust set is logged at WARN").

## D062 — Unified clients and webhook delivery

**Context:** The product had two consumer kinds for an inbox: account-scoped API
keys (pull, with per-mailbox roles) and inbox-bound Hermes relay connections
(push over an outbound WebSocket). Both the dashboard and the operating model
already treated them together as "Clients". A third consumer was wanted: a
generic HTTP webhook that pushes incoming mail to a URL, with either a small
notification (the receiver fetches the body later with its own API key) or a
full forward of the raw MIME.

**Decision:** Model every consumer as a row in a single `clients` base table
with a `type` discriminator (`api_key`, `hermes`, `webhook`), plus type detail
tables: `client_api_keys` (hashed bearer), `client_inbox_bindings` (per-inbox
roles, formerly `api_key_mailbox_roles`) and `client_push` (inbox-bound push
config: gateway id, encrypted secrets, cursor, URL, payload mode, auth mode,
enabled). `webhook_deliveries` is the durable per-event delivery queue.

- Migration 032 creates the client tables and copies existing API keys and
  Hermes connections into them, preserving their ids (`key…`, `hrm…`) so
  `messages.client_id`, `draft_send_requests` and role bindings keep resolving.
  The legacy `api_keys` / `api_key_mailbox_roles` / `hermes_connections` tables
  are retained and dual-written for API-key lifecycle, so any not-yet-migrated
  reader stays correct; all runtime reads go through the client tables.
- Webhook clients are bound to exactly one inbox, delivery-only: no bearer, no
  `/v1` access, no outbound send. Hermes keeps its `outbound_role`.
- Payload modes: `notify` (fixed small JSON: event, cursor, inbox_id,
  message_id; the receiver fetches the rest with its own API key) and `forward`
  (byte-for-byte raw MIME streamed as `message/rfc822`, bounded by
  `MAX_MESSAGE_BYTES`). Both modes deliver `message.received` and
  `message.spam_state_changed`, skipping currently-Spam mail, matching Hermes.
- Auth is chosen per client: `signature` (timestamped
  `X-MailMoose-Signature: t=…,v1=…`, HMAC-SHA256 over `t + "." + body`) or
  `bearer` (static `Authorization: Bearer`). The secret is generated once,
  shown once, stored encrypted, and rotatable.
- Delivery reuses the durable event log and a background worker (the same
  outbox worker pass): one inbox-ordered head event per client, HTTP 2xx acks
  and advances the cursor, failures retry with capped exponential backoff, and
  a delivery that has not succeeded within `WEBHOOK_RETRY_WINDOW_DAYS`
  (default 7) is marked failed and the cursor advances so a dead endpoint
  cannot block a mailbox forever. `enabled` pauses delivery without losing the
  cursor.
- The destination is validated as HTTPS at save and dialled with the existing
  `netutil` public-routable guard, so a webhook cannot be pointed at a private
  or loopback address.
- `/v1/admin/clients` is the canonical listing; `/v1/admin/clients/webhooks*`
  manages webhook clients; the existing `/v1/admin/keys` and
  `/v1/admin/hermes` routes remain as compatibility aliases.

**Reason:** One identity table gives one list, one id space and one revoke
path for every way mail reaches a consumer, and the webhook is structurally
the Hermes connection with an HTTP transport instead of a WebSocket, so it
shares the same cursor, secret custody and cancellation machinery. Keeping
API-key and Hermes HTTP routes as aliases preserves existing integrations.

**Consequences:**

- A webhook is a delivery accelerator like Relay: SQLite remains the durable
  source of truth and the cursor is what makes retries and catch-up correct.
- The legacy client tables still exist on upgraded databases. Dropping them is
  deferred until every reader is migrated; this is recorded here so it is not
  mistaken for an oversight.
- Mode and auth are editable without resetting the cursor; mode changes only
  the shape of future payloads.

## D063 — Subdomains inherit their parent domain's receiver and sender

**Context:** Cloudflare Email Routing is a zone-level feature whose catch-all
rule is stored only on the apex domain, yet mail sent to an explicitly onboarded
subdomain (Email Routing → Settings → Subdomains) is delivered to the single
Worker that the zone catch-all points at. In other words one Cloudflare Worker
and its generated shared secret connect a whole zone, and no separate Worker or
secret is needed per subdomain. (Cloudflare documents the catch-all as
apex-only and the "match every address" selector lists only apex domains; the
observed behaviour that an onboarded subdomain is still delivered to that Worker
is relied on, but Cloudflare's documentation does not state it explicitly, so a
future change is a risk to watch.) MailMoose's model, however, required every
recipient domain to have its own `domains` row with its own receiving config, so
routing `foo@agent.example.com` needed a second domain row and a second receiver
config/secret — even though Cloudflare already delivered it to the same Worker.
The product goal is that adding one top-level domain and one connector lets new
addresses be created on any of its (onboarded) subdomains without configuring
another receiver.

**Decision:** A domain may be a subdomain of another domain in the same account
and optionally inherit the parent's receiving and/or sending configuration,
resolved at read time rather than copied.

- Migration 033 adds `domains.parent_domain_id` (nullable, self-reference,
  `ON DELETE CASCADE`), `inherit_receiving` and `inherit_sending`.
  `CreateDomainWithOptions` records the nearest same-account ancestor whose name
  is a proper label-suffix of the new name and, by default, sets both inherit
  flags; the operator can opt out per slot on create or toggle later.
- `ResolveDomainReceivingConfig` / `ResolveDomainSendingConfig` return the
  domain's own configuration when present, otherwise the nearest ancestor's
  while the matching inherit flag is set. A domain explicitly configured for a
  *different* provider does not fall through — it is simply unconfigured for the
  requested provider.
- `ResolveInboundBinding` resolves the recipient's own domain row (so the
  downstream account/domain scope check is unchanged and always names the child)
  but takes the credential from the effective resolver. This covers every
  webhook provider and the MX edge, because they all resolve bindings the same
  way. `ResolveRecipient` is untouched: a subdomain is an ordinary domain owning
  its own inboxes, aliases and catch-all.
- Inheritance is provider-agnostic. Rotating the parent's secret or switching
  the parent's provider applies to every descendant automatically.
- Sending inherits the same way, but a provider may still reject an outbound
  From on a subdomain it has not authorised (for example Cloudflare Email
  Sending onboarding, or Mailgun domain verification); the UI warns rather than
  blocks, since the operator may have provisioned it externally.

**Reason:** It keeps subdomain addresses first-class (each is its own `domains`
row with its own catch-all, storage and future per-subdomain settings) while
removing the receiver duplication that Cloudflare's zone model already implies.
Resolving at read time means the connector stays singular per zone: one Worker,
one secret, no repaste per subdomain. The alternative — treating subdomain mail
as the parent's namespace with a wildcard flag — would collapse address identity
and make subdomain `From:` impossible, so it was rejected.

**Consequences:**

- External routing is still per-provider: Cloudflare requires each subdomain to
  be onboarded under Email Routing → Settings → Subdomains before its mail
  reaches the Worker. Inheritance removes the *app-side* receiver duplication,
  not the provider-side onboarding.
- `ResolveRecipient` and the ingest scope check are unchanged; a subdomain's
  catch-all inbox must belong to the subdomain, not the parent.
- Domain-ownership rules for subdomains and parent domains are out of scope
  here; this decision settles the Cloudflare subdomain inheritance behaviour only.
- `docs/CLOUDFLARE_INBOUND.md` is corrected: catch-all is apex-only, each
  subdomain must be onboarded, and one connector serves the zone.

**Amendment — linking a parent added after the subdomain.** Creation-time
detection only links a subdomain to an ancestor that already exists, so a
subdomain added first stays a root domain when its parent is added later.
Rather than back-fill automatically (which would silently change the routing of
existing mail), linking is an explicit action:

- `Store.SetDomainParent` links or unlinks a domain. Linking validates that the
  target is a proper suffix ancestor in the same account and is not the domain
  itself or one of its descendants (cycle guard bounded by
  `maxDomainAncestorDepth`), records the parent, and turns both inherit switches
  on — matching the create-time default. An empty parent id unlinks and clears
  both switches. No parent configuration is copied; resolution stays read-time.
- The dashboard's sending/receiving provider menus offer **Inherited (from
  <parent>)** for such a domain, naming the nearest existing ancestor; saving
  that choice links the domain and turns on the matching inherit switch. The
  Admin API accepts `parent_domain_id` on `PATCH /v1/admin/domains/{id}` (empty
  string to unlink, omission to leave unchanged). `POST /v1/admin/domains` and
  the UI creation dialog are unchanged.

## D064 — Self-hosted-only product; hosted mode removed

**Context:** The product was scoped for two deployment shapes from one codebase:
a free multi-tenant hosted service and a self-hosted container. The hosted
multi-tenant implementation was explored on a separate branch and then
abandoned; it is archived outside this repository. The project is published as
free, open-source, self-hosted software.

**Decision:** MailMoose is self-hosted-only. `MODE` accepts only `selfhosted`.
The hosted branches are removed from the codebase:

- the `SelfHostedOnlyProvider` transport gate and the `OutboundAllowed` helper;
- the hosted branch of `Config.RequirePublicOutbound`;
- the hosted refusal in external-alias authorization and the UI/API gates;
- the `(Admin, self-hosted)` distinction in the API spec;
- the hosted design and review documents.

**Reason:** Carrying a multi-tenant mode the project does not operate and does
not test is a liability in a public repository: it advertises behaviour that has
no implementation, and it keeps dead gating paths alive in security-relevant
code. Behaviour for a self-hosted deployment is unchanged; the only
user-visible difference is that `MODE=hosted` is now a startup error rather than
a silently self-hosted boot.

**Consequences:**

- `the removed hosted design notes` and the hosted implementation are not part of this repository.
- The account-level storage controls and the self-hosted Direct MX provider are
  retained; only the hosted-mode gating around them is gone.
- Account registration remains configuration-controlled and defaults to closed.

## D065 — In-flight delivery attempts are durably recorded

**Context:** A large outbound message (~19 MB) sent over Direct MX was delivered
to the recipient, but the message stayed `pending`, its claim lease was left
set, and the sending log had no row for it: the only writers of
`outbound_delivery_log` were the terminal `MarkSent`/`MarkFailed`, which run
after the provider call returns. An interrupted send — a process restart, crash
or dropped connection during the upload — therefore left no trace at all and
silently looped as pending even though the remote had accepted the message. A
second defect compounded it: `smtpTransaction` applied a single 45-second
deadline to the entire SMTP transaction, including the multi-megabyte `DATA`
upload, and treated a failed cosmetic `QUIT` as a delivery failure even after
the remote had accepted the message with a `250`.

**Decision:** A delivery attempt is recorded durably when it starts, before the
provider call:

- `outbound_delivery_log.status` gains `sending` (in flight) and `interrupted`
  (an in-flight attempt abandoned before an outcome was written). Migration 034
  rebuilds the table with the widened CHECK; existing rows are carried over.
- `Store.RecordDeliveryStarted` / `RecordWorkflowDeliveryStarted` append a
  `sending` row before `provider.Send` and mark any earlier `sending` row for the
  same message/job `interrupted`, so a re-claim records the previous attempt's
  fate instead of leaving a dangling row.
- The MX outbound transaction uses a per-command deadline that each SMTP command
  phase refreshes and that is cleared for the body upload; the body is bounded
  only by the caller's context. The acceptance of `DATA` (`w.Close()` returning a
  2xx) is the delivery outcome, and a failed `QUIT` afterwards is ignored.

**Reason:** Durable truth before notification is the project's core invariant.
Recording a send only on its terminal outcome inverts it: an interrupted send is
invisible, so an operator cannot see that the remote may already have accepted
the message, and the worker re-claims and re-sends it. A whole-transaction SMTP
deadline is wrong for a large message, and treating a post-acceptance `QUIT`
failure as a failure would itself cause duplicate delivery.

**Consequences:**

- The sending log and outbox show an in-flight message as "Sending…" and an
  abandoned attempt as "Interrupted"; the outbox listing sets `sending` on a
  message with an in-flight row.
- A message that is actually delivered but whose outcome is lost is now visible
  as `sending`/`interrupted` rather than an empty log, so an operator can
  reconcile it instead of re-sending blind.
- Retry and success/failure semantics are unchanged for genuine provider errors.
- The delivery-log retention (newest 5000 rows / 30 days) is unchanged; the
  extra in-flight row counts toward it.

## D066 — Persist the transport envelope sender; forward envelope metadata; terminal webhook skips

**Context:** Two gaps surfaced while wiring MailMoose to an agent that consumes
forwarded inbound mail:

1. Inbound messages persisted the canonical original envelope recipient
   (`messages.envelope_recipient`, D029/D030) but **dropped the transport-supplied
   envelope sender** at the shared ingest boundary. Every adapter already parsed
   it into `transport.InboundMessage.EnvelopeFrom` (Cloudflare Worker header,
   Mailgun `sender` form field, Resend Svix payload, MX `MAIL FROM`), but
   `deliverStaged` never wrote it to the message row. The approval workflow used
   the in-memory value and then lost it, so a durable consumer had no way to
   read the original envelope sender.
2. D062 stated that webhook delivery "skips currently-Spam mail, matching
   Hermes", but `NextWebhookDelivery` did not filter on Spam (or anything else).
   A message that was Spam, internal, or deleted could pin the head of a
   client's queue indefinitely, and a forward of a deleted message failed on
   every retry.

**Decision:**

- **Persist the envelope sender.** Migration 035 adds
  `messages.envelope_from TEXT NOT NULL DEFAULT ''`. The shared ingest
  (`deliverStaged`) normalizes the relay value once and passes it through
  `store.InboundRecord.EnvelopeFrom` for both the approval control path and the
  message row. It is bounded to 254 octets (RFC 5321 address maximum) and
  rejected as empty if it exceeds that or contains control characters. A
  missing/null sender stays empty: there is **no MIME-header fallback and no
  backfill guessing**, on upgraded or fresh databases. The message reads back
  `envelope_from` and `envelope_recipient` on the normalized API model.
- **Forward envelope metadata on the wire.** A forward webhook adds two headers
  to the byte-for-byte `message/rfc822` body: `X-MailMoose-Envelope-From` and
  `X-MailMoose-Envelope-To`. Values are percent-encoded UTF-8 using RFC 3986
  escaping (space `%20`, plus `%2B`, `@` `%40`; not query/`form` encoding where
  `+` means space). Exactly one header each; a missing sender is sent as an
  empty value so the receiver fails closed. The values come from the persisted
  transport metadata, so they are identical on retries. Existing event,
  delivery, message and cursor headers, the `message/rfc822` content type, the
  raw MIME bytes, and the bearer/signature scheme are unchanged. The sender is
  **relay-supplied**, not provider-attested (D029/D058): this is metadata a
  receiver may record and display, not authority it should trust without a
  provenance check.
- **Skip non-deliverable events terminally.** Migration 035 widens
  `webhook_deliveries.status` to include `skipped`. Immediately before dispatch
  the worker re-checks the message: if the event is a "moved to Spam"
  transition, or the message is currently Spam, internal, or has been deleted,
  it records a terminal `skipped` delivery and advances the cursor, with no
  network call. `message.spam_state_changed` uses the event's own new state, so
  a stale spam transition is skipped even if the message is later released
  (releasing enqueues its own deliverable event). The worker drains a run of
  skipped events and then delivers the first genuine head, so a Spam/deleted/
  internal event cannot block the queue and the `skipped` status is filtered
  from future head selection.

**Reason:** Durable truth before notification applies to inbound metadata too:
persisting the envelope sender makes the approval boundary's input auditable and
lets downstream consumers read exactly what the transport observed without
re-parsing MIME. The webhook skip completes D062's stated behaviour and closes a
head-of-line stall: a delivery accelerator must never be able to wedge a
mailbox's event stream because one message was later quarantined or deleted.
Bounding the value and rejecting malformed metadata keeps the persisted field
safe and prevents a crafted relay value from being echoed as authority.

**Consequences:**

- A forward receiver that needs an attested sender must establish provenance
  itself; the header is documented as relay-supplied in `SECURITY.md`.
- Older messages keep an empty `envelope_from`; nothing is fabricated for them.
- Webhook clients gain a terminal `skipped` state; a delivery in that state is
  not retried and does not advance a client's error state. The dashboard-facing
  `last_error` is not set by a skip.
- No backward-compatibility alias is needed for a capability that has not yet
  shipped.
- The joint wire contract with the consuming agent (Billbot) is documented in
  `docs/MAILMOOSE_CONTRACT.md`, with a byte-identical fixture committed at
  `tests/fixtures/mailmoose-webhook-contract.json` in both repositories. Both
  are maintained by hand; the generating worker test is
  `TestWebhookForwardContractFixture`.

## D067 — Dial MX: an outbound-dialing standalone receiver

**Context:** Direct SMTP ingress needs the core to accept a connection. The
existing optional edge (`D031`) runs as a policy-free process that calls the core
over signed HTTPS, but the core still exposes an inbound `:8082`. A deployment
that cannot publish an inbound port — behind NAT, with no reverse proxy, or with
a policy against opening one — could not use direct SMTP at all. The requirement
is to receive direct SMTP for a domain with only **outbound** HTTPS/2 from the
core to a receiver the operator runs.

**Decision:** Add Dial MX, a second, independent direct-SMTP path. Per domain,
the receiving provider is either `mx` (the edge calls the core) or `dialmx` (the
core dials a receiver); never both. The receiver is one pure-Go process, built
from `dialmx/cmd/receiver` and shipped as a separate minimal image
(`dialmx/Dockerfile`), that:

- terminates SMTP and computes SPF/DKIM/DMARC evidence on the original bytes,
  exactly as the MX edge does (`MX_VERIFY_*` reuses the documented surface);
- exposes an HTTPS/2 session endpoint (`POST /mx/v2/session`) to which the core
  dials out, proves control of the domain's Ed25519 signing key via a
  DNS-anchored challenge against `_mailmoose-mx.<domain>`, and then serves
  `Resolve`/`Ingest` requests;
- holds no `/data`, no database, no `APP_ENCRYPTION_KEY` and no core HMAC
  secret, stages messages in memory, and returns SMTP success only after the
  core durably handled every accepted recipient (a partial/lost ack is `451`).

The core stores a per-domain Ed25519 key pair encrypted with
`APP_ENCRYPTION_KEY`; the matching public record is published in DNS. A
subdomain may inherit receiver settings but always has its own exact-domain key
and DNS proof. Key rotation uses the credential revision, independently of the
receiving configuration revision, and never copies inherited settings.
Receiver URLs are bounded (at most eight, HTTPS origins with no path, userinfo,
query or fragment) and each is authorised independently. `MX_ENABLE` governs
only the existing inbound MX edge. Local/remote MX domains and Dial MX domains
can coexist, and a Dial-MX-only deployment needs no shared edge secret.

**Reason:** Reversing the connection direction removes the public inbound-core
requirement while retaining synchronous SMTP disposition. The new `mx-v2`
session contract (24-byte header, bounded JSON metadata, chunked body) is shared
by the dialer and receiver; existing `mx-v1` POST/HMAC behaviour is retained.
Making the receiver stateless with in-memory staging preserves the project's
invariants — durable truth stays in the core, realtime/transport layers never
hold it — and keeps the receiver outside the app's trust boundary: it never sees
the database or the app key. Reusing the `MX_*` names avoids a parallel
configuration vocabulary.

**Consequences:**

- The standalone receiver is a deliberate **optional component** outside the
  single-process default. It is an operator-chosen exception, like the MX edge,
  and does not restore a hosted/account mode: `D064` stands, only self-hosted
  `MODE` is accepted.
- The core opens one outbound HTTPS/2 session per receiver URL and registers the
  union of that receiver's domains; direct SMTP needs no inbound port on the
  core, though the general inbound webhook listener on `:8082` still runs for
  the webhook connectors unless the operator does not expose it.
- Revocation is DNS-driven: the receiver re-resolves the TXT record at every
  proof, so a removed or rotated key fails closed as soon as the receiver's
  resolver observes it. An upstream DNS cache with a long TTL can delay that
  observation beyond the 5-minute local binding lifetime; the binding itself is
  never extended past `AuthLifetime` from the last proof.
- The protocol is **not exactly-once** (same limits as `D031`): deduplication is
  the core's 7-day `mxfp-v1` receipt window, and a replaced binding may keep
  serving already-pinned DATA until it expires.
- Domain authentication expires after five minutes and renews with fresh DNS
  and a fresh, connection-bound Ed25519 proof. Last valid authentication wins
  on each receiver; old unexpired pins may finish DATA but receive no new RCPT.
- No mTLS is required. `DIALMX_CA_FILE` optionally adds private receiver CAs to
  system roots with mandatory hostname verification. Multi-core HA, a durable
  receiver queue, wildcard authority and overlap rotation remain deferred.
  Private seeds are encrypted using the existing root key, superseding the
  roadmap's original plaintext-at-rest assumption.
- `MODE=hosted`, account billing and a service-wide registry are not introduced:
  the receiver is configured per domain through the existing encrypted receiving
  configuration, and the UI provider is named **Dial MX** (slug `dialmx`,
  retained for compatibility).

## D070 — One MX receiver transport, private single-core and public shared modes

**Requirement:** Simplify MX code and maintenance by replacing the private
edge-to-core HTTP/HMAC transport with Dial MX sessions. The operator explicitly
accepts breaking compatibility; only a test instance is live.

**Decision:** Build `mailmoose-mx` from `dialmx/cmd/receiver` for standalone and
embedded deployments. `DIALMX_MODE` defaults to `single`, authenticating one
core using `DIALMX_CORE_KEY` in an HTTP bearer header. No domain registrations
or ownership proofs occur; the core's `mx` receiving configuration decides
recipients. `shared` preserves DNS-backed Ed25519 domain authentication and TLS.

Single-mode sessions use verified TLS when configured, or cleartext HTTP/2
when certificates are absent. The built-in isolated child binds loopback and
gets an automatically provisioned bearer key. Cleartext transmission of the
bearer credential and email content on operator-controlled loopback/LAN is an
explicitly accepted tradeoff. SMTP STARTTLS remains independent. Forwarded IP
headers are not trusted and no proxy configuration is required.

The newest established private session receives new work; old pinned work may
finish while its session remains live. The core is the durable source of truth,
and no SMTP success precedes durable acknowledgement. Retry receipts remain.

**Complexity:** No dependency or runtime service is added. The existing Go HTTP/2
support handles both encrypted and unencrypted sessions. A mode-specific session
authorization/routing boundary replaces the signed HTTP client, core endpoints,
replay cache and HMAC/framing helpers. One delivery implementation serves both
modes. Old transport configuration has no compatibility aliases.

This supersedes prior MX decisions on transport direction, HMAC request
authentication and embedded credentials, while retaining isolation, email
authentication policy and durable delivery semantics.

## D071 — MX receiver configuration is persisted and reconciled at runtime

**Requirement:** The installation-wide MX receiver must be configurable from the
system-administrator UI/API and survive restarts, instead of being fixed by the
process environment (`MX_ENABLE`, `MX_RECEIVER_URL`, `DIALMX_CORE_KEY`). The
embedded child must be spawnable before the database is open, and a receiver
failure must never take the core down.

**Decision:** Add a singleton `mx_settings` row (migration 038): `mode`
(`included` or `remote`), a cleartext receiver URL, an encrypted bearer
credential and an encrypted auxiliary configuration, plus a CAS `revision`. The
encrypted configuration covers the whole included-edge SMTP surface: hostname,
message/staging/recipient/connection limits, RequireTLS, the SPF/DKIM/DMARC
verification toggles, the DNS resolver and DNS/read/write/data timeouts, and an
optional STARTTLS certificate/private-key PEM pair (the blob is encrypted with
`APP_ENCRYPTION_KEY`, so the private key is protected at rest like the bearer
credential). The public read returns the certificate but never the private key,
reporting `smtp_tls_key_configured` instead; the runtime accessor decrypts the
key. Environment variables are no longer authoritative, and `Load` never rejects
them: a stale or partial value must not block a deployment whose configuration is
already persisted. On first start, if no row exists, the legacy environment is
imported once through the same validated save path — every `MX_*` included
tunable, the `MX_VERIFY_*` toggles (default on), and the `MX_TLS_CERT`/`MX_TLS_KEY`
files read once into the stored PEM pair (a partial or unreadable pair aborts
the import, leaving MX unconfigured while the core remains available) — and a partial remote environment
(`MX_ENABLE=remote` without URL or key) is rejected at that point. Once a row
exists (including an explicitly cleared one) the environment is ignored, and
clearing keeps the row so the import never re-fires. Unset legacy values stay
zero so the child's own defaults apply, and a save with blank values normalises
rather than persisting environment-style defaults.

`cmd/server` spawns the included child in standby before the privilege drop,
unconditionally when root, with no credential in its environment; the persisted
key and, when configured, the STARTTLS certificate/key PEM are sent over the
private control channel on activation (the child cannot read the core's `/data`,
so file paths cannot be used; `control.Settings` carries
`TLSCertificatePEM`/`TLSPrivateKeyPEM` and the runtime builds the certificate in
memory). The child stays dormant when no receiver is configured. An `mxRuntime`
controller in `cmd/server` reconciles the persisted revision to the live
receiver. Every mode change first
drains the child (deactivates it, letting the still-running private session finish
in-flight work) and only then stops the private dialer, so reconfiguring an
already-active child never fails with "already active". A failed apply does not
advance the applied revision, so the 30-second reconcile tick retries it; a
rootless core supports `remote` only and reports `included` as unavailable rather
than failing. Remote readiness is taken from the dialer's live connection status
(never inferred from the mere existence of a dialer), so a receiver that has not
completed a handshake reports `connecting`, not `active`. Receiver failures are
surfaced in `MXReceiverStatus` and never exit the core. `MXReceiverStatus`
consults the runtime even when no settings are persisted, so a fresh install
still reports whether the included shape is available. A second, shared dial
manager continues to serve per-domain Dial MX domains.

The bearer key is mode-bound: switching `remote`→`included` generates a fresh
included key (the remote key is never reused), and switching `included`→`remote`
requires the operator to supply the remote key rather than reusing the generated
one.

**Complexity:** No dependency or runtime service is added. One table, one
controller goroutine, bounded child control framing and a status read. The bearer credential is stored through
the existing application-key encryption; the runtime reads it through an
internal-only service accessor and the UI/API never sees it. The controller sits
behind a small interface so its ordering and retry behaviour are unit-tested
without spawning a child.

This supersedes D038/D039's environment-driven activation and default-on MX:
the included process starts dormant, and saved UI configuration activates it.
Installation API operations use the system-administrator session and require
CSRF protection on writes; account API keys gain no installation privileges.
Auto MX is a deferred, predefined shared service with manual DNS publication
but no core/edge credentials or enrollment for its users. No platform mTLS,
hosted-core domain claims, resource-fairness quotas or overlap rotation are
introduced by this change.

## D072 — Bounded shared MX shards and dial-time destination policy

**Requirement:** A shared receiver must serve more domains than one session's
domain cap without losing healthy registrations, while preserving private
loopback/LAN receivers and preventing arbitrary shared URLs from bypassing the
existing public-destination boundary.

**Decision:** Shared domains are packed into bounded sessions with stable
assignments, a local domain ceiling, advertised receiver limits and a bounded
number of shards. New domains fill available capacity without moving healthy
bindings; exhausted capacity is reported explicitly. Initial proofs and
receiver-driven renewals are paced. Temporary local authentication saturation
can retry within an existing grant, but cannot extend that grant's expiry.
Private single-core receivers remain one session.

The dedicated streaming transport checks DNS inside the actual dial and connects
to the checked numeric IP. Included and explicitly configured Remote sessions
allow local destinations; that permission is connection-scoped. Existing custom
shared URL configurations follow public-outbound enforcement and its deliberate
self-hosted opt-out. A future predefined Auto service must always use its
public-only policy. Redirects remain disabled and TLS hostname verification
remains mandatory on HTTPS sessions.

**Compatibility:** New Ready limit fields require coordinated core/receiver
upgrades because old strict JSON decoders reject unknown fields. Old Ready
frames remain readable by the new core using bounded local defaults. This
supersedes D067's one-session-per-URL assumption; it does not introduce automatic
core HA or resource-fairness identity accounting.

**Complexity:** No dependency or service is added. Session assignment bookkeeping,
bounded authentication scheduling and receiver capability fields stay within
the existing Go processes. Durable delivery and retry receipts are unchanged.

## D073 — Trash: soft-delete, restore, configurable retention

**Decision:** Deleting a message moves it to Trash instead of erasing it.
A message carries a `deleted_at` timestamp (NULL when live) and is hidden from
every ordinary read surface — message lists, search, thread listings and unread
counts — while its row, raw MIME, attachments, FTS entry and storage accounting
are retained. Trashed mail is a computed view over `deleted_at`, exactly like
Spam is a view over `is_spam`; there is no separate Trash table.

- `DELETE /v1/messages/{id}` and the UI delete actions move to Trash
  (Assistant/Owner on the inbox). `POST /v1/messages/{id}/restore` clears
  `deleted_at` (Assistant/Owner). `DELETE /v1/messages/{id}/purge` erases a
  trashed message permanently and unlinks its raw file (Owner). A message must
  be trashed before it can be purged.
- `POST /v1/inboxes/{id}/trash/empty` (Owner) purges every trashed message in an
  inbox. `DELETE /v1/outbox/{id}` now moves a pending/failed outbound message to
  Trash rather than erasing it; trashed outbound messages leave the outbox
  listing immediately.
- Trashed messages continue to count toward `accounts.storage_used_bytes` until
  purged, matching how Spam and every other retained message are accounted.
- Each account has `trash_retention_days` (default 0). The maintenance worker's
  `purgeExpiredTrash` sweep permanently purges trashed messages older than the
  window and unlinks their raw files. A value of 0 disables automatic purging,
  so trash is retained until emptied by hand; a positive value enables the
  sweep. It is read/written through
  `GET`/`PATCH /v1/account/settings` and the Account page (Owner/Admin).
- Durable events `message.trashed`, `message.restored` and `message.purged` are
  emitted transactionally and published to SSE/long-poll. They are deliberately
  **not** relayed over Hermes (which still carries only `message.received` and
  `message.spam_state_changed`); Webhook delivery treats a trashed message like
  a deleted one (terminal `skipped`).
- The dormant `messages.is_archived` column and the `archived` PATCH field are
  removed. Archive was never a real folder (it only excluded messages from the
  unread count); Trash is the first real mailbox state. Migration 039 rebuilds
  the `messages` table to drop `is_archived` and add `deleted_at`, and adds
  `accounts.trash_retention_days`.

**Reason:** A delete that erases mail with no recovery is hostile to both human
and agent users. Trash gives reversible deletion with bounded storage growth,
and per-account retention lets an operator choose between aggressive reclamation
and keep-until-manual-purge without a global policy.

**Complexity:** No dependency or service is added. Trash is one nullable column
plus a per-account integer, a handful of store methods, a maintenance sweep that
reuses the existing file-unlink path, and new API/UI routes. The `messages`
table rebuild in migration 039 carries every row over unchanged.

## D074 — OpenClaw is a second relay connector kind over the same transport

**Decision:** OpenClaw is an inbox-level outbound relay connector, exactly like
Hermes: the OpenClaw host dials an authenticated WebSocket out to MailMoose, no
inbound port or webhook is required, MailMoose remains the durable source of
truth, unacknowledged email is replayed after a reconnect, and replies return
over the same socket through the normal send path. OpenClaw is nevertheless a
distinct connector kind in the product model (`clients.type = 'openclaw'`), not
a Hermes alias.

- The wire contract, HMAC upgrade token, `hello`/`descriptor` handshake,
  `inbound`+`bufferId`/`inbound_ack`, `outbound`/`outbound_result`, ping and the
  delivery log are unchanged and shared. There is no second relay transport and
  no `/openclaw-relay` route.
- Relay persistence stays `clients` + `client_push`. Migration 042 widens the
  `clients.type` CHECK to accept `openclaw` (table rebuild, `fkOff`, because four
  tables reference `clients`) and adds `hermes_enroll_tokens.kind` so a one-time
  setup code remembers which connector it creates. Existing rows default to
  `hermes`.
- One-time setup codes reuse `POST /relay/enroll`. The code is an authority, not
  an address: the claimant supplies the MailMoose URL. The connector UI shows
  `openclaw channels add --channel mailmoose --code <setup-url>`, where the URL
  origin is the base URL and the `#fragment` carries the code; a manual
  `channels.mailmoose` JSON5 block is the air-gapped fallback. The code expires
  after 15 minutes, is stored hashed, and is single-use.
- Admin surface is connector-specific and mirrors Hermes:
  `GET/POST/PUT/DELETE /v1/admin/openclaw` plus
  `POST /v1/admin/openclaw/setup-code`. OpenClaw connectors render beside
  Hermes and Webhook in the inbox connector list with the same Settings, Log and
  delete actions, and the same no-allow-list risk warning.
- Outbound authority reuses the relay role semantics: `owner` sends directly,
  `assistant` creates an approval-required draft. Sender provenance is
  deliberately untrusted on the connector side: the OpenClaw plugin dispatches
  email as external, unauthenticated input and the MailMoose inbox allow list is
  the primary sender gate (the same accepted risk as D058).

**Reason:** The existing relay already provides every mechanic OpenClaw needs.
Making OpenClaw a connector kind rather than a second transport keeps the
user-visible model honest (a distinct connector chip, log and revocation path)
without duplicating authentication, replay or acknowledgement code, and it keeps
the small-runtime constraint: one process, one container, one store.

**Complexity:** One migration rebuild, a kind discriminator on the existing
relay rows, connector-specific admin routes and UI parity, and a small external
OpenClaw channel plugin. No new runtime service or dependency is added to
MailMoose.

## D075 — Passkey backup flags, sysadmin break-glass, and atomic auth-method changes

**Decision:** Passkeys are an additive sign-in method with three supporting
invariants made explicit:

- The backup-eligible and backup-state flags observed at registration are
  persisted and reconstructed on the credential before assertion verification.
  The `go-webauthn` library rejects an assertion whose authenticator data
  disagrees with the stored backup-eligible flag, so a synced passkey
  (iCloud Keychain, Google Password Manager) cannot log in if the flag is
  dropped.
- The system administrator can never disable password sign-in. The
  "make this my only sign-in method" flow is hidden and refused for that
  account, and `SyncSystemAdmin` always writes `password_auth_enabled=1` when it
  reconciles the config-owned credential. The config password remains the
  break-glass recovery path documented in SECURITY.md.
- `SetPasswordAuth` and `UpdateWebAuthnCredentialUse` perform their read and
  write in a single `BEGIN IMMEDIATE` transaction, so a concurrent passkey
  delete cannot interleave with disabling password auth and leave an account
  with no usable method.

**Reason:** These are correctness and safety requirements, not preferences. The
backup-flag omission broke every synced passkey; the sysadmin guard preserves
the documented recovery invariant; the transaction removes a check-then-act
race in the lockout guard.

**Complexity:** No new dependency, service or schema change beyond reading
columns that already existed. The sysadmin guard adds one conditional in the
store, and the transaction change is local to two store methods.

## D076 — Account-admin settings separation and per-inbox Trash retention

**Decision:** The Account page is organised into three labelled sections so
personal preferences, account-wide settings and account administration are
visually distinct:

- **Your settings** — a user's own display time zone override, passkeys, and
  email/password. Available to every signed-in user.
- **Account settings** — account name, account default display time zone, and
  the account Trash auto-purge window. Account-Admin only, enforced on the
  server (`uiSettingsAccount`, `uiSettingsAccountTimezone`, and the existing
  operator routes require `users.is_admin`).
- **Account administration** — the account mailer, mailbox operators and
  invitations. Account-Admin only.

The account default Trash retention may be overridden per inbox from the inbox
Quota tab (`inboxes.trash_retention_days`, nullable). `NULL` inherits the
account's `accounts.trash_retention_days`; `0` keeps that inbox's trashed mail
until purged by hand; a positive value purges after that many days. The
maintenance sweep computes the effective window as
`COALESCE(inbox, account)`. The override is exposed on
`PATCH /v1/inboxes/{id}` as `trash_retention_days` (integer to set, `null` to
inherit, absent to leave unchanged). Per the Account page's one-route grouping
decision, no new page or route is introduced.

**Reason:** Account-level controls (rename, account-wide retention and time
zone, operator management) were interleaved with personal settings on one
unlabelled page, and the account name could be changed by any signed-in user.
Grouping plus server-side Admin enforcement makes the blast radius of each
control explicit. Retention was account-wide only; a single mailbox with
different legal or operational retention needs had no way to express it. A
nullable per-inbox override adds that without disturbing the account default.

**Complexity:** One nullable column (`migration044`), a `COALESCE` join in the
retention sweep, one store setter, an optional API field, and template/JS
grouping. No new dependency, service or route.

## D077 — Sign in to the web UI with a non-admin mailbox API key

**Decision:** The login page gains a *Sign in with an API key* path
(`POST /login/key`) beside password and passkey. A **non-admin API key that
carries at least one mailbox binding** can be exchanged for a browser session:

- The session resolves to the key's own per-inbox roles from
  `client_inbox_bindings` and lands on the existing operator view. It grants
  nothing the bearer API did not already grant — no account Admin dashboard, no
  `/admin` plane, no installation-management API, no Account administration.
- **Admin keys are rejected.** An admin key carries no mailbox bindings, and
  this path deliberately maps mailbox access, not admin mode.
- The session lives in a new `key_sessions` table (`migration045`), because a
  key is not a `users` row and the `sessions` table's `user_id` is `NOT NULL`
  and FK-bound to `users`. Only the hashed session token and a CSRF token are
  stored; the key's live state (existence, `revoked_at`, bindings) is resolved
  on every request, so **revoking or rotating the key invalidates the browser
  session immediately**. Revoke/rotate/edit also delete the key's
  `key_sessions` rows outright.
- The session TTL is `min(SESSION_TTL_HOURS, 24h)`: never longer than an
  ordinary session, never more than a day, because a machine credential is more
  likely to be shared or pasted than a password.
- A key session's Account page is a slim variant that states the key and its
  mailbox access; it renders none of the human email/password/passkey/time-zone
  controls. Key-authenticated sends and approvals keep recording the key's
  actor identity, not "UI".

**Reason:** A human who holds only a scoped key (for example to try a key's
access visually, or an agent-first user whose sole credential is a key) had no
way into the web UI short of an invitation, which is a separate credential and
a separate person. Exchanging the key for a disposable session lets them use
the UI without inventing new permissions and without placing the long-lived
machine credential in a browser cookie. Restricting the path to non-admin,
mailbox-scoped keys keeps the rule simple and the surface small: the session is
exactly the operator view, and the admin plane stays unreachable because key
principals never carry `Admin` or `SystemAdmin`.

**Complexity:** One additive table (`migration045`), three store methods
(`CreateKeySession`, the key-session fallback in `SessionPrincipal`, and
`DeleteKeySessionsForClient`), one login handler and limiter, one login-page
form, one slim Account template, and four `DeleteKeySessionsForClient` calls on
the existing revoke/rotate paths. No new dependency, service or route beyond
`POST /login/key`. Tests cover resolution/scope, revoke, rotation teardown,
admin-key rejection, session death on revoke, and read-only write refusal.

## D078 — Delivery-triggered per-inbox auto-actions for agent connectors

**Decision:** An inbox may carry a delivery-triggered auto-action policy for its
agent/relay connectors: mark a message read once a connector has delivered it,
and optionally move it to Trash a configurable number of hours after delivery.
The policy is per-inbox (it applies to every connector bound to that inbox) and
is off by default. It applies only to agent and relay connectors; API keys and
human accounts never trigger it, because a poll is not a delivery.

- Three inbox columns carry the policy: `auto_mark_read_on_delivery`
  (default 0), `auto_trash_after_delivery_hours` (nullable; NULL disables the
  sweep), and `delivery_trigger` (`any` or `all`, default `all` — see D081, which
  supersedes the original `default any`). They are
  read/written through `PATCH /v1/inboxes/{id}`, the inbox Connectors tab (its
  own form and save control, posting to `POST /ui/inboxes/{id}/auto-actions`),
  and may also be seeded from the connector-create dialog.
- The Connectors tab is a **separate form** from the rest of the inbox edit
  dialog, because its fields are absent from every other tab's save. An inbox
  save that does not carry the auto-action controls leaves the policy untouched
  rather than reading the missing fields as "off" — treating absence as a clear
  silently wiped the policy on any unrelated edit. The submitter therefore
  applies the auto-actions only when the form actually carried them.
- A delivery is recorded per connector: `message_deliveries(message_id,
  client_id, delivered_at)` gets one idempotent row per successful delivery. The
  relay ack paths (`AckHermesEventLogged`, `RecordHermesDeliveryAcknowledged`) and
  the webhook success path (`RecordWebhookDelivery`) record it inside their own
  transactions, so the cursor advance and the delivery row commit together.
- The `any` trigger fires the actions the moment the first connector delivers.
  The `all` trigger waits until **every connector that existed when the message
  arrived** has delivered; a connector added later is not required, so old mail
  is never pinned by a new connector. When satisfied, `messages.delivery_action_due_at`
  is stamped once (delivery instant plus the configured hours); a later connector
  ack cannot extend an already-set window.
- A maintenance sweep (`trashDeliveredMail`) moves live messages whose
  `delivery_action_due_at` has passed to Trash, emitting the ordinary
  `message.trashed` event. The Trash model then applies its own retention
  unchanged. The actions skip Spam, internal (workflow) and already-trashed mail.
- Messages expose their per-connector delivery history (`deliveries`, each a
  `client_id` and `delivered_at`) and `delivery_action_due_at` on the read
  surfaces, so a caller can see which connector has handled a message and when
  it becomes eligible for auto-trash.

**Reason:** An agent-fronted inbox accumulates mail the agent has already
handled, and there was no way to express "once my agent has seen this, mark it
read and clear it out" without a human or a bespoke script. The Trash retention
control only ran from the moment a message was trashed, never from delivery, so
an unattended relay inbox could not self-maintain. Per-inbox ownership matches
the existing Trash-retention model (D073/D076) and avoids a per-connector
surface; the `any`/`all` choice covers both a single-agent inbox and a shared
one without a second policy object.

**Complexity:** One migration (three inbox columns, `message_deliveries`,
`messages.delivery_action_due_at` and its partial index), four store methods, one
maintenance step reusing the existing event path and file-unlink flow, optional
`PATCH` fields, and a dedicated auto-actions form on the inbox Connectors tab
(its own save control, posting to the existing `POST /ui/inboxes/{id}/auto-actions`
endpoint). No new dependency or service.

## D079 — Per-inbox storage quotas

**Decision:** An inbox carries an optional storage cap layered on top of the
existing account-level quota. It bounds **stored bytes** for one inbox so a
single agent mailbox cannot consume an account's whole allowance.

- `inboxes.storage_quota_bytes` is nullable: `NULL` means the inbox has no cap
  of its own (only the account quota applies), `0` means explicitly unlimited,
  and a positive value is the cap in bytes. `inboxes.storage_used_bytes` is the
  maintained counter and is nullable so a pre-migration inbox is distinguishable
  from an empty one.
- Accounting mirrors the account rule exactly: non-workflow messages in any
  direction or state (inbound, outbound, Spam, trashed) plus the inbox's
  editable drafts and draft attachments. Workflow mail remains excluded (D041).
- Enforcement is transactional, in the same `BEGIN IMMEDIATE` write that
  persists the message and adjusts the account counter, so the two layers cannot
  drift and there is no check-then-act window. Inbound, outbound and draft writes
  that exceed either cap return the existing `store.ErrQuota`, which every
  transport already maps (webhook 507, MX 452 transient, API 507). The account
  cap is checked first, then the inbox cap.
- Counters are **lazily initialized**: an inbox whose `storage_used_bytes` is
  NULL has it computed once from a SUM over its messages, drafts and draft
  attachments at the first write that adjusts it (or the first read that needs a
  number), after which it is authoritative. `CreateInbox` seeds new inboxes at 0.
  A migration that scanned every inbox was avoided so the upgrade stays cheap.
- Every path that frees bytes refunds the inbox counter in the same transaction
  (`PurgeMessage`, `EmptyTrash`, `DeleteMessage`, `PurgeExpiredTrash`, draft and
  draft-attachment deletion, and the SUM-based account refunds in
  `PurgeInbox`/`PurgeDomain`), flooring at zero.
- The cap is exposed on the inbox read model as `storage_quota_bytes` (settable;
  Admin only) and `storage_used_bytes` (read-only) and on
  `PATCH /v1/inboxes/{id}` using the same absent/null/integer convention as
  `trash_retention_days`. Account Admins edit it from the inbox Quota tab
  (Add and Edit dialogs); the dashboard Size cell turns amber at ≥90% and red at
  the cap, with a usage tooltip.
- Lowering the cap below current usage is allowed: existing mail is untouched,
  the inbox shows as over-quota, and new inbound/outbound mail is refused with
  `ErrQuota` until usage falls (for example by purging mail).

**Reason:** The account quota is the only capacity control, so one runaway
inbox could starve every other mailbox in the account with no way to bound it.
An inbox already presented a Size column and a Quota tab, but the tab said
"storage is limited at the account level" and did nothing. A nullable per-inbox
cap adds real fairness without disturbing the account default, mirroring the
per-inbox Trash-retention override (D076). Reusing `ErrQuota` and the existing
transport mappings keeps every caller's behaviour unchanged.

**Complexity:** One migration (two nullable inbox columns), a widened
`adjustStorageTx` helper, inline checks in the two commit paths, one setter and
one usage read, optional API/UI fields and a size-cell class. No new dependency,
service or route.

## D080 — Configurable HTTP receiver listener and public origin

**Decision:** The HTTP webhook listener defaults to enabled on port 8082,
but `DEDICATED_RECEIVER_ENABLE` and `DEDICATED_RECEIVER_PORT` make it optional
and configurable. This revises the fixed, always-on listener in D019.
Webhook routes remain on both listeners for compatibility. The dedicated
listener continues to expose only inbound ingestion and health checks.

`DEDICATED_RECEIVER_URL` optionally supplies the public origin for provider
webhook registrations, receiving instructions, and Cloudflare Worker code.
Omitted or blank falls back to `BASE_URL`, even when the listener is enabled.
The public origin and listener enable flag are independent; a reverse proxy
can route the receiver origin to either available listener. `BASE_URL` remains
the canonical UI/API origin for passkeys, invitations, and agent connectors.

**Reason:** Some deployments expose public receiving through a different
hostname from the UI. Using the receiver hostname as `BASE_URL` makes browser
passkey registration fail its RP-domain check. A distinct receiver origin
supports that topology without changing passkey trust policy.

**Complexity:** Three environment options, startup validation, a conditional
existing listener, and one effective receiver-origin accessor. No new process,
service, dependency, or authentication policy. The passkey UI translates the
browser's RP-domain rejection into an actionable configuration error.

## D081 — Delivery auto-actions default to the `all` trigger

**Decision:** An inbox created without an explicit trigger carries
`delivery_trigger = 'all'`, and a submitted auto-action form that omits the
field resolves to `all` as well. This supersedes D078's `default any`. `any`
remains available per inbox, and the auto-actions themselves are still off until
enabled.

**Reason:** "The agent has handled this" is a statement about the whole inbox,
not about whichever connector happened to poll first. With `any`, a relay that
delivers before a webhook — or before a second relay — marks a message read and
starts its Trash clock while another connector on the same inbox has not seen it
at all, and a human checking the inbox through the other connector finds the
mail already gone. `all` costs nothing on a single-connector inbox, which is the
common case, and it is the safe reading when one is added.

**Scope:** Defaults only, for new and unset values. An inbox that already
carries a concrete `any` keeps it: the column is `NOT NULL DEFAULT`, so an
explicit `any` and an untouched default are indistinguishable, and a blanket
rewrite would silently override a deliberate choice. Existing inboxes move when
their owner saves the Connectors tab.

Because SQLite cannot alter a column default, migration046's DDL now reads
`DEFAULT 'all'` for fresh databases while a database that already applied it
keeps `DEFAULT 'any'`; `CreateInbox` writes the default explicitly, so no code
path reads the DDL default and the two diverge harmlessly. No table rebuild and
no new migration.

**Complexity:** One constant, one explicit column write in `CreateInbox` (which
also fixes its returned `model.Inbox` reporting no trigger at all), two blank-form
defaults, four preselected options, one JavaScript fallback, and doc/spec text.

## D082 — Antler MX: a named shared receiver service with live endpoint discovery

**Requirement:** Give a self-hoster a zero-config direct-SMTP option that needs no
public inbound port on the core and no receiver of their own: select a named
service, enter a contact email, publish the shown records. The service operator
must be able to add receiver capacity for new setups without a core release, and
the core must retain enough metadata (contact email, setup id, domains,
messages) to account for usage.
**Decision:** Extend the per-domain Dial MX receiving provider (`dialmx`) with a
`service` choice:

- `antler` (default) — Antler MX, the hosted shared relay. The core resolves the
  receiver set from a versioned JSON manifest, snapshots it into the domain's
  encrypted receiving configuration, generates the domain's exact Ed25519 key,
  and shows the MX and `_mailmoose-mx.<domain>` TXT records to publish.
- `custom` — the pre-existing manual `receiver_urls` behaviour, unchanged. A
  legacy save with only `receiver_urls` is never silently treated as Antler.

The manifest is compiled in (`internal/transport/mxdial/antler-endpoints.json`)
and fetched live from the project repository at setup-save time, with a
ten-minute cache, a last-known-good fallback and the embedded copy as the final
fallback, so a hosting outage never blocks setup. It carries both the HTTPS
session origins and the SMTP MX hostnames/priorities. The manifest decoder
tolerates unknown fields so the hosted document can grow without breaking older
cores; known fields are validated strictly (canonical HTTPS origins, public-only
destinations, DNS hostnames, bounded priority). Existing setups keep their
snapshot, so adding or re-pointing capacity only affects new setups; the service
leaves old receivers running.

Registration metadata travels per domain: `DomainAuth` gains optional
`contact_email` and `setup_id`. Both are operational metadata, never credentials
— domain authority remains the existing DNS-anchored Ed25519 proof, and a
malformed value is dropped rather than failing the proof. The receiver logs both
on the domain proof registration/renewal and on the per-recipient resolve and
handoff records, so usage (contact, setup, domains, messages) can be read or
exported from the receiver's structured logs. Antler MX does not introduce an
account tie-in, an operator dashboard, a receiver database or a durable queue.

The receiving API (`GET /v1/admin/domains/{id}/receiving`) returns the live
setup picture for Dial MX: per-receiver authentication `status` learned over the
outbound session, cached best-effort published-record `dns` checks (MX hostnames
and the TXT key), and copy-ready `instructions`. The checks are asynchronous and
never block a save or a render.

The standalone receiver gains `DIALMX_BROWSER_REDIRECT_URL` (https, no
fragment): a browser `GET /` with an HTML `Accept` header is 302-redirected to
the landing page; health, readiness and the session endpoint are unchanged, and
a client that does not accept HTML still gets 404.

**Reason:** The named service makes the common self-host case genuinely
zero-config while reusing every settled Dial MX invariant: core-dials-receiver,
no inbound core port, policy-free stateless receiver, exact-domain DNS proof and
durable-truth-before-`250`. A repository manifest lets capacity changes reach new
setups immediately without shipping the receiver fleet and core in lockstep,
while per-setup snapshots keep existing published MX records stable. Per-domain
contact metadata preserves the multi-tenant model (one installation may serve
several accounts with different contacts) without weakening authority to a
self-asserted value.

**Accepted risk:** the live manifest is fetched over TLS with no signature; an
actor able to write the manifest path in the project repository could point new
setups at different receiver origins. Existing setups are unaffected because
they keep their snapshot. Manifest signing on top of the embedded copy is a
later addition requiring no rework.

**Consequences:**

- The Dial MX provider selector reads **Antler MX (Free SMTP Relay - no port
  forwards required)**; the short label stays **Antler MX**.
- `DomainAuth` metadata is an optional field on the strict mx-v2 contract:
  a receiver or core built before it rejects the frame as malformed when it
  receives the fields, so **upgrade the shared receiver first, then cores** —
  the same coordinated-release pattern as D072. Custom cores send no metadata
  and are unaffected.
- Antler MX endpoints are public-only regardless of a self-hosted operator's
  outbound opt-out: the hosted service must never resolve to a private
  destination.
- No new dependency, service or runtime component is added to the core; the
  receiver remains one stateless process outside the trust boundary.

**Complexity:** One embedded manifest and resolver, per-domain config
fields/snapshot, two optional wire fields and their logging, cached DNS checks
plus a setup API/UI panel, and one receiver environment setting. Durable
delivery, retry receipts and existing mx-v1/mx-v2 transport are unchanged.

## D083 — Cleartext shared-mode sessions from a trusted TLS-terminating proxy

**Requirement:** Run a `DIALMX_MODE=shared` standalone receiver behind a
TLS-terminating reverse proxy (for example Nginx Proxy Manager's Streams
feature) so the receiver holds no certificate or private key, while a
single-core deployment that dials loopback keeps working unchanged. Real
inbound mail must continue to arrive directly on the receiver's SMTP port so
the sender IP used for SPF and per-source limits is the real peer, not the
proxy.

**Decision:** Let a shared-mode session be served over cleartext HTTP/2 (h2c,
prior knowledge) when — and only when — the immediate socket peer is loopback
or matches `DIALMX_TRUSTED_PROXIES`, a comma-separated list of IPs/CIDRs parsed
with the same floor as the core's `TRUSTED_PROXIES` (bare IP is a /32 or /128;
anything wider than /8 is refused). The startup check that requires
`DIALMX_TLS_CERT`/`DIALMX_TLS_KEY` in shared mode is relaxed only when that
allowlist is non-empty; with no allowlist the strict requirement stands, so a
misconfiguration cannot silently expose the listener. The admission gate
(`dialmx/receiver`) rejects a cleartext session from any other peer with `426`
and reason `cleartext_not_trusted`, and the `session opened` record carries
`cleartext` and `cleartext_trusted` so an unencrypted session is visible in the
metadata log. `dialmx/cmd/receiver` enables h2c whenever no certificate pair is
configured, in either mode. The SMTP edge is not proxied: public `:25` reaches
the receiver directly, so the observed sender IP — and therefore SPF and the
per-source caps — is unchanged. No PROXY protocol is added for the session leg.
The per-source connection and authentication caps become tunable
(`MX_PER_IP_CONN_LIMIT`, `MX_PER_IP_CONN_WINDOW_MAX`, `MX_PER_IP_AUTH_CONCURRENT`,
`MX_PER_IP_AUTH_WINDOW_MAX`) so an operator fronting many cores through one
proxy can raise them; behind such a proxy the socket peer is the proxy for every
core, so those caps are shared per proxy. Per-domain and subdomain fair-share
remains deferred.

**Reason:** A TLS-terminating proxy is a common deployment and nginx cannot
proxy upstream over HTTP/2 (`proxy_http_version 2` only from nginx 1.29.4), so
the session endpoint must be fronted at L4 with the proxy terminating TLS. The
peer allowlist keeps the relaxation fail-closed and narrow: only a configured
proxy address may present a cleartext session, the DNS-anchored Ed25519 proof
(shared mode) and the bearer key (single mode) remain the authority, and
core→proxy TLS with hostname verification is unchanged. Keeping SMTP direct
avoids PROXY-protocol support and preserves an accurate sender IP for SPF and
rate limiting.

**Accepted risk:** the proxy→receiver hop carries the challenge transcript,
recipient addresses and message content in cleartext. This is the same trust
the single-mode loopback path already accepts, extended to a named proxy
address; it is bounded by the narrow-prefix rule and the mandatory DNS proof.
An attacker able to spoof the proxy's source IP on that confined hop could read
or inject session traffic. Loopback remains implicitly trusted, so a host-local
core is unaffected.

**Complexity:** One receiver environment allowlist, one admission branch, one
h2c-enable condition and four env-tunable caps, plus tests and documentation.
No new dependency, service, database or core change; the wire contract, SPF,
the envelope-sender accepted risk and `emersion/go-smtp` are untouched.

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
