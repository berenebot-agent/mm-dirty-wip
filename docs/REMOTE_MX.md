# Remote SMTP receiving

This is for **receivers you operate separately** from MailMoose. For the included Direct MX receiver and the hosted Antler MX option, see [Email providers](EMAIL_PROVIDERS.md).

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
