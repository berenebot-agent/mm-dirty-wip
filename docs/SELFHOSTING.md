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

MailMoose serves the web UI and API on port `8081`. Put an HTTPS reverse proxy in front of it when accessed over a network, and set `BASE_URL` to the address users will open.

| Port | Purpose |
| --- | --- |
| `8081` | Web interface, REST API and agent connections |
| `8082` | Separate listener for incoming provider webhooks and health checks |
| `25:2525` | Optional direct SMTP receiving with the included MX receiver |

### Keep the web interface private

**Port `8082` is useful when you want MailMoose's UI and API to stay on your LAN or VPN, but still need internet-based email providers to deliver messages.** It accepts the inbound provider webhook requests (including Microsoft mail-change notifications) without making the web interface, REST API or agent connections available on that port.

For example:

- Keep `https://mail.example.com` (port `8081`) accessible only through your LAN or VPN.
- Expose `https://inbound.example.com` through your reverse proxy to container port `8082`, allowing providers such as Mailgun, Cloudflare, Resend and Postmark to reach their authenticated webhook endpoints.

Set `DEDICATED_RECEIVER_URL=https://inbound.example.com`. The listener defaults to port `8082`; `DEDICATED_RECEIVER_PORT` changes it and `DEDICATED_RECEIVER_ENABLE=false` disables it.

**This does not automatically secure port `8081`.** Restrict that port with your firewall and reverse proxy, and do not publish it directly to the internet if you want private-only access. The main listener also has inbound webhook routes for compatible setups.

You do **not** need a publicly accessible webhook listener for Antler MX. MailMoose connects outbound to Antler instead. For Direct MX receiving, publish host port `25:2525` and select Direct MX in the relevant domain; publishing a port alone does not enable receiving.

Set `TRUSTED_PROXIES` to the reverse proxy's IP or CIDR if you need forwarded client information. The proxy must overwrite incoming `X-Forwarded-For`. Set `FORCE_HTTPS=true` when using a public HTTPS origin.

See [Email providers](EMAIL_PROVIDERS.md) or [Remote SMTP receiving](REMOTE_MX.md).

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
