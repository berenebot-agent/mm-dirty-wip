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

The copy buttons in the interface (secrets, DNS records, webhook URLs) need a secure context: a browser only exposes the clipboard API over HTTPS or on `localhost`. On a plain-HTTP origin the interface hides those buttons rather than offering a copy that cannot work — select the value and copy it by hand, or serve the interface over HTTPS.

| Port | Purpose |
| --- | --- |
| `8081` | Web interface, REST API and agent connections |
| `8082` | Separate listener for incoming provider webhooks and health checks |
| `25:2525` | Optional direct SMTP receiving with the included MX receiver |

### Public versus restricted installations

Decide who may reach the web interface before publishing it.

- **Public installation:** users and agents connect from the internet. Terminate TLS at a reverse proxy, forward only the ports you intend to expose, set `BASE_URL` to the public HTTPS origin, and set `FORCE_HTTPS=true` with `TRUSTED_PROXIES` covering the proxy.
- **Restricted installation:** only a LAN, VPN or the host itself may connect. Keep the port off public interfaces and restrict it in the host firewall.

MailMoose's own protections are authentication, scoped API keys and connector roles, CSRF-protected state changes, login rate limiting, HMAC-signed webhook deliveries (when signature mode is chosen), and the separate `8082` receiver listener. **It has no built-in client-IP allowlist.** Deciding which addresses may connect to the port is an operator control supplied by your firewall, reverse proxy or tunnel.

#### The default port is published on every interface

The repository's Compose files publish the web port as `0.0.0.0:8081:8081`, which binds `8081` on every host interface. A reverse proxy in front of MailMoose does not change that: anyone who can reach the host can bypass the proxy and talk to `8081` directly. A private-only installation must restrict the binding independently:

- Bind loopback when the proxy runs on the same host: `127.0.0.1:8081:8081`.
- Bind one LAN or VPN address: `192.0.2.10:8081:8081` (use your host's address).
- Or remove the `ports:` entry and attach the proxy container to MailMoose's Docker network, so no host port is published.
- Also deny the port from untrusted networks in the host firewall. Publishing `8081` and relying only on the proxy's access rules is not enough.

### Reverse proxies and tunnels

Put the IP allowlist in the proxy: deny by default and allow only the addresses or networks that should reach the interface. Keep that policy there; MailMoose has no equivalent setting to copy.

Tunnels and local proxies (for example Cloudflare Tunnel, Tailscale, or a sidecar proxy) connect to MailMoose from the tunnel client's address — usually loopback or a Docker network address, not the remote user's address. That is the peer MailMoose sees, so allow the tunnel client's address in the host firewall (bind the port to loopback when the tunnel runs on the host) and restrict who may use the tunnel in the tunnel's own configuration. MailMoose neither sees nor allowlists the remote client's address.

### Forwarded headers and `TRUSTED_PROXIES`

Set `TRUSTED_PROXIES` to the exact addresses or CIDRs your proxy connects from — never a wider range than the proxy itself. A catch-all entry such as `0.0.0.0/0` is refused at startup.

When the direct peer is trusted, MailMoose trusts its `X-Forwarded-For` and `X-Forwarded-Proto` headers. It strips trusted proxies from the right of the chain and uses the first untrusted address as the client identity for rate limiting, so a client-supplied leftmost entry cannot choose its own limit key. The proxy must add the peer it accepted to `X-Forwarded-For` and the original scheme to `X-Forwarded-Proto`; a proxy that forwards client-supplied values unchanged hands callers control of both.

`TRUSTED_PROXIES` is not an access control: it does not allow or deny a request, it only makes forwarding information usable. Leave it empty when nothing proxies MailMoose. Behind a tunnel that is not listed there, every client shares the tunnel's address for rate limiting.

### Keep the web interface private

**Port `8082` is useful when you want MailMoose's UI and API to stay on your LAN or VPN, but still need internet-based email providers to deliver messages.** It accepts the inbound provider webhook requests (including Microsoft mail-change notifications) without making the web interface, REST API or agent connections available on that port.

For example:

- Keep `https://mail.example.com` (port `8081`) accessible only through your LAN or VPN.
- Expose `https://inbound.example.com` through your reverse proxy to container port `8082`, allowing providers such as Mailgun, Cloudflare, Resend and Postmark to reach their authenticated webhook endpoints.

Set `DEDICATED_RECEIVER_URL=https://inbound.example.com`. The listener defaults to port `8082`; `DEDICATED_RECEIVER_PORT` changes it and `DEDICATED_RECEIVER_ENABLE=false` disables it.

**This does not automatically secure port `8081`.** Restrict that port with your firewall and reverse proxy, and do not publish it directly to the internet if you want private-only access. The main listener also has inbound webhook routes for compatible setups.

You do **not** need a publicly accessible webhook listener for Antler MX. MailMoose connects outbound to Antler instead. For Direct MX receiving, publish host port `25:2525` and select Direct MX in the relevant domain; publishing a port alone does not enable receiving.

Set `FORCE_HTTPS=true` when using a public HTTPS origin, and include the proxy in `TRUSTED_PROXIES` so its `X-Forwarded-Proto` is believed (see [Forwarded headers and `TRUSTED_PROXIES`](#forwarded-headers-and-trusted_proxies)).

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

Database schema is created from the public baseline on a new installation at
startup. If startup reports a failed or partial schema step, restore from a
consistent backup rather than deleting the database. If you use Direct MX, keep
the host port 25 mapping across upgrades. **The repository's default Compose
file now leaves `25:2525` commented out. Existing Included MX operators must
retain or uncomment that mapping when updating their Compose file; removing it
stops incoming SMTP mail even though the receiver remains enabled in the UI.**

## Uncertain deliveries

When an outbound provider may have accepted an email but MailMoose cannot confirm the result, the message is shown as **Delivery uncertain**. It is not automatically sent again, avoiding duplicate email.

Check the provider or recipient before choosing **Retry**. Retrying an uncertain send requires acknowledging that a copy may already have arrived. The API uses `POST /v1/outbox/{id}/retry?confirm_duplicate_risk=true` for that explicit action.

For diagnostics, use `docker compose logs -f mailmoose`. Inbox lifecycle events are logged there, along with delivery and connection errors.
