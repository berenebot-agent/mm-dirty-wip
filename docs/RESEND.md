# Resend inbound and outbound

This guide connects Resend to Gatehouse Email for both directions:

- **Inbound (receive):** Resend accepts mail for a verified domain and notifies
  Gatehouse over a signed webhook; Gatehouse then fetches the raw MIME from the
  Resend API.
- **Outbound (send):** Gatehouse sends through the Resend send API.

Flow for receiving:

```text
Internet email -> Resend MX -> Resend -> signed HTTPS webhook (metadata only)
    -> /internal/ingest/resend -> Gatehouse fetches raw MIME from Resend API
    -> logical inbox
```

Unlike Mailgun and Cloudflare, a Resend `email.received` webhook contains only
metadata (sender, recipients, subject, `email_id`). The raw MIME is fetched from
`GET /emails/receiving/{email_id}` using the account API key, then staged and
parsed by the shared ingest core.

## What you need

- A domain verified in Resend, with the inbound MX record applied (custom
  domain) or your account's `<id>.resend.app` receiving subdomain.
- A Resend **API key with Full access**. A send-only (`sending_access`) key
  cannot read received mail.
- The **webhook URL** to register in Resend:
  `https://<your-host>/internal/ingest/resend`, where `<your-host>` is the
  instance's `BASE_URL`. Gatehouse shows this exact URL in the receive-path
  dialog and on the setup page.
- The Resend **webhook signing secret** (`whsec_...`), created when you add the
  webhook. Resend generates this secret; you paste it into Gatehouse.

## 1. Add the receive path in Gatehouse Email

In the Admin **Settings** tab, edit the domain you receive on:

1. Under **Receive path**, choose **Add new receive path…** and pick **Resend**.
   The dialog shows the exact **Webhook URL** to use.
2. In Resend, open **Webhooks** → **Add Webhook**, paste that URL, tick
   **email.received** (leave the other events unchecked), and save. Open the
   webhook and copy its **signing secret** (`whsec_...`).
3. Back in Gatehouse, enter the **Resend API key** (full access) and the
   **Webhook signing secret**. Optionally set **API base URL** (default
   `https://api.resend.com`).
4. Save. Gatehouse opens the Resend setup page with the webhook URL and the
   remaining steps.
5. Assign the receive path to the domain.

Repeat for each domain you receive on. If several domains share one Resend
webhook, store the same signing secret on each receive path.

The operator supplies both the API key and the webhook signing secret. Gatehouse
does not generate Resend secrets (contrast Cloudflare, where Gatehouse generates
the Worker shared secret).

You can also use the REST API:

```bash
curl -X POST "$BASE_URL/v1/admin/inbound" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"provider":"resend","config":{"api_key":"re_...","webhook_secret":"whsec_..."}}'
```

Then assign it to the domain:

```bash
curl -X PATCH "$BASE_URL/v1/admin/domains/$DOMAIN_ID" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"inbound_credential_id":"<id from the create response>"}'
```

## 3. Add the sending provider in Gatehouse Email

1. Click **Add outbound provider**, pick **Resend**, and enter the **API key**.
2. Assign the provider as the domain's **Sending provider**.

The `from` domain must be a verified sending domain in Resend.

## Webhook contract

- Endpoint: `POST /internal/ingest/resend`
- Auth: Svix HMAC signature headers `svix-id`, `svix-timestamp`, and
  `svix-signature`, verified with the domain's receive-path **webhook signing
  secret**.
- Body: JSON metadata for the event. The recipient (`data.to`) selects the
  domain and its assigned credential; the first recipient that resolves to a
  configured domain is used.

Handling details:

- Only `email.received` triggers ingest. Other event types are acknowledged with
  `200` and ignored so Resend does not retry them; no state is changed and no
  content is fetched.
- The raw MIME is downloaded from the signed `raw.download_url` returned by
  `GET /emails/receiving/{email_id}`. The URL expires after roughly one hour, so
  it is fetched immediately.
- Delivery idempotency uses the Resend `email_id`, scoped to
  `(account_id, provider, canonical recipient, email_id)`. Resend webhooks are
  at-least-once; duplicates are collapsed.
- Unknown recipients resolve to the domain catch-all when configured; otherwise
  the endpoint returns `406`. Missing credentials, unknown domains, and bad
  signatures return `401`.
- Failures fetching the content return `500` so Resend retries.
- Once admitted for processing, Resend ingestion has a three-minute deadline
  independent of the webhook connection. A caller disconnect does not cancel
  the metadata fetch, raw MIME download, or database commit. The handler still
  waits for persistence before returning `200`; retries after a lost response
  are deduplicated by `email_id`.

## Verification

Send an email to the configured address, then check it appears in the inbox via
the UI or `GET /v1/messages`. If it does not arrive:

- confirm the webhook is subscribed to `email.received` and the URL matches
  `BASE_URL`;
- confirm the signing secret matches the domain's receive-path credential;
- confirm the API key can read received emails;
- confirm the domain's **Receive path** is configured and the inbox exists.

Provider/DNS setup is external: saving a receive path alone does not establish
delivery.
