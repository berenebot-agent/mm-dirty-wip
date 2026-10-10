# MailMoose — V1 Architecture

## 1. Runtime topology

```text
                         INTERNET EMAIL
                               │
                     ┌─────────┴─────────┐
                  Mailgun/CF/Resend   optional mailmoose-mx
                   SMTP / MX edge       direct SMTP :25 edge
                     │                      │ HTTP/2 session
                  HTTPS webhook              │
                     └───────────┬────────────┘
                               ▼
                  ┌────────────────────────┐
                  │   MailMoose       │
                  │                        │
                  │ Go HTTP server         │
                  │ auth                   │
                  │ mailbox core           │
                  │ MIME ingest/parser     │
                  │ SQLite + FTS5          │
                  │ filesystem MIME store  │
                  │ replayable event history      │
                  │ SSE / long-poll        │
                  │ Hermes Relay           │
                  │ outbound adapters      │
                  │ embedded web UI        │
                  └──────────┬─────────────┘
                             │
                           /data
```

The optional `mailmoose-mx` edge is built into the same image but runs as a
separate, non-root process with no `/data` mount and no encryption key; it is
an optional exception to the one-process topology (decision `D031`). See
[MX.md](MX.md).

A second optional direct-SMTP path, **Dial MX**, inverts the direction: a
standalone receiver terminates SMTP and the core **dials out** to it over
verified HTTPS/2, so no inbound port is needed on the core. Like the MX edge it
is a deliberate, operator-chosen optional component outside the default
single-process topology; it does not introduce a hosted/account mode. See
[DIALMX.md](DIALMX.md) and decision `D067`.

Production runtime:

```text
1 binary
1 process
1 container
1 SQLite DB
1 filesystem data root
1 main HTTP listener + optional dedicated inbound webhook listener
```

The second listener defaults to `:8082`; `DEDICATED_RECEIVER_ENABLE=false`
disables it and `DEDICATED_RECEIVER_PORT` changes its port. It serves only the authenticated
inbound webhook routes and `/healthz`, so an operator can expose a dedicated
port to mail providers while keeping the main API/UI listener private. Both
listeners run in the same process and share the same store. Webhook routes remain
available on the main listener. `DEDICATED_RECEIVER_URL` controls generated
receiver URLs and falls back to `BASE_URL`; the latter remains the UI/API origin.

An inbox has a `kind`: `domain` (the classic managed-domain mailbox) or
`standalone`. A standalone inbox owns an address independent of any managed
domain and is reached through an optional per-inbox remote IMAP/SMTP connector
(decision `D097`). Both kinds are first-class mailboxes with folders, labels,
threads, events and API keys; only their transport differs. A standalone inbox's
message and thread **metadata** is cached locally while bodies and attachments
stay live on the provider and are never archived. The common mailbox boundary is
`internal/app`'s `MailboxRouter`/`RemoteMailboxService` plus
`internal/httpapp/mailbox_access.go`; see
[MAILBOX_SERVICE_CONTRACT.md](MAILBOX_SERVICE_CONTRACT.md).

## 2. Suggested Go packages

```text
/cmd/server

/internal
  /app
  /auth
  /accounts
  /domains
  /inboxes
  /messages
  /threads
  /attachments
  /events
  /search
  /quota

  /transport
      /mailgun
      /cloudflare
      /resend
      /smtp
      /imap
      /mx

  /integrations
      /hermesrelay

  /store
      /sqlite

  /web
  /config
/web
```

Packages should follow capability boundaries rather than generic framework layers.
The remote IMAP/SMTP adapter lives entirely in `/internal/transport/imap` and
`/internal/transport/smtp`; `/internal/app` maps the shared mailbox model onto it
and never speaks IMAP itself.

## 3. Canonical inbound boundary

Transport adapters authenticate their own webhook, stage raw MIME to a bounded
temp file, and return both a normalized message and the explicit binding they
verified:

```go
type InboundMessage struct {
    Provider          string
    Recipient         string // canonical original envelope recipient
    EnvelopeFrom      string
    RawPath           string
    Size              int64
    DeliveryID        string
    ProviderMessageID string
}

type InboundBinding struct {
    AccountID, DomainID, CredentialID, Provider, Recipient string
    Config map[string]any // decrypted provider configuration
}

type InboundTransport interface {
    Name() string
    Description() string
    ConfigFields() []ConfigField
    Receive(ctx context.Context, r *http.Request, resolver BindingResolver, tmpPath string, maxBytes int64) (InboundMessage, InboundBinding, error)
}
```

The service implements `BindingResolver`, which maps the envelope recipient to
the account, domain, and that domain's optional encrypted receiving
configuration (`CredentialID` is the config row's id). Provider auth material
stays inside the adapter. The mailbox core is transport-neutral beyond this
boundary.

## 4. Inbound transaction

```text
receive provider request
       ↓
resolve recipient → domain → receiving configuration (decrypted config)
       ↓
authenticate provider (before MIME is parsed; Cloudflare before MIME is read)
       ↓
stream raw MIME to controlled temporary path (bounded)
       ↓
validate the resolved inbox against the authenticated account (and domain, except on an alias route)
       ↓
parse required metadata
       ↓
begin SQLite transaction
       ↓
dedup on (account, provider, canonical recipient, provider delivery id)
       ↓
insert message / recipients / inbox links / thread / event
       ↓
commit
       ↓
finalize MIME path
       ↓
publish in-process realtime notification
       ↓
provider success response
```

Mailgun delivery idempotency uses the authenticated Mailgun webhook `token`;
Cloudflare uses its delivery id or a raw-MIME hash. Store the delivery id only
with the successfully committed message so a failed first attempt can be
retried safely. The dedup key is scoped to the account, provider, and canonical
original envelope recipient, so the same delivery id for a different recipient
or account is not collapsed, and replacing a domain's receiving configuration
does not turn a retry into a new delivery. MIME `Message-ID` remains message
metadata rather than the delivery deduplication key.

Resend inbound is webhook-triggered pull: the Svix-signed webhook carries only
metadata, so the adapter verifies the signature, resolves the binding from the
first recipient that maps to a configured domain, then fetches the raw MIME from
`GET /emails/receiving/{email_id}` (Bearer API key) and stages it. The pull stays
inside the adapter; the core still receives a staged `InboundMessage`. The
delivery id is the Resend `email_id`, and event types other than
`email.received` are acknowledged with `200` and ignored via
`transport.ErrInboundIgnored`.

Thread lookup is always scoped to the same `account_id` and `inbox_id`. Standard `Message-ID`, `In-Reply-To`, and `References` headers select the thread only inside that boundary.

An inbox may carry aliases: alternate `local@domain` addresses that resolve to
it. Resolution precedence is exact inbox, then an alias on the recipient's
domain, then the domain catch-all. An alias may live on any domain the account
owns, so it may deliver across domains within the account; the binding check is
account-scoped on the alias route and account-plus-domain-scoped otherwise.
Aliases are also sendable identities: a send or reply may choose the primary or
any alias as its From address, and the outbound provider is resolved from the
chosen address's own domain (`messages.sending_domain_id`, falling back to the
inbox domain). A per-inbox `default_sender` preselects it, and each alias may
carry its own sender display name (falling back to the inbox name). Managed
aliases belong to **domain** inboxes; a standalone inbox has no managed aliases
and sends only as its own connected address.

Unknown recipients resolve to the domain catch-all inbox when configured. Otherwise return `406` and create a minimal audit entry. Missing receiving configuration, unknown domains, and bad authentication return a uniform `401`.

## 5. Persistence

### SQLite

Use WAL mode, foreign keys, and a busy timeout.

Use one serialized writer connection (`SetMaxOpenConns(1)`) plus a small bounded read pool so concurrent ingestion, event writes, and UI/API reads have predictable behaviour.

Keep write transactions short.

Core tables should include:

```text
accounts
users
sessions
domains
inboxes
inbox_aliases
inbox_folders
inbox_remote_credentials
inbox_remote_messages
inbox_remote_labels
inbox_remote_cursors
inbox_remote_arrivals
inbox_remote_notifications
inbox_remote_actions
remote_sent_copies
assistant_handling_requests
messages
message_recipients
threads
thread_messages
attachments
message_tags
api_keys
api_key_inboxes
api_key_mailbox_roles
user_mailbox_roles
invites
events
domain_sending_configs
domain_receiving_configs
outbound_delivery_log
hermes_connections
audit_log
settings
system_settings
pending_file_cleanup
```

`inbox_folders` is the single folder tree for **both** inbox kinds; a message's
folder membership is `messages.mailbox_id` (`NULL` = the implicit system Inbox).
A standalone inbox's `inbox_remote_*` tables hold cached header/thread metadata,
labels, the durable detection cursor and the durable arrival/notification action
state; no message body is ever stored. Encrypted remote credentials live in
`inbox_remote_credentials` under `APP_ENCRYPTION_KEY`.

A domain owns at most one row in `domain_sending_configs` and at most one in
`domain_receiving_configs`, each holding encrypted provider configuration keyed
by a composite `(domain_id, account_id)` foreign key. There is no account-level
credential pool and no assignment column on `domains`.

FTS5 tables index normalized searchable message content.

### Raw MIME

Use generated internal IDs for paths, for example:

```text
/data/messages/ab/cd/msg_01K....eml
```

### Attachments

Store attachment metadata in SQLite.

Read attachment bytes from the canonical MIME blob on demand in V1.

Serve attachment content as a download with `Content-Disposition: attachment` and `X-Content-Type-Options: nosniff`. Any future inline preview uses a separately sanitized rendering path.

## 6. Replayable event history

Mailbox events are persistent SQLite rows:

```text
event_id
account_id
type
entity_id
created_at
payload_summary
```

An in-process hub tracks currently connected SSE/long-poll/Relay consumers.

Sequence:

```text
DB event commit
    ↓
hub publish
    ↓
connected clients
```

Reconnect sequence:

```text
client presents cursor
    ↓
read newer rows from SQLite
    ↓
deliver backlog
    ↓
join live stream
```

## 7. Hermes Relay

Hermes Relay is an integration adapter over the internal event/message model.

Responsibilities:

- enrollment token redemption (`POST /relay/enroll`; codes are minted for the OpenClaw kind only)
- gateway authentication
- WebSocket lifecycle
- capability handshake
- inbound email → Hermes `MessageEvent`
- Hermes response/action → canonical mailbox send/reply operation
- durable/replay behaviour consistent with the Relay contract

Relay-specific protocol structures remain isolated in `/internal/integrations/hermesrelay`.

## 8. Outbound

Expose a small internal abstraction:

```go
type OutboundTransport interface {
    Name() string
    Description() string
    Send(ctx context.Context, cfg map[string]any, msg OutboundMessage) (OutboundResult, error)
}
```

V1 implementations register through the outbound transport registry:

```text
Mailgun HTTP API
Generic SMTP
Brevo HTTP API
```

Each adapter receives decrypted provider-specific configuration and may expose a `ConfigFields()` schema so the Admin UI can render provider-specific inputs instead of raw JSON. Each domain owns at most one optional sending configuration (`domain_sending_configs`); there is no account-level connector pool, no reusable named credential, and no assignment selector, so a domain with no sending configuration queues mail instead of sending through another domain's provider. A queued send resolves the domain's current configuration at worker delivery time; if the domain has none, the message is held without consuming a retry attempt. SES uses the generic SMTP path initially. Send and reply may carry bounded base64-JSON attachments; adapters translate them to Mailgun multipart fields, Brevo attachment objects, or raw SMTP MIME.

Generic SMTP validates resolved destinations as public-routable addresses before connecting and applies bounded connect/read/write timeouts. The same public-routable check guards every HTTP provider client and is available to operators via `ALLOW_PRIVATE_OUTBOUND`; because self-hosting is the primary model the check is **off by default** (private gateways, local relays and LAN receivers are allowed), and a hosted operator confines outbound traffic by setting `ALLOW_PRIVATE_OUTBOUND=false`.

A **standalone** inbox does not send through a managed domain. Its outbound is
its own optional remote SMTP binding, resolved through the generic SMTP adapter
with the same destination policy. When the binding is absent, a send is queued
and held (`ErrNoProvider`) rather than attributed to a domain that does not
exist. After a send commits, a separate durable job copies the message into the
inbox's remote Sent folder (toggle-able and independently retried; it never
re-sends, and it owns an independent frozen copy of the raw MIME so a purge of
the outbound message cannot strand a pending copy). A RemoteDraft handoff is a
distinct, token-free path handled by the outbox worker — see
[MAILBOX_SERVICE_CONTRACT.md](MAILBOX_SERVICE_CONTRACT.md) §3.

Account-wide listings that span the local store and several remote inboxes are
merged into one globally date-sorted stream by
`internal/httpapp/mailbox_merge.go`; the opaque cursor carries each source's own
progress plus the last item's global stable key, so resuming never duplicates or
skips an item. The remote index is built **progressively** (a persisted per-folder
backfill cursor), so an ordinary large folder reaches `complete` over successive
passes; only a folder whose complete UID set exceeds the 500k snapshot ceiling
stays `partial` and un-pruned.

### Optional MX receiving edge

An operator may enable direct-SMTP ingress. The `mailmoose-mx` edge (same
module and image, separate non-root process) terminates SMTP, strictly frames
and stages the original bytes and computes SPF/DKIM/DMARC evidence. The core
connects outward over HTTP/2; private `single` mode uses a bearer key and optional
TLS, while public `shared` mode uses TLS and DNS-backed domain proofs. Both use
the same resolution, streaming and durable acknowledgement machinery.
The receiver holds no policy snapshot, database access
or encryption key. Auth failure becomes a durable Spam delivery rather than an
SMTP rejection. Durable per-recipient receipts (7 days) make sender retries
idempotent, including after the original message is deleted. See
[MX.md](MX.md).

## 9. Authentication

### Human sessions

Use random opaque session tokens and secure HTTP-only SameSite cookies.

Protect cookie-authenticated state-changing requests with CSRF tokens.

Store a hash of the session token in SQLite.

Self-hosted mode creates the installation's **system administrator** from `ADMIN_EMAIL` / `ADMIN_PASSWORD` configuration at startup and defaults account registration to closed. While present those credentials are authoritative and rotate the stored login on restart (an existing user with that email is adopted in place); when absent the stored login is preserved. A system administrator has an own account and an Admin page listing accounts but no automatic access to other accounts' mail. Each account has one Admin; the Account page separates a user's personal settings from account-wide controls (**Account settings**: account name, default time zone, Trash retention) and the account administration section (non-admin mailbox users, Owner on selected inboxes, and the per-account **mailer** mailbox that sends the account's invitations), both restricted to the account Admin. Trash retention is account-wide by default with an optional per-inbox override, and its sweep uses the inbox override when set. An inbox may also carry an optional per-inbox storage cap (set from its Quota tab), enforced transactionally alongside the account quota so one mailbox cannot fill the account. An inbox may additionally carry delivery-triggered auto-actions for its agent/relay connectors — marking a delivered message read and, optionally, moving it to Trash a configurable number of hours after delivery; these run from a per-inbox policy, are off by default, and never apply to API keys or human accounts. People are added by invitation (a separate account with its own Admin, or a mailbox user on an existing account); the invitee sets their own password from a single-use link. Account-level Admin remains separate, and non-admin members carry per-inbox roles in `user_mailbox_roles`. An inbox's settings additionally expose a **Clients & Access** tab (account Admin only) that shows and edits the API keys and mailbox users with access to just that inbox, plus its pending invitations — removing an inbox binding never revokes the underlying key or the user's other mailbox roles.

A **non-admin mailbox API key** can also be exchanged for a browser session from the login page (*Sign in with an API key*). The session is stored as a hashed token in `key_sessions` (a key is not a `users` row, so it cannot live in `sessions`), resolves to the key's own per-inbox roles, and is refused when the key is revoked or rotated. It never carries the account Admin or system-administrator role, so it cannot reach the account Admin dashboard, the `/admin` plane, or the installation-management API; admin keys (which have no mailbox bindings) are rejected outright. The session is capped at 24 hours and protected by the same CSRF and cookie rules as a human session.

### Agent API keys

Generate high-entropy opaque keys with a recognizable prefix.

Store only a secure hash/digest used for verification.

Mailbox permissions are stored per inbox:

```text
Read       → read/search/attachments
Assistant  → Read + delete + create/edit drafts
Owner      → Assistant + send/reply + mailbox settings
```

A key may hold different roles across different inboxes.

Account-level administration is represented separately:

```text
Admin      → full account access, including inbox/domain/key/provider management
```

Authorization evaluates the mailbox role for mailbox operations and the account-level Admin role for account administration.

### Provider configuration

Sending and receiving provider configurations are recoverable secrets. Each
domain owns at most one optional configuration of each kind; the encrypted bytes
belong to that domain alone. The same external API key may still be entered on
more than one domain, producing independent stored copies.

## 10. Web UI

Prefer server-rendered Go templates plus HTMX/vanilla JavaScript, or another comparably small embedded frontend.

Production serves UI assets from the Go binary using `embed`.

## 11. Dependency policy

A runtime dependency should contribute a clear capability such as:

- security-critical HTML sanitization;
- robust protocol handling;
- SQLite driver;
- WebSocket support where standard-library coverage is insufficient.

Keep dependency surfaces narrow and actively maintained.

The optional standalone remote mailbox uses the approved IMAP client
`github.com/emersion/go-imap/v2` and MIME primitives
`github.com/emersion/go-message` (decision `D097`); both are confined to
`internal/transport/imap`. Attribution is in
[THIRD_PARTY_NOTICES.md](../THIRD_PARTY_NOTICES.md).

## 12. Resource behaviour

### Connections

SSE and Relay connections are mostly idle.

Use goroutines/channels with bounded queues.

Apply per-account/global connection limits as operational safeguards.

### MIME

Stream message bodies and downloads.

Use bounded streaming buffers for message content.

### Search

Use FTS5 directly.

### Background activity

Use in-process goroutines for lightweight maintenance such as:

- expired session cleanup
- storage quota cleanup
- delivered-mail auto-trash (per-inbox auto-actions)
- temporary file cleanup
- optional outbound retry processing

The outbox worker also drives workflow mail, webhook retries, RemoteDraft
handoff publication and the remote Sent-copy queue. A dedicated in-process
remote watcher holds one bounded connection per configured standalone inbox
(IDLE, with a polling fallback) and records durable arrivals; a blocking IDLE on
one inbox therefore never starves the others.

Persist any work that must survive restart before execution.

Use small in-process rate limiters for login and outbound-send endpoints. Account and IP limits both run through the same in-process limiter.

## 13. HTTP and TLS boundary

The application listens on plain HTTP inside its deployment network. A reverse proxy such as Nginx Proxy Manager, Caddy, or Traefik terminates public TLS and forwards the original scheme/host using trusted proxy headers. For the Dial MX session listener specifically, the proxy must be able to forward **cleartext HTTP/2** to the upstream — see [MX.md](MX.md) for the per-proxy settings, including Traefik's `loadBalancer.server.scheme=h2c`.

A second HTTP listener defaults to `:8082` and serves only the inbound webhook connector and health checks. It can be disabled or assigned another port using `DEDICATED_RECEIVER_ENABLE` and `DEDICATED_RECEIVER_PORT`. Expose that listener through a reverse proxy with a provider-appropriate policy (TLS, IP allowlist, WAF) and keep the main listener on a private interface or firewall. The receiver can also terminate TLS using `INBOUND_TLS_CERT_FILE` and `INBOUND_TLS_KEY_FILE`.
