# Cloudflare Email Routing inbound

This guide sets up Cloudflare Email Routing so incoming mail reaches your
Open Agent Inbox instance over the generic inbound webhook.

Email Routing cannot POST directly to an arbitrary URL - it delivers each
message to a Cloudflare Worker. The Worker below receives the message, wraps
the raw MIME in JSON, and forwards it to Open Agent Inbox.

Flow:

```text
Internet email -> Cloudflare MX -> Email Routing -> Worker -> HTTPS POST
    -> /internal/ingest/cloudflare -> logical inbox
```

## Webhook contract

- Endpoint: `POST /internal/ingest/cloudflare`
- Auth: `Authorization: Bearer <CLOUDFLARE_WEBHOOK_SECRET>`
- Content-Type: `application/json`
- Body:

```json
{
  "recipient": "hermes@example.com",
  "envelope_from": "sender@outside.test",
  "raw_mime_b64": "RnJvbTogc2VuZGVyQG91dHNpZGUudGVzdA0K...",
  "received_at": "2026-09-08T19:00:00Z",
  "delivery_id": "<Message-Id or empty>"
}
```

- `recipient` must resolve to an enabled inbox (or the domain catch-all) on
  your account, otherwise the server returns `406` and drops the message.
- `delivery_id` is the dedup key. Provide the message `Message-Id` if
  available; if empty, the server derives it from a SHA-256 of the raw MIME.

## 1. Set the server secret

In `.env` set:

```text
CLOUDFLARE_WEBHOOK_SECRET=generate-a-long-random-secret
```

Keep this secret matching the `SECRET` constant in the Worker.

## 2. Copy the Worker example

Take `docs/cloudflare-worker.js` as your Worker. Set `WEBHOOK_URL` to your
instance's ingest endpoint and set `SECRET`:

```js
const WEBHOOK_URL = "https://mail.example.com/internal/ingest/cloudflare";
const SECRET = "your-cloudeflare-webhook-secret";
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
4. Give the rule a **Custom email address** that matches an inbox in Open
   Agent Inbox (e.g. `hermes@example.com`).
5. For **Action**, select your Worker name (e.g. `oa-gatehouse`) from the
   "Send to a Worker" dropdown.
6. Click **Save**.

Repeat for each inbox address you want to receive on. For a catch-all, add a
rule with the `@` local part (all addresses) and send it to the Worker; the
server routes unknown local parts according to the domain catch-all setting.

## Verification

Send an email to the configured address, then check the message appears in
the inbox via the UI or `GET /v1/messages`. If it does not arrive, check the
Worker logs (Workers & Pages -> your Worker -> Logs) for the forwarded
status, and confirm `CLOUDFLARE_WEBHOOK_SECRET` matches the Worker `SECRET`.
