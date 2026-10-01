# Private MX receiving

All direct SMTP deployments use the same receiver binary and HTTP/2 session
transport. The receiver terminates SMTP and computes SPF/DKIM/DMARC evidence;
the MailMoose core connects outward, resolves recipients, applies policy and
durably stores mail. SMTP success is returned only after durable acknowledgement.

## Built-in receiver

Set `MX_ENABLE=true` on the core and select **Receiving → MX** for the domain.
The core starts a separate-uid receiver child, generates a bearer key when one
is not configured, and connects over cleartext HTTP/2 on `127.0.0.1:8443`.
No certificates or domain authentication TXT records are needed. The child
receives neither `/data` access nor `APP_ENCRYPTION_KEY`.

The container must start as root to launch the isolated child before the core
drops privileges. `MX_UID` / `MX_GID` default to `65533` and must differ from
the core's runtime identity. For namespace isolation use the sidecar deployment.

## Standalone / sidecar receiver

Receiver environment:

```dotenv
DIALMX_MODE=single
DIALMX_CORE_KEY=<random-secret>
DIALMX_LISTEN_ADDR=:8443
MX_HOSTNAME=mx.example.com
MX_LISTEN_ADDR=:2525
```

Core environment:

```dotenv
MX_ENABLE=remote
MX_RECEIVER_URL=http://receiver:8443
DIALMX_CORE_KEY=<same-random-secret>
```

Generate a key with `openssl rand -hex 32`. The core sends it in the session's
`Authorization: Bearer` header. The receiver compares hashed values in constant
time and does not log the credential. It forwards all recipient decisions to
one authenticated core; the core only accepts configured MX recipients. It is
not an open relay and unknown recipients remain rejected.

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
enables verified HTTPS/HTTP2 instead; set `MX_RECEIVER_URL=https://...` on the
core. `DIALMX_CA_FILE` adds private CAs while retaining hostname verification.
Cleartext sessions carry both the bearer credential and email content without
encryption.

SMTP STARTTLS is independent: use `MX_TLS_CERT` / `MX_TLS_KEY`, optionally
`MX_REQUIRE_TLS=true`. Session certificates do not automatically enable SMTP TLS.

Trusted proxies are not required. Forwarded IP headers are ignored; connection
limits use the socket peer. A proxy must support bidirectional streaming and
HTTP/2 to the receiver, including cleartext HTTP/2 if it terminates TLS itself.

## Bounds and policy

The existing `MX_VERIFY_SPF|DKIM|DMARC`, DNS resolver, message-size, staging,
recipient, connection, transaction and timeout settings are documented in
[DIALMX.md](DIALMX.md). Domain ownership proof/renewal settings apply only to
shared mode. In single mode the core's receiving configuration is authoritative;
adding or removing a domain needs no receiver-side registration.

Authentication failure under the core's moderate/hard policy is a durable Spam
delivery, not an SMTP rejection. SMTP success requires a durable outcome for
every accepted recipient; quota/transient failures cause sender retries.
`MX_RECEIPT_RETENTION_HOURS` defaults to 168 hours, preserving retry deduplication
even after the original message is deleted. This is not exactly-once delivery.

## Shared receivers

Use `DIALMX_MODE=shared` for a public multi-tenant receiver. It retains TLS and
per-domain DNS-backed Ed25519 authentication; the core selects **Dial MX** per
domain. See [DIALMX.md](DIALMX.md).

## Breaking change

The old edge-to-core HTTP/HMAC transport, `/internal/mx/resolve` and
`/internal/mx/ingest` routes, `MX_EDGE_KEYS`, `MX_EDGE_KEY_ID`, `MX_EDGE_SECRET`,
`MX_EDGE_NAME`, `MAILMOOSE_INGEST_URL` and `MX_SIGNATURE_SKEW_SECONDS` are removed.
Update private deployments to the receiver URL and bearer key above. Existing
public Dial MX receivers must explicitly select `DIALMX_MODE=shared`.
