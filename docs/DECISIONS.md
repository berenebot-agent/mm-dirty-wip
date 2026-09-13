# Gatehouse Mail — V1 Decision Register

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

**Decision:** The application always runs a second HTTP listener on `:8082` that serves only the authenticated inbound webhook routes (`/internal/ingest/mailgun/raw-mime`, `/internal/ingest/cloudflare`, and `/internal/ingest/{provider}`) and `/healthz`. The main listener continues to serve all routes.

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
See the migration note in the root `DECISIONS.md`.

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

**Wire contract:** HMAC-SHA256 over a bounded, exact-bytes JSON envelope
(protocol version, timestamp, request ID, key ID, recipient, envelope sender,
client IP, HELO, normalized auth evidence, content digest, size) with
constant-time verification and overlapping accepted keys for rotation. Key IDs
map to configured operator credentials; remote links require verified TLS.
Timestamp skew bounds replay duration, not replay itself; request IDs are bound
to the authenticated fingerprint and conflicting reuse is rejected. Replayed
retries return the recorded durable disposition.

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
BIMI, supervisor, durable edge queue or SMTP rejection in V1.

**Deferrals:** authentication-based SMTP rejection / `on_auth_fail=delete`; ARC
verification and trusted-forwarder policy; BIMI; all-in-one supervisor; a
Haraka/mailauth alternate edge; policy snapshots and local rejection; durable
edge queue and end-to-end HA; scoped credential-to-domain binding; reputation
and content filtering; DMARC report generation; authenticated submission/relay.

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
