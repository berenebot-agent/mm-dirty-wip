# Optional MX (direct SMTP) ingress

Gatehouse receives mail two ways:

- **Webhook providers** (Mailgun, Cloudflare Email Routing, Resend) — the
  default, app-only deployment. No port 25, no extra process.
- **Direct SMTP (MX)** — an optional policy-free edge binary, `gatehouse-mx`,
  built into the same image, that terminates SMTP on port 25 and calls the core
  over signed HTTPS. Use it when you own the domain and want mail delivered
  straight to Gatehouse without a third-party receiver.

The edge holds **no** `/data` mount, no database access and no
`APP_ENCRYPTION_KEY`. Routing, policy, quota and durable storage stay in the
core. See decision `D031` in [DECISIONS.md](DECISIONS.md).

## Architecture

```text
Internet TCP :25
       |
       v
gatehouse-mx (non-root, in-memory staging, SMTP + SPF/DKIM/DMARC)
       | RCPT: POST /internal/mx/resolve
       | DATA: POST /internal/mx/ingest   (one request per message)
       v
gatehouse-mail inbound connector :8082
       | HMAC-verified edge identity + recipient binding
       | core fans out per accepted recipient: per-domain auth policy + mailbox rules
       v
SQLite + raw MIME under /data -> durable events -> API / UI / Relay
```

The edge stages the original MIME once and sends the whole accepted recipient
set in a single signed ingest; the core parses once and fans out internally, so
neither side holds a full copy per recipient. The edge returns SMTP success only
after the core has durably handled every accepted recipient. Auth failure is a
durable **Spam** delivery, not a rejection. There is no local durable queue:
temporary failures return `451`/`452` and rely on the sending MTA to retry.

## 1. Configure the core

One setting selects the MX deployment:

```env
MX_ENABLE=true   # false (default) | true | remote
#MX_HOSTNAME=mail.example.com   # optional; defaults to gatehouse-mx
```

- `true` embeds the edge in this container (below).
- `remote` enables the core endpoints for an edge running as a separate
  container/host, and requires a shared credential:

  ```env
  MX_EDGE_KEYS=edge-1:<secret>   # or MX_EDGE_SECRET with the sidecar compose
  ```

In `true` (embedded) mode no secret is required: the edge credential is generated
automatically and shared with the core in-process. To use a fixed or rotating
credential there, set `MX_EDGE_KEYS` yourself:

```env
#MX_EDGE_KEYS=edge-1:<old>,edge-2:<new>
```

The core then serves `POST /internal/mx/resolve` and
`POST /internal/mx/ingest` on the inbound listener (`:8082`). Both use
HMAC-SHA256 over the SHA-256 digests of the request metadata and body; the
ingest body is a raw two-part stream (a 4-byte metadata length, the metadata
JSON, then the original MIME), so the message is streamed and hashed, never
buffered. Unsupported versions, stale timestamps, replayed request IDs and
unsigned fields are rejected.

For a **remote** edge, terminate TLS at the core (or a proxy in front of it) so
the edge reaches `:8082` over verified TLS. HMAC is always required even on a
private network.

Optionally serve the inbound listener over TLS directly:

```env
INBOUND_TLS_CERT_FILE=/certs/inbound.crt
INBOUND_TLS_KEY_FILE=/certs/inbound.key
```

## 2. Run the edge

The mode chooses how the edge runs. `true` (embedded) is the default and needs
no second service.

### true (single container, default)

```bash
# .env
MX_ENABLE=true
docker compose up -d --build
```

The app spawns `gatehouse-mx` as a child under a separate uid (`MX_UID`/`MX_GID`,
default 65533) with a scrubbed environment, then drops its own privileges to the
app runtime uid (65532). The edge has no `/data` access and no
`APP_ENCRYPTION_KEY`, and writes nothing to disk (staging is in memory). This is
DAC + separate-uid isolation, not namespaces. Because it must spawn the child
before dropping, the container **must start as root**: a strict compose `user:`
or `cap_drop: [ALL]` disables the uid separation and startup refuses with a
clear error. In that case use `remote`.

The embedded edge is told to stop by closing an inherited pipe (the dropped
parent cannot signal a child owned by a different uid).

### remote — separate edge container/image (`docker-compose.mx-sidecar.yml`)

The edge runs from its own minimal image (`Dockerfile.mx`, tag `gatehouse-mx`)
in its own filesystem and network namespace. This is the strongest isolation and
is recommended when you can run two containers:

```bash
# .env
MX_EDGE_SECRET=<long random secret>
docker compose -f docker-compose.mx-sidecar.yml up -d
```

The sidecar file sets `MX_ENABLE=remote` on the core and runs the `gatehouse-mx`
image for the edge. The edge image runs as a non-root user and holds no `/data`,
so it needs no writable filesystem or privilege. It listens on an unprivileged
internal port; publish host `25:2525`.

### remote — another host

The same `gatehouse-mx` image on another host, pointed at the core's public
HTTPS inbound address. No cert files are needed if a reverse proxy terminates
TLS: the signature covers the method and path only, not the host or scheme.
Forward to the core's `:8082` **without rewriting the path**.

Set `MX_HEALTH_ADDR` to expose `/healthz` (liveness plus counters) and
`/readyz` (readiness, which reflects usable core connectivity) on a separate
port.

## 3. DNS and domain setup

1. Point the domain's **MX** record at the edge hostname (`MX_HOSTNAME`).
2. Publish an **SPF** record for the domain; the edge evaluates it.
3. Add **DKIM** keys at the sending side and a **DMARC** record for the domain.
4. In the Admin UI, set the domain's receiving provider to **MX** and choose an
   enforcement mode.

PTR is operationally useful for outbound deliverability but does **not**
authorize inbound recipients.

## Authentication policy

The edge computes normalized SPF/DKIM/DMARC evidence on the **original bytes**,
before any local trace header is added. Incoming `Authentication-Results`,
`Received-SPF`, `Received` and `Return-Path` headers are never trusted as
substitutes for the actual peer IP, HELO and MAIL FROM.

Per-domain enforcement:

- **moderate** (default): Spam on a definitive DMARC failure, or when SPF and
  DKIM both definitively fail.
- **hard**: Spam on any one of SPF fail, DKIM fail or a definitive DMARC fail.

`none`, `neutral`, `softfail`, `temperror`, `permerror` and missing/unsupported
evidence never count as a failure on their own. A cryptographically valid but
unaligned DKIM signature is a pass with `aligned=false`, not a failure. The
published DMARC policy (`p=`) is preserved and displayed even when the local
disposition differs. A DMARC pass does not clear an independent SPF failure in
hard mode; forwarded mail may therefore be quarantined even when aligned DKIM
passes. Authentication is evidence, not a general spam filter.

The `MX_VERIFY_SPF`, `MX_VERIFY_DKIM` and `MX_VERIFY_DMARC` toggles control
which evidence classes the edge computes. Core policy consumes whatever the
authenticated edge supplies.

## Spam handling

Spam is a computed view over `messages.is_spam` — not a separate table — so
Spam still counts toward quota and retains MIME, attachments, identity and
recovery. Spam is excluded from the inbox, unread counts, default search, normal
threads and default message waits, and is reachable through the inbox **Spam**
tab (or `?spam=true` on the API). Releasing a message commits a durable
`message.spam_state_changed` event and makes it visible again. A Spam message
must be released before it can be used as a reply source.

## Retry identity and deduplication

The delivery fingerprint is a versioned digest over canonical envelope sender,
canonical recipient and the SHA-256 of the original MIME — computed before any
local change. RFC Message-ID is descriptive metadata, never the delivery token.
Core records a durable receipt per recipient for **7 days**, so a sender retry
(including one that arrives after the message was deleted) returns the recorded
disposition without a second message, quota charge or event.

There is no perfect sender-independent SMTP delivery ID: identical intentional
resends inside the window collapse, and changed upstream trace headers can
defeat cross-node deduplication. This is documented rather than claimed as
exactly-once.

## Deferred

Authentication-based SMTP rejection and `on_auth_fail=delete`; ARC and BIMI;
an all-in-one supervisor; a durable edge queue / end-to-end HA; policy
snapshots and local rejection; scoped credential-to-domain binding; reputation
and content filtering; DMARC report generation; authenticated submission/relay.
