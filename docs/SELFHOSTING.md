# Self-hosting

Use the [README quickstart](../README.md#quickstart) to run MailMoose with one Docker Compose file. This guide covers the operational settings that are not part of the initial setup.

## Administrator account

Set `ADMIN_EMAIL` and `ADMIN_PASSWORD` together in your Compose environment. They create the system administrator at startup and remain authoritative while configured. Changing them and restarting updates the login and signs out existing sessions.

Alternatively, leave both unset on a new installation and complete the one-time browser setup at `/setup`. Once an account exists, that page is no longer available. Do not expose an unclaimed installation publicly.

The system administrator manages installation settings and other accounts but does not automatically have access to their mail. Account administrators manage users and inbox access within their own account.

For Docker secrets, use `ADMIN_EMAIL_FILE` and `ADMIN_PASSWORD_FILE` instead. Never configure only one of the pair.

To reset an ordinary account user's password:

```bash
docker compose exec mailmoose mailmoose admin reset-password admin@example.com
```

The system administrator's password is managed through the Compose settings, not this command.

## HTTPS and receiving ports

MailMoose serves HTTP on port `8081`. Put an HTTPS reverse proxy in front of it for external access and set `BASE_URL` to the public HTTPS origin.

Set `TRUSTED_PROXIES` to your reverse proxy's IP or CIDR if you want MailMoose to honor forwarded client information. The proxy must overwrite incoming `X-Forwarded-For` values. `FORCE_HTTPS=true` enables redirection to the configured HTTPS origin.

Only publish additional ports when needed:

| Port | Purpose |
| --- | --- |
| `8081` | Web interface and API, preferably behind the HTTPS proxy |
| `8082` | Optional separate receiving listener for provider webhooks |
| `25:2525` | Direct SMTP receiving with the included MX receiver |

Select the receiver in the application's settings as well. Publishing port 25 alone does not configure a domain to accept mail. Antler MX and most connected-mailbox setups do not need it.

For a separate webhook hostname, set `DEDICATED_RECEIVER_URL` to that HTTPS origin, and configure your reverse proxy to reach the receiving listener. Its port is set by `DEDICATED_RECEIVER_PORT` (default `8082`); disable the separate listener with `DEDICATED_RECEIVER_ENABLE=false` when not needed.

See [Email providers](EMAIL_EMAIL_PROVIDERS.md) for provider setup or [Remote SMTP receiving](REMOTE_MX.md) for dedicated receivers.

## Data and backups

Everything persists in `./data`. Back up that directory with a consistent filesystem snapshot or while the container is stopped.

If `APP_ENCRYPTION_KEY` is **not** supplied, MailMoose generates `./data/encryption_key`. It is included in your data backup. If you provide the key through Compose or a secret, **back it up separately**. Keep the original key: replacing it makes encrypted credentials unreadable.

For a more restricted container configuration, the repository provides `docker-compose.advanced.yml` with a read-only root filesystem and other hardening settings. The regular single-container setup is the simplest starting point.

## Updates and recovery

Before upgrading, back up `./data` and your external encryption key if used. Update the image and recreate the container:

```bash
docker compose pull
docker compose up -d
docker compose logs --tail 100 mailmoose
```

Database migrations run at startup. If startup reports a failed or partial migration, restore from a consistent backup rather than deleting the database. If you use Direct MX, keep the host port 25 mapping across upgrades.

## Uncertain deliveries

When an outbound provider may have accepted an email but MailMoose cannot confirm the result, the message is shown as **Delivery uncertain**. It is not automatically sent again, avoiding duplicate email.

Check the provider or recipient before choosing **Retry**. Retrying an uncertain send requires acknowledging that a copy may already have arrived. The API uses `POST /v1/outbox/{id}/retry?confirm_duplicate_risk=true` for that explicit action.

For diagnostics, use `docker compose logs -f mailmoose`. Inbox lifecycle events are logged there, along with delivery and connection errors.
