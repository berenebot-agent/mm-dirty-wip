# MailMoose — Self-Hosting Guide

Operator guide for self-hosted deployments: first-run configuration, deployment,
hardening, backup, and upgrades. For receiving/sending provider setup see
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

## Inbox lifecycle logs

The application emits `INFO inbox.created` and `INFO inbox.deleted` records to
its existing `[Core]` terminal/container log stream. Watch them with:

```bash
docker compose logs -f mailmoose | grep -E 'inbox\.(created|deleted)'
```

Each record includes the account ID, inbox ID, address, inbox kind, source
(`ui`, `api`, or `internal`), operation, and actor type. User/API-key IDs and
domain IDs are included when applicable. Provider connections identify the
operation as `google_connect`, `microsoft_connect`, or `one_import`.

Deletion includes a reason: `direct_delete`, `domain_delete`, or
`setup_rollback`. Deleting a domain produces one record per deleted inbox.
If connector setup creates an inbox and then removes it, both changes are
logged. Reconnection and idempotent imports do not log a new creation.

Records describe persisted database changes, not completion of filesystem
cleanup. Transactional changes are logged only after commit. The standalone
inbox insert is independently committed before its subsequent connector setup,
so its creation is logged even if that later setup fails. Logs are operational
output, not a durable audit trail: a crash between commit and emission can lose
a record, and retention follows the container/operator log policy.

Lifecycle records do not include credentials, tokens, request bodies, client
IPs, user-agents, or remote connection settings. They do include inbox addresses
and stable identity IDs; restrict access to logs accordingly.

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
reference deployment. Host port 25 is commented out by default. For Included
MX receiving, uncomment `0.0.0.0:25:2525` before running Compose, then select
Included in the domain's Direct MX settings. Selecting Included in the UI cannot
publish a Docker port. Webhook-only deployments need no port-25 mapping.

**Upgrading an existing Included-MX deployment:** retain/uncomment that mapping
before recreating the container with the updated Compose file. Otherwise public
SMTP receiving stops even though the Included receiver remains configured.

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

## Direct MX and remote receivers

The default Compose deployment includes the Direct MX receiver. To receive
public SMTP, publish host port `25:2525`, start the deployment, and select
**Direct MX** in each domain's **Receiving** settings. Opening port 25 alone does
not change a domain's receiving provider.

For Antler MX, shared Dial MX, or a separate single-mode receiver, follow the
operator guide for [Remote SMTP receiving](REMOTE_MX.md). The setup panels show
the DNS records and connection status for each domain.

## Separate webhook hostname

By default, provider webhooks use `BASE_URL`. To receive webhooks on a separate
public hostname, set `DEDICATED_RECEIVER_URL` to that HTTPS origin and route the
webhook listener to the container port configured by `DEDICATED_RECEIVER_PORT`
(default `8082`). In Compose, publish the port with a mapping such as
`8082:8082`; update the provider's webhook URL after changing the hostname.
`BASE_URL` remains the address for the UI and sign-in links. Set
`DEDICATED_RECEIVER_ENABLE=false` to disable the separate listener.

## Uncertain outbound deliveries

If a provider call has started but MailMoose cannot verify its outcome
(for example, a network timeout or a failed database outcome write),
MailMoose leaves the message in the **Outbox** with **Delivery uncertain**.
It does not automatically retry or claim that the recipient definitely
did not receive the message. A restart or an expired in-flight claim
reconciles any such pending messages into that visible failed state.

An operator may delete the message or select **Retry**. Retrying requires
acknowledging that the first attempt could already have delivered a copy.
For API clients, use `POST /v1/outbox/{id}/retry?confirm_duplicate_risk=true`
only after the user has explicitly acknowledged that risk; without the
parameter the endpoint returns `409` for an uncertain send. Ordinary failed
messages remain retryable without this parameter.

An ambiguous approval-notification send is likewise not retried
automatically; the draft send request shows **Notification uncertain**.

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

### This release: standalone mailboxes and external-alias removal

This release adds migrations **052–058** (the standalone mailbox foundation and
the shared folder/remote model; see
[MAILBOX_SERVICE_CONTRACT.md](MAILBOX_SERVICE_CONTRACT.md)) and **removes
external sending aliases** (migration 053). Both are additive/non-destructive
except the deliberate alias removal:

- Standalone mailboxes are new; no existing inbox changes kind. A standalone
  inbox connects to an existing mailbox over IMAP/SMTP and is configured per
  inbox (its connector secrets are encrypted and require `APP_ENCRYPTION_KEY`,
  which is unchanged). Remote message metadata is cached in new tables; no
  message body is archived.
- External sending aliases (send-only identities on domains MailMoose does not
  manage) are deleted with no compatibility shim. Migration 053 discards their
  unsent drafts, attachments and queued sends/jobs and refunds the account and
  inbox storage counters, keeps sent history and its `from_address` attribution,
  removes orphaned threads, and preserves ordinary mail and managed-domain
  aliases. Raw files are retired after the migration commits via a durable
  cleanup queue.

No manual database edit is required. Back up `/data` and `APP_ENCRYPTION_KEY`
before upgrading, as above.
