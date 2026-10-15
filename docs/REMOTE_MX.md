# Remote SMTP receiving

This guide covers SMTP receivers that run separately from the MailMoose core.
Choose one when you want to operate the SMTP receiver yourself, or when using
the shared Antler MX relay. For the out-of-the-box receiver bundled with
MailMoose, see [Direct MX in Providers](PROVIDERS.md#inbound-direct-mx).

## Choose a receiving option

| Option | Who runs the receiver? | Setup in MailMoose | Domain proof |
|---|---|---|---|
| **Antler MX** | MailMoose-hosted shared relay | Choose **Antler MX** for the domain | Publish the TXT and MX records shown in the UI |
| **Dial MX** | You run a shared receiver for one or more domains | Choose **Dial MX** for each domain and enter its receiver URL | MailMoose provides a per-domain TXT record; publish it and the shown MX records |
| **Remote MX** | You run one private receiver for one account | Configure the account's receiver once, then choose **Remote MX** for its domains | No TXT proof; the core and receiver share a bearer key |
| **Direct MX — Remote receiver** | You run a private receiver for the whole installation | A system administrator selects **Remote** in the installation's Direct MX settings | No TXT proof; the core and receiver share a bearer key |

Antler MX needs no receiver installation or inbound port on your server. The
other choices require an SMTP hostname reachable by sending mail servers on
port 25. For private single-mode receivers, the core must also be able to reach
the receiver's session URL.

## Antler MX

1. Open the domain's **Receiving** settings and choose **Antler MX (Free SMTP
   Relay - no port forwards required)**.
2. Enter a contact email and save.
3. Publish the exact MX and `_mailmoose-mx.<domain>` TXT records shown in the
   setup panel. The contact email is service-operator metadata; it is not an
   account credential.
4. Wait for the receiver and DNS status indicators to turn ready.

The receiver checks both that the TXT record authorizes the domain and that its
SMTP hostname appears in the domain's MX records. MailMoose displays missing or
incorrect records in the setup panel. Antler MX receives and forwards mail to
your MailMoose instance; it does not host a mailbox or retain delivered mail.

## Dial MX: operate a shared receiver

Use this for a public receiver you operate that can serve multiple domains. The
receiver must have a public SMTP hostname, port 25 open to sending mail servers,
a public HTTPS session URL, and a TLS certificate valid for that URL.
Point the session URL hostname and advertised SMTP hostname at the receiver's
public IP before configuring domains.

From the repository root, prepare a certificate and set the receiver environment
in the shell or a `.env` file used by Compose:

```dotenv
DIALMX_MODE=shared
DIALMX_TLS_CERT=/certs/fullchain.pem
DIALMX_TLS_KEY=/certs/privkey.pem
MX_HOSTNAME=mx.example.com
```

Place the certificate files in `dialmx/certs/`, then start the receiver:

```bash
docker compose -f dialmx/compose.yml up -d --build
```

The Compose file publishes SMTP on host port 25 and the secure session listener
on host port 443. Keep the SMTP listener directly reachable by sending mail
servers; any TLS or network front for the session listener is a hosting-platform
choice. Do not place SMTP behind a proxy that hides the sending server's IP.

For each domain in MailMoose:

1. Open **Receiving** and choose **Dial MX**.
2. Enter the receiver's HTTPS base URL. Multiple receiver URLs may be entered
   when using a redundant set.
3. Save and copy the generated `_mailmoose-mx.<domain>` TXT record and MX
   records from the setup panel.
4. Publish those records in DNS. Each exact domain, including a subdomain, needs
   its own TXT proof. Inherited receiver settings do not share domain keys.
5. Wait for the receiver and DNS status indicators in MailMoose to report ready.

Choose the domain's authentication enforcement setting in its **Receiving**
panel. If a receiver becomes unavailable, senders retry temporary failures;
check the receiver status and DNS records before changing configuration.

## Remote MX: one receiver for an account

Use this for a private, single-account receiver. The receiver runs in
`single` mode and authenticates the MailMoose core with a shared key. It does
not require per-domain TXT records.

Generate a long random key and keep it private:

```bash
openssl rand -hex 32
```

Configure a separate receiver with:

```dotenv
DIALMX_MODE=single
DIALMX_CORE_KEY=<same-random-key>
MX_HOSTNAME=mx.example.com
DIALMX_TLS_CERT=/certs/fullchain.pem
DIALMX_TLS_KEY=/certs/privkey.pem
```

For public session traffic, configure a TLS certificate and key for the session
listener and place the files under `dialmx/certs/` for the included Compose
setup. Plain HTTP/2 may be used only on a private network that you control.
Publish SMTP port 25 to the internet and keep the session listener reachable
from the MailMoose core. A receiver container can be built from this repository
with:

```bash
docker compose -f dialmx/compose.yml up -d --build
```

In MailMoose, an account administrator configures the account's **Remote MX**
receiver with the session URL and the same key, then selects **Remote MX** in
each domain's **Receiving** settings. Point each domain's MX records at the
receiver's SMTP hostname. One receiver URL can belong to only one account, and
it cannot be removed while a domain still uses it.

## Direct MX with a separate receiver

If you are configuring a receiver for the whole installation instead, a system
administrator selects **Remote** in the installation's **Direct MX** settings.
The receiver still uses `single` mode and the same URL/key pairing; domains then
select **Direct MX**.

## Check delivery

- Confirm the domain's MX records point to the receiver's advertised SMTP
  hostname and that inbound port 25 is reachable.
- For Antler MX and shared Dial MX, confirm the `_mailmoose-mx.<domain>` TXT
  record matches what MailMoose displays.
- For a single-mode receiver, confirm its URL and bearer key match the values
  configured in MailMoose. Keep the key private and regenerate it if exposed.
- Check the receiver's container status/logs and the domain's Receiving panel
  for connection or DNS errors.
- Send a test message and confirm it appears in the intended inbox.

The receiver does not replace MailMoose storage: messages are retained by the
core. Back up the core's `/data` and `APP_ENCRYPTION_KEY` as described in the
[self-hosting guide](SELFHOSTING.md#backup).
