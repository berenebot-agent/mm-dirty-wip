# Dial MX — standalone receiver

A single-process SMTP receiver that terminates mail for domains the core has
registered, verifies each dialer against a DNS-anchored Ed25519 key, and hands
accepted messages to the MailMoose core. See [docs/DIALMX.md](../docs/DIALMX.md)
for the full deployment guide and wire contract.

## What it is

- One binary (`./dialmx/cmd/receiver`), one process, two listeners:
  - **SMTP edge** on `:2525` (map host `:25`), policy-free. It computes
    SPF/DKIM/DMARC evidence on the original bytes.
  - **HTTPS/2 session endpoint** on `:8443` (map host `:443`), where the core
    dials in, proves control of a domain's signing key, and receives mail.
- **Stateless**: no `/data` mount, no database, no core HMAC secret, no
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

Then, in the core's Admin UI, add the receiver for each domain:

1. Set the domain's **Receiving** provider to **Dial MX**.
2. Enter the receiver's HTTPS base URL (for example `https://mx.example.com`).
   Up to eight comma-separated receivers may be listed.
3. Save to generate the domain credential and view the DNS instructions.
4. Publish the shown `_mailmoose-mx.<domain>` TXT and MX record. Subdomains each
   need their own proof even when they inherit receiver settings.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `MX_HOSTNAME` | `localhost` | SMTP greeting hostname; independent of the listener address. |
| `DIALMX_LISTEN_ADDR` | `:8443` | HTTPS/2 session listener. |
| `DIALMX_TLS_CERT` | — | Session listener certificate (required). |
| `DIALMX_TLS_KEY` | — | Session listener private key (required). |
| `MX_TLS_CERT` / `MX_TLS_KEY` | unset | Optional SMTP STARTTLS pair; set together. With the bundled compose they default to the listener pair. |
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
| `MX_AUTH_TIMEOUT_SECONDS` | `10` | Bounds one DNS proof. |
| `MX_RESOLVE_TIMEOUT_SECONDS` | `10` | Bounds one recipient resolve. |
| `MX_INGEST_TIMEOUT_SECONDS` | `180` | Bounds one staged message handoff. |
| `MX_REVALIDATE_SECONDS` | `240` | Nominal binding renewal period. |

See [docs/DIALMX.md](../docs/DIALMX.md) for the session protocol and the DNS
authorisation flow, and [docs/MX.md](../docs/MX.md) for the shared SMTP options.
