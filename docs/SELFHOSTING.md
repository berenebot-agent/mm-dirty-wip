# MailMoose — Self-Hosting Guide

Operational reference for self-hosted deployments: first-run configuration,
reverse proxy setup, direct-SMTP (MX) deployment modes, hardening, backup, and
upgrades. For receiving/sending provider setup see
[docs/PROVIDERS.md](PROVIDERS.md).

## System administrator

The installation has one **system administrator**: the login named by
`ADMIN_EMAIL` with the password in `ADMIN_PASSWORD`. Both values may instead be
read from files with `ADMIN_EMAIL_FILE` / `ADMIN_PASSWORD_FILE` (intended for
Docker secrets); set only one form per setting, and set both the email and the
password together. `ADMIN_ACCOUNT_NAME` optionally sets the display name of the
system administrator's own account (default `MailMoose`).

The configured credentials are **authoritative while present**. On first start
they create the system administrator; on every later start they rotate its
stored login to match, so changing the secret changes the login (existing
sessions are signed out). If `ADMIN_EMAIL` already belongs to an existing user,
that user is **adopted in place** — it keeps its account and is forced to
account Admin and system Admin — so pointing the secret at an existing login
does not fail startup. When **both** settings are absent the stored login is
left untouched, so a deployment may drop them once provisioned. A
half-configured pair, or an `*_FILE` path that cannot be read, is a startup
error. If no system administrator exists and no credentials are supplied,
MailMoose still starts and serves a page explaining that it is not configured;
there is no unauthenticated setup form.

The system administrator can manage the installation and use their own account
normally, but does **not** automatically get access to other accounts' mail.
Because the deployment secret owns this login, the account settings page does
not let the system administrator change its email or password; update the
secret and restart instead.

From the **Admin** page the system administrator sees the list of accounts and
invites a new person as a **separate account Admin** (each account has one
Admin). The invitation is emailed from the **mailer** selected on the system
administrator's own Account page, or its one-time link can be copied.

Each **account Admin** manages their account's **mailbox operators** (non-admin
users who are Owner of selected inboxes) and the account's own **mailer** from
the **Account** page. An operator signs in with their own login and only sees
the mailboxes assigned to them.

## Password recovery

If an ordinary account Admin loses access, reset the password from the server:

```bash
docker compose exec mailmoose mailmoose admin reset-password admin@example.com
```

The command prompts for the new password without echoing it (or reads it from
`--password-file` for automation), applies the normal password rules, and
revokes every existing browser session. API keys are left untouched; revoke
them separately with `mailmoose admin revoke-api-keys admin@example.com`.

The system administrator's login is owned by `ADMIN_EMAIL` / `ADMIN_PASSWORD`,
so `reset-password` refuses it and tells you to update the deployment secret
and restart instead.

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
DIALMX_CORE_KEY=<long random secret>   # generate: openssl rand -hex 32
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
| `MX_RECEIPT_RETENTION_HOURS` | `168` (7 days) | How long a delivery receipt deduplicates a sender retry, surviving message deletion. |
| `MX_RECEIVER_URL` | built-in loopback | Receiver HTTP/HTTPS origin; required for `remote`. |
| `DIALMX_CORE_KEY` | auto-generated (embedded) | Bearer key shared with the private receiver; required for `remote`. |
| `INBOUND_TLS_CERT_FILE` / `INBOUND_TLS_KEY_FILE` | empty | Optional TLS directly on the core's `:8082`; set both or neither. Usually unnecessary when a reverse proxy terminates TLS. |

Edge service (`mailmoose-mx`):

| Variable | Default | Meaning |
|---|---|---|
| `DIALMX_MODE` | `single` | Private single-core bearer mode; `shared` uses DNS-backed domain authentication. |
| `DIALMX_LISTEN_ADDR` | `:8443` | Session listener; built-in binds `127.0.0.1:8443`. |
| `DIALMX_CORE_KEY` | required in single | Matching core bearer key. |
| `DIALMX_TLS_CERT` / `DIALMX_TLS_KEY` | empty | Optional session TLS in single mode; required in shared mode. |
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

A remote receiver is the same binary with `DIALMX_MODE=single` and a shared
bearer key. The core connects outward using `MX_RECEIVER_URL`; it no longer
exposes MX ingest endpoints. Session TLS is optional for LAN use and verified
when the URL is HTTPS. A reverse proxy must support bidirectional HTTP/2
streaming to the receiver. See [docs/MX.md](MX.md).

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
