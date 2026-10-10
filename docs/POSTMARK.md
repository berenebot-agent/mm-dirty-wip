# Postmark inbound

This guide connects Postmark inbound processing to MailMoose for receiving:

- **Inbound (receive):** Postmark accepts mail for a receiving domain and POSTs
  the message as JSON, including the raw MIME, to a MailMoose webhook protected
  by HTTP Basic authentication.

Postmark is a **receiving-only** connector here. Sending continues through
whichever sending provider the domain has configured; no Postmark outbound
adapter is added.

Flow for receiving:

```text
Internet email -> Postmark MX -> Postmark inbound
    -> JSON webhook (Basic auth) -> /internal/ingest/postmark -> logical inbox
```

## What you need

- A domain you control, with access to its DNS records.
- A Postmark account with a **Server** and its **Inbound Message Stream**.
- MailMoose's generated **webhook username and password**, and the resulting
  Basic-auth webhook URL. MailMoose generates these when you save the receiving
  configuration and shows the URL once.

## 1. Point the receiving domain's MX at Postmark

Add an MX record for the receiving host at `inbound.postmarkapp.com` with
priority 10. A dedicated subdomain (for example `inbound.yourdomain.com`) is
recommended over the root domain.

```text
HOST      TYPE  PRIORITY  VALUE
inbound.  MX    10        inbound.postmarkapp.com
```

## 2. Enable raw email content

In Postmark, open your Server's **Inbound Message Stream → Settings** and enable
**Include raw email content in JSON payload**. Postmark does not sign inbound
webhooks, so the endpoint is protected by Basic authentication instead.

## 3. Configure receiving in MailMoose

On the dashboard, click **Receiving** for the domain you receive on:

1. Choose **Postmark**. The form shows the setup steps.
2. Save. MailMoose generates a **username** and **password** and shows a
   one-time dialog with the Basic-auth **webhook URL**, for example:

   ```text
   https://<username>:<password>@<your-host>/internal/ingest/postmark
   ```

   The credentials are stored encrypted on the domain and shown only once. Use
   **Regenerate secret** on the same dialog if you lose them; the old
   credentials stop working immediately.

3. Copy the shown URL into the Inbound Message Stream's **webhook** field and
   save. Postmark embeds the credentials when posting to the URL.

Repeat for each domain you receive on. Each domain stores its own credentials.
If several domains share one Postmark stream, either use one domain's credentials
for all of them or configure each domain separately.

You can also use the REST API (the response returns `generated` once):

```bash
curl -X PUT "$BASE_URL/v1/admin/domains/$DOMAIN_ID/receiving" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"provider":"postmark","config":{}}'
```

## Webhook contract

- Endpoint: `POST /internal/ingest/postmark`
- Auth: `Authorization: Basic <base64(username:password)>`, compared in constant
  time against the domain's generated credentials.
- Body: the Postmark inbound JSON document. `OriginalRecipient` selects the
  domain; `RawEmail` carries the raw MIME.

Handling details:

- The `OriginalRecipient` (RCPT TO) selects the account/domain and its
  credentials. The raw MIME is not persisted until the credentials verify.
- `RawEmail` is extracted from the JSON without conversion through a Go string,
  so binary MIME content that is not valid UTF-8 survives intact.
- Delivery idempotency uses the Postmark `MessageID`, scoped to
  `(account_id, provider, canonical recipient, MessageID)`. Postmark retries are
  at-least-once; duplicates are collapsed.
- Unknown recipients resolve to the domain catch-all when configured; otherwise
  the endpoint returns `406`. Missing receiving configuration, unknown domains,
  and bad credentials return `401`.
- A missing/empty `RawEmail` is a permanent `406` with a message telling the
  operator to enable the raw-content option, so Postmark does not retry a
  misconfigured stream forever.

## Attestation

Postmark does not sign inbound webhooks. Basic authentication authenticates the
**caller** holding the domain's credentials, not the values it asserts, so the
envelope sender (`From`) is **not provider-attested**. This matches the
Mailgun/Cloudflare accepted risk in [../SECURITY.md](../SECURITY.md); the
approval workflow consumes the envelope sender, so treat Postmark inbound as
unattested.

## Verification

Send an email to an address on the receiving domain, then check it appears in the
inbox via the UI or `GET /v1/messages`. If it does not arrive:

- confirm the MX record resolves to `inbound.postmarkapp.com`;
- confirm the stream's webhook URL contains the credentials MailMoose generated;
- confirm **Include raw email content in JSON payload** is enabled;
- confirm the domain's **Receiving** provider is configured and the inbox exists.
