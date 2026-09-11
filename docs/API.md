# Gatehouse Mail — V1 API Contract

This document defines the initial canonical interface. Exact field additions may evolve during implementation while preserving the semantics below.

## 1. Authentication

```http
Authorization: Bearer <api-key>
```

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

### `GET /.well-known/gatehouse`

Returns a compact discovery document:

```json
{
  "name": "Gatehouse Mail",
  "api_version": "v1",
  "api_base": "/v1",
  "agent_guide": "/agent",
  "openapi": "/openapi.json",
  "bootstrap": "/v1/bootstrap",
  "capabilities": ["inboxes","messages","threads","search","labels","attachments","events","drafts","draft-approval","outbox","send","hermes-relay"]
}
```

### `GET /agent`

Concise Markdown usage guide for LLM clients.

### `GET /v1/bootstrap`

Returns key-specific capabilities and accessible inboxes.

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
  "enabled": true
}
```

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
```

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
  "labels": ["Invoices", "Unpaid"]
}
```

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
GET    /v1/drafts
POST   /v1/drafts
GET    /v1/drafts/{id}
PATCH  /v1/drafts/{id}
DELETE /v1/drafts/{id}

GET    /v1/drafts/{id}/attachments
POST   /v1/drafts/{id}/attachments
DELETE /v1/drafts/{id}/attachments/{attId}
```

`assistant` and `owner` keys can create and edit drafts and upload attachments
(multipart field `attachments`). Sending a draft requires `owner`.

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
- Workflow events: `draft.send_requested`, `draft.send_request_cancelled`,
  `draft.approved`, `draft.rejected`, `draft.sent`, `draft.send_failed`.

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
  "to": ["recipient@example.org"],
  "subject": "Hello",
  "text": "Message body"
}
```

### Reply

```http
POST /v1/messages/{id}/reply
Idempotency-Key: <caller-generated-key>
```

```json
{
  "text": "Reply body"
}
```

The application creates appropriate `In-Reply-To` and `References` headers.

## 10. Replayable event history

### Incremental read

```http
GET /v1/events?after=evt_01K...
```

### Long poll

```http
GET /v1/events/wait?after=evt_01K...&timeout=60
```

### SSE

```http
GET /v1/events/stream?after=evt_01K...
Accept: text/event-stream
```

Example event:

```text
id: evt_01K...
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
bearer plus `X-Gatehouse-Recipient`; Resend Svix signature headers) before
parsing or persisting MIME, then converts delivery to the canonical
`InboundMessage` with an explicit authenticated binding. Resend webhooks carry
metadata only, so the adapter fetches the raw MIME from the Resend API after
verification. The legacy `/internal/ingest/mailgun` alias has been removed;
`/internal/ingest/{provider}` remains for other providers.

Delivery idempotency is scoped to `(account_id, provider, canonical original
envelope recipient, provider_delivery_id)`. Mailgun uses its authenticated
webhook `token`; Cloudflare uses `X-Gatehouse-Delivery-ID` or a SHA-256 hash of
the raw MIME; Resend uses `data.email_id`. Unknown recipients route to the
domain catch-all when configured; otherwise the endpoint returns `406`. Missing
receiving configuration, unknown domains, and bad authentication return `401`.
Resend event types other than `email.received` are acknowledged with `200` and
ignored.

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
  `cloudflare: {"webhook_secret"}` (generated by Gatehouse);
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

The hosted UI issues single-use enrollment tokens and displays the corresponding Hermes CLI command.

Relay protocol implementation should follow the Hermes connector contract while isolating its versioning from the canonical API.

## 13. openagent.email compatibility

Where semantics align naturally, support familiar compatibility operations such as:

```text
/v1/identities
/v1/messages
/v1/messages/{id}
/v1/messages/wait
/v1/send
```

Compatibility should translate into the canonical model.

Gatehouse Mail native resources remain authoritative for multi-inbox scopes, replayable event history, threads, search, domains, and Hermes Relay.

Compatibility behaviour requires explicit contract tests.
