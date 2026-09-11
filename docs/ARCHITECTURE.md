# Gatehouse Mail — V1 Architecture

## 1. Runtime topology

```text
                         INTERNET EMAIL
                               │
                            Mailgun
                         SMTP / MX edge
                               │
                         HTTPS webhook
                               ▼
                  ┌────────────────────────┐
                  │   Gatehouse Mail       │
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

Production runtime:

```text
1 binary
1 process
1 container
1 SQLite DB
1 filesystem data root
2 HTTP listeners (main + dedicated inbound webhook listener)
```

The second listener always runs on `:8082`. It serves only the authenticated
inbound webhook routes and `/healthz`, so an operator can expose a dedicated
port to mail providers while keeping the main API/UI listener private. Both
listeners run in the same process and share the same store.

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

  /integrations
      /hermesrelay

  /store
      /sqlite

  /web
  /config
/web
```

Packages should follow capability boundaries rather than generic framework layers.

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
Aliases are inbound only; a reply still sends from the inbox's primary address.

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
messages
message_recipients
threads
thread_messages
attachments
message_tags
api_keys
api_key_inboxes
api_key_mailbox_roles
events
domain_sending_configs
domain_receiving_configs
outbound_delivery_log
hermes_connections
audit_log
settings
```

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

- enrollment token issuance/redemption
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

Generic SMTP validates resolved destinations as public-routable addresses before connecting and applies bounded connect/read/write timeouts. The same public-routable check guards every HTTP provider client and is on by default in all modes; self-hosted operators who intentionally send through a private gateway can opt out with `ALLOW_PRIVATE_OUTBOUND=true`, which hosted mode ignores.

## 9. Authentication

### Human sessions

Use random opaque session tokens and secure HTTP-only SameSite cookies.

Protect cookie-authenticated state-changing requests with CSRF tokens.

Store a hash of the session token in SQLite.

Self-hosted mode bootstraps the first Admin account and defaults account registration to closed. Hosted deployments can enable public registration by configuration.

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

Encrypt with authenticated encryption using `APP_ENCRYPTION_KEY` supplied through environment/config. Treat this key as a root secret and document separate backup/recovery alongside `/data`. A migration copies existing shared credential bytes into per-domain configs without re-keying, so this key must survive the upgrade.

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
- temporary file cleanup
- optional outbound retry processing

Persist any work that must survive restart before execution.

Use small in-process rate limiters for login and outbound-send endpoints. Hosted deployments apply account/IP limits through the same in-process limiter.

## 13. HTTP and TLS boundary

The application listens on plain HTTP inside its deployment network. A reverse proxy such as Nginx Proxy Manager, Caddy, or Traefik terminates public TLS and forwards the original scheme/host using trusted proxy headers.

A second plain-HTTP listener always runs on `:8082` and serves only the inbound webhook connector and `/healthz`. Expose that listener through a reverse proxy with a provider-appropriate policy (TLS, IP allowlist, WAF) and keep the main listener on a private interface or firewall. The application never terminates TLS itself.
