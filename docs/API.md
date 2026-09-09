# Gatehouse Email — V1 API Contract

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
  "name": "Gatehouse Email",
  "api_version": "v1",
  "api_base": "/v1",
  "agent_guide": "/agent",
  "openapi": "/openapi.json",
  "bootstrap": "/v1/bootstrap",
  "capabilities": ["inboxes","messages","threads","search","attachments","events","send"]
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
  "archived": false
}
```

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
```

`assistant` and `owner` keys can create and edit drafts. Sending a draft requires `owner`.

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

## 11. Inbound endpoints and receive configuration

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
credentials, unknown domains, and bad authentication return `401`. Resend event
types other than `email.received` are acknowledged with `200` and ignored.

Receive connections are account-owned, encrypted credentials configured
through the Admin API (Admin role required):

```http
GET    /v1/admin/inbound
POST   /v1/admin/inbound
PATCH  /v1/admin/inbound/{id}
DELETE /v1/admin/inbound/{id}
```

`POST`/`PATCH` accept `provider` and a provider-specific `config`
(`mailgun: {"signing_key": "..."}`; `cloudflare: {"webhook_secret": "..."}`;
`resend: {"api_key": "...", "webhook_secret": "whsec_..."}`).
Provider identity is immutable on update; a blank secret retains the stored
value; secret fields are never returned. Assign a credential to a domain with
`inbound_credential_id` on `POST`/`PATCH /v1/admin/domains[/{id}]`
(`PATCH` with `""` clears it).

It is provider-facing rather than agent-facing.

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

Gatehouse Email native resources remain authoritative for multi-inbox scopes, replayable event history, threads, search, domains, and Hermes Relay.

Compatibility behaviour requires explicit contract tests.
