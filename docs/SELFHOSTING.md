# MailMoose — Self-Hosting Guide

Operational reference for self-hosted deployments: first-run configuration,
reverse proxy setup, direct-SMTP (MX) deployment modes, hardening, backup, and
upgrades. For receiving/sending provider setup see
[docs/PROVIDERS.md](PROVIDERS.md).

## First-run administrator

On the first start against an empty database, MailMoose creates the account
named by `INITIAL_ADMIN_EMAIL` with the password in `INITIAL_ADMIN_PASSWORD`
and logs `Initial administrator created: <email>`. Both values may instead be
read from files with `INITIAL_ADMIN_EMAIL_FILE` / `INITIAL_ADMIN_PASSWORD_FILE`
(intended for Docker secrets); set only one form per setting, and set both the
email and the password together. `INITIAL_ACCOUNT_NAME` optionally sets the
account display name (default `MailMoose`).

These values are **bootstrap-only**: as soon as any user exists they are
ignored, so they can be left in place or removed from Compose. They never
update an existing account, change an email, or create another administrator.
If they are not supplied, MailMoose still starts and serves a page explaining
that it is not configured; there is no unauthenticated setup form.

## Password recovery

If the administrator loses access, reset the password from the server:

```bash
docker compose exec mailmoose mailmoose admin reset-password admin@example.com
```

The command prompts for the new password without echoing it (or reads it from
`--password-file` for automation), applies the normal password rules, and
revokes every existing browser session. API keys are left untouched; revoke
them separately with `mailmoose admin revoke-api-keys admin@example.com`.

## Reverse proxy and TLS

Set `TRUSTED_PROXIES` to your proxy's address so its `X-Forwarded-*` headers
are believed (the proxy should overwrite `X-Forwarded-For`, not append to a
client-supplied value), and set `FORCE_HTTPS=true` to redirect any plaintext
request to `https://<BASE_URL host>` and advertise HTTPS in discovery
documents. `/health`, `/healthz` and the `/internal/*` webhook endpoints are
never redirected.

The application never terminates TLS itself. Put a reverse proxy (Nginx Proxy
Manager, Caddy, Traefik, …) in front and forward the original scheme/host.

## Deployment topologies

### Build-from-source example (repo default)

`docker-compose.yml` in the repository root builds the image locally and is the
reference deployment. It enables the embedded MX edge by default and publishes
host port 25. If you receive mail only through a webhook provider (Mailgun,
Cloudflare Email Routing, Resend), set `MX_ENABLE=false` in `.env`; port 25
then has no listener.

### Minimal pull-and-run

The README quickstart shows the minimal compose file: one service running
`ghcr.io/dellarb/mailmoose:latest`, all persistent state in a `./data` bind
mount, no uid/gid configuration — the image drops privileges itself.

### Hardened stack

For a hardened stack (read-only root filesystem, a /tmp tmpfs, dropped
capabilities), use `docker-compose.advanced.yml`:

```bash
docker compose -f docker-compose.advanced.yml up -d
```

## Direct SMTP (MX) deployment

### true (one container, Compose default)

```bash
# .env — MX_ENABLE=true is already the docker-compose.yml default
#MX_HOSTNAME=mail.example.com   # optional; defaults to mailmoose-mx
```

```bash
docker compose up -d --build
```

Then, in the Admin UI, open the domain and set **Receiving → MX**. Point the
domain's MX record at `MX_HOSTNAME` and publish SPF. The domain is only an MX
receiver once you set this in the UI; a domain left on a webhook provider is
unaffected.

`true` (embedded) requires the container to start as root (it must spawn the
edge under a different uid before dropping). A strict compose `user:` or
`cap_drop: [ALL]` disables that; startup then refuses with a clear error — use
`MX_ENABLE=remote` instead.
This is DAC + separate-uid isolation, not namespaces.

### remote (separate edge container/image, strongest isolation)

The edge runs from its own minimal image (`Dockerfile.mx`) in its own
filesystem and network namespace. Recommended when you can run two containers:

```bash
# .env
MX_EDGE_SECRET=<long random secret>   # generate: openssl rand -hex 32
docker compose -f docker-compose.mx-sidecar.yml up -d
```

The sidecar file sets `MX_ENABLE=remote` on the core and runs the
`mailmoose-mx` image for the edge. The edge does not boot as root and needs no
writable filesystem.

That is the whole required setup. Everything below is optional tuning.

### Advanced options (all optional — defaults shown)

You do not need to set any of these to run MX. They exist to tune limits or
enable TLS/resolver overrides.

Core service (`mailmoose`):

| Variable | Default | Meaning |
|---|---|---|
| `MX_UID` / `MX_GID` | `65533` | Uid/gid the embedded edge runs as (must differ from the app's). |
| `MX_SIGNATURE_SKEW_SECONDS` | `600` | How old a signed edge request may be (replay window bound). |
| `MX_RECEIPT_RETENTION_HOURS` | `168` (7 days) | How long a delivery receipt deduplicates a sender retry, surviving message deletion. |
| `MX_EDGE_KEYS` | auto-generated (embedded) | Override the edge credential (`key_id:secret`, comma-separated for rotation). Required for `remote`. Every secret must carry 32 bytes / 256 bits of entropy; startup refuses weaker values. |
| `INBOUND_TLS_CERT_FILE` / `INBOUND_TLS_KEY_FILE` | empty | Optional TLS directly on the core's `:8082`; set both or neither. Usually unnecessary when a reverse proxy terminates TLS. |

Edge service (`mailmoose-mx`):

| Variable | Default | Meaning |
|---|---|---|
| `MAILMOOSE_INGEST_URL` | `http://127.0.0.1:8082` | Core URL (a public HTTPS proxy for a remote edge). |
| `MX_EDGE_NAME` | `mx-1` | Edge name shown in logs and signed metadata. |
| `MX_LISTEN_ADDR` | `:2525` | SMTP listener (unprivileged internally; publish host `25`). |
| `MX_TLS_CERT` / `MX_TLS_KEY` | empty | Optional STARTTLS; set both or neither. |
| `MX_VERIFY_SPF` / `MX_VERIFY_DKIM` / `MX_VERIFY_DMARC` | `true` | Which evidence classes the edge computes. |
| `MX_MAX_MESSAGE_BYTES` | `31457280` | Largest message the edge accepts. Advertised to senders as the ESMTP `SIZE` value at `EHLO` and echoed in the oversize `552` rejection. |
| `MX_STAGING_BYTES` | `268435456` (256 MiB) | Total in-memory staging across concurrent transactions; a burst above it returns a temporary failure. |
| `MX_MAX_RECIPIENTS` / `MX_MAX_CONNECTIONS` | `100` / `256` | Recipients per transaction and concurrent connections. |
| `MX_READ_TIMEOUT_SECONDS` / `MX_WRITE_TIMEOUT_SECONDS` / `MX_DATA_TIMEOUT_SECONDS` | `60` / `60` / `300` | Command, write and DATA read timeouts. |
| `MX_DNS_RESOLVER` / `MX_DNS_TIMEOUT_SECONDS` | system / `10` | Optional resolver `host:port` for SPF/DKIM/DMARC. |
| `MX_HEALTH_ADDR` | empty | Optional listener for `/healthz` and `/readyz`. |

Per-domain settings — enforcement mode (moderate/hard), catch-all — live in the
**Admin UI**, not in either environment. The edge is policy-free: at `RCPT` it
asks the core, and the core decides from the domain's receiving configuration.

Full details, the wire contract, authentication policy, Spam handling and retry
semantics are in [docs/MX.md](MX.md).

### Remote edge

A remote edge is the same binary pointed at a public HTTPS endpoint. You do
**not** need cert files if a reverse proxy terminates TLS: the signature covers
the method and path only, not the host or scheme. Point `MAILMOOSE_INGEST_URL`
at the proxy (for example `https://inbound.example.com`) and forward to the
core's `:8082` **without rewriting the path**. Use `MX_TLS_CERT`/`MX_TLS_KEY`
only when the edge speaks directly to the core with no proxy. See
[docs/MX.md](MX.md).

## Backup

Back up a filesystem-consistent snapshot of `./data` and separately retain
`APP_ENCRYPTION_KEY`. Both are required to restore encrypted provider
configurations.

## Upgrading

The current release replaces account-level, named provider connectors with one
optional sending and one optional receiving configuration owned by each domain.
It is a single-install breaking change: the old connector APIs
(`/v1/admin/outbound*`, `/v1/admin/inbound*`), the domain credential-assignment
fields, and the standalone connector UI are removed.

Migration 013 runs automatically the first time the new binary opens the
database. It:

- copies each domain's assigned connector into a private per-domain config,
  preserving the encrypted bytes (a connector shared by several domains becomes
  independent copies, and no re-key is needed because `APP_ENCRYPTION_KEY` is
  unchanged);
- drops connectors not assigned to any domain;
- preserves mailbox, auth, message, storage, and account data;
- preserves delivery-log history, attributing each attempt to a domain where it
  can be derived from the attempt's message and inbox (attempts that cannot be
  attributed keep an unattributed entry instead of being dropped).

There is no full database wipe. Back up `/data` and `APP_ENCRYPTION_KEY` before
running the new binary. If startup reports a partially applied schema, restore
from backup and retry rather than deleting the database.