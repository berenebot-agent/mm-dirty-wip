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

### Inbox kinds

An inbox has a `kind`, `domain` (default) or `standalone`. A **domain** inbox is
the classic managed-domain mailbox described above. A **standalone** inbox owns
an address independent of any managed domain and is reached through a per-inbox
remote IMAP/SMTP connector; it is otherwise a first-class mailbox with the same
folder, label, thread, event and API-key model. The kind is immutable after
creation. Managed `aliases` apply to domain inboxes only. See the common mailbox
surface below and [MAILBOX_SERVICE_CONTRACT.md](MAILBOX_SERVICE_CONTRACT.md).

## 3a. Common mailbox surface

Routes under `/v1/inboxes/{id}/…` dispatch on the inbox kind, so one set of
endpoints reads a domain inbox (local store) and a standalone inbox (live remote
server) alike. Every listing returns the shared **envelope**:

```json
{
  "items": [ ... ],
  "next_cursor": "opaque-token-or-empty",
  "completeness": "complete",
  "errors": []
}
```

- `items` is the page; `next_cursor` is an opaque token for the next page and is
  empty on the last page. **Pagination and completeness are orthogonal:** having
  a `next_cursor` does not mean the result set is partial.
- `completeness` is `complete`, `partial` or `unknown`, describing whether the
  source could enumerate the whole result set at all. A standalone inbox's index
  is **progressive**: ordinary large folders backfill to `complete` over
  successive passes, so completeness is `partial` only while a folder's backfill
  has not yet reached the bottom (or a pass was interrupted / a folder exceeds
  the 500k-UID snapshot ceiling). A client must not present a `partial` listing
  as complete. See [MAILBOX_SERVICE_CONTRACT.md](MAILBOX_SERVICE_CONTRACT.md) §3/§5.
- `errors` carries per-inbox failures (`{inbox_id, code, message, retryable}`)
  when an account-wide listing spans several inboxes, so one unreachable mailbox
  does not fail the whole request. Only a stable `code` and a short safe message
  are exposed; never raw provider or store text.
- An **account-wide** listing (no `inbox` scope) merges the local store and every
  authorized remote inbox into one globally date-sorted stream; the opaque cursor
  records each source's own progress plus the global key of the last item, so
  resuming never duplicates or skips an item. A per-source failure is reported in
  `errors` and does not fail the page.

The common routes:

```text
GET    /v1/inboxes/{id}/folders
POST   /v1/inboxes/{id}/folders
PATCH  /v1/inboxes/{id}/folders/{folderId}
DELETE /v1/inboxes/{id}/folders/{folderId}
GET    /v1/inboxes/{id}/messages        ?folder=&before=&label=&from=&to=&limit=
GET    /v1/inboxes/{id}/messages/{messageId}
GET    /v1/inboxes/{id}/messages/{messageId}/content
GET    /v1/inboxes/{id}/messages/{messageId}/attachments/{part}
GET    /v1/inboxes/{id}/threads         ?folder=&limit=
GET    /v1/inboxes/{id}/threads/{threadId}
GET    /v1/inboxes/{id}/search          ?q=&folder=&from=&to=&label=&limit=
GET    /v1/inboxes/{id}/labels
```

Also under `/v1/messages/{id}` for both kinds: `GET …/content` (raw MIME) and
`GET …/attachments/{part}` (one MIME part). `part` is a numeric dotted MIME part
path (e.g. `1.2`).

### Folders, roles and labels

Every inbox has a single hierarchical **folder** tree. A folder carries a
`role` (`folder`, `inbox`, `sent`, `drafts`, `trash`, `spam`, `archive`,
`outbox`, `label`) or is an ordinary custom folder. A message belongs to exactly
one folder; moving it emits `message.folder_changed`. A protected system folder
cannot be renamed or deleted, and a non-empty folder is refused (nothing is
silently cascaded). **Labels are not folders**: a label is free-text metadata on
a message and is independent of the folder tree on both inbox kinds.

### Standalone (remote) specifics

```text
GET  /v1/inboxes/{id}/remote
PUT  /v1/inboxes/{id}/remote
POST /v1/inboxes/{id}/remote/test
POST /v1/inboxes/{id}/remote/refresh
POST /v1/inboxes/{id}/remote/roles/{role}
GET /v1/inboxes/{id}/handoffs
GET /v1/inboxes/{id}/authoring
PUT /v1/inboxes/{id}/authoring
```

- `GET /remote` returns the secret-free binding (`host`, `port`, `username`,
  `security`, `namespace`, optional SMTP fields, `imap_password_set`,
  `smtp_password_set`, `configured`, `capabilities`, `sent_copy_enabled`,
  `sent_copy_folder`) plus `missing_roles` — the special roles not yet mapped, so
  a client can prompt an explicit select or create. Secrets are never returned.
- `PUT /remote` sets the binding (Owner/Admin). `security` is `tls` (default),
  `starttls` or `plain`; plain is an explicit per-inbox choice and is never
  auto-selected or silently downgraded. A blank password field retains the
  stored value. It also sets `sent_copy_enabled` and an optional
  `sent_copy_folder` for the remote Sent copy.
- `POST /remote/test` authenticates and resolves the folder scope without
  mutating mailbox state. `POST /remote/refresh` forces a reconcile pass and
  returns the resulting index `status`.
- `POST /remote/roles/{role}` maps a role: it returns the folder already
  carrying the role, or with `{"create":true}` creates the conventional folder;
  without either it answers `404` so the client prompts explicitly. A mapping is
  never silently invented.
- `GET /handoffs` lists the inbox's RemoteDraft handoffs newest first as the
  shared envelope, retaining terminal records (`published`, `ambiguous`,
  `failed`/`cancelled`) even after the source draft is cleaned up. Each record
  carries its `publication` and `notification_status` independently and never an
  approval token or the frozen content hash.
- `GET`/`PUT /authoring` read/write the assistant authoring mode
  (`mailmoose_approval` or `remote_draft`) and the notify override. A standalone
  inbox defaults to `remote_draft`; the mode is snapshotted onto each request.
  The read also reports `standalone` and `approver_enabled` (true exactly when the
  effective mode is `mailmoose_approval`).

For a standalone inbox, a message's body and attachments are fetched **live**
from the remote server on each request and are never archived; the attachment
listing is empty and parts are downloaded structurally by MIME part path. The
single-message `GET` returns the **live body**, or `503` if the connector is
unreachable — there is no offline fallback that serves cached metadata as the
body. A purge (`DELETE /v1/messages/{id}/purge`) requires Owner and only acts on
a message already in the Trash-role folder.

## 4. Messages

```http
GET /v1/messages
GET /v1/messages/{id}
PATCH /v1/messages/{id}
DELETE /v1/messages/{id}
POST /v1/messages/{id}/restore
DELETE /v1/messages/{id}/purge
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
trashed
```

`spam=true` lists only Spam; `include_spam=true` includes Spam and non-Spam.
By default (neither set) Spam is excluded from ordinary reads. `trashed=true`
lists only messages in Trash; by default trashed messages are excluded from
every read. `PATCH /v1/messages/{id}` also accepts `spam` to release (`false`)
or quarantine (`true`) a message, which commits a durable
`message.spam_state_changed` event.

### Trash

Deleting a message (`DELETE /v1/messages/{id}`, Assistant or Owner) moves it to
Trash rather than erasing it: the message is hidden from lists, search, threads
and unread counts, but its raw MIME, attachments and storage accounting are
retained. `POST /v1/messages/{id}/restore` (Assistant or Owner) returns it to
the mailbox. `DELETE /v1/messages/{id}/purge` (Owner) erases a trashed message
permanently and unlinks its raw file; a message must be trashed first, otherwise
the call answers `409`. `POST /v1/inboxes/{id}/trash/empty` (Owner) purges every
trashed message in an inbox.

Each account has a `trash_retention_days` preference (default 0; `0` keeps
trashed mail until it is emptied by hand, a positive value purges it after that
many days), read and written through
`GET`/`PATCH /v1/account/settings`. Trashed messages older than the window are
purged by the maintenance sweep. The events `message.trashed`,
`message.restored` and `message.purged` are emitted and streamed; they are not
relayed over Hermes.

`envelope_from` is the transport-supplied SMTP envelope sender (MAIL FROM),
exactly as the receiving adapter observed it; it is empty when the transport
supplied none and is never derived from the MIME `From` header.
`envelope_recipient` is the canonical original envelope recipient, which may be
a catch-all or alias address rather than the resolved inbox address.

Normalized message:

```json
{
  "id": "msg_01K...",
  "thread_id": "thr_01K...",
  "direction": "inbound",
  "inbox_id": "in_01K...",
  "envelope_to": ["hermes@example.com"],
  "envelope_from": "sender@example.org",
  "envelope_recipient": "hermes@example.com",
  "from": {"name":"Jane","address":"jane@example.org"},
  "to": ["hermes@example.com"],
  "cc": [],
  "subject": "Quote",
  "received_at": "2026-09-08T04:00:00Z",
  "text": "Hi...",
  "has_attachments": true,
  "read": false,
  "deleted_at": null,
  "is_spam": false,
  "labels": ["Invoices", "Unpaid"]
}
```

Messages received through the optional MX edge carry `spam_reason` (a bounded
classification string) and `auth_results` (bounded normalized SPF/DKIM/DMARC
evidence).

Outbound messages carry `source`: the name of the API key or Hermes credential
that sent the message (`UI` for a web-UI send, omitted when there is no
credential, e.g. an email-approved send). Inbound message rows have no
`source`; the domain activity log's `source` (below) names the receiving
receiver instead.

### Labels

Labels are free-text tags on a message. There is no label catalogue: a label
exists only while at least one message carries it. Labels are shared across the
account and a message may have many. Matching ignores case and surrounding
whitespace; the displayed casing is the one first assigned.

A label is trimmed and internal runs of whitespace collapsed. It must be
non-empty, at most 64 characters, and must not contain control characters, `/`
or `\`. (The path separators are rejected so a label can be addressed by its own
URL query parameter without path-encoding ambiguity.) An invalid label is
rejected with a validation error.

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
GET /v1/messages/{message_id}/attachments/{part}
GET /v1/messages/{message_id}/content
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

`GET /v1/messages/{id}/content` streams the raw RFC5322 MIME; `GET
/v1/messages/{id}/attachments/{part}` streams one MIME part by its numeric
dotted part path (e.g. `1.2`). For a **standalone** inbox's message only the
header is cached, so the `attachments` listing is empty and parts are downloaded
structurally by path (the body is fetched live and never archived).

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

The inbox's **authoring mode** decides how a request is handled (snapshotted at
creation). **MailMoose approvals** (domain default) is the workflow above.
**Remote draft handoff** (standalone default) is a one-way handoff to the
connected mailbox's remote Drafts folder instead: MailMoose never sends it, a
notification without a token tells the human it is waiting, and publication,
notification and Sent-copy states advance independently. The handoff events are
`draft.handoff_requested`, `draft.handoff_published`, `draft.handoff_ambiguous`,
`draft.handoff_failed`, `draft.handoff_cancelled`, `draft.handoff_notification_sent`
and `draft.handoff_notification_failed`. An unverifiable remote append is
reported `ambiguous`, never retried blindly. A handoff on an inbox with no remote
Drafts folder is refused.

A draft response carries a **`handoff`** object when a RemoteDraft handoff is
active or most recent, exposing the immutable `handoff_id` and the independent
`publication` (`pending`/`published`/`ambiguous`/`failed`) and
`notification_status` states, so a client that requested a handoff can act on
its outcome rather than only seeing the frozen draft. The full history is at
`GET /v1/inboxes/{id}/handoffs` (terminal records are retained after the local
draft is cleaned up).

Only a **pending** handoff can be cancelled (the UI `cancel-handoff` action;
`Store.CancelHandoff`): it becomes `failed` with `last_error="cancelled"`, the
draft returns to editable, and `draft.handoff_cancelled` is emitted; a
`published` or `ambiguous` handoff cannot be cancelled. A **retry**
(`retry-handoff`) is a deliberate re-request through the inbox's effective
authoring mode, so it only produces a new handoff while the inbox is in
`remote_draft` mode. An `ambiguous` append is **never** automatically retried —
the record is terminal and re-requesting is a human choice.

Mailbox roles apply to API keys and to human users alike. An account Admin is an Owner of every mailbox in the account; a non-admin mailbox operator holds Owner on only the inboxes assigned to them.
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

For a **standalone** inbox, a new arrival's event `entity_id` is the durable
**arrival id**, not the cached-metadata message id (detection is independent of,
and may run before, the metadata reconcile). The canonical read resolves an
arrival id to its message — materializing the metadata row from the arrival's
durable header fields when the reconcile has not yet run — so a client can follow
an event id straight into `GET /v1/messages/{id}` and obtain the canonical
message (including its message id, which a reply/forward then uses). An arrival
whose folder UIDVALIDITY has since changed resolves to `404`.

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

SendGrid endpoint:

```http
POST /internal/ingest/sendgrid
```

Postmark endpoint:

```http
POST /internal/ingest/postmark
```

Each adapter authenticates the provider (Mailgun HMAC signature; Cloudflare
bearer plus `X-MailMoose-Recipient`; Resend Svix signature headers; SendGrid
ECDSA webhook signature over the raw body; Postmark HTTP Basic authentication)
before parsing or persisting MIME, then converts delivery to the canonical
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

Direct SMTP uses a core-established HTTP/2 session to the unified receiver.
Private MX sessions use bearer authentication; shared Dial MX sessions use
DNS-backed domain proofs. Recipient resolution and durable ingestion are framed
session operations, not core HTTP endpoints. Duplicate delivery fingerprints
return the recorded disposition. See [MX.md](MX.md) and [DIALMX.md](DIALMX.md).

### Installation MX receiver (system administrator, session-authenticated)

A domain chooses whether it receives by MX; the **installation** decides how the
core reaches the receiver every MX domain uses. That installation setting is the
single `mx_settings` row, edited from the Admin UI or this API:

```http
GET    /v1/admin/mx
PUT    /v1/admin/mx
DELETE /v1/admin/mx?revision=<n>
```

These are **installation-management routes**: they authenticate with a logged-in
system administrator's cookie session (`mmm_session`), not a bearer API key.
Writes additionally require a CSRF token (`X-CSRF-Token` header or `_csrf` form
field). An account bearer key is rejected (`401`), and a logged-in non-system
administrator is rejected (`403`); API keys can never be system administrators.
The OpenAPI operation for each carries `x-authentication: session` and requires
the `sessionAuth` security scheme.

`GET` returns the redacted settings plus a live `status`:

```json
{
  "mode": "remote",
  "url": "https://receiver.example:8443",
  "bearer_key": "",
  "key_configured": true,
  "ca": "",
  "hostname": "",
  "max_message_bytes": 0,
  "max_staging_bytes": 0,
  "max_recipients": 0,
  "max_connections": 0,
  "require_tls": false,
  "verify_spf": true,
  "verify_dkim": true,
  "verify_dmarc": true,
  "dns_resolver": "",
  "dns_timeout_seconds": 0,
  "read_timeout_seconds": 0,
  "write_timeout_seconds": 0,
  "data_timeout_seconds": 0,
  "smtp_tls_cert": "",
  "smtp_tls_key_configured": false,
  "revision": 3,
  "updated_at": "2026-10-01T00:00:00Z",
  "status": {
    "mode": "remote",
    "configured": true,
    "revision": 3,
    "state": "active",
    "included_supported": true,
    "smtp_addr": "",
    "session_addr": "",
    "active_connections": 0
  }
}
```

`mode` is `included` (the embedded receiver runs inside this deployment:
credentials are generated automatically and the port forward and DNS are handled
by the container) or `remote` (a receiver in its own container, on another host,
or on the LAN, reached at `url` with `bearer_key`). There is no `auto` mode yet —
it is a deferred UI-only placeholder and is rejected here.

- `bearer_key` is never returned; `key_configured` reports whether one is
  stored. On `PUT` a blank `bearer_key` retains the stored credential, and for
  included mode the core generates and retains the key (a supplied value is
  ignored). A remote receiver requires a `url` and a key.
- `hostname`, `max_message_bytes`, `max_staging_bytes`, `max_recipients`,
  `max_connections`, `require_tls`, `verify_spf`, `verify_dkim`, `verify_dmarc`,
  `dns_resolver`, `dns_timeout_seconds`, `read_timeout_seconds`,
  `write_timeout_seconds`, `data_timeout_seconds`, `smtp_tls_cert` and
  `smtp_tls_key` tune the included receiver; a zero value uses the receiver
  default for a limit or timeout.
- `max_message_bytes` defaults to `31457280` (30 MiB), `max_staging_bytes` to
  `268435456` (256 MiB, and never below the message cap), `max_recipients` to
  `100`, `max_connections` to `256`, the DNS timeout to `10 s`, and the read,
  write and DATA timeouts to `60 s`, `60 s` and `300 s`.
- `require_tls` defaults **off** and refuses plaintext SMTP; it requires a
  STARTTLS certificate and key.
- `verify_spf` / `verify_dkim` / `verify_dmarc` default **on**. They are a
  tri-state: omit a toggle to leave the default, or send an explicit `true` /
  `false` to set it. A GET omits a toggle that was never explicitly set, so a
  GET→PUT round-trip cannot turn "default" into "explicitly on".
- `smtp_tls_cert` is the public STARTTLS certificate (PEM) and is returned by
  GET. `smtp_tls_key` is the private key (PEM) and is **write-only**: it is never
  returned, and GET reports `smtp_tls_key_configured` (always present) instead. A
  blank `smtp_tls_key` retains the stored key; supplying a certificate with a
  blank key keeps the pair; clearing `smtp_tls_cert` (with a blank key) removes
  the pair and clears the key. A certificate and key must be supplied together,
  and the pair must parse as a valid X.509 key pair, or the save is rejected
  (`400`).
- A remote save that carries any included-only field (the SMTP/staging limits,
  the TLS pair, the DNS/timeout settings, `require_tls` or a verification
  toggle) is rejected (`400`), so the operator is never misled by values the
  runtime ignores.
- `revision` is an optimistic-concurrency token; a stale `PUT` returns `409`.
  Zero creates the configuration when none exists.
- **Readiness is never claimed before the receiver is live.** `state` is
  `disabled`, `unknown`, `standby`, `connecting`, `active`, `draining`, `failed`
  or `unavailable`. `connecting` means a settings change or the receiver's
  session handshake is still in progress; `active` is reported only once the
  handshake completes, so a receiver that never connects is never described as
  ready. Without an attached runtime (e.g. `included` in a rootless deployment)
  the state is `unknown`/`unavailable` rather than `active`, and
  `included_supported` is false.
- `DELETE` clears the configured receiver and preserves the initialized marker,
  so the one-time environment import never re-fires. It requires the current
  `revision`.

The legacy `MX_ENABLE`, `MX_RECEIVER_URL` and `DIALMX_CORE_KEY` environment
values, together with the legacy `MX_*` SMTP settings (hostname, limits,
verification, DNS resolver, timeouts and the `MX_TLS_CERT`/`MX_TLS_KEY` files),
are imported once on first start when no row exists; thereafter the persisted
settings are authoritative and the environment is ignored. The only MX
environment that still belongs to the core deployment is the standalone receiver
container's own configuration (see [MX.md](MX.md)); the core itself is
configured here.

> **Legacy STARTTLS files.** The one-time import reads `MX_TLS_CERT` and
> `MX_TLS_KEY` from disk. If only one path is set, or a file cannot be read, the
> import currently continues **without** TLS and logs a warning rather than
> failing closed. This is a known weakness during the transition: a deployment
> that intended to require STARTTLS can come up offering plaintext. Until the
> importer fails closed, verify after upgrading that `smtp_tls_key_configured`
> is `true` (or set the pair here), and do not rely on the environment to
> enforce TLS.

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
  `smtp: {"host","port?","username?","password?","security?","from_domain?"}`;
  `mx: {"helo"}` (self-hosted only; one envelope recipient per message).
  (`security` is `starttls`, `tls`, or `plain`; `port` defaults to `587`).
- receiving — `mailgun: {"signing_key"}`;
  `cloudflare: {"webhook_secret"}` (generated by MailMoose);
  `resend: {"api_key","webhook_secret","api_base?"}`;
  `sendgrid: {"public_key"}` (the Inbound Parse security-policy ECDSA public
  key, base64 or PEM);
  `postmark: {"username","password"}` (both generated by MailMoose);
  `dialmx: {"service?","contact_email?","receiver_urls?","enforcement?"}` —
  `service` is `antler` (the zero-config Antler MX shared relay, the default) or
  `custom`. Antler requires a valid `contact_email` and resolves the hosted
  receiver set; supplying `receiver_urls` for Antler is a `400`, because the
  receiver set is never caller-controlled. Custom requires one or more HTTPS
  `receiver_urls` and ignores `contact_email`. `enforcement` is `moderate`
  (default) or `hard`. A save that supplies only `receiver_urls` is treated as
  custom, so legacy configurations are never silently migrated.

A blank secret on a same-provider save keeps the stored value; changing provider
never reuses old fields or secrets. Non-secret fields are whole-config values:
an omitted field takes the provider default or is a required-field error, never a
merge. Saving performs no network or DNS validation, except that an Antler MX
save resolves the endpoint manifest (with cached and embedded fallbacks).

`GET` on an unconfigured slot returns
`200 {"domain_id":"...","configured":false,"provider":"","config":{}}`. A
configured slot adds the non-secret config, `updated_at`, and (receiving only)
`webhook_url`. A configured Dial MX slot additionally returns `key_id`,
`public_key`, `txt_record`, and the live setup picture:

- `status[]` — per-receiver state learned over the core's outbound session:
  `receiver_url`, `state` (`ready`, `rejected`, `connecting`, `unavailable`, …),
  bounded `reason`, advertised `smtp_hostname` and `expires_at`. A receiver is
  `ready` only once it has proved both domain authority (its `_mailmoose-mx` TXT
  key) and routing (its own SMTP hostname is published in the domain's MX
  records). A receiver that is authorized but not routed reports
  `rejected`/`not_mx`. For an Antler MX setup an unreachable receiver reports no
  advertised hostname, so the configured `smtp_hostname` (from the saved
  receiver snapshot) is returned instead; the status rows always name their
  connector.
- `dns[]` — published-record checks: `kind` (`mx`/`txt`), `name`, `expected`,
  bounded `found`, bounded `matched`, `state` (`ok`, `pending`, `mismatch`) and a
  short `reason`. Checks resolve DNS live on every request (bounded by a
  four-second timeout); nothing is cached in the core, so a record change is
  reflected on the next read. A resolver failure is reported as a check state,
  never an API error. For an MX check `matched` lists the expected receiver
  hostnames actually published; the check is `ok` once at least one expected
  hostname is published (the receiver set is redundancy), and `mismatch` only
  when MX records exist but none belongs to a receiver. The TXT check remains
  exact. These checks are the local "has the record propagated yet?" feed behind
  the receiving dialog's remediation block; routing itself is the receiver's
  `status[]` verdict, not a second core-side decision.
- `instructions` — for Antler MX, the copy-ready `txt_name`/`txt_value` and the
  `mx[]` list (`hostname`, `priority`) to publish, plus the contact email; for a
  custom service, the TXT record only.

Generated secrets (`cloudflare.webhook_secret`) are created only when missing,
preserved by a normal same-provider save, and replaced only by an explicit
`regenerate_secret` for the currently configured provider (a request that
supplies a generated value while regenerating, or regenerates an unconfigured
provider, is a `400`). The fresh value is returned once in a `generated` map and
is never returned again. Responses that may carry a generated secret are
`no-store`.

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
kind, timestamp, inbox, provider, `source`, from/to, subject and size. Blocked
rows add the reason; approval rows set `subject` to the reviewed draft subject
labelled `Approval: <subject>` (or `Rejected: <subject>` when the outcome is
`rejected`), carry the `action` (`approve`/`reject`) and the `status` outcome
(`approved`, `rejected`, `invalid` or `error`) plus the reason, and set `source`
to `Control` when no receiver source was recorded; delivered rows add the
click-through
`message_id`. The domain's UI log
merges this with the sending side into one two-way timeline. Both logs are
scoped by domain rather than by the current provider config.

Domain creation accepts `name` only, and `PATCH /v1/admin/domains/{id}` accepts
`catch_all_inbox_id` only. The domain object exposes the configured provider
names (`sending_provider`, `receiving_provider`) but no secrets or settings.

This configuration surface is provider-facing (Admin role) rather than
agent-facing.

## 12. Outbound webhook delivery

A **webhook** client (`type` `webhook` in `/v1/admin/clients`) is an inbox-bound
push destination. It has two payload modes:

- **notify** — `POST` with `Content-Type: application/json` and a fixed body:

  ```json
  {"event":"message.received","cursor":"evt_123","inbox_id":"in_01K...","message_id":"msg_01K..."}
  ```

- **forward** — `POST` with `Content-Type: message/rfc822` whose body is the raw
  stored MIME, byte-for-byte unchanged.

Every delivery carries the durable event headers:

```text
X-MailMoose-Event:      message.received | message.spam_state_changed
X-MailMoose-Delivery:   <client_id>:<cursor>
X-MailMoose-Message-Id: <message_id>
X-MailMoose-Cursor:     evt_<n>
```

A forward delivery additionally carries the original transport envelope
metadata, read from the persisted message and therefore identical on retries:

```text
X-MailMoose-Envelope-From: <percent-encoded original envelope sender, or empty>
X-MailMoose-Envelope-To:   <percent-encoded canonical original envelope recipient>
```

- Values are **percent-encoded UTF-8 using RFC 3986 escaping**: a space is
  `%20`, a literal plus is `%2B`, and `@` is `%40`. This is *not*
  `application/x-www-form-urlencoded`, where `+` would mean a space. A receiver
  must decode strictly and treat an unparseable value as absent.
- Exactly one header of each name is sent. A missing/null envelope sender is
  sent as an empty value, so a receiver that requires one fails closed rather
  than falling back to the spoofable MIME `From:` header. The decoded value is
  bounded to 254 octets (RFC 5321); a receiver should reject a longer encoded
  value (a 2048-octet encoded bound is generous).
- The sender is **relay-supplied, not provider-attested** (see `SECURITY.md`).
  Record it; do not treat it as authority without an independent provenance
  check.

Authentication is chosen per client:

- **bearer** — `Authorization: Bearer <secret>` (the same generated token shown
  once at creation/rotation).
- **signature** — `X-MailMoose-Signature: t=<unix>,v1=<hex>`, HMAC-SHA256 over
  `t + "." + body` with the client secret.

Delivery is durable and inbox-ordered: HTTP 2xx acknowledges and advances the
cursor; failures retry with capped exponential backoff and a delivery not
succeeded within the retry window is marked failed with the cursor advanced.
Mail that is currently Spam, internal, or has since been deleted is terminally
**skipped** (no request) with the cursor advanced, so it cannot block the queue.
An empty `X-MailMoose-Envelope-From` means the transport supplied no envelope
sender; it never reflects the MIME header.

## 13. Hermes Relay

Relay endpoint:

```text
wss://<host>/relay
```

Enrollment:

```http
POST /relay/enroll
```

The UI issues connector credentials directly and displays the corresponding `.env` block. The same management operations are available programmatically to an Admin principal:

```http
GET    /v1/admin/hermes
POST   /v1/admin/hermes/enroll
PUT    /v1/admin/hermes/{id}
DELETE /v1/admin/hermes/{id}
```

`POST /v1/admin/hermes/enroll` takes `{"inbox_id","name"}`, issues the relay
credentials directly, returns `201` with the `gateway_id`, `secret`,
`connector_url` and a ready-to-paste `env` block, and is the API
equivalent of the Create key dialog. There is no enrollment-token step: a
one-time code can only be minted for the OpenClaw kind (see §14). `GET` lists the account's relay
connections. `PUT /v1/admin/hermes/{id}` takes `{"role"}` and sets the
connection's outbound role: `owner` lets the relay send directly, while
`assistant` makes it draft and request approval instead. `DELETE
/v1/admin/hermes/{id}` removes the connection and returns `204`.

Relay protocol implementation should follow the Hermes connector contract while isolating its versioning from the canonical API.

## 14. OpenClaw Connector

OpenClaw connects over the same relay transport as Hermes (`wss://<host>/relay`)
and is a distinct connector kind, so it has its own management routes:

```http
GET    /v1/admin/openclaw
POST   /v1/admin/openclaw/enroll
POST   /v1/admin/openclaw/setup-code
PUT    /v1/admin/openclaw/{id}
DELETE /v1/admin/openclaw/{id}
```

`POST /v1/admin/openclaw/enroll` takes `{"inbox_id","name"}` and returns the
minted `gateway_id`, `secret`, `connector_url` and `kind` once.
`POST /v1/admin/openclaw/setup-code` takes the same body and returns a
single-use `code`, `expires_in`, the `setup_url` and a ready-to-paste
`command`; the OpenClaw host redeems the code at `POST /relay/enroll`, which
returns the connector credentials plus its `kind` and `name`. `GET` lists
OpenClaw connectors, `PUT` sets the outbound role (`owner` or `assistant`), and
`DELETE` removes the connector, closes its live socket and returns `204`.

## 15. openagent.email compatibility

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
