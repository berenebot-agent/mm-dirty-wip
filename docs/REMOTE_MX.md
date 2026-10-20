# Remote SMTP receiving

**Advanced users only. Not required for typical installations.**

This guide covers separate SMTP receivers that you operate yourself. For easy SMTP receiving configuration without running another receiver or opening inbound SMTP ports, use [Antler MX](EMAIL_PROVIDERS.md#inbound-antler-mx).

For the receiver included with MailMoose, see [Direct MX](EMAIL_PROVIDERS.md#inbound-direct-mx).

| Option | When to use it |
| --- | --- |
| **Dial MX** | One public receiver you run for multiple independent domains or installations |
| **Remote MX** | A private receiver dedicated to one MailMoose account |
| **Direct MX with remote receiver** | One receiver for your whole MailMoose installation, managed by the system administrator |

All three need an SMTP hostname reachable on **port 25**. Your MailMoose core opens a connection **outbound** to the receiver; it does not need an incoming SMTP port. The receiver passes mail to the core, which stores it in `/data`.

## Dial MX: shared receiver

Run a public receiver with an HTTPS session endpoint and a valid TLS certificate.

1. Point its SMTP and session hostnames at the receiver server.
2. Set the following environment for `dialmx/compose.yml` and put the certificate files in `dialmx/certs/`:

```dotenv
DIALMX_MODE=shared
MX_HOSTNAME=mx.example.com
DIALMX_TLS_CERT=/certs/fullchain.pem
DIALMX_TLS_KEY=/certs/privkey.pem
```

3. Start the receiver from the repository root:

```bash
docker compose -f dialmx/compose.yml up -d --build
```

4. For each domain, choose **Receiving → Dial MX** in MailMoose, enter the receiver's HTTPS URL, and publish the MX and `_mailmoose-mx.<domain>` TXT records shown.

Each domain, including subdomains, needs its own TXT proof. You can configure multiple receiver URLs for redundancy. Keep public SMTP traffic directly reachable on port 25; a proxy that hides the sending server's IP can interfere with sender checks.

## Remote MX: one receiver for an account

Use `single` mode with a shared key. Generate one with `openssl rand -hex 32` and configure the remote receiver:

```dotenv
DIALMX_MODE=single
DIALMX_CORE_KEY=<generated-secret>
MX_HOSTNAME=mx.example.com
DIALMX_TLS_CERT=/certs/fullchain.pem
DIALMX_TLS_KEY=/certs/privkey.pem
```

Start the receiver with the same Compose command above. Publish its SMTP port 25 and make its session URL reachable from MailMoose. Use a valid TLS certificate if the session crosses a public network; unencrypted HTTP/2 is only suitable on a network you control.

In MailMoose, configure the receiver's session URL and key under the account's **Remote MX** settings. Then choose **Remote MX** for each domain and point its MX records to your receiver's hostname. No per-domain TXT proof is needed in this mode. A receiver URL can belong to only one account.

## Direct MX with a separate receiver

For an installation-wide receiver, configure the same `single` mode and key above. As system administrator, select **Remote** in the installation's **Direct MX** settings, then select **Direct MX** on each receiving domain.

## Check delivery

Check the receiving domain's DNS records and MailMoose receiver status. For Dial MX, both the MX and domain TXT proof must match. For single-mode receivers, confirm the shared key and session URL.

Send a test email. If it does not arrive, check the receiver's container logs, the SMTP port and the domain's **Receiving** panel. Receiver containers do not store mailbox history; back up the MailMoose core's `/data` as described in [Self-hosting](SELFHOSTING.md#data-and-backups).

## Antler MX restart and troubleshooting

[Antler MX](EMAIL_PROVIDERS.md#inbound-antler-mx) is the hosted shared receiver, so there is no receiver container on your host to restart. MailMoose keeps an outbound session to the receivers in the domain's saved setup and reconnects without operator action.

### Reconnecting and authentication

Restarting MailMoose ends the session; the new process redials the same saved receiver set. Reconnection retries indefinitely while MailMoose runs, with jittered exponential backoff capped at 60 seconds, so there is no guaranteed recovery time after an outage — watch the status rather than waiting for a fixed interval.

The domain's **Receiving** panel reports the receiver status:

- **ready** — the receiver accepted this domain's key over a live session.
- **connecting**, **disconnected**, **deferred** or **unavailable** — MailMoose is reconnecting, waiting out a short cooldown, or the receiver ended the binding. No restart is needed.
- **unreachable**, with a DNS or TLS reason — no session could be established. Check outbound HTTPS to the receiver, DNS resolution, and the host clock and certificate chain.
- **rejected** — the receiver answered and refused the domain. The `_mailmoose-mx.<domain>` TXT value must be the key currently shown in the Receiving panel, and the MX records must point at the receiver. A receiver set is chosen from the Antler manifest when a setup is saved, and the domain keeps that snapshot; the manifest cache does not re-point or re-authenticate a live session, so changing it is not a repair step.

### Temporary SMTP failures and sender retries

There is no SMTP queue in MailMoose or Antler to replay a message later. If MailMoose is offline, the receiver cannot hand the message over and the sending server must retry according to its own policy. A 4xx reply is a temporary failure and may be retried; a 5xx reply is permanent for that attempt. If a test message does not arrive, check the sending server's queue and retry state first.

A `250` reply to `RCPT TO` only means the receiver accepted that recipient for the current transaction. It is not proof that the message was stored. Also check the final reply to `DATA`, and then MailMoose's ingestion record: the arrival in the inbox (including Spam) and the domain's **Activity log**, which names the receiver as the source. `docker compose logs mailmoose` shows ingestion errors at the core.
