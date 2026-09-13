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
gatehouse-mx (non-root, bounded staging, SMTP + SPF/DKIM/DMARC)
       | RCPT: POST /internal/mx/resolve
       | DATA: POST /internal/mx/ingest   (one request per accepted recipient)
       v
gatehouse-mail inbound connector :8082
       | HMAC-verified edge identity + recipient binding
       | per-domain auth policy + existing mailbox rules
       v
SQLite + raw MIME under /data -> durable events -> API / UI / Relay
```

The edge returns SMTP success only after the core has durably handled every
accepted recipient. Auth failure is a durable **Spam** delivery, not a rejection.
There is no local durable queue: temporary failures return `451`/`452` and rely
on the sending MTA to retry.

## 1. Configure the core

Generate one or more edge credentials (operator-managed; never tenant values).
Each is a key ID and a long random secret:

```bash
openssl rand -hex 32
```

In the app environment:

```env
MX_RECEIVE_ENABLED=true
MX_EDGE_KEYS=edge-1:<secret>
# Optional: rotate by overlapping keys, then remove the old one.
#MX_EDGE_KEYS=edge-1:<old>,edge-2:<new>
```

The core then serves `POST /internal/mx/resolve` and
`POST /internal/mx/ingest` on the inbound listener (`:8082`). These use
HMAC-SHA256 over a bounded JSON envelope; unsupported versions, stale
timestamps, replayed request IDs and unsigned fields are rejected.

For a **remote** edge, terminate TLS at the core (or a proxy in front of it) so
the edge reaches `:8082` over verified TLS. HMAC is always required even on a
private network.

Optionally serve the inbound listener over TLS directly:

```env
INBOUND_TLS_CERT_FILE=/certs/inbound.crt
INBOUND_TLS_KEY_FILE=/certs/inbound.key
```

## 2. Run the edge

Same-image sidecar (profile-gated):

```bash
docker compose --profile mx up -d
```

The compose service overrides `entrypoint` explicitly. Do **not** use
`command:` alone — the image has a fixed `ENTRYPOINT`, so a command would be
passed as an argument to the application instead of starting the edge.

Give the edge its own environment (never the app `.env`):

```env
GATEHOUSE_INGEST_URL=http://gatehouse-mail:8082
MX_EDGE_KEY_ID=edge-1
MX_EDGE_SECRET=<same secret as MX_EDGE_KEYS>
MX_EDGE_NAME=mx-1
MX_HOSTNAME=mail.example.com
MX_LISTEN_ADDR=:2525
MX_STAGING_DIR=/staging
#MX_TLS_CERT=/certs/mx.crt
#MX_TLS_KEY=/certs/mx.key
#MX_VERIFY_SPF=true
#MX_VERIFY_DKIM=true
#MX_VERIFY_DMARC=true
```

Publish host `25:2525` because the edge binds an unprivileged port internally
and runs as a non-root user with a read-only root filesystem and a dedicated
bounded tmpfs staging area.

A **remote** edge is the same binary and protocol, pointed at the core's public
inbound TLS address.

## 3. DNS and domain setup

1. Point the domain's **MX** record at the edge hostname (`MX_HOSTNAME`).
2. Publish an **SPF** record for the domain; the edge evaluates it.
3. Add **DKIM** keys at the sending side and a **DMARC** record for the domain.
4. In the Admin UI, set the domain's receiving provider to **Gatehouse MX
   (direct SMTP)** and choose an enforcement mode.

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
