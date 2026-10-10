# MailMoose — Receiving and Sending Providers

Receiving and sending are configured **per domain**. Each domain owns at most
one optional receiving configuration and one optional sending configuration;
there is no account-level connector pool and no assignment step, so mail can
only enter or leave through the provider configured on its domain.

A subdomain can instead **inherit** its parent domain's receiving and/or sending
configuration. When a domain is added that is a subdomain of an existing domain,
MailMoose records the parent and, by default, reuses the parent's connectors, so
one connector (for example one Cloudflare Worker and its shared secret) can
serve every onboarded subdomain of a zone. The subdomain still owns its own
inboxes, aliases and catch-all, and can be given its own configuration instead.
See `docs/DECISIONS.md` D063 and `docs/CLOUDFLARE_INBOUND.md`.

If a subdomain is added **before** its parent domain, it is created as a root
domain with no link. Adding the parent afterwards does not re-link it
automatically; instead the subdomain's **Receiving** / **Sending** provider menu
offers **Inherited (from <parent>)**, naming the nearest existing ancestor.
Selecting it links the domain and inherits that direction. The Admin API can do
the same by sending `parent_domain_id` on `PATCH /v1/admin/domains/{id}`; sending
an empty `parent_domain_id` unlinks the domain and clears both inherit switches.
Unlinking never touches the domain's own connectors.

Both directions are configured in the Admin UI (domain page → **Receiving** /
**Sending**) or through the Admin API. Provider secrets are stored encrypted
using `APP_ENCRYPTION_KEY`.

---

## Inbound: Mailgun

1. Add and verify the receiving domain in Mailgun, including the MX records Mailgun provides.
2. In the Admin UI, open the domain's page (**Dashboard → Settings → the domain**) and configure a **Receiving** provider of type **Mailgun**, entering the account's webhook signing key. The secret is stored encrypted on that domain; it is not read from the environment.
3. Create a Mailgun catch-all route for the domain that forwards incoming mail to:

```text
https://your-host.example/internal/ingest/mailgun/raw-mime
```

MailMoose resolves the recipient to its logical inbox. A configured domain catch-all handles unmatched local parts. The `raw-mime` suffix is protocol-significant: it selects raw MIME delivery. The legacy `/internal/ingest/mailgun` alias has been removed.

## Inbound: Cloudflare Email Routing

Inbound can be received via Cloudflare Email Routing through a Worker that
streams the raw MIME to the generic webhook. Full dashboard navigation is in
[docs/CLOUDFLARE_INBOUND.md](CLOUDFLARE_INBOUND.md).

High-level steps:

1. On the dashboard, click **Receiving** for the domain and configure a **Receiving** provider of type **Cloudflare Worker**. MailMoose generates the shared secret, stores it encrypted on that domain, and shows the complete Worker code and Cloudflare steps in a one-time dialog. The generated secret is shown once; use **Regenerate secret** in the same dialog if you lose it (this replaces it, and the old Worker stops working until you paste the new code).
2. In Cloudflare, create a Worker (**Workers & Pages** -> **Create application** -> **Start with Hello World** -> **Deploy**), then paste the generated code into **Edit code** (it already contains your ingest URL and secret) and deploy it.
3. In Cloudflare, onboard **Email Routing** for your domain, adding the DNS records Cloudflare lists.
4. Under Email Routing -> **Routing rules**, edit the **catch-all** rule to **Send to a Worker**, select your Worker as the action, and enable it.

Cloudflare hands each message to the Worker, which streams the raw MIME to:

```text
https://your-host.example/internal/ingest/cloudflare
```

The server resolves the recipient to its logical inbox with the same behavior as Mailgun.

## Inbound: Resend

Inbound can also be received via Resend. Resend posts a signed metadata webhook; MailMoose verifies it and then fetches the raw MIME from the Resend API. Full setup is in [docs/RESEND.md](RESEND.md).

The webhook URL to register in Resend is:

```text
https://your-host.example/internal/ingest/resend
```

1. In Resend, verify the domain (including the inbound MX record) and create a **full access** API key. A send-only key cannot read received mail.
2. On the dashboard, click **Receiving** for that domain and choose **Resend**. The form shows the exact webhook URL before you save. Create the Resend webhook for that URL subscribed to **`email.received`**, copy its signing secret (`whsec_...`), then enter it with the API key and save.
3. Each domain stores its own receiving configuration; if several domains share one Resend webhook, enter the same signing secret on each domain.

Resend also works as a sending provider (see below).

## Inbound: SendGrid

Inbound can be received via Twilio SendGrid Inbound Parse. SendGrid POSTs the raw MIME in a signed `multipart/form-data` payload. Full setup is in [docs/SENDGRID.md](SENDGRID.md).

The webhook URL to register in SendGrid is:

```text
https://your-host.example/internal/ingest/sendgrid
```

1. Point the receiving domain's MX record at `mx.sendgrid.net` (priority 10).
2. In SendGrid, open **Settings → Inbound Parse** and add a host: set the receiving domain and paste the webhook URL above, and tick **POST the raw, full MIME message**.
3. Create a webhook **security policy** with signature verification, attach it to the parse setting, and copy the returned **public key**.
4. On the dashboard, click **Receiving** for that domain, choose **SendGrid**, paste the public key, and save.

Unlike Mailgun and Cloudflare, SendGrid's ECDSA signature covers the raw body (including the SMTP envelope), so its envelope sender is **provider-attested**.

## Inbound: Postmark

Inbound can be received via Postmark. Postmark POSTs the message as JSON (including the raw MIME) to a webhook protected by HTTP Basic authentication. Full setup is in [docs/POSTMARK.md](POSTMARK.md).

The webhook URL to register in Postmark is:

```text
https://<generated-user>:<generated-password>@your-host.example/internal/ingest/postmark
```

1. Point the receiving domain's MX record at `inbound.postmarkapp.com` (priority 10).
2. In Postmark, enable **Include raw email content in JSON payload** on the Server's Inbound Message Stream.
3. On the dashboard, click **Receiving** for that domain and choose **Postmark**. MailMoose generates the Basic-auth username and password and shows the ready-to-paste webhook URL once; copy it into the stream's webhook field.

Postmark does not sign inbound webhooks, so the `From` envelope sender is **not provider-attested** (see [SECURITY.md](../SECURITY.md)).

## Inbound: Direct MX

Instead of a webhook provider you can receive mail straight on port 25 with the
optional MX edge. The installation receiver is configured by a system
administrator under **Domain → Receiving → Direct MX**; the legacy `MX_ENABLE` environment
variable selects the mode only on first import. The server binary defaults to
`false` when the variable is unset; the shipped `docker-compose.yml` sets it to
`true`.

- **`true`** (default `docker-compose.yml`) — receive on port 25 with the edge
  embedded in the app container as a separate, unprivileged uid. The edge
  credential is generated automatically.
- **`false`** (server default) — no MX; receive via a webhook provider only. Set
  this in `.env` for a webhook-only deployment.
- **`remote`** — receive on port 25 with the edge in its own container/image
  (`mailmoose-mx`, see README.md) or on another host. Needs
  `MX_RECEIVER_URL` and a shared `DIALMX_CORE_KEY`.

The edge holds no `/data` access and no `APP_ENCRYPTION_KEY`, and stages
messages in memory only.

Deployment modes, tuning, and the wire contract are covered in
[docs/SELFHOSTING.md](SELFHOSTING.md) and [docs/MX.md](MX.md).

## Inbound: Dial MX

For a public shared receiver, receive direct SMTP with **Dial MX**. The core
**dials out** to a standalone receiver the operator runs: the receiver
terminates SMTP, verifies the core's right to speak for the domain with a
DNS-anchored Ed25519 challenge, and hands accepted mail to the core over
verified HTTPS/2. Set `DIALMX_MODE=shared` on the receiver. No shared bearer key
or private MX mode is needed.

A domain's receiving provider is either **Direct MX** (private bearer session
backed by the installation receiver) or **Dial MX** (core dials receiver), never
both. **Direct MX** is offered per domain in the receiving dialog to account
admins only, and shows the installation receiver's live status and advertised
SMTP hostname; it has no per-domain key. Configure **Dial MX** per domain under
**Receiving → Dial MX**: enter one or more HTTPS receiver base URLs and publish
the shown `_mailmoose-mx.<domain>` TXT record and the domain's MX record. The
signing key is generated per exact domain and stored encrypted. Subdomains can
inherit receiver URLs and enforcement but always have their own key and TXT
proof. The receiver proves both the TXT authority and that it is named in the
domain's MX before it reports ready, so each receiver shows one status light and
any missing record is fixed inline in the dialog. Other domains may continue
using local/remote MX or webhook providers.

Full setup, the session protocol, and known limitations are in
[docs/DIALMX.md](DIALMX.md).

## Inbound: Remote MX

**Remote MX** is the account-owned direct-SMTP receiver: an account admin runs
their own standalone Dial MX receiver (in `DIALMX_MODE=single`, authenticated by
a shared bearer key) and points any of the account's domains at it. It is the
per-account counterpart of the installation **Direct MX** receiver, with the
same "configure once, select per domain" flow.

1. In the domain's **Receiving → Remote MX** panel, an account admin enters the
   receiver's HTTPS (or `http`, if the private/LAN option is ticked) base URL and
   the receiver's `DIALMX_CORE_KEY`. This is stored once for the account.
2. Any domain in the account then selects **Receiving → Remote MX**.
3. Point each domain's MX record at the receiver's advertised SMTP hostname.

One physical single-mode receiver belongs to exactly one account; registering
the same receiver URL under a second account is rejected. No DNS `_mailmoose-mx`
record is needed — the receiver authenticates the core with its bearer key.
Removing the account receiver is refused while a domain still routes to it. The
same operations are available through the account API:

```http
GET    /v1/admin/account/mx
PUT    /v1/admin/account/mx
DELETE /v1/admin/account/mx
```

Full receiver deployment, the single-mode session contract, and limitations are
in [docs/DIALMX.md](DIALMX.md); the receiving-provider overview is in
[docs/MX.md](MX.md).

---

## Outbound

Sending is configured per domain. Open a domain's page, choose a **Sending**
provider, and fill in the fields it asks for (for example Brevo only needs an
API key). Each domain owns its own configuration. A domain with no sending
provider queues mail until one is configured, and removing a provider pauses
sending for that domain only.

The same operations are available through the Admin API, scoped to a domain:

```http
GET    /v1/admin/domains/{id}/sending
PUT    /v1/admin/domains/{id}/sending
DELETE /v1/admin/domains/{id}/sending
```

`PUT` accepts `{"provider":"...","config":{...}}`, and
`GET /v1/admin/domains/{id}/sending/deliveries` lists the domain's send
attempts newest first. The legacy `POST /v1/admin/outbound` endpoint, the
account-level connector model, and the `/ui/outbound*` admin pages have been
removed.

### Provider configurations

Mailgun:

```json
{"api_key":"key-...","domain":"mg.example.com"}
```

Brevo:

```json
{"api_key":"xkeysib-..."}
```

Brevo requires the inbox sender address to be a verified sender in Brevo.

Resend:

```json
{"api_key":"re_..."}
```

The `from` domain must be a verified sending domain in Resend.

Generic SMTP:

```json
{"host":"smtp.example.com","port":587,"username":"user","password":"secret","security":"starttls"}
```

Direct MX:

```json
{"helo":"mail.example.com"}
```

Direct MX resolves the recipient domain's MX records and connects directly to
port 25 without credentials. It accepts one unique envelope recipient per
message; send separate messages when delivering to multiple domains or
recipients. The configured HELO hostname should have matching forward and PTR
DNS where possible. Operators must provide port 25 egress, publish SPF for the
sending IP, and arrange DKIM signing separately if aligned DKIM is required.
STARTTLS is used opportunistically when the receiving server advertises it.
Direct MX requires the operator to control the network identity and reputation
needed for direct Internet delivery.

### Attachments

Send, reply and draft write requests may include base64-encoded attachments:

```json
{"filename":"quote.pdf","content_type":"application/pdf","content":"<base64>"}
```

Use the object above in an `attachments` array. The application translates
attachments to each provider's native format and stores sent attachment
metadata with the raw MIME message. Draft writes also accept an `action` of
`draft`, `request-send` or `send`, so a draft can be created, attached and
submitted for approval in one request.

---

## Dedicated inbound listener

The server has a main listener and an optional dedicated webhook listener:

- `LISTEN_ADDR` (default `:8081`) serves the API, web UI, Relay, and inbound webhooks.
- The dedicated listener defaults to `:8082` and serves **only** inbound webhook
  routes (`/internal/ingest/mailgun/raw-mime`, `/internal/ingest/{provider}`)
  and health checks. Set `DEDICATED_RECEIVER_PORT` to change the port or
  `DEDICATED_RECEIVER_ENABLE=false` to disable it. SMTP/MX is configured separately.

To keep the API and UI off the public internet, expose only `:8082` to your
reverse proxy and keep `LISTEN_ADDR` bound to a private interface or blocked by
the firewall. Set `DEDICATED_RECEIVER_URL=https://inbound.example.com` to generate
provider webhook URLs using that origin. Omitted or blank uses `BASE_URL`, even
when enabled. Keep `BASE_URL` set to the UI origin for passkeys. Example URLs:

```text
https://inbound.example.com/internal/ingest/mailgun/raw-mime
https://inbound.example.com/internal/ingest/cloudflare
https://inbound.example.com/internal/ingest/resend
https://inbound.example.com/internal/ingest/sendgrid
https://inbound.example.com/internal/ingest/postmark
```

The ingest routes remain available on the main listener for backward
compatibility. Usually terminate TLS at the reverse proxy; the dedicated listener
can also serve TLS directly with `INBOUND_TLS_CERT_FILE` and `INBOUND_TLS_KEY_FILE`.
