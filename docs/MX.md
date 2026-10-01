# Private MX receiving

All direct SMTP deployments use the same receiver binary and HTTP/2 session
transport. The receiver terminates SMTP and computes SPF/DKIM/DMARC evidence;
the MailMoose core connects outward, resolves recipients, applies policy and
durably stores mail. SMTP success is returned only after durable acknowledgement.

## Receiver configuration (Admin → MX receiver)

The receiver a core uses is an **installation setting**, not core environment
configuration. A system administrator chooses it under **Admin → MX receiver**
in the UI (or via `/v1/admin/mx`, a session-authenticated route; see
[API.md](API.md#installation-mx-receiver-system-administrator-session-authenticated)):

- **Included** — the core runs the receiver itself as a separate-uid child,
  generates the bearer key automatically, and handles the port forward and DNS
  records. No per-domain key registration is needed. The full advanced SMTP
  surface is editable here: the greeting hostname, message and staging caps,
  recipient and connection limits, `RequireTLS`, SPF/DKIM/DMARC verification,
  the DNS resolver and DNS/read/write/DATA timeouts, and an optional STARTTLS
  certificate and private key.
- **Remote** — the core connects outward to a receiver in its own container, on
  another host, or on the LAN, using the receiver's URL and a bearer key. This is
  the option for a rootless deployment, where the Included child cannot be
  launched.

The keys are generated and retained by the core; the UI never echoes the bearer
key or the STARTTLS private key (a blank field keeps the stored value). The
STARTTLS certificate is public and is re-displayed; the private key is not, not
even when another field fails validation. Clearing the certificate clears the
stored key. The setting is persisted in the database, not the environment. A
legacy `MX_ENABLE` / `MX_RECEIVER_URL` / `DIALMX_CORE_KEY` environment, and the
legacy `MX_*` SMTP settings including `MX_TLS_CERT`/`MX_TLS_KEY`, are imported
**once** on first start when no setting exists, and ignored thereafter. `auto` is
not implemented yet; it is a deferred placeholder.

After a domain is set to **Receiving → MX**, an operator points the domain's MX
record at the receiver's advertised SMTP hostname and publishes SPF; DKIM and
DMARC are computed at the receiver. No per-domain key id or secret is registered
with the core for a private receiver — the core's receiving configuration is
authoritative.

## Built-in (Included) receiver

Select **Included** under Admin → MX receiver and select **Receiving → MX** for
the domain. The core starts a separate-uid receiver child, generates a bearer
key when one is not configured, and connects over cleartext HTTP/2 on
`127.0.0.1:8443`. No certificates or domain authentication TXT records are
needed. The child receives neither `/data` access nor `APP_ENCRYPTION_KEY`.

The container must start as root to launch the isolated child before the core
drops privileges. In a rootless deployment Included is unavailable and the UI
steers the operator to Remote. `MX_UID` / `MX_GID` default to `65533` and must
differ from the core's runtime identity. For namespace isolation use the sidecar
deployment.

### STARTTLS and the one-time legacy import

The optional SMTP STARTTLS certificate and private key are stored in the
database, not read from files at run time. A pre-existing deployment that used
the legacy `MX_TLS_CERT` / `MX_TLS_KEY` file paths has them read **once**, during
the one-time import, and carried into the persisted pair; after that the paths
are no longer consulted.

A partial or unreadable legacy pair **fails closed**: if only one of
`MX_TLS_CERT` / `MX_TLS_KEY` is set, or a file cannot be read, the one-time
import is abandoned entirely. No receiver settings are written, the core keeps
running unconfigured, and the error is logged; the operator fixes the paths (or
sets the pair in Admin → MX receiver) and restarts. A receiver is never
persisted with its STARTTLS silently dropped. After a successful upgrade,
`/v1/admin/mx` reports `smtp_tls_key_configured: true` when a pair is stored.
`RequireTLS` cannot be enabled without a certificate, so it is rejected at save.

## Standalone / sidecar (Remote) receiver

The receiver container keeps its own environment — the **core** does not take
receiver settings from the environment any more. Receiver environment:

```dotenv
DIALMX_MODE=single
DIALMX_CORE_KEY=<random-secret>
DIALMX_LISTEN_ADDR=:8443
MX_HOSTNAME=mx.example.com
MX_LISTEN_ADDR=:2525
```

Then, under **Admin → MX receiver**, choose **Remote**, enter the receiver URL
(e.g. `http://receiver:8443`) and the **same** `DIALMX_CORE_KEY` value as the
bearer key, and save. The bundle `docker-compose.mx-sidecar.yml` wires this for
you (receiver env only). Generate a key with `openssl rand -hex 32`.

The core sends the key in the session's `Authorization: Bearer` header. The
receiver compares hashed values in constant time and does not log the credential.
It forwards all recipient decisions to one authenticated core; the core only
accepts configured MX recipients. It is not an open relay and unknown recipients
remain rejected.

The newest successfully established core session receives new transactions.
Already-pinned transactions may finish on the old live session. Disconnection
does not restore an older session as the default. An unavailable core results
in temporary SMTP failures so sending MTAs retry.

For the bundled sidecar:

```bash
# Put DIALMX_CORE_KEY in .env first.
docker compose -f docker-compose.mx-sidecar.yml up -d --build
```

The receiver needs inbound SMTP and the session listener; the core needs only
outbound connectivity to it. No MX ingest HTTP endpoint exists on the core.
Its separate `:8082` listener remains for webhook receiving providers.

## TLS and proxies

Single mode supports cleartext HTTP/2 (prior knowledge, no HTTP/1 upgrade) for
loopback/LAN connections. Supplying both `DIALMX_TLS_CERT` and `DIALMX_TLS_KEY`
enables verified HTTPS/HTTP2 instead; enter an `https://` URL under Admin → MX
receiver (Remote). A private CA for that remote receiver is pasted into the
**Private CA certificate** field there; hostname verification remains mandatory
and there is no insecure mode. (`DIALMX_CA_FILE` is a separate core environment
setting used only by the legacy per-domain Dial MX dialer.) Cleartext sessions
carry both the bearer credential and email content without encryption.

SMTP STARTTLS is independent: use `MX_TLS_CERT` / `MX_TLS_KEY`, optionally
`MX_REQUIRE_TLS=true`. Session certificates do not automatically enable SMTP TLS.

Trusted proxies are not required. Forwarded IP headers are ignored; connection
limits use the socket peer. A proxy must support bidirectional streaming and
HTTP/2 to the receiver, including cleartext HTTP/2 if it terminates TLS itself.

## Bounds and policy

The included receiver's SPF/DKIM/DMARC verification is controlled from Admin →
MX receiver (Included) with three checkboxes that default **on**; an unchecked
box is saved as an explicit off. The receiver container's own `MX_VERIFY_SPF`,
`MX_VERIFY_DKIM`, `MX_VERIFY_DMARC`, DNS resolver, message-size, staging,
recipient, connection, transaction and timeout settings remain available and are
documented in [DIALMX.md](DIALMX.md). Domain ownership proof/renewal settings
apply only to shared mode. In single mode the core's receiving configuration is
authoritative; adding or removing a domain needs no receiver-side registration.

Readiness is never claimed prematurely: the installation status reports
`connecting` while a change or the receiver session handshake is still in
progress, and only reports `active` once it completes. A remote receiver that
never completes the handshake is shown as `connecting` (or `failed` with the
reason), never as ready.

Authentication failure under the core's moderate/hard policy is a durable Spam
delivery, not an SMTP rejection. SMTP success requires a durable outcome for
every accepted recipient; quota/transient failures cause sender retries.
`MX_RECEIPT_RETENTION_HOURS` defaults to 168 hours, preserving retry deduplication
even after the original message is deleted. This is not exactly-once delivery.

## Shared receivers (legacy per-domain Dial MX)

Use `DIALMX_MODE=shared` for a public multi-tenant receiver. It retains TLS and
per-domain DNS-backed Ed25519 authentication; the core selects **Dial MX** per
domain. See [DIALMX.md](DIALMX.md).

Per-domain **Dial MX** is retained as **legacy compatibility** and is *not* a
fourth global choice in the installation receiver setting: it predates the
unified receiver and is configured per domain (with its own per-domain key and
TXT record), whereas Admin → MX receiver is a single installation-wide choice of
Included or Remote. New deployments should use the installation receiver.

**Auto** (pick the best receiver automatically) is **not implemented** and is
shown disabled in the UI as a placeholder; the API rejects it.

## Breaking change

The old edge-to-core HTTP/HMAC transport, `/internal/mx/resolve` and
`/internal/mx/ingest` routes, `MX_EDGE_KEYS`, `MX_EDGE_KEY_ID`, `MX_EDGE_SECRET`,
`MX_EDGE_NAME`, `MAILMOOSE_INGEST_URL` and `MX_SIGNATURE_SKEW_SECONDS` are removed.
A private deployment is now configured under Admin → MX receiver (Included or
Remote), not with core environment variables; existing public Dial MX receivers
must explicitly select `DIALMX_MODE=shared`.
