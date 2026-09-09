# Cloudflare Email Routing inbound

This guide sets up Cloudflare Email Routing so incoming mail reaches your
Gatehouse Email instance over the generic inbound webhook.

Email Routing cannot POST directly to an arbitrary URL - it delivers each
message to a Cloudflare Worker. The Worker below streams the raw MIME and the
envelope metadata to Gatehouse Email.

Flow:

```text
Internet email -> Cloudflare MX -> Email Routing -> Worker -> HTTPS POST
    -> /internal/ingest/cloudflare -> logical inbox
```

## 1. Add the receive path in Gatehouse Email

Inbound provider secrets are no longer environment variables. Configure them
per domain:

1. Open the Admin **Dashboard** and edit the domain you receive on.
2. Under **Receive path**, choose **Add new receive path…**, pick
   **Cloudflare Worker**, and enter a long random **Worker shared secret**.
3. Save. Gatehouse creates the encrypted credential and assigns it to the
   domain. Repeat for each domain you receive on.

You can also use the REST API:

```bash
curl -X POST "$BASE_URL/v1/admin/inbound" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"provider":"cloudflare","config":{"webhook_secret":"generate-a-long-random-secret"}}'
```

Then assign it to the domain:

```bash
curl -X PATCH "$BASE_URL/v1/admin/domains/$DOMAIN_ID" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"inbound_credential_id":"<id from the create response>"}'
```

## Webhook contract

- Endpoint: `POST /internal/ingest/cloudflare`
- Auth: `Authorization: Bearer <webhook_secret>` (the domain's receive-path
  credential)
- Content-Type: `message/rfc822`
- Headers:
  - `X-Gatehouse-Recipient` (required) - the envelope recipient; selects the
    domain and its assigned credential. It is a routing hint and grants no
    authority until the bearer matches.
  - `X-Gatehouse-Envelope-From` (optional) - the envelope sender.
  - `X-Gatehouse-Delivery-ID` (optional, bounded) - a stable id used for
    deduplication. When absent, the server derives one from a SHA-256 hash of
    the raw MIME. Reuse the same id when the Worker retries a delivery.
- Body: the raw RFC822 message, streamed to a bounded temp file.

The server verifies the bearer **before** reading the message body. Invalid or
missing authentication is rejected without consuming MIME.

- `recipient` must resolve to an enabled inbox (or the domain catch-all) on the
  assigned account, otherwise the server returns `406` and drops the message.
- A domain with no receive path returns `401` (uniform unauthorized).

The content-hash fallback can collapse separate identical messages to the same
recipient, so provide a delivery id when you can.

## 2. Copy the Worker example

Take `docs/cloudflare-worker.js` as your Worker. Set `WEBHOOK_URL` to your
instance's ingest endpoint and configure the secret (prefer a Worker secret
named `GATEHOUSE_WEBHOOK_SECRET`, or set the `SECRET` constant):

```js
const WEBHOOK_URL = "https://mail.example.com/internal/ingest/cloudflare";
const SECRET = "your-gatehouse-webhook-secret";
```

## 3. Create the Worker (Cloudflare dashboard)

Walk through the Cloudflare navigation:

1. Sign in to <https://dash.cloudflare.com>.
2. Pick the Cloudflare account that owns the domain you want to receive on.
3. In the top navigation, open **Workers & Pages**.
4. Click **Create application** -> **Worker** -> **Deploy**.
5. Give the Worker a name, e.g. `oa-gatehouse`.
6. Click **Deploy** to create a stub Worker, then **Edit code**.
7. Replace the default stub with the contents of `docs/cloudflare-worker.js`.
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
- confirm the Worker secret matches the domain's receive-path credential;
- confirm the domain's **Receive path** is configured and the inbox exists.

Provider/DNS setup is external: saving a receive path alone does not establish
delivery.
