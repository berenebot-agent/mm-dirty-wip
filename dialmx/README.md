# Dial MX — standalone receiver

A single-process SMTP receiver with two modes: `single` (default) authenticates
one core with `DIALMX_CORE_KEY`, with optional TLS and no domain registration;
`shared` verifies each domain against a DNS-anchored Ed25519 key and requires TLS
unless `DIALMX_TRUSTED_PROXIES` names the TLS-terminating reverse proxy that
fronts it. Both hand
accepted messages to the MailMoose core. See [docs/DIALMX.md](../docs/DIALMX.md)
for the full deployment guide and wire contract.

## What it is

- One binary (`./dialmx/cmd/receiver`), one process, two listeners:
  - **SMTP edge** on `:2525` (map host `:25`), policy-free. It computes
    SPF/DKIM/DMARC evidence on the original bytes.
  - **HTTP/2 session endpoint** on `:8443`, where the core dials in and receives
    mail after bearer authentication (single) or domain key proof (shared).
- **Stateless**: no `/data` mount, no database, no
  `APP_ENCRYPTION_KEY`. Messages stage in memory and are streamed to the core.
- **Non-root**: the image runs as uid/gid `65532` and needs no capabilities.

The core remains the durable source of truth. The receiver never stores mail:
it returns SMTP success only after the core has durably handled every accepted
recipient, and a partial or lost acknowledgement is returned as `451` so the
sending MTA retries.

## Quick start

```bash
mkdir -p dialmx/certs
cp /path/to/fullchain.pem /path/to/privkey.pem dialmx/certs/
docker compose -f dialmx/compose.yml up -d --build
```

For private mode, set `DIALMX_CORE_KEY` on the receiver and core, and set
`MX_ENABLE=remote` and `MX_RECEIVER_URL=http://receiver:8443` on the core. Select
**Receiving → MX** for its domains. See [docs/MX.md](../docs/MX.md).

For shared mode, set `DIALMX_MODE=shared` and the session certificates. To run
it behind a TLS-terminating reverse proxy instead, set
`DIALMX_TRUSTED_PROXIES` to the proxy's address and omit the session
certificates: the proxy terminates TLS and forwards cleartext HTTP/2 to the
session listener, which admits cleartext only from the allowlist (or loopback).
Then,
in the core's Admin UI, add the receiver for each domain:

1. Set the domain's **Receiving** provider to **Dial MX**.
2. Enter the receiver's HTTPS base URL (for example `https://mx.example.com`).
   Up to eight comma-separated receivers may be listed.
3. Save to generate the domain credential and view the DNS instructions.
4. Publish the shown `_mailmoose-mx.<domain>` TXT and MX record. Subdomains each
   need their own proof even when they inherit receiver settings.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `DIALMX_MODE` | `single` | Single bearer-authenticated core or shared DNS-authenticated domains. |
| `DIALMX_CORE_KEY` | — | Required in single mode; unused in shared mode. |
| `MX_HOSTNAME` | `localhost` | SMTP greeting hostname; independent of the listener address. |
| `DIALMX_LISTEN_ADDR` | `:8443` | HTTPS/2 session listener. |
| `DIALMX_TLS_CERT` | — | Optional single-mode certificate; required in shared mode unless `DIALMX_TRUSTED_PROXIES` is set. |
| `DIALMX_TLS_KEY` | — | Session private key; set together with certificate. |
| `DIALMX_TRUSTED_PROXIES` | unset | Comma-separated IPs/CIDRs that may open a cleartext session in shared mode (the TLS-terminating proxy). Empty keeps shared mode TLS-only. |
| `MX_TLS_CERT` / `MX_TLS_KEY` | unset | Optional SMTP STARTTLS pair, independent of the session pair; set together. |
| `MX_REQUIRE_TLS` | `false` | Refuse plaintext SMTP. |
| `MX_VERIFY_SPF` / `MX_VERIFY_DKIM` / `MX_VERIFY_DMARC` | `true` | Which evidence classes the edge computes. |
| `MX_DNS_RESOLVER` | system | Resolver for SPF/DKIM/DMARC and the TXT proof. |
| `MX_MAX_MESSAGE_BYTES` | `31457280` | Per-message cap. |
| `MX_STAGING_BYTES` | `268435456` | In-memory staging budget across concurrent transactions. |
| `MX_MAX_RECIPIENTS` | `100` | Recipients per transaction. |
| `MX_MAX_CONNECTIONS` | `256` | Concurrent SMTP connections. |
| `MX_MAX_TRANSACTIONS` | `128` | Global receiver-to-core transaction groups. |
| `MX_MAX_DOMAINS_PER_CONNECTION` | `128` | Distinct domains admitted on one session. |
| `MX_MAX_TRANSACTIONS_PER_CONNECTION` | `16` | Concurrent transaction groups per session. |
| `MX_MAX_TRANSACTIONS_PER_DOMAIN` | `8` | Concurrent transactions per domain. |
| `MX_PER_IP_CONN_LIMIT` | `16` | Concurrent sessions per source IP. Behind a trusted proxy every core shares the proxy's IP. |
| `MX_PER_IP_CONN_WINDOW_MAX` | `128` | Sessions one source IP may open per minute. |
| `MX_PER_IP_AUTH_CONCURRENT` | `16` | Concurrent authentication jobs per source IP. |
| `MX_PER_IP_AUTH_WINDOW_MAX` | `256` | Authentication jobs one source IP may start per minute. |
| `MX_AUTH_TIMEOUT_SECONDS` | `10` | Bounds one DNS proof. |
| `MX_RESOLVE_TIMEOUT_SECONDS` | `10` | Bounds one recipient resolve. |
| `MX_INGEST_TIMEOUT_SECONDS` | `180` | Bounds one staged message handoff. |
| `MX_REVALIDATE_SECONDS` | `240` | Nominal binding renewal period. |

See [docs/DIALMX.md](../docs/DIALMX.md) for the session protocol and the DNS
authorisation flow, and [docs/MX.md](../docs/MX.md) for the shared SMTP options.

## Metadata logs

The receiver emits compact `[MX]`-tagged plain text to the container stream, the
same format the core uses for its `[Core]` lines: SMTP and core connection
lifecycle, domain DNS proofs and renewals, recipient routing, message
authentication, per-core handoffs and per-recipient acknowledgements.
Correlation IDs link each message to its destination domain and core session.
Process lifecycle, mail receipt, mail transfer and failures are INFO; the
per-connection and per-session transport chatter is DEBUG, enabled with
`DIALMX_LOG_LEVEL=debug`. The bundled Compose uses Docker's `local` driver with
`20m` × 10 rotated files. Logs survive restarts but are removed when the
container is removed/recreated; they are not stored in `/data`. See the logging
contract in [docs/DIALMX.md](../docs/DIALMX.md#metadata-logging).
