# Gatehouse Mail — V1 Product Specification

**Status:** Implementation-ready V1  
**Working description:** Open email infrastructure for AI agents: unlimited logical inbox identities, realtime delivery, BYO outbound sending, free hosted or self-hosted.

## 1. Pitch

Gatehouse Mail is an API-native email inbox platform for autonomous agents.

It gives agents persistent email identities, searchable mail history, attachments, scoped access, realtime delivery, and outbound sending through user-provided credentials.

The same codebase supports:

1. **Hosted mode** — a free multi-tenant service operated by the project.
2. **Self-hosted mode** — a single-container deployment using the operator's transport/provider credentials.

> **Create as many agent email identities as you need, receive mail in realtime, bring your own outbound provider, or self-host the whole platform.**

## 2. Primary use case

A user owns a domain and operates multiple agents, particularly Hermes agents.

They can rapidly create addresses such as:

```text
hermes@example.com
research@example.com
accounts@example.com
supplier-acme@example.com
trip-japan@example.com
task-8291@example.com
```

Inbox identities are lightweight enough to represent roles, suppliers, services, projects, tasks, or temporary workflows.

## 3. Product objectives

### 3.1 Agent-native mailbox model

The canonical model is:

```text
account → domain → inbox → thread → message → attachment/event
```

The application directly owns inbox and message state.

### 3.2 Unlimited logical inbox identities

Inbox creation is a lightweight database operation.

Hosted capacity is governed primarily through account-level storage and fair-use controls.

### 3.3 Realtime delivery

Incoming mail creates a persistent ordered event.

Hermes receives new mail through Hermes Relay.

Generic clients can consume the same event history using REST, long-poll, or SSE.

### 3.4 BYO outbound

Users connect their own outbound provider.

V1 supports:

- Mailgun HTTP API
- Brevo HTTP API
- Resend HTTP API
- generic SMTP, including SES and other SMTP-compatible services

Send and reply accept optional base64-encoded attachments. The application translates them to each provider's native format and stores sent attachment metadata alongside the raw MIME message.

### 3.5 Hosted and self-hosted from one codebase

Hosted and self-hosted deployments share the same mailbox model, API, event semantics, UI, and integrations.

### 3.6 Lean runtime

The V1 runtime is:

```text
one Go binary
one process
one container
one SQLite database
one /data directory
one HTTP port
```

## 4. Key features

### Accounts and domains

An account owns domains, inboxes, messages, API keys, storage allocation, and Hermes connections. Each domain owns at most one optional sending and one optional receiving provider configuration.

Domains support hosted addresses and custom-domain operation.

### Logical inboxes

Create, disable, rename, and manage inbox identities quickly.

Support optional catch-all routing while preserving the original envelope recipient.

Give an inbox aliases: alternate addresses that deliver to it and can be chosen
as the From address when sending. An alias may be on any domain the account
owns, so an inbox can send and receive at several addresses across domains; a
send-as-alias uses the alias domain's own sending configuration. An inbox has an
optional default sender that preselects the From address; when unset, sends use
the inbox's primary address.

### Messages and raw MIME

Persist the original MIME message plus normalized metadata.

### Threads

Group messages deterministically using standard email headers:

- `Message-ID`
- `In-Reply-To`
- `References`

### Search

Use SQLite FTS5 across:

- sender
- recipient
- subject
- body
- attachment filename
- inbox address
- original envelope recipient

### Attachments

Expose attachment metadata and authenticated content retrieval.

### Scoped API keys

Permissions are assigned **per mailbox**, using three simple mailbox roles:

**Read**
- read messages and threads
- search
- download attachments

**Assistant**
- everything in Read
- delete messages
- create and edit drafts
- request authorization to send a draft

**Owner**
- everything in Assistant
- send and reply
- approve or reject draft send requests
- manage settings for that mailbox

A single API key may have different roles on different mailboxes. These roles
apply to API keys; a human user signed in to the web UI is always an Owner.

Example:

```text
Key: Hermes EA

hermes@example.com    Owner
accounts@example.com  Assistant
travel@example.com    Read
```

An additional **Admin** role applies at the account level.

**Admin**
- full account access
- create and delete inboxes
- manage domains
- manage API keys and users
- manage each domain's sending and receiving provider settings
- manage Hermes connections
- manage account-wide settings

Admin provides account administration, while Owner provides full operation of an assigned mailbox.

### Replayable event history

Each account keeps a persistent ordered history of mailbox events in SQLite.

Every event receives a cursor. A connected client remembers the last cursor it processed. After a disconnect or restart, it asks for events after that cursor and catches up before returning to live delivery.

Example:

```text
evt_100  message.received
evt_101  message.received
evt_102  message.sent
```

If a client last processed `evt_100`, it can reconnect and request everything after `evt_100`, receiving `evt_101` and `evt_102` in order.

This gives realtime delivery the reliability of stored mailbox history.

### Hermes Relay

Hermes establishes an outbound authenticated WebSocket to the service.

Incoming email can become a native Hermes message event immediately.

The hosted UI generates a one-time enrollment command for the user.

### Human UI

Provide a lightweight interface for:

- account overview
- domains
- inboxes
- messages
- threads
- search
- attachments
- API keys
- per-domain sending and receiving settings
- Hermes connections
- storage usage

### Agent self-discovery

Expose:

```text
/.well-known/gatehouse
/agent
/v1/bootstrap
/openapi.json
/examples/python
/examples/curl
```

A capable agent should be able to begin with a base URL and API key.

## 5. Hosted V1

The hosted service provides:

- BYO receiving (managed SaaS receiving addresses are deferred)
- effectively unlimited logical inbox identities
- incoming mail
- modest account-level storage
- REST API
- replayable event history
- SSE / long-poll
- Hermes Relay
- scoped API keys
- search
- attachments
- basic human UI
- custom domains as supported by the deployment
- BYO per-domain sending and receiving provider configuration

The initial public service focuses on free access and operational simplicity.

## 6. Self-hosted V1

The same application is published as a prebuilt multi-architecture Docker image.

Target deployment:

```yaml
services:
  gatehouse-mail:
    image: ghcr.io/<org>/gatehouse-mail:latest
    restart: unless-stopped
    volumes:
      - ./data:/data
    environment:
      - APP_ENCRYPTION_KEY=...
      - BASE_URL=https://mail.example.com
```

The operator configures an optional receiving provider per domain (Mailgun, Cloudflare Worker, Resend, or the optional Gatehouse MX direct-SMTP edge) and an optional sending provider (Mailgun, Brevo, Resend, or generic SMTP) in the Admin UI. Each domain's provider configuration is stored encrypted in the database rather than in the process environment, and there is no account-level connector pool or assignment step.

Self-hosted setup uses a first-run Admin bootstrap. Public account registration is configuration-controlled and defaults to closed for self-hosted deployments.

The application serves HTTP behind the operator's reverse proxy, which provides public TLS termination.

## 7. Transport model

### Inbound

V1 supports Mailgun, Cloudflare Email Routing (via a Worker), and Resend as
inbound transports, plus the optional Gatehouse MX direct-SMTP edge (see
[MX.md](MX.md)). Provider webhook mail never carries trusted authentication
evidence; only the authenticated MX edge supplies SPF/DKIM/DMARC evidence, and
its per-domain policy can classify a message as Spam. Spam is a computed view
over `messages.is_spam`, counts toward quota, is excluded from ordinary reads
and message waits, and is recoverable through an explicit Spam view with a
release action and a durable `message.spam_state_changed` event.

```text
Internet SMTP
    ↓
Mailgun, Cloudflare Email Routing, or Resend
    ↓ HTTPS webhook
Gatehouse Mail
```

One catch-all transport route can serve many logical inbox identities. Each
domain owns at most one optional receiving provider configuration, stored as an
encrypted secret on that domain. A domain with no receiving configuration cannot
accept mail. Inbound provider secrets are not read from the environment.

Mailgun delivers raw MIME to `/internal/ingest/mailgun/raw-mime` (the suffix is
protocol-significant) using its signed webhook fields. The Cloudflare Worker
streams raw MIME to `/internal/ingest/cloudflare` with bearer authentication
and envelope headers. Resend posts a Svix-signed metadata webhook to
`/internal/ingest/resend`; the adapter verifies the signature and then fetches
the raw MIME from the Resend API using the account key. In all cases the adapter
authenticates before MIME is parsed or persisted.

Delivery identity is scoped to the account, provider, canonical original
envelope recipient, and provider delivery id, so retries are deduplicated even
when a receiving configuration is replaced. A catch-all preserves the original
recipient.

For an unknown recipient, the domain's configured catch-all inbox receives the message when one is set. Otherwise the ingest endpoint returns a terminal rejection to the transport and records a minimal audit entry.

### Outbound

Each domain can be configured with one of:

- Mailgun API credentials;
- Brevo API credentials;
- Resend API credentials; or
- generic SMTP credentials.

Provider configurations are encrypted at rest. Outbound adapters are registered through a provider registry, so provider-specific code remains behind a narrow transport package. Each adapter declares the fields the Admin UI should collect, so adding a provider only asks for its API key and relevant settings rather than raw JSON.

Each domain owns at most one optional sending configuration; there is no account-level connector pool, reusable named credential, or assignment step. A domain with no sending configuration queues mail until one is configured, and a queued send resolves the domain's current configuration when the worker delivers it.

Domain setup documentation covers the provider DNS records required for receiving and authenticated sending, including MX plus the applicable SPF/DKIM records.

### Transport extension

The mailbox core consumes a normalized inbound-message interface plus an explicit authenticated binding, giving additional transports the same inbox and message semantics.

Potential future adapters include direct SMTP/Maddy, SES inbound, and other webhook providers.

## 8. API direction

The canonical API uses straightforward REST resources for:

```text
/v1/inboxes
/v1/messages
/v1/threads
/v1/search
/v1/events
/v1/send
```

Where openagent.email endpoint semantics map naturally to the data model, preserve compatible paths/fields so integrations can migrate with minimal changes.

Gatehouse Mail extends the model with multi-inbox scopes, replayable event history, cross-inbox search, first-class threads, multi-domain operation, and Hermes Relay.

## 9. Data and backup

Persistent self-hosted state lives under:

```text
/data/
├── inbox.db
├── messages/
└── config/
```

A consistent, quiesced or filesystem-snapshot copy of `/data` is sufficient for ordinary self-host recovery. `APP_ENCRYPTION_KEY` is backed up separately as a root secret.

## 10. V1 success experience

A hosted user can:

1. create an account;
2. create an inbox;
3. receive an email address;
4. create an agent API key;
5. connect Hermes using one enrollment command;
6. send a test email;
7. see Hermes receive it immediately;
8. configure a sending provider on the domain when sending is needed;
9. create additional inbox identities immediately from the same domain configuration.

A self-hosted user can run the same application from the published Docker image and receive the same API and Hermes experience.
