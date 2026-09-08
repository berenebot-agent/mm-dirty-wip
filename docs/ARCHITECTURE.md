# Open Agent Inbox — V1 Architecture

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
                  │   Open Agent Inbox     │
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
      /smtp

  /integrations
      /hermesrelay

  /store
      /sqlite

  /web
  /config

/migrations
/web
```

Packages should follow capability boundaries rather than generic framework layers.

## 3. Canonical inbound boundary

Transport adapters normalize provider delivery into a core type:

```go
type InboundMessage struct {
    Transport     string
    EnvelopeFrom  string
    EnvelopeTo    []string
    ReceivedAt    time.Time
    RawMIME       io.Reader
    ProviderID    string
}
```

The mailbox core should be transport-neutral beyond this boundary.

## 4. Inbound transaction

```text
receive provider request
       ↓
authenticate provider
       ↓
derive provider delivery key
       ↓
stream raw MIME to controlled temporary path
       ↓
resolve recipient(s)
       ↓
parse required metadata
       ↓
begin SQLite transaction
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

Mailgun delivery idempotency uses the authenticated Mailgun webhook `token` as the provider delivery key. Store the token only with the successfully committed message so a failed first attempt can be retried safely. MIME `Message-ID` remains message metadata rather than the delivery deduplication key.

Thread lookup is always scoped to the same `account_id` and `inbox_id`. Standard `Message-ID`, `In-Reply-To`, and `References` headers select the thread only inside that boundary.

Unknown recipients resolve to the domain catch-all inbox when configured. Otherwise return Mailgun `406` and create a minimal audit entry.

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
aliases
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
outbound_credentials
hermes_connections
audit_log
settings
```

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

Each adapter receives decrypted provider-specific configuration and may expose a `ConfigFields()` schema so the Admin UI can render provider-specific inputs instead of raw JSON. An account stores multiple credentials but designates one active provider (`accounts.active_outbound_credential_id`) used for all sending. SES uses the generic SMTP path initially. Send and reply may carry bounded base64-JSON attachments; adapters translate them to Mailgun multipart fields, Brevo attachment objects, or raw SMTP MIME.

Hosted-mode generic SMTP validates resolved destinations as public-routable addresses before connecting and applies bounded connect/read/write timeouts.

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

### Provider credentials

Outbound credentials are recoverable secrets.

Encrypt with authenticated encryption using `APP_ENCRYPTION_KEY` supplied through environment/config. Treat this key as a root secret and document separate backup/recovery alongside `/data`.

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
