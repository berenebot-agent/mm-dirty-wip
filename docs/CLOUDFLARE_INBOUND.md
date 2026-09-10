# Cloudflare Email Routing inbound

This guide sets up Cloudflare Email Routing so incoming mail reaches your
Gatehouse Email instance over the generic inbound webhook.

Email Routing cannot POST directly to an arbitrary URL - it delivers each
message to a Cloudflare Worker. Gatehouse Email generates a ready-to-paste
Worker that streams the raw MIME and the envelope metadata to Gatehouse Email.

Flow:

```text
Internet email -> Cloudflare MX -> Email Routing -> Worker -> HTTPS POST
    -> /internal/ingest/cloudflare -> logical inbox
```

## 1. Add the receiving configuration in Gatehouse Email

Inbound provider secrets are no longer environment variables, and there is no
separate credential to create and then assign. Receiving is configured directly
on the domain:

1. On the dashboard, click **Receiving** for the domain you receive on.
2. Choose **Cloudflare Worker** and save the form.
3. Gatehouse generates the shared secret, stores it encrypted on that domain,
   and shows the complete Worker code (the generated
   secret is already embedded) and the Cloudflare steps below in a one-time
   dialog. The generated
   secret is shown only once; use **Regenerate secret** in the domain's
   receiving dialog if you lose it, then paste the new Worker code.
4. Repeat for each domain you receive on. Each domain has its own configuration;
   a normal re-save keeps its existing secret.

The generated Worker uses `BASE_URL`. That hostname must resolve to a
**public** IP: Cloudflare Workers cannot fetch private addresses (`10.x`,
`192.168.x`, `172.16-31.x`, `127.x`) and fail with error `1002`. If the setup
page shows a reachability warning, set `BASE_URL` (or fix the DNS record) to a
public hostname and regenerate.

You can also use the REST API. Omitting the secret lets Gatehouse generate one,
returned once in `generated.webhook_secret`:

```bash
curl -X PUT "$BASE_URL/v1/admin/domains/$DOMAIN_ID/receiving" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"provider":"cloudflare","config":{}}'
```

To supply your own secret instead, include it (it is never returned):

```bash
curl -X PUT "$BASE_URL/v1/admin/domains/$DOMAIN_ID/receiving" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"provider":"cloudflare","config":{"webhook_secret":"generate-a-long-random-secret"}}'
```

## Webhook contract

- Endpoint: `POST /internal/ingest/cloudflare`
- Auth: `Authorization: Bearer <webhook_secret>` (the domain's receiving
  configuration)
- Content-Type: `message/rfc822`
- Headers:
  - `X-Gatehouse-Recipient` (required) - the envelope recipient; selects the
    domain and its receiving configuration. It is a routing hint and grants no
    authority until the bearer matches.
  - `X-Gatehouse-Envelope-From` (optional) - the envelope sender.
  - `X-Gatehouse-Delivery-ID` (optional, bounded) - a stable id used for
    deduplication. When absent, the server derives one from a SHA-256 hash of
    the raw MIME. Reuse the same id when the Worker retries a delivery.
- Body: the raw RFC822 message, streamed to a bounded temp file.

The server verifies the bearer **before** reading the message body. Invalid or
missing authentication is rejected without consuming MIME.

- `recipient` must resolve to an enabled inbox (or the domain catch-all) on the
  authenticated account, otherwise the server returns `406` and drops the
  message.
- A domain with no receiving configuration returns `401` (uniform unauthorized).

The content-hash fallback can collapse separate identical messages to the same
recipient, so provide a delivery id when you can.

The Worker shared secret is a bearer credential between Cloudflare and this
instance. Configuring a receiving provider stores the secret on that domain, but
it does not by itself isolate domains from one another: a secret copied to
several domains (or one migrated from the old shared-credential model) still
authenticates whichever recipient header it carries. Treat the generated secret
as an external credential and regenerate it if it leaks.

## 2. Copy the generated Worker code

The one-time dialog shows the complete Worker code for your instance. It already
contains the ingest URL and the generated shared secret, so you can paste it
as-is:

```js
const WEBHOOK_URL = "https://mail.example.com/internal/ingest/cloudflare";
const SECRET = "<generated-shared-secret>";
```

## 3. Create the Worker (Cloudflare dashboard)

Walk through the Cloudflare navigation:

1. Sign in to <https://dash.cloudflare.com>.
2. Pick the Cloudflare account that owns the domain you want to receive on.
3. In the top navigation, open **Workers & Pages**.
4. Click **Create application** -> **Worker** -> **Deploy**.
5. Give the Worker a name, e.g. `oa-gatehouse`.
6. Click **Deploy** to create a stub Worker, then **Edit code**.
7. Replace the default stub with the generated code from the dialog.
8. Click **Deploy** (top-right) to publish the new code.

## 4. Enable Email Routing for your domain

1. In the same account, go to the **dashboard home**.
2. Select your domain from **Domains** (e.g. `example.com`).
3. In the left sidebar under **Email**, click **Email Routing**.
4. Click **Get started** and confirm the MX records Cloudflare shows you are
   applied at your DNS provider. Cloudflare-managed domains apply these
   automatically.
5. Follow the on-screen verification; Email Routing shows **Active** when
   done.

## 5. Add a routing rule that sends mail to the Worker

1. Still under **Email Routing**, open the **Routing rules** tab.
2. Click **Create rule**.
3. Choose **Send to a Worker**.
4. Give the rule a **Custom email address** that matches an inbox in Gatehouse
   Email (e.g. `hermes@example.com`).
5. For **Action**, select your Worker name (e.g. `oa-gatehouse`) from the
   "Send to a Worker" dropdown.
6. Click **Save**.

Repeat for each inbox address you want to receive on. For a catch-all, add a
rule with the `@` local part (all addresses) and send it to the Worker; the
server routes unknown local parts according to the domain catch-all setting.

## Verification

Send an email to the configured address, then check the message appears in
the inbox via the UI or `GET /v1/messages`. If it does not arrive:

- check the Worker logs (Workers & Pages -> your Worker -> Logs) for the
  forwarded status; the Worker throws on a non-2xx response so failures are
  visible and Cloudflare can retry;
- confirm the Worker secret matches the domain's receiving configuration;
- confirm the domain's **Receiving** provider is configured and the inbox
  exists.

Provider/DNS setup is external and there is no automatic verification: saving a
receiving configuration checks nothing at the provider and does not establish
delivery. Enable Email Routing, apply the MX records, and add the Worker rule
before expecting mail.
