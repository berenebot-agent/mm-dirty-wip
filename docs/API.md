# MailMoose — API Contract

This document describes the native API contract and its explicit compatibility
surfaces. OpenAPI defines the operation schemas; the sections below explain
semantics and provider capabilities.

> **Route listings are generated.** `internal/apispec` is the single source of
> truth for the endpoint reference at the end of this document; run `make docs`
> to update it. The served agent guide is [`/agent`](/agent), and
> [`/openapi.json`](/openapi.json) is authoritative for the complete machine-
> readable operation list. The contract sections below explain behavior,
> provider schemas, and compatibility details that route tables cannot carry.

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

Writing either `aliases` or `alias_names` requires **account Admin**, including
clearing the set or changing only a display name. A forbidden alias write rejects
the entire PATCH before any settings change. Owners may still send through
existing Admin-created aliases, including aliases on another account-owned domain.
Inbox list, detail and bootstrap responses omit `approver_email` for read-only
principals; Assistant, Owner and Admin retain access to that field.

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
surface below and [MAILBOX_BEHAVIOR.md](MAILBOX_BEHAVIOR.md).

## 3a. Common mailbox surface

Standalone Google/Gmail inboxes also use this surface (D098). Connect using the
human wizard with a BYO Google OAuth Web client; see
[Google/Gmail setup](EMAIL_PROVIDERS.md#google--gmail).
Google list views cache metadata and synchronize through the History API; body
and attachment reads remain live. Folder locators are Gmail label IDs, with
`ARCHIVE` a derived view. A message can occur in multiple label views with the
same stable ID. Label changes update Gmail; archive removes `INBOX`. Sending
uses Gmail API, RemoteDraft creates a native Gmail draft, and Gmail maintains
the Sent copy. Permanent deletion and custom system-role remapping are
unsupported. Search uses the same `before` / `next_cursor` convention as other
listings; tokens are opaque and must be passed back without parsing. Provider
credentials and client secrets are never exposed.

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
  as complete. See [MAILBOX_BEHAVIOR.md](MAILBOX_BEHAVIOR.md) §3/§5.
- `errors` carries per-inbox failures (`{inbox_id, code, message, retryable}`)
  when an account-wide listing spans several inboxes, so one unreachable mailbox
  does not fail the whole request. Only a stable `code` and a short safe message
  are exposed; never raw provider or store text.
- An **account-wide** listing (no `inbox` scope) merges authorized sources by
  choosing their newest available heads, not by concatenating inboxes. Message
  and search continuations preserve each provider's native order and tie-breaks;
  sorting a provider window by MailMoose IDs would break its continuation. The
  opaque cursor records source progress. A per-source failure is reported in
  `errors` and does not fail the page. Listings are not a frozen snapshot when
  messages change between requests.

Message, thread and search pages use **`before=<next_cursor>`** on both
inbox-scoped and account-wide routes. Keep the same filters when continuing and
stop when `next_cursor` is empty or absent. Tokens are opaque, not dates or
provider identifiers. Search no longer accepts the old `cursor` parameter.
`/v1/messages?inbox={id}`, `/v1/threads?inbox={id}` and
`/v1/search?inbox={id}` use the same handlers as their inbox-scoped forms.
Empty optional envelope fields (`next_cursor`, `errors`) may be omitted.

### Shared contract and provider capabilities

| Operation | Domain | Connected mailbox |
|---|---|---|
| Message items, filters, pagination, roles | Same native contract | Same native contract |
| Message detail and raw MIME | Locally stored | Live fetch; unavailable provider returns an error, not a cached body |
| Labels | Persistent additive tags and folder destinations | Catalogue describes additive labels/categories or exclusive folder destinations |
| Permanent deletion | Supported after Trash | IMAP/Outlook subject to provider constraints; Google/One unsupported |
| Assistant request-send | MailMoose approval | Remote draft by default; MailMoose approval can be configured |
| Sending identities | Primary, managed aliases and explicit domain grants | Connected address; no managed-domain alias grant |
| Transport setup | Domain sending/receiving settings | Per-inbox connector or human OAuth setup |

These differences do not select a different message schema. Inspect the label
catalogue's capabilities and the inbox's authoring settings rather than guessing
from `kind`. `permissions.can_send` reports authorization, not provider readiness.

The common routes:

```text
GET    /v1/inboxes/{id}/folders
POST   /v1/inboxes/{id}/folders
PATCH  /v1/inboxes/{id}/folders/{folderId}
DELETE /v1/inboxes/{id}/folders/{folderId}
GET    /v1/inboxes/{id}/messages        ?folder=&before=&label_id=&from=&to=&unread=&has_attachment=&limit=
GET    /v1/inboxes/{id}/messages/{messageId}
GET    /v1/inboxes/{id}/messages/{messageId}/content
GET    /v1/inboxes/{id}/messages/{messageId}/attachments/{part}
POST   /v1/inboxes/{id}/messages/{messageId}/move   {"folder_id":"..."}
GET    /v1/inboxes/{id}/threads         ?folder=&before=&limit=
GET    /v1/inboxes/{id}/threads/{threadId}
GET    /v1/inboxes/{id}/search          ?q=&folder=&from=&to=&subject=&label_id=&unread=&has_attachment=&before=&limit=
GET    /v1/inboxes/{id}/label-catalog
```

Also under `/v1/messages/{id}` for both kinds: `GET …/content` (raw MIME) and
`GET …/attachments/{part}` (one attachment). For local mail `part` is a flat
numeric attachment index; the attachment-ID download route is preferable. For
connected mail it is a dotted MIME part path (e.g. `1.2`), discovered from MIME.

### Folders, roles and labels

Every inbox has a single hierarchical **folder** tree. A folder carries a
`role` (`folder`, `inbox`, `sent`, `drafts`, `trash`, `spam`, `archive`,
`outbox`, `label`) or is an ordinary custom folder. Local, IMAP and Outlook mail
belongs to one folder; Gmail supports multiple label views. Moving a message
emits `message.folder_changed`. A protected system folder
cannot be renamed or deleted, and a non-empty folder is refused (nothing is
silently cascaded). The **label catalogue** distinguishes additive tags from
exclusive folder-backed move destinations; inspect its capabilities rather than
assuming every label is independent of folders.

Move a message with `POST /v1/inboxes/{id}/messages/{messageId}/move` and
`{"folder_id":"<folder-id>"}`. The destination must belong to that inbox.
Like other message actions, it queues by default and accepts `?wait=true` to
wait for completion; the response contains the resulting message when complete.

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
  stored value only when that protocol's host, effective port, username and
  security are unchanged. Changing any of those requires re-entering its password.
  SMTP never inherits the stored IMAP password. Connection settings and encrypted
  credentials are saved atomically with revision checking. It also sets `sent_copy_enabled` and an optional
  `sent_copy_folder` for the remote Sent copy.
- `POST /remote/test` requires Owner/Admin and authenticates and resolves the folder scope without
  mutating mailbox state. The same password-retention rule applies before dialing;
  `smtp_password` supplies a candidate SMTP password and `clear_smtp` excludes SMTP
  from the test. `POST /remote/refresh` forces a reconcile pass and
  returns the resulting index `status`.
- `POST /remote/roles/{role}` maps a role: it returns the folder already
  carrying the role, or with `{"create":true}` creates the conventional folder;
  without either it answers `404` so the client prompts explicitly. A mapping is
  never silently invented.
- `GET /handoffs` lists the inbox's RemoteDraft handoffs newest first as the
  shared envelope, retaining terminal records (`published`, `ambiguous`,
  `failed`) even after the source draft is cleaned up. Each record carries its
  `publication` state and never an approval token, notification state, or frozen
  content hash. Remote-draft handoffs do not send a notification email.
- `GET`/`PUT /authoring` read/write the assistant authoring mode
  (`mailmoose_approval` or `remote_draft`). A standalone inbox defaults to
  `remote_draft`; a domain inbox is **preset** to `mailmoose_approval` —
  `remote_draft` is a standalone-only mode and is rejected (`400`) for a domain
  inbox. The mode is snapshotted onto each request. Setting a standalone inbox to
  `remote_draft` clears any approver. The read also reports `standalone` and
  `approver_enabled` (true exactly when the effective mode is
  `mailmoose_approval`). The legacy `notify_address`/`notify_overridden` fields
  are still returned but have no effect.

For a standalone inbox, a message's body and attachments are fetched **live**
from the remote server on each request and are never archived; the attachment
listing is empty and parts are downloaded structurally by MIME part path. The
single-message `GET` returns the **live body**, or `503` if the connector is
unreachable — there is no offline fallback that serves cached metadata as the
body. A purge (`DELETE /v1/messages/{id}/purge`) requires Assistant or Owner and
only acts on a message already in the Trash-role folder.

## 4. Messages

### One message schema for every inbox

All native message responses use the OpenAPI `Message` schema, regardless of
whether the inbox is domain-backed, IMAP, Google, Microsoft, or Gmail via One.
Inbox-scoped and account-wide routes use the same projection, including messages
inside thread responses and search results. Clients must not select a message
type based on the route, inbox kind, or provider. See decisions **D109/D110**.

The required core is `id`, `inbox_id`, `from`, `to`, `cc`, `subject`, `read`,
`has_attachments`, `size_bytes`, `provider`, `message_transport`, `direction`,
and `snippet`. `to` and `cc` are always arrays (`[]` when empty), never `null`.
All other fields are optional; unavailable values are
omitted, not replaced by invented values. A known boolean `false` is distinct
from an absent/unknown boolean. Empty optional strings and arrays are omitted.

`inbox_kind` identifies the owning inbox (`domain` or `standalone`). `provider`
identifies the mailbox implementation: `mailmoose` for a domain inbox, or
`imap`, `google`, `microsoft`, or `one` for a connected inbox. These are
informational metadata, not schema switches. `message_transport` names the
message's receive/send transport for a domain inbox (for example `cloudflare`,
`mx`, or `brevo`), and **equals `provider` for a connected inbox**, even for an
imported message. It describes the connector, not authenticated provenance of
the message's original delivery. An unavailable transport is the string
`unknown`, never `null`. The former `inbox_provider` name is removed. Other
configuration schemas' `provider` fields are unchanged.

IMAP `uid`, `uid_validity`, and handoff `remote_uid` are **internal-only**: they
are absent from message/handoff responses, event replay, webhook metadata and
relay notifications. Clients address messages through the canonical `id`.
Optional mailbox metadata includes `flagged`, `answered`, `draft`, `flags`,
`internal_date`, and `indexed_at`. RFC identity, reply references, and known
received/sent timestamps survive every projection. Unknown delivery `status`,
`is_spam`, or `created_at` remain absent. `created_at` is not synthesized from
receipt or indexing time; `indexed_at` can advance on a remote header refresh.

`label_ids` is the authoritative membership array for the owning inbox's label
catalogue, including system classifications such as Inbox, Sent, Spam and Trash.
It is always present, can contain multiple IDs, and uses the same projection on
every inbox kind and route. Fresh ordinary inbound mail has Inbox membership even
when its underlying local placement is implicit. Mail filed elsewhere, outbound
mail, Spam and Trash do not acquire Inbox membership merely by being in the inbox
object. The legacy `labels` array remains the additive-name view only; new clients
should read `label_ids` and filter with `label_id`.

Use catalogue IDs verbatim. Existing `tag:<Name>` and `folder:<id>` identities
remain stable; their prefixes describe backing representation, **not** whether
membership is exclusive. Gmail can carry several system and custom labels at
once. Native domain tags and Outlook categories remain additive; IMAP labels and
Outlook folder classifications are exclusive. Folders are a backend detail, not
a separate contract agents must implement.
The endpoint reference below lists the label routes. Filtering and membership
semantics are described in this contract and in the served [`/agent`](/agent)
guide.

`snippet` is always a plain-text string, normalized to at most 200 Unicode
characters. `"-"` means no usable preview is available: not yet generated,
generation failed, or the body was empty. New local messages cache previews on
ingestion/enqueue. New ordinary IMAP arrivals queue a bounded background
`BODY.PEEK[]<0.65536>` MIME-prefix read; no full body is fetched or archived for a
preview, and preview failures never block delivery. Control/workflow arrivals
are not queued. Existing mail is **not** back-generated, and list/detail requests
never generate missing previews. Google/Microsoft/One use their provider previews.

`direction` is always `inbound`, `outbound`, or `unknown`, using one policy:
recorded receipt/send direction wins; otherwise drafts are unknown, native Sent
labels or mapped Sent-role membership imply outbound, and Inbox-role evidence
implies inbound only when stronger Sent evidence is absent. Archive/arbitrary
folders otherwise remain unknown. Sender addresses and literal folder names
are not evidence. Inference affects presentation and reply defaults, never
authorization or eligibility for the local delivery queue.

Replies without explicit `to` use the original sender for inbound mail or the
original To recipients for outbound mail. Unknown direction requires explicit
recipients; the API returns `400` rather than guessing, and the UI leaves reply
and reply-all recipients blank with an explanation.

Message lists, search results, outbox lists, and messages inside thread responses
are metadata-only: they omit `text` and `html` for every inbox kind. Single-message
detail reads include `text` (possibly empty) and non-empty sanitized `html`.
Remote detail bodies are fetched live, never archived, and a fetch/parse failure
returns `503` rather than a successful metadata-only response. Lists do not fetch
remote MIME to manufacture body parity. Send/reply results retain their returned
body; mutation responses use the same message schema without fetching a remote
body just to acknowledge an action.

Messages can be filtered by label with `label` (legacy name) or `label_id`
(catalogue id); repeated values AND together and both forms resolve identically
on the inbox-scoped and account-wide routes. A folder-backed `label_id` requires
an `inbox=` scope account-wide. Repeated filters AND together; `tag:<Name>` and
a bare name are equivalent, and a `folder:<id>` filter is valid only for a folder
in the scoped inbox.

**Compatibility:** Clients that used list or thread responses to read local
message bodies must now fetch a message detail. Clients that relied on empty
remote direction must now handle `unknown`; synthesized remote `created_at`
remains absent. Native message mutations answer `202` with a queued action unless
`?wait=true` settles them into the primary body, and OpenAPI documents both.
This is a development-time contract change: no native aliases
for old provider names or IMAP identifiers are retained. The explicit
openagent.email compatibility projections remain separate and unchanged.

```http
GET /v1/messages
GET /v1/messages/{id}
PATCH /v1/messages/{id}
DELETE /v1/messages/{id}
POST /v1/messages/{id}/restore
DELETE /v1/messages/{id}/purge
```

Message mutations (including `POST /v1/messages/{id}/seen`) now use the same
async-default / `?wait=true` convention as sending, for both domain and standalone
inboxes. The default response is `202` with `{"queued":true,"action":{...}}`.
The action contains an opaque `id`, `inbox_id`, `message_id`, `input`, and `state`.
Follow the response's `Location` (`GET /v1/messages/{id}?action={action_id}`) to
inspect completion, including after purge removes the message itself.
The same queue is available through `GET /v1/actions` and `GET /v1/actions/{id}`.
`POST /v1/actions/{id}` accepts `{"action":"retry"}` for failed jobs or
`{"action":"dismiss"}` for settled jobs. These controls require Assistant or
Owner and are the same core operations the human queue panel uses.
States are `pending`, `running`, `done`, `failed`, or `ambiguous`.
Each action also carries `inbox_name`: the display name of the mailbox it
belongs to, or that mailbox's address when it has no name. The queue spans the
account's whole mailbox set, so the name places a row.

The queue list also includes a read-only `syncs` array for visible standalone
inboxes with unsettled metadata sync. Each entry has `inbox_id`, `name`, `address`,
`provider`, `state`, `phase`, `indexed`, `total`, and an optional `updated_at` and
safe `error`. Sync states are `queued`, `running`, `waiting`, `paused`, or `error`;
phase is `initial` or `refresh`. `total: 0` means unknown: use the indexed count,
not a percentage. Sync rows have no action ID and cannot be retried/dismissed via
action endpoints. Metadata sync progress is safe state only; status reads do not
fetch provider mail or expose credentials.

Add `?wait=true` to wait up to 30 seconds. Successful PATCH/restore returns the
updated message; delete/purge returns `204`; seen returns `{id,seen}`.
A wait timeout returns `504` with the action handle; it does not cancel the job.
Failed or uncertain actions return `409` with their action record. Submission
checks mailbox scope and roles before enqueueing; purge still requires Trash.
Clients that previously depended on immediate mutation must add `?wait=true`.
Empty Trash (`POST /v1/inboxes/{id}/trash/empty`) starts a durable parent action
that discovers Trash targets and purges them in background batches without a
total batch cap. IMAP/Outlook enumerate provider-side Trash, not only the cache;
Google/One permanent deletion remains unsupported. The default `202` includes
the parent action and a `Location` for progress. Repeated submissions reuse an
unsettled operation. `?wait=true` waits up to 30 seconds: `200` includes `purged`,
`409` reports a failed/uncertain action, and `504` includes its continuing handle.

Actions execute in submission order per message. Transient absolute-state patches
have bounded retries; moves and purge with uncertain remote outcomes are never
blindly replayed. A background rechecker reconsiders `failed` actions every ten
minutes: a retryable absolute-state patch returns to the queue and is retried,
while a location-changing action (move/delete/restore/purge) is only retried when
the message is still observed in its source folder — if it already reached the
intended destination the action settles as done, and if its location is unknown it
stays failed for a human. An action is never replayed out of order behind a newer
unsettled action for the same message, and `ambiguous` actions are never
reconsidered. The human UI's header status flag opens the shared queue,
shows a red exclamation for failures, and offers Retry/Dismiss. An ambiguous
action must be verified before dismissing it and submitting a new action.

Filters may include:

```text
inbox
thread
folder
label
from
to
before
unread
has_attachment
spam
include_spam
trashed
```

`folder` scopes the listing to one folder: a folder id for a domain inbox, a
folder path for a standalone inbox — the same rule the inbox-scoped route uses.
On the account-wide route it applies to the inbox named by `inbox=`; without that
scope it narrows the connected (standalone) inboxes by folder path and leaves the
local store unfiltered.

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
the mailbox. `DELETE /v1/messages/{id}/purge` (Assistant or Owner) erases a
trashed message permanently and unlinks its raw file; a message must be trashed
first, otherwise the call answers `409`. `POST /v1/inboxes/{id}/trash/empty`
(Assistant or Owner) purges every trashed message in an inbox.

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
  "provider": "mailmoose",
  "message_transport": "mx",
  "snippet": "Hi...",
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
  "size_bytes": 2048,
  "is_spam": false,
  "labels": ["Invoices", "Unpaid"],
  "label_ids": ["folder:fld_example_inbox", "tag:Invoices", "tag:Unpaid"]
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

Use the inbox's **provider-aware catalogue**, including when it has no labels:

```http
GET    /v1/inboxes/{id}/label-catalog
POST   /v1/inboxes/{id}/label-catalog
PATCH  /v1/inboxes/{id}/label-catalog/{labelId}
DELETE /v1/inboxes/{id}/label-catalog/{labelId}
POST   /v1/inboxes/{id}/messages/{messageId}/labels?wait=true
```

Catalogue entries expose `id`, `name`, `kind` (`tag` or `folder`), `membership`
(`multiple` or `exclusive`), `source`, and `can_add`, `can_remove`, `can_rename`
and `can_delete` capabilities. System entries also expose `role`, such as `inbox`,
`sent`, `spam`, `trash` or `archive`; their rename/delete capabilities are false.
Sent and remote Drafts are discoverable but read-only classification targets.
Local drafts and outbox jobs have their own APIs and are not Message memberships.
The catalogue's `capabilities` describes creation and edit support even when
`items` is empty. A domain inbox can keep an empty named tag; Gmail/One own their
provider labels, Outlook exposes categories and folder destinations, and raw
IMAP uses folder-backed destinations. Labels are inbox-scoped; an account-wide
name filter matches that name within each authorized inbox.

Filter ordinary mailbox views with the same parameter on every backend:
`GET /v1/inboxes/{id}/messages?label_id=<catalogue-id>` (or the corresponding
search route). Repeated `label_id` filters AND together; multiple Gmail system
memberships can be intersected. Only one exclusive classification may be
requested. Inbox-scoped and account-wide list/search routes share resolution;
account-wide `folder:<id>` filters still require `inbox=`.

Apply a catalogue entry with `{"label_id":"tag:Invoices","operation":"add"}`;
use `add` only where `can_add` is true and `remove` only where `can_remove` is
true. This includes supported system classifications. Adding an exclusive label
**moves** the message rather than adding an independent classification; removing
one without a replacement is rejected. Applying Inbox, Spam or Trash to local
mail updates placement and visibility state atomically. Additive labels are
preserved. Gmail system actions preserve unrelated memberships and use Google's
normal Inbox/Spam/Trash transitions. Label writes require Assistant or Owner and
follow the normal queued-action / `?wait=true` convention.

Existing `folder=`, folder endpoints and `/move` remain compatibility interfaces;
their folder-only behaviour is unchanged. Agents do not need them to discover,
filter or apply supported ordinary classifications. The web reader selects its
labels using the same membership projection as API responses.

A native additive tag is trimmed and internal runs of whitespace collapsed. It must be
non-empty, at most 64 characters, and must not contain control characters, `/`
or `\`. (The path separators are rejected so a label can be addressed by its own
URL query parameter without path-encoding ambiguity.) An invalid label is
rejected with a validation error. Provider-owned labels follow provider naming
rules and can include hierarchical names; use returned catalogue IDs verbatim.

```http
GET /v1/labels
```

This is the legacy flat-name view, scoped to the key's authorized inboxes. It
does not describe capabilities, empty labels or exclusive destinations.

Legacy replace-set additive-label writes remain available through
`PATCH /v1/messages/{id}`:

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
label_id
folder
from
to
subject
unread
before
has_attachment
limit
```

Search operates only within the key's authorized inbox set. Both route forms
accept `q`, `folder`, `label`, `label_id`, `from`, `to`, `subject`, `unread`,
`has_attachment`, `before`, and `limit`; the account-wide form also accepts
`inbox`. Omit `q` for a filter-only search. Use plain text for portable searches:
local mail uses FTS5 while connected mailboxes use their provider's search engine;
advanced syntax, tokenization and searchable fields are not identical. There is
no portable date-range filter; `before` is a continuation, never a timestamp.

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
/v1/messages/{id}/attachments/{part}` downloads a local attachment by flat
numeric index or a connected attachment by dotted MIME part path (e.g. `1.2`).
For a **standalone** inbox's message only the
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
- An Owner's direct `POST /v1/drafts/{id}/send` on a pending request follows the
  same fingerprint-checked approval flow and sends only the stored draft. The
  web compose/save Send action returns `409` for a pending draft; use its Review
  Draft approval action instead. Unsaved edits cannot authorize different content.
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
**Remote draft** (standalone default) just places the draft in the connected
mailbox's remote Drafts folder: MailMoose never sends it, sends no notification
email, and never involves an approver (switching a standalone inbox to
`remote_draft` clears any approver). The handoff events are
`draft.handoff_requested`, `draft.handoff_published`, `draft.handoff_ambiguous`
and `draft.handoff_failed`. An unverifiable remote append is reported
`ambiguous`, never retried blindly, and its local draft is unfrozen so it is not
trapped. A handoff whose remote Drafts folder does not exist fails terminally:
the append is refused as `failed` with the reason `the connected mailbox has no
Drafts folder`, the handoff is not retried, and the local draft is unfrozen so
it can be edited or requested again.

A draft response carries a **`handoff`** object when a RemoteDraft handoff is
active or most recent, exposing the immutable `handoff_id` and
`publication` (`pending`/`published`/`ambiguous`/`failed`), so a client can
observe the placement outcome rather than only seeing the frozen draft. There is
no notification state: a handoff never notifies. The full history is at
`GET /v1/inboxes/{id}/handoffs` (terminal records are retained after the local
draft is cleaned up). The web UI exposes no handoff surface: a request to send in
`remote_draft` mode simply places the draft, with no pane, badge or
cancel/retry control.

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

When the sending key holds a **domain-wide sending grant** on the inbox — see
`send_any_in_domain` in `GET /v1/bootstrap` — `sender` may instead be **any
address on the inbox's domain**, for example `billing@example.com`. The grant is
live only while the inbox is its domain's enabled catch-all and is checked at
send time, so a revoked grant rejects the send with the same `403`. A session
(the web UI) never has the grant, so it keeps the primary-or-alias rule.

An **account Admin key** holds the grant implicitly, on the same terms: it may
send as any address on a domain, but only from that domain's enabled catch-all
inbox, and the grant is lost as soon as the key stops being an Admin key.
`GET /v1/bootstrap` therefore reports `send_any_in_domain` for an Admin key on
the catch-all inbox only, with `send_any_domain` naming the domain.

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

Postmark endpoint:

```http
POST /internal/ingest/postmark
```

Each adapter authenticates the provider (Mailgun HMAC signature; Cloudflare
bearer plus `X-MailMoose-Recipient`; Resend Svix signature headers;
Postmark HTTP Basic authentication)
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
return the recorded disposition. For receiver choices and setup, see
[Remote SMTP receiving](REMOTE_MX.md).

### Installation MX receiver (system administrator, session-authenticated)

A domain chooses whether it receives by MX; the **installation** owns the
receiver used by Direct MX domains. The Included receiver settings are edited
in the domain's Direct MX Receiving panel; the installation-level `mx_settings`
row can also be read or changed through this API, including its advanced Remote
mode:

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
- `DELETE` clears the configured receiver and leaves the settings
  unconfigured; nothing is re-seeded from the environment. It requires the
  current `revision`.

The persisted settings are the **only** source of the core's receiver
configuration. The legacy `MX_ENABLE`, `MX_RECEIVER_URL`, `DIALMX_CORE_KEY` and
`MX_*` SMTP tunables are no longer imported or validated, and a stale or partial
value in the environment does not change the configured receiver. The only MX
environment that belongs to a deployment is the **standalone receiver's own
configuration**, given to the receiver container (see
[Remote SMTP receiving](REMOTE_MX.md)); the core itself is configured here.

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
  `postmark: {"username","password"}` (both generated by MailMoose);
  `sendgrid: {"public_key"}` (the base64 public key of the SendGrid Inbound
  Parse security policy);
  `remotemx: {}` (no config — the receiver's URL and bearer key are
  account-scoped and set through the account's Remote MX settings, not per
  domain);
  `dialmx: {"service?","contact_email?","receiver_urls?","enforcement?"}` —
  `service` is `antler` (the zero-config Antler MX shared relay, the default) or
  `custom`. Antler requires a valid `contact_email` and resolves the hosted
  receiver set; supplying `receiver_urls` for Antler is a `400`, because the
  receiver set is never caller-controlled. Custom requires one or more HTTPS
  `receiver_urls` and ignores `contact_email`. `receiver_urls` is a **string**,
  not an array: a comma-separated list of up to eight HTTPS base URLs, with no
  path, userinfo, query or fragment — for example
  `"receiver_urls": "https://mx1.example.com,https://mx2.example.com"`.
  `enforcement` is `moderate`
  (default) or `hard`. A save that supplies only `receiver_urls` is treated as
  custom, so legacy configurations are never silently migrated.

The `provider` field takes the provider's **name**, not the label the web
interface shows:

| `provider` value | Interface label | Direction |
| --- | --- | --- |
| `dialmx` | Antler MX (Dial MX for a custom receiver) | receiving |
| `mx` | Direct MX | receiving and sending |
| `remotemx` | Remote MailMoose MX | receiving |
| `cloudflare` | Cloudflare | receiving |
| `mailgun` | Mailgun (Mailgun API when sending) | receiving and sending |
| `postmark` | Postmark | receiving |
| `resend` | Resend | receiving and sending |
| `sendgrid` | SendGrid | receiving |
| `brevo` | Brevo API | sending |
| `smtp` | SMTP | sending |

`dialmx` carries two services: `"service": "antler"` (the hosted Antler MX
relay, the default) and `"service": "custom"` (a receiver you run yourself,
shared by many domains — the Dial MX option in
[Remote SMTP receiving](REMOTE_MX.md)). The picker shows a longer description
for Antler MX and Remote MX, and Direct MX's own description already names the
bundled receiver; the name is the same in every case.

A name is accepted only in its own direction — `remotemx` is a receiving
provider and `brevo` a sending one — and a name that is not registered for the
direction being saved is refused with a `400`.

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
  connector. `http_code` is the HTTP status the receiver URL answered the
  session with, when it answered with one: whatever answered at that URL — the
  receiver, or a proxy in front of it — owns that status, and the core cannot
  tell the two apart, so the code is returned and nothing is attributed. It is
  absent when the attempt was never answered.
- `dns[]` — the watcher's shared published-record checks: `kind` (`mx`/`txt`),
  `name`, `expected`, bounded `found`, bounded `matched`, bounded `unexpected`
  (published MX hostnames that are not Antler receivers), `state` (`ok`,
  `not_found`, `mismatch`, `dns_error`) and a short `reason`. The checks are
  computed by a background watcher
  (bounded by a four-second timeout per lookup), so every read is an in-memory
  lookup and runs no DNS; a verdict change is pushed as the transient
  `mx.dns_changed` event. A resolver failure is reported as a check state, never an
  API error. For an MX check `matched` lists the expected receiver hostnames
  actually published; the check is `ok` once at least one expected hostname is
  published (the receiver set is redundancy), and `mismatch` only when MX records
  exist but none belongs to a receiver. The TXT check remains exact, parsed by the
  same MM1 validator the receiver applies, so a matching verdict agrees with what
  the receiver accepts. `not_found` means the record is definitively absent (NODATA
  or NXDOMAIN) and `dns_error` means the lookup could not be completed — the two
  are distinct so a transient resolver fault is never presented as a missing
  record. Only `ok` is a settled good verdict: on the receiving side `mismatch`,
  `not_found` and `dns_error` are all rendered red, because the operator must not
  proceed on a record that is wrong, absent, or unconfirmed. On a `dns_error` MX
  check the previously observed `unexpected` hosts
  are carried forward, so a known extra-MX warning survives a resolver outage and
  is only cleared by a completed check. These checks
  are the local "has the record propagated yet?" feed behind the setup wizard's
  record table and the status view's fix prompts; the extra-MX verdict itself is
  surfaced separately as the top-level `unexpected_mx[]` (see below), and routing is
  the receiver's `status[]` verdict, not a second core-side decision.
- `instructions` — for Antler MX, the copy-ready `txt_name`/`txt_value` and the
  `mx[]` list (`hostname`, `priority`) to publish, plus the contact email, the
  current `enforcement` policy and, for a domain that inherits its setup,
  `inherited_from`. It is omitted entirely for a domain with
  no copy-ready records.
- `unexpected_mx[]` — published MX hostnames that are not one of the domain's
  configured Antler receivers. It is a **top-level** field, independent of
  `instructions`, and is the shared verdict behind both the dashboard's amber light
  and the status dialog's "additional MX servers" warning, so the two surfaces
  cannot disagree. It is the `unexpected` list of the `mx` check in `dns[]`, owned
  by the same background watcher (which pushes changes as the transient
  `mx.dns_changed` event), so the HTTP read is an in-memory lookup and runs no DNS.
  Empty when no rogue records exist.

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
log: delivered inbound mail (`kind` `received`), inbound mail that was not
stored (`kind` `blocked`) — rejected by an inbox's allowed-senders rule, or
refused because the account or the inbox storage quota is full (reason
`storage quota exceeded`) — and consumed approval control mail (`kind`
`approval`), newest first (`limit`, default `100`, max `200`;
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
`catch_all_inbox_id` only. A `name` that is already registered — in the caller's
own account or, under global domain uniqueness, in any other — answers `409`
with `a domain with that name already exists`; the other owner is never
disclosed. The domain object exposes the configured provider names
(`sending_provider`, `receiving_provider`) but no secrets or settings.

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

<!-- BEGIN GENERATED API REFERENCE -->

## Endpoint reference

Generated from `internal/apispec`; do not edit by hand.

### Discovery

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/bootstrap | Discover key capabilities and accessible inboxes |  |
| GET | /v1/limits | Get server pagination, size and rate limits | read |
| GET | /v1/account/settings | Get account preferences (Admin) | admin |
| PATCH | /v1/account/settings | Update account preferences (Admin) | admin |

### Inboxes

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/inboxes | List inboxes | read |
| POST | /v1/inboxes | Create an inbox (Admin) | admin |
| GET | /v1/inboxes/{id} | Get an inbox | read |
| PATCH | /v1/inboxes/{id} | Update an inbox (display_name, enabled, allowed_senders, sender_restricted, approver_email, aliases, alias_names, default_sender, trash_retention_days, storage_quota_bytes, auto_mark_read_on_delivery, auto_trash_after_delivery_hours, delivery_trigger) | owner |
| DELETE | /v1/inboxes/{id} | Delete an inbox (Admin) | admin |
| POST | /v1/inboxes/{id}/trash/empty | Empty an inbox's Trash (Assistant/Owner) | assistant |

### Mailbox

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/inboxes/{id}/folders | List an inbox's folders (common) | read |
| POST | /v1/inboxes/{id}/folders | Create a folder (Assistant/Owner) | assistant |
| PATCH | /v1/inboxes/{id}/folders/{folderId} | Rename a folder (Assistant/Owner) | assistant |
| DELETE | /v1/inboxes/{id}/folders/{folderId} | Delete a folder (Assistant/Owner) | assistant |
| GET | /v1/inboxes/{id}/messages | List an inbox's messages (common envelope) | read |
| GET | /v1/inboxes/{id}/messages/{messageId} | Get a message (common) | read |
| GET | /v1/inboxes/{id}/messages/{messageId}/content | Download a message's raw MIME (common) | read |
| GET | /v1/inboxes/{id}/messages/{messageId}/attachments/{part} | Download one message attachment (common) | read |
| POST | /v1/inboxes/{id}/messages/{messageId}/move | Move a message to a folder in the inbox (Assistant/Owner) | assistant |
| GET | /v1/inboxes/{id}/threads | List an inbox's threads (common envelope) | read |
| GET | /v1/inboxes/{id}/threads/{threadId} | Get a thread (common) | read |
| GET | /v1/inboxes/{id}/search | Search an inbox (common envelope) | read |
| GET | /v1/inboxes/{id}/labels | List legacy flat labels | read |
| GET | /v1/inboxes/{id}/remote | Get a standalone inbox's remote configuration | read |
| PUT | /v1/inboxes/{id}/remote | Configure a standalone inbox's remote connector (Owner/Admin) | owner |
| POST | /v1/inboxes/{id}/remote/test | Test a standalone inbox's remote connector (Owner/Admin) | owner |
| POST | /v1/inboxes/{id}/remote/refresh | Reconcile a standalone inbox against its server (Assistant/Owner) | assistant |
| POST | /v1/inboxes/{id}/remote/roles/{role} | Map or create a standalone inbox's special folder role (Assistant/Owner) | assistant |
| GET | /v1/inboxes/{id}/authoring | Get an inbox's assistant authoring settings | read |
| PUT | /v1/inboxes/{id}/authoring | Set an inbox's assistant authoring settings (Owner/Admin) | owner |
| GET | /v1/inboxes/{id}/handoffs | List an inbox's RemoteDraft handoff history | read |

### Identities

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/identities | List identities (openagent.email compat) | admin |
| POST | /v1/identities | Create an identity (Admin) | admin |
| DELETE | /v1/identities/{address} | Delete an identity (openagent.email compat) | admin |

### Messages

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/messages | List messages with filters (inbox, thread, label, from, to, unread, has_attachment, before, trashed) | read |
| GET | /v1/messages/wait | Long-poll for a new message | read |
| POST | /v1/messages/wait | Long-poll for a new message (compat) | read |
| GET | /v1/actions | List scoped mailbox action queue | read |
| GET | /v1/actions/{id} | Get mailbox action status | read |
| POST | /v1/actions/{id} | Retry or dismiss a mailbox action | assistant |
| GET | /v1/messages/{id} | Get a message | read |
| PATCH | /v1/messages/{id} | Update read/labels/spam state | assistant |
| DELETE | /v1/messages/{id} | Queue a move to Trash (Assistant/Owner) | assistant |
| POST | /v1/messages/{id}/restore | Restore a trashed message (Assistant/Owner) | assistant |
| DELETE | /v1/messages/{id}/purge | Queue permanent deletion of a trashed message (Assistant/Owner) | assistant |
| POST | /v1/messages/{id}/seen | Mark a message seen (compat) | assistant |

### Attachments

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/messages/{id}/attachments | List message attachments | read |
| GET | /v1/messages/{id}/attachments/{part} | Download one message attachment by MIME part path | read |
| GET | /v1/messages/{id}/content | Download a message's raw MIME | read |
| GET | /v1/attachments/{id} | Download an attachment | read |

### Threads

Thread list items and thread detail responses include integer `unread_count`,
including an explicit `0` when all counted messages are read. This applies to
domain and standalone inboxes (IMAP, Gmail and Outlook), both inbox-scoped and
account-wide routes. List counts respect the same folder/label filters and
visibility rules as `message_count`; hidden Spam, Trash and internal local mail
do not inflate ordinary thread counts. Detail `unread_count` counts unread
messages in the returned `messages` array, matching the detail's `message_count`.
The `/messages` subroute remains a message array, not a thread summary.

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/threads | List threads | read |
| GET | /v1/threads/{id} | Get a thread | read |
| GET | /v1/threads/{id}/messages | List messages in a thread | read |

### Search

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/search | Search messages | read |

### Labels

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/labels | List distinct labels in use | read |
| GET | /v1/inboxes/{id}/label-catalog | List provider-aware labels | read |
| POST | /v1/inboxes/{id}/label-catalog | Create a label | assistant |
| PATCH | /v1/inboxes/{id}/label-catalog/{labelId} | Rename a label if supported | assistant |
| DELETE | /v1/inboxes/{id}/label-catalog/{labelId} | Delete a label if supported | assistant |
| POST | /v1/inboxes/{id}/messages/{messageId}/labels | Apply or remove a message label | assistant |

### Events

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/events | Incremental event history | read |
| GET | /v1/events/wait | Long-poll for events | read |
| GET | /v1/events/stream | SSE event stream | read |

### Send

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| POST | /v1/send | Send email as an Owner | owner |
| POST | /v1/messages/{id}/reply | Reply to a message (Owner); optional sender chooses the From identity | owner |

### Drafts

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/drafts | List drafts (filter by inbox; supports before and limit) | assistant |
| POST | /v1/drafts | Create a draft | assistant |
| GET | /v1/drafts/{id} | Get a draft (includes attachments) | assistant |
| PATCH | /v1/drafts/{id} | Update a draft (partial) | assistant |
| DELETE | /v1/drafts/{id} | Delete a draft | assistant |
| POST | /v1/drafts/{id}/send | Send a draft (Owner) | owner |
| GET | /v1/drafts/{id}/attachments | List draft attachments | assistant |
| POST | /v1/drafts/{id}/attachments | Upload draft attachments (multipart field 'attachments') | assistant |
| GET | /v1/drafts/{id}/attachments/{attId} | Download a draft attachment | assistant |
| DELETE | /v1/drafts/{id}/attachments/{attId} | Delete a draft attachment | assistant |

### Send requests

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| POST | /v1/drafts/{id}/request-send | Request authorization to send a draft (Assistant) | assistant |
| POST | /v1/drafts/{id}/cancel-send-request | Cancel a pending send request (Assistant) | assistant |
| POST | /v1/drafts/{id}/approve | Approve and send a pending draft (Owner) | owner |
| POST | /v1/drafts/{id}/reject | Reject a pending send request (Owner) | owner |
| GET | /v1/drafts/{id}/send-request | Get the latest send request for a draft | assistant |
| GET | /v1/send-requests | List draft send requests (filter by inbox and active) | assistant |

### Outbox

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/outbox | List pending and failed outbound messages | read |
| POST | /v1/outbox/{id}/retry | Re-queue a failed outbound message | owner |
| DELETE | /v1/outbox/{id} | Cancel a pending send or discard a failed one | owner |

### Admin: domains

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/admin/domains | List domains (Admin) | admin |
| POST | /v1/admin/domains | Create a domain (Admin) | admin |
| PATCH | /v1/admin/domains/{id} | Update a domain (Admin) | admin |
| DELETE | /v1/admin/domains/{id} | Delete a domain (Admin) | admin |
| GET | /v1/admin/domains/{id}/sending | Get a domain's sending provider config (Admin) | admin |
| PUT | /v1/admin/domains/{id}/sending | Set a domain's sending provider config (Admin) | admin |
| DELETE | /v1/admin/domains/{id}/sending | Clear a domain's sending provider config (Admin) | admin |
| GET | /v1/admin/domains/{id}/receiving | Get a domain's receiving provider config (Admin) | admin |
| PUT | /v1/admin/domains/{id}/receiving | Set a domain's receiving provider config (Admin) | admin |
| DELETE | /v1/admin/domains/{id}/receiving | Clear a domain's receiving provider config (Admin) | admin |
| GET | /v1/admin/domains/{id}/sending/deliveries | List delivery attempts for a domain (Admin) | admin |
| GET | /v1/admin/domains/{id}/receiving/deliveries | List receiving activity for a domain (Admin) | admin |

### Admin: keys

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/admin/keys | List API keys (Admin) | admin |
| POST | /v1/admin/keys | Create an API key (Admin) | admin |
| DELETE | /v1/admin/keys/{id} | Revoke an API key (Admin) | admin |

### Admin: Hermes

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/admin/hermes | List Hermes connections (Admin) | admin |
| POST | /v1/admin/hermes/enroll | Enroll a Hermes Relay connection (Admin) | admin |
| PUT | /v1/admin/hermes/{id} | Update a Hermes connection outbound role (Admin) | admin |
| DELETE | /v1/admin/hermes/{id} | Delete a Hermes connection (Admin) | admin |

### Admin: OpenClaw

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/admin/openclaw | List OpenClaw connections (Admin) | admin |
| POST | /v1/admin/openclaw/enroll | Create an OpenClaw relay connection (Admin) | admin |
| POST | /v1/admin/openclaw/setup-code | Mint an OpenClaw one-time setup code (Admin) | admin |
| PUT | /v1/admin/openclaw/{id} | Update an OpenClaw connection outbound role (Admin) | admin |
| DELETE | /v1/admin/openclaw/{id} | Delete an OpenClaw connection (Admin) | admin |

### Admin: MX

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/admin/mx | Get the installation MX receiver settings and live status (System admin session) | admin |
| PUT | /v1/admin/mx | Set the installation MX receiver settings (System admin session) | admin |
| DELETE | /v1/admin/mx | Clear the installation MX receiver settings (System admin session) | admin |
| GET | /v1/admin/account/mx | Get the account Remote MX receiver settings and live status (Admin) | admin |
| PUT | /v1/admin/account/mx | Set the account Remote MX receiver settings (Admin) | admin |
| DELETE | /v1/admin/account/mx | Clear the account Remote MX receiver settings (Admin) | admin |

### Admin: clients

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/admin/clients | List clients (Admin) | admin |
| GET | /v1/admin/clients/webhooks | List webhook clients (Admin) | admin |
| POST | /v1/admin/clients/webhooks | Create a webhook client (Admin) | admin |
| PUT | /v1/admin/clients/webhooks/{id} | Update a webhook client (Admin) | admin |
| DELETE | /v1/admin/clients/webhooks/{id} | Delete a webhook client (Admin) | admin |
| POST | /v1/admin/clients/webhooks/{id}/rotate | Rotate a webhook signing secret (Admin) | admin |
| POST | /v1/admin/clients/webhooks/{id}/enabled | Enable or pause a webhook client (Admin) | admin |

<!-- END GENERATED API REFERENCE -->
