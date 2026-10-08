# Private MX receiving

All direct SMTP deployments use the same receiver binary and HTTP/2 session
transport. The receiver terminates SMTP and computes SPF/DKIM/DMARC evidence;
the MailMoose core connects outward, resolves recipients, applies policy and
durably stores mail. SMTP success is returned only after durable acknowledgement.

## Receiver configuration (Domain → Receiving → Direct MX)

The receiver a core uses is an **installation setting**, not core environment
configuration. A system administrator configures the **Included** receiver under
**Domain → Receiving → Direct MX**. Remote receiver
configuration remains available via `/v1/admin/mx`, a session-authenticated route (see
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
  the API option for a rootless deployment, where the Included child cannot be
  launched. The Direct MX editor offers Included only.

The keys are generated and retained by the core; the UI never echoes the bearer
key or the STARTTLS private key (a blank field keeps the stored value). The
STARTTLS certificate is public and is re-displayed; the private key is not, not
even when another field fails validation. Clearing the certificate clears the
stored key. The setting is persisted in the database, not the environment. A
legacy `MX_ENABLE` / `MX_RECEIVER_URL` / `DIALMX_CORE_KEY` environment, and the
legacy `MX_*` SMTP settings including `MX_TLS_CERT`/`MX_TLS_KEY`, are imported
**once** on first start when no setting exists, and ignored thereafter. `auto` is
not implemented yet; it is a deferred placeholder.

After a domain is set to **Receiving → Direct MX**, an account admin points the
domain's MX record at the receiver's advertised SMTP hostname and publishes SPF;
DKIM and DMARC are computed at the receiver. **Direct MX** is offered in the
per-domain receiving dialog to account admins only, and shows the installation
receiver's mode, live state and advertised SMTP hostname inline. No per-domain key
id or secret is registered with the core for a private receiver — the core's
receiving configuration is authoritative.

System administrators see the Included receiver settings inline in that dialog,
with advanced controls collapsed. Account admins see status only. One **Save**
action configures the shared receiver and selects Direct MX for the current domain.
The Included receiver runs only while at least one domain across the installation
uses Direct MX. Removing the last receiving configuration, switching its provider,
or deleting its domain drains the receiver and leaves it in quiet standby; saved
settings are retained. Selecting Direct MX again restarts it automatically.
The domain dialog shows built-in configuration and the MX destination, rather
than an internal core-to-receiver connection badge. The installation API retains
the live operational status for diagnostics.
The SMTP greeting field is prefilled from the hostname of `DEDICATED_RECEIVER_URL`
when supplied, otherwise `BASE_URL`, with scheme, port and path removed. A stored
greeting takes precedence; the operator can edit the field before saving.

## Built-in (Included) receiver

Select **Included** in the domain's Direct MX receiver settings and save **Receiving → Direct MX**
for the domain. The core starts a separate-uid receiver child, generates a bearer
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
sets the pair in the domain's Direct MX receiver settings) and restarts. A receiver is never
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

Then, via `/v1/admin/mx`, save mode `remote`, the receiver URL
(e.g. `http://receiver:8443`) and the **same** `DIALMX_CORE_KEY` value as the
bearer key, and save. The pull and compose examples are in the README.
Generate a key with `openssl rand -hex 32`.

The core sends the key in the session's `Authorization: Bearer` header. The
receiver compares hashed values in constant time and does not log the credential.
It forwards all recipient decisions to one authenticated core; the core only
accepts configured MX recipients. It is not an open relay and unknown recipients
remain rejected.

The newest successfully established core session receives new transactions.
Already-pinned transactions may finish on the old live session. Disconnection
does not restore an older session as the default. An unavailable core results
in temporary SMTP failures so sending MTAs retry.

For the bundled sidecar, see README.md for the pull and compose examples.

The receiver needs inbound SMTP and the session listener; the core needs only
outbound connectivity to it. No MX ingest HTTP endpoint exists on the core.
Its separate listener remains for webhook receiving providers (enabled by default
on `:8082`, configurable with `DEDICATED_RECEIVER_ENABLE` and `DEDICATED_RECEIVER_PORT`).

## TLS and proxies

Single mode supports cleartext HTTP/2 (prior knowledge, no HTTP/1 upgrade) for
loopback/LAN connections. Supplying both `DIALMX_TLS_CERT` and `DIALMX_TLS_KEY`
enables verified HTTPS/HTTP2 instead; save an `https://` URL via `/v1/admin/mx`
(Remote). Supply a private CA certificate in the API's `ca` field when needed;
hostname verification remains mandatory
and there is no insecure mode. (`DIALMX_CA_FILE` is a separate core environment
setting used only by the legacy per-domain Dial MX dialer.) Cleartext sessions
carry both the bearer credential and email content without encryption.

Shared mode normally requires a session certificate. To front it with a
TLS-terminating reverse proxy, set `DIALMX_TRUSTED_PROXIES` to the proxy address
and omit the pair: the proxy terminates TLS, forwards cleartext HTTP/2 to the
session listener, and the receiver admits cleartext only from that allowlist
(or loopback). Nothing else changes — the core still dials the proxy's `https`
origin with hostname verification, and the per-domain DNS proof is still the
authority. The proxy must support cleartext HTTP/2 to the upstream; nginx
`proxy_pass` does not, but Nginx Proxy Manager **Streams** with SSL, Caddy
(`transport http { versions h2c }`) and HAProxy do. Leave SMTP direct: a proxy
on `:25` would hide the sender's IP from SPF.

SMTP STARTTLS is independent: use `MX_TLS_CERT` / `MX_TLS_KEY`, optionally
`MX_REQUIRE_TLS=true`. Session certificates do not automatically enable SMTP TLS.

Trusted proxies are not required. Forwarded IP headers are ignored; connection
limits use the socket peer, so a session proxy collapses every core onto one
per-source bucket (`MX_PER_IP_*`). A proxy must support bidirectional streaming
and HTTP/2 to the receiver, including cleartext HTTP/2 if it terminates TLS
itself.

## Bounds and policy

The included receiver's SPF/DKIM/DMARC verification is controlled from the domain's
Direct MX receiver settings (Included → Verification) with three checkboxes that default **on**; an unchecked
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

## Shared receivers (per-domain Dial MX and Antler MX)

Use `DIALMX_MODE=shared` for a public multi-tenant receiver. It retains TLS and
per-domain DNS-backed Ed25519 authentication; the core selects **Antler MX (Free
SMTP Relay - no port forwards required)** for a zero-config hosted relay, or
Dial MX with a **custom** service for operator-run receivers. See
[DIALMX.md](DIALMX.md).

An Antler MX setup stores a per-domain **contact email** and a generated
**setup id** and sends them as optional registration metadata on `DomainAuth`.
They are operational metadata for the service operator's usage accounting, never
credentials: domain authority remains the DNS-anchored Ed25519 proof. The
receiver logs them with the `dialmx domain auth` summary and per-recipient
message records. The
hosted receiver set is resolved from a versioned manifest (embedded and fetched
live at setup-save time) and snapshotted per domain, so capacity changes reach
new setups without a core release and existing MX records stay stable.

Per-domain **Dial MX** is retained as **legacy compatibility** and is *not* a
fourth global choice in the installation receiver setting: it predates the
unified receiver and is configured per domain (with its own per-domain key and
TXT record), whereas the Direct MX receiver editor is a single installation-wide choice of
Included or Remote. New deployments should use the installation receiver or
Antler MX.

## Remote MX (account-owned receiver)

**Remote MX** is the third direct-SMTP receiving provider, alongside Direct MX
(installation) and Antler MX (shared DNS relay). It lets an **account admin** run
their own standalone Dial MX receiver and point any of the account's domains at
it, with the same "configure once, select per domain" UX as the installation
Direct MX receiver — but scoped to the account instead of the instance.

It is Dial MX in **single mode only**: the core dials the account's receiver
outbound over HTTPS/2 (or cleartext h2c for a private/LAN receiver) and
authenticates with a shared bearer key, exactly like the installation **Remote**
receiver. There is no DNS proof and no per-domain key; the receiver authorizes
any domain on its one authenticated connection, and the core keeps the
per-recipient account/domain authorization. (DNS-authenticated shared receivers
remain Antler MX / per-domain Dial MX custom; Remote MX does not do DNS auth.)

- **One receiver per account.** A system administrator's installation Direct MX
  receiver is installation-wide; an account's Remote MX receiver is configured
  once per account under the domain **Receiving → Remote MX** panel (or the
  account API, `GET/PUT/DELETE /v1/admin/account/mx`, account-admin session or
  admin bearer key). Any domain in the account then selects **Remote MX** under
  Receiving → Provider, and receives through that receiver.
- **One owner per receiver.** Because a single-mode receiver serves exactly one
  core, one physical receiver (by URL) may be registered by exactly one account;
  a second account registering the same URL is rejected. A wrong bearer key
  simply never authenticates: the receiver's status shows connecting/failed and
  only the correct-key core becomes its live session.
- **Private/LAN receivers.** By default a Remote MX receiver URL must be an
  HTTPS public-routable origin. The account admin can tick **Allow a private /
  LAN receiver** to permit an `http` origin on a loopback or RFC1918 address (for
  example a receiver on the account's own LAN), mirroring the installation
  Remote receiver's private-destination support. A private CA bundle can be
  supplied for a self-signed receiver certificate.
- **Fail-closed clear.** Removing the account receiver is refused while one or
  more domains still route to it, so a clear never silently breaks receiving.

The core runs one outbound session per account receiver that is in use by at
least one domain, and stops the session when the last domain stops using it.
Nothing listens on the core: Remote MX opens no inbound port.

**Auto** (pick the best receiver automatically) in the installation receiver
setting is **not implemented** and is not offered in the UI;
the API rejects it. Antler MX is the implemented zero-config service, offered
per domain rather than as an installation mode.## Breaking change

The old edge-to-core HTTP/HMAC transport, `/internal/mx/resolve` and
`/internal/mx/ingest` routes, `MX_EDGE_KEYS`, `MX_EDGE_KEY_ID`, `MX_EDGE_SECRET`,
`MX_EDGE_NAME`, `MAILMOOSE_INGEST_URL` and `MX_SIGNATURE_SKEW_SECONDS` are removed.
A private deployment is now configured under Domain → Receiving → Direct MX
(Included), or via `/v1/admin/mx` (Included or Remote), not with core environment
variables; existing public Dial MX receivers
must explicitly select `DIALMX_MODE=shared`.
