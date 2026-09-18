# MailMoose — V1 API Contract

This document defines the initial canonical interface. Exact field additions may evolve during implementation while preserving the semantics below.

> **Route listings are generated.** The endpoint list is no longer maintained by
> hand here. `internal/apispec` is the single source of truth; the served guide is
> [`/agent`](/agent), the generated reference is
> [`docs/API-REFERENCE.md`](API-REFERENCE.md), and
> [`/openapi.json`](/openapi.json) is authoritative for the complete operation
> list. This document keeps the rationale, provider schemas, and compatibility
> notes that a route table cannot carry.

## 1. Authentication

```http
Authorization: Bearer <api-key>
```

API error responses include a stable `code` alongside the human-readable
`error` message. Clients should branch on `code` and use the HTTP status as the
broad category. Authorization failures use codes such as `unauthorized`,
`forbidden`, `admin_required` and `sender_not_allowed`; missing resources use
`not_found`.

Mailbox access is assigned per inbox using one of three roles:

| Role | Access |
|---|---|
| `read` | Read messages/threads, search, download attachments |
| `assistant` | Read access plus delete messages and create/edit drafts |
| `owner` | Assistant access plus send/reply and mailbox settings |

A single key may hold different roles on different inboxes.

Account-wide administration uses a separate `admin` role:

| Role | Access |
|---|---|
| `admin` | Full account access, including inbox/domain/key/provider/Hermes management |

Example authorization result:

```json
{
  "admin": false,
  "mailboxes": {
    "in_hermes": "owner",
    "in_accounts": "assistant",
    "in_travel": "read"
  }
}
```

## 2. Discovery

### `GET /.well-known/mailmoose`

Returns a compact discovery document:

```json
{
  "name": "MailMoose",
  "api_version": "v1",
  "api_base": "/v1",
  "auth": {"scheme": "bearer", "header": "Authorization", "key_prefix": "mmm_"},
  "agent_guide": "/agent",
  "openapi": "/openapi.json",
  "bootstrap": "/v1/bootstrap",
  "capabilities": ["inboxes","messages","threads","search","labels","attachments","events","drafts","draft-approval","outbox","send","hermes-relay"]
}
```

### `GET /agent`

Concise Markdown usage guide for LLM clients.

### `GET /v1/bootstrap`

Returns key-specific capabilities, effective per-inbox permissions and accessible
inboxes. `mailbox_roles` remains for compatibility; use `permissions` when a
client needs to decide which operation it can perform without mapping role
names itself.

```json
{
  "mailbox_roles": {"inb_01K...": "owner"},
  "permissions": {
    "inb_01K...": {
      "role": "owner",
      "can_read": true,
      "can_draft": true,
      "can_send": true,
      "can_approve": true
    }
  }
}
```

The quick-start send example is at the top of `/agent`. The machine-readable
operation and schema contract is at `/openapi.json`.

## 3. Inboxes

```http
GET    /v1/inboxes
POST   /v1/inboxes
GET    /v1/inboxes/{id}
PATCH  /v1/inboxes/{id}
DELETE /v1/inboxes/{id}
```

Example:

```json
{
  "id": "in_01K...",
  "address": "hermes@example.com",
  "display_name": "Hermes",
  "enabled": true,
  "aliases": ["sales@example.com", "billing@other.com"]
}
```

`PATCH /v1/inboxes/{id}` accepts `aliases` as a replace-set: each entry is a
full `local@domain` address on any domain the account owns. An alias delivers
inbound mail to this inbox (resolution precedence: exact inbox, alias, then
domain catch-all) and may be chosen as the From address when sending. Sending
`[]` clears the set (and clears `default_sender` if it referenced a removed
alias).

`default_sender` (optional) is the full address compose/reply preselects as
From — the primary `address` or one of `aliases`; `""` clears it to the primary.
An invalid value is rejected. The response includes `default_sender` when set.

`alias_names` (optional) maps an alias address to its sender display name, e.g.
`{"sales@example.com":"Acme Sales"}`. When present without `aliases`, the
existing alias set is kept and only the names are applied; with `aliases`, the
names for the supplied addresses are set. An empty name clears it (the sender
falls back to the inbox `display_name`). The response includes `alias_names`
for aliases that have a name.

### External sending aliases (self-hosted, Admin)

An inbox may also carry **external sending aliases**: full addresses on domains
MailMoose does not manage, usable only as outbound From identities. They never
receive mail and are managed per-alias (not as a replace-set), so an inbox save
cannot drop one or its connector. Inbox responses include a read-only
`external_aliases` array (ids, addresses, display names, provider and
`configured` status; never credentials).

```http
GET    /v1/admin/inboxes/{id}/external-aliases
POST   /v1/admin/inboxes/{id}/external-aliases
PATCH  /v1/admin/inboxes/{id}/external-aliases/{aliasID}
DELETE /v1/admin/inboxes/{id}/external-aliases/{aliasID}
GET    /v1/admin/inboxes/{id}/external-aliases/{aliasID}/sending
PUT    /v1/admin/inboxes/{id}/external-aliases/{aliasID}/sending
DELETE /v1/admin/inboxes/{id}/external-aliases/{aliasID}/sending
GET    /v1/admin/inboxes/{id}/external-aliases/{aliasID}/sending/deliveries
```

- `POST` body: `{"address":"agent@gmail.com","display_name":"Agent"}`. The
  address must be a full `local@domain`, must not be the inbox primary or a
  managed alias/inbox in the account, and is **immutable** after creation.
- `PATCH` body: `{"display_name":"..."}` only.
- `/sending` mirrors the domain sending-config contract (`provider`, `config`,
  CAS via revision, blank same-provider secret retention, `DELETE` clears the
  connector). Saving a connector requeues only that alias's pending sends.
- `/sending/deliveries` returns that alias's outbound attempts (same shape as
  the domain delivery log).
- All of these require an Admin principal and are rejected with `403` in hosted
  mode.

`default_sender` may name an external alias; a send from it uses that alias's
connector, and a message whose alias is later deleted fails with a clear error
rather than falling back to a domain connector.

## 4. Messages

```http
GET /v1/messages
GET /v1/messages/{id}
PATCH /v1/messages/{id}
```

Filters may include:

```text
inbox
thread
label
from
to
after
before
unread
has_attachment
spam
include_spam
```

`spam=true` lists only Spam; `include_spam=true` includes Spam and non-Spam.
By default (neither set) Spam is excluded from ordinary reads. `PATCH
/v1/messages/{id}` also accepts `spam` to release (`false`) or quarantine
(`true`) a message, which commits a durable `message.spam_state_changed` event.

Normalized message:

```json
{
  "id": "msg_01K...",
  "thread_id": "thr_01K...",
  "direction": "inbound",
  "inbox_id": "in_01K...",
  "envelope_to": ["hermes@example.com"],
  "from": {"name":"Jane","address":"jane@example.org"},
  "to": ["hermes@example.com"],
  "cc": [],
  "subject": "Quote",
  "received_at": "2026-09-08T04:00:00Z",
  "text": "Hi...",
  "has_attachments": true,
  "read": false,
  "archived": false,
  "is_spam": false,
  "labels": ["Invoices", "Unpaid"]
}
```

Messages received through the optional MX edge carry `spam_reason` (a bounded
classification string) and `auth_results` (bounded normalized SPF/DKIM/DMARC
evidence).

Outbound messages additionally carry `client`: the name of the API key or
Hermes credential that sent the message (`UI` for a web-UI send, omitted when
there is no credential, e.g. an email-approved send). Inbound mail has no
`client`.

### Labels

Labels are free-text tags on a message. There is no label catalogue: a label
exists only while at least one message carries it. Labels are shared across the
account and a message may have many. Matching ignores case and surrounding
whitespace; the displayed casing is the one first assigned.

```http
GET /v1/labels
```

Returns the distinct labels currently in use, scoped to the key's authorized
inboxes.

Set a message's labels with `PATCH /v1/messages/{id}`:

```json
{"labels": ["Invoices", "Unpaid"]}
```

`labels` replaces the whole set; an empty array clears it and omitting the field
leaves it unchanged. Unknown labels are created implicitly. Requires Assistant
or Owner on the message's inbox. Label changes emit a durable
`message.labels_changed` event.

Filter by label with `label` on `/v1/messages` and `/v1/search`:

```text
GET /v1/messages?inbox={id}&label=Invoices&label=Unpaid
```

Repeated `label` parameters are combined with AND (a message must carry every
listed label).

## 5. Threads

```http
GET /v1/threads
GET /v1/threads/{id}
GET /v1/threads/{id}/messages
```

Thread identity is deterministic/stable based on standard email threading headers and persisted mapping.

## 6. Search

```http
GET /v1/search?q=<query>
```

Optional filters:

```text
inbox
label
from
to
after
before
has_attachment
```

Search operates only within the key's authorized inbox set.

## 7. Attachments

```http
GET /v1/messages/{message_id}/attachments
GET /v1/attachments/{id}
```

Metadata:

```json
{
  "id": "att_01K...",
  "filename": "quote.pdf",
  "content_type": "application/pdf",
  "size": 184920
}
```

Attachment content responses use download disposition and `nosniff` headers.

## 8. Drafts

```http
GET    /v1/drafts?inbox={id}&before={id}&limit=100
POST   /v1/drafts
GET    /v1/drafts/{id}
PATCH  /v1/drafts/{id}
DELETE /v1/drafts/{id}

GET    /v1/drafts/{id}/attachments
POST   /v1/drafts/{id}/attachments
GET    /v1/drafts/{id}/attachments/{attId}
DELETE /v1/drafts/{id}/attachments/{attId}
```

`assistant` and `owner` keys can create and edit drafts and upload attachments
(multipart field `attachments`). Sending a draft requires `owner`.

`POST /v1/drafts`, `PATCH /v1/drafts/{id}`, `POST /v1/drafts/{id}/send` and
`POST /v1/drafts/{id}/request-send` accept an optional JSON `attachments` array
in the same shape as send/reply, so a draft can be created and submitted — or
sent — in one request:

```json
{
  "inbox_id": "in_01K...",
  "to": ["recipient@example.org"],
  "subject": "Quote",
  "text": "See attached",
  "attachments": [
    {"filename": "quote.pdf", "content_type": "application/pdf", "content": "<base64>"}
  ],
  "action": "request-send"
}
```

- `action` is `draft` (default), `request-send` (Assistant; requires an owner
  to approve) or `send` (Owner). `POST /v1/drafts/{id}/send` always sends.
- Inline attachments are appended to any already uploaded for the draft.
- `PATCH /v1/drafts/{id}` is a partial update: only the fields present in the
  body are changed; omitted fields are left untouched. Send `[]`/`""` to clear
  a list or string field.
- Draft reads include an `attachments` array, and
  `GET /v1/drafts/{id}/attachments/{attId}` downloads one attachment's bytes.
- `sender` is accepted on draft writes as an alias for `from_address`.
- `POST /v1/drafts/{id}/send` returns `provider_message_id` like `/v1/send`.

### Draft approval workflow

An Assistant can draft and request send; an Owner authorizes.

```http
POST /v1/drafts/{id}/request-send          # assistant; freezes the draft
POST /v1/drafts/{id}/cancel-send-request   # assistant; unfreezes
POST /v1/drafts/{id}/approve               # owner; approve and send
POST /v1/drafts/{id}/reject                # owner; reject with optional feedback
GET  /v1/drafts/{id}/send-request          # latest request, survives send
GET  /v1/send-requests?inbox={id}&active=true
```

- Draft `status` is `draft`, `pending_approval` or `rejected`. A draft is
  frozen while `pending_approval`; changing it requires cancelling the request
  first. Editing a rejected draft returns it to `draft`.
- `approve` authorizes the exact frozen draft and enqueues it through the
  normal outbound flow. `reject` stores optional feedback and leaves the draft
  editable.
- Approval and delivery are separate: approval enqueues a pending message;
  `delivery_status` moves `none` → `pending` → `sent`/`failed`.
- If the inbox has a configured `approver_email`, `request-send` is external:
  the request records `notification_status` (`queued` → `sent`/`failed`) and a
  one-time token is emailed to that approver. The approver is an inbox setting,
  not a per-request argument. Expiry begins only when the notification is handed
  to the outbound path, so a request whose notification could not be sent is
  reported as `failed` rather than silently awaiting approval.
- Workflow events: `draft.send_requested`, `draft.send_request_cancelled`,
  `draft.approved`, `draft.rejected`, `draft.sent`, `draft.send_failed`,
  `draft.notification_sent`, `draft.notification_failed`.

Human users in the web UI are always Owners. Mailbox roles apply to API keys.
Hermes Relay sends as an owner directly and does not use the draft workflow.

## 9. Send and reply

### Send

```http
POST /v1/send
Idempotency-Key: <caller-generated-key>
```

```json
{
  "inbox_id": "in_01K...",
  "sender": "sales@example.com",
  "to": ["recipient@example.org"],
  "subject": "Hello",
  "text": "Message body"
}
```

`inbox_id` is required unless the compatibility `from` field identifies one
accessible inbox or a non-admin key owns exactly one inbox. `to` must contain at
least one recipient, `subject` must be non-empty, and at least one of `text` or
`html` must be non-empty. `sender`,
`from`, `cc`, `bcc` and `attachments` are optional. `sender` selects the From
identity; `from` selects an inbox and is not a From identity.

Common failures use the JSON error envelope with a stable `code`:

```json
{"error":"sender not allowed","code":"sender_not_allowed"}
{"error":"forbidden","code":"forbidden"}
{"error":"not found","code":"not_found"}
```

`sender` (optional) selects the From identity: the inbox's primary address or
one of its aliases. When it is an alias on another domain, the sending provider
is resolved from that alias domain's configuration. The From display name is the
alias's `alias_names` entry, falling back to the inbox `display_name`. An
address that is neither the primary nor an alias is rejected with `403`. Omit
`sender` to send from the inbox primary (or its `default_sender`, when set for
UI compose).

### Reply

```http
POST /v1/messages/{id}/reply
Idempotency-Key: <caller-generated-key>
```

```json
{
  "sender": "sales@example.com",
  "text": "Reply body"
}
```

The application creates appropriate `In-Reply-To` and `References` headers.
`sender` is optional and works as above.

Add `?wait=true` to block until the worker delivers or fails (up to 30
seconds), returning the terminal message; a timeout returns `504`.

### Drafts

`POST /v1/drafts` and `PATCH /v1/drafts/{id}` accept `from_address` (the chosen
sender). An Assistant may set it; an approved send uses the stored sender, and
it is part of the frozen approval fingerprint.

## 10. Replayable event history

### Incremental read

```http
GET /v1/events?after=evt_123
```

### Long poll

```http
GET /v1/events/wait?after=evt_123&timeout=60
```

### SSE

```http
GET /v1/events/stream?after=evt_123
Accept: text/event-stream
```

Example event:

```text
id: evt_123
event: message.received
data: {"message_id":"msg_01K...","inbox_id":"in_01K...","thread_id":"thr_01K..."}
```

On connection, backlog after the supplied cursor is delivered before the stream joins live events.

## 11. Inbound endpoints and domain provider configuration

Mailgun canonical endpoint:

```http
POST /internal/ingest/mailgun/raw-mime
```

Cloudflare Worker endpoint:

```http
POST /internal/ingest/cloudflare
```

Resend endpoint:

```http
POST /internal/ingest/resend
```

Each adapter authenticates the provider (Mailgun HMAC signature; Cloudflare
bearer plus `X-MailMoose-Recipient`; Resend Svix signature headers) before
parsing or persisting MIME, then converts delivery to the canonical
`InboundMessage` with an explicit authenticated binding. Resend webhooks carry
metadata only, so the adapter fetches the raw MIME from the Resend API after
verification. The legacy `/internal/ingest/mailgun` alias has been removed;
`/internal/ingest/{provider}` remains for other providers.

Delivery idempotency is scoped to `(account_id, provider, canonical original
envelope recipient, provider_delivery_id)`. Mailgun uses its authenticated
webhook `token`; Cloudflare uses `X-MailMoose-Delivery-ID` or a SHA-256 hash of
the raw MIME; Resend uses `data.email_id`. Unknown recipients route to the
domain catch-all when configured; otherwise the endpoint returns `406`. Missing
receiving configuration, unknown domains, and bad authentication return `401`.
Resend event types other than `email.received` are acknowledged with `200` and
ignored.

Optional MX (direct SMTP) ingest uses two authenticated endpoints on the same
inbound listener, called only by the `mailmoose-mx` edge with an operator
HMAC edge key (not a provider webhook and not a tenant credential):

```http
POST /internal/mx/resolve
POST /internal/mx/ingest
```

`resolve` maps a bounded recipient list to routing decisions (distinguishing an
unknown recipient from a transient internal failure); `ingest` persists one
recipient's original MIME and returns a durable disposition (`stored`, `spam`,
or a typed error). Duplicate delivery fingerprints return the recorded
disposition. See [MX.md](MX.md).

Sending and receiving are configured per domain. A domain owns at most one
optional sending configuration and at most one optional receiving
configuration. There is no account-level connector pool, no reusable named
credential, no shared assignment, and no API for managing connectors on their
own. The same external API key may still be entered independently on more than
one domain; the stored configs are separate. Admin role required:

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

`PUT` accepts `{"provider": "...", "config": {...}}`; receiving additionally
accepts `"regenerate_secret": true`. Provider schemas:

- sending — `mailgun: {"api_key","domain","api_base?"}`;
  `brevo: {"api_key","api_base?"}`; `resend: {"api_key","api_base?"}`;
  `smtp: {"host","port?","username?","password?","security?","from_domain?"}`
  (`security` is `starttls`, `tls`, or `plain`; `port` defaults to `587`).
- receiving — `mailgun: {"signing_key"}`;
  `cloudflare: {"webhook_secret"}` (generated by MailMoose);
  `resend: {"api_key","webhook_secret","api_base?"}`.

A blank secret on a same-provider save keeps the stored value; changing provider
never reuses old fields or secrets. Non-secret fields are whole-config values:
an omitted field takes the provider default or is a required-field error, never a
merge. Saving performs no network or DNS validation.

`GET` on an unconfigured slot returns
`200 {"domain_id":"...","configured":false,"provider":"","config":{}}`. A
configured slot adds the non-secret config, `updated_at`, and (receiving only)
`webhook_url`. Generated secrets (`cloudflare.webhook_secret`) are created only
when missing, preserved by a normal same-provider save, and replaced only by an
explicit `regenerate_secret` for the currently configured provider (a request
that supplies a generated value while regenerating, or regenerates an
unconfigured provider, is a `400`). The fresh value is returned once in a
`generated` map and is never returned again. Responses that may carry a
generated secret are `no-store`.

`DELETE` returns `204` and is idempotent for a domain with no config; a missing
or foreign domain returns `404`. Validation faults return `400`, a concurrent
change returns `409`, and internal failures are redacted as `500`.

`GET .../sending/deliveries` returns the domain's delivery attempts, newest
first (`limit`, default `100`, max `200`; `before` is a keyset cursor on the
attempt id). History is scoped by domain, not by the current config, so removing
or replacing a provider does not hide past attempts.

`GET .../receiving/deliveries` returns the receiving side of the same domain
log: delivered inbound mail (`kind` `received`), inbound mail rejected by an
inbox's allowed-senders rule (`kind` `blocked`), and consumed approval control
mail (`kind` `approval`), newest first (`limit`, default `100`, max `200`;
`before` is a timestamp keyset cursor on `created_at`). Each row carries the
kind, timestamp, inbox, provider, from/to, subject and size. Blocked rows add
the reason; approval rows set `subject` to the reviewed draft subject labelled
`Approval: <subject>` (or `Rejected: <subject>` when the outcome is `rejected`),
carry the `action` (`approve`/`reject`) and the `status` outcome (`approved`,
`rejected`, `invalid` or `error`) plus the reason, and set `client` to `Control`;
delivered rows add the click-through
`message_id`. The domain's UI log
merges this with the sending side into one two-way timeline. Both logs are
scoped by domain rather than by the current provider config.

Domain creation accepts `name` only, and `PATCH /v1/admin/domains/{id}` accepts
`catch_all_inbox_id` only. The domain object exposes the configured provider
names (`sending_provider`, `receiving_provider`) but no secrets or settings.

This configuration surface is provider-facing (Admin role) rather than
agent-facing.

## 12. Hermes Relay

Relay endpoint:

```text
wss://<host>/relay
```

Enrollment:

```http
POST /relay/enroll
```

The hosted UI issues single-use enrollment tokens and displays the corresponding Hermes CLI command. The same management operations are available programmatically to an Admin principal:

```http
GET    /v1/admin/hermes
POST   /v1/admin/hermes/enroll
PUT    /v1/admin/hermes/{id}
DELETE /v1/admin/hermes/{id}
```

`POST /v1/admin/hermes/enroll` takes `{"inbox_id","name"}`, issues a new
single-use enrollment token, returns `201` with the `gateway_id`, `secret`,
`delivery_key`, `connector_url` and a ready-to-paste `env` block, and is the API
equivalent of the `/relay/enroll` UI. `GET` lists the account's relay
connections. `PUT /v1/admin/hermes/{id}` takes `{"role"}` and sets the
connection's outbound role: `owner` lets the relay send directly, while
`assistant` makes it draft and request approval instead. `DELETE
/v1/admin/hermes/{id}` removes the connection and returns `204`.

Relay protocol implementation should follow the Hermes connector contract while isolating its versioning from the canonical API.

## 13. openagent.email compatibility

Where semantics align naturally, support familiar compatibility operations such as:

```text
/v1/identities
DELETE /v1/identities/{address}
/v1/messages
/v1/messages/{id}
/v1/messages/wait
/v1/send
```

Compatibility should translate into the canonical model.

MailMoose native resources remain authoritative for multi-inbox scopes, replayable event history, threads, search, domains, and Hermes Relay.

Compatibility behaviour requires explicit contract tests.
