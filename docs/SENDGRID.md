# SendGrid inbound

This guide connects Twilio SendGrid Inbound Parse to MailMoose for receiving:

- **Inbound (receive):** SendGrid accepts mail for a receiving domain and POSTs
  the raw MIME to MailMoose over a signed webhook.

SendGrid is a **receiving-only** connector here: it carries signed raw MIME into
MailMoose's existing inbound pipeline. Sending continues through whichever
sending provider the domain has configured (for example the generic SMTP
connector); no SendGrid outbound adapter is added.

Flow for receiving:

```text
Internet email -> SendGrid MX -> SendGrid Inbound Parse
    -> signed multipart HTTPS webhook -> /internal/ingest/sendgrid
    -> logical inbox
```

## What you need

- A domain you control, with access to its DNS records.
- A SendGrid account.
- The **webhook URL** to register in SendGrid:
  `https://<your-host>/internal/ingest/sendgrid`, where `<your-host>` is the
  instance's `DEDICATED_RECEIVER_URL` (or `BASE_URL` if omitted/blank).
  MailMoose shows this exact URL in the receiving dialog before you save.
- The webhook **verification public key** from a SendGrid Inbound Parse security
  policy that has signature verification enabled. SendGrid generates the key
  pair; you copy the public key into MailMoose.

## 1. Point the receiving domain's MX at SendGrid

Add an MX record for the receiving host at `mx.sendgrid.net` with priority 10.
SendGrid documents using a dedicated subdomain (for example
`parse.yourdomain.com`) rather than the root domain.

```text
HOST    TYPE  PRIORITY  VALUE
parse.  MX    10        mx.sendgrid.net
```

## 2. Create the Inbound Parse setting

In the SendGrid console, open **Settings → Inbound Parse** and click **Add Host
& URL**:

1. Set the **Receiving domain** (the host whose MX you just pointed).
2. Set the **Destination URL** to the MailMoose webhook URL.
3. Under **Additional Options**, tick **POST the raw, full MIME message**.
   MailMoose requires the raw MIME (`email` part); the parsed fields are not
   used.

## 3. Attach a signed-webhook security policy

SendGrid Inbound Parse sends request payloads as `multipart/form-data`, and any
security mechanism you attach is verified over the raw bytes, so MailMoose
verifies the signature before parsing anything.

1. Create a webhook **security policy** with **signature** enabled (the SendGrid
   API `POST /v3/user/webhooks/security/policies`).
2. The policy response includes a `public_key`. Copy it.
3. Attach the policy to the parse setting by setting its `security_policy` id.

Once attached, every Inbound Parse request carries:

- `X-Twilio-Email-Event-Webhook-Signature` — base64 ECDSA (ASN.1) signature.
- `X-Twilio-Email-Event-Webhook-Timestamp` — the signed timestamp.

MailMoose verifies `ECDSA(public_key, sha256(timestamp || raw_request_body))`.

## 4. Configure receiving in MailMoose

On the dashboard, click **Receiving** for the domain you receive on:

1. Choose **SendGrid**. The form shows the exact **Webhook URL** to use before
   you save, plus the setup steps.
2. Paste the **webhook verification public key** (base64 or PEM).
3. Save.

Repeat for each domain you receive on. Each domain stores its own receiving
configuration and its own public key.

You can also use the REST API:

```bash
curl -X PUT "$BASE_URL/v1/admin/domains/$DOMAIN_ID/receiving" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"provider":"sendgrid","config":{"public_key":"MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE..."}}'
```

## Webhook contract

- Endpoint: `POST /internal/ingest/sendgrid`
- Body: `multipart/form-data`. The `envelope` field (JSON) selects the domain;
  the `email` field carries the raw MIME.
- Auth: the ECDSA signature over `sha256(timestamp || raw body)`, verified with
  the domain's receiving **public key**.

Handling details:

- The envelope recipient selects the account/domain and its public key. The raw
  MIME is not parsed or persisted until the signature verifies.
- Delivery idempotency uses the raw MIME `Message-ID`, falling back to a hash of
  the raw body when it is absent, scoped to
  `(account_id, provider, canonical recipient, delivery id)`. SendGrid retries
  are at-least-once; duplicates are collapsed.
- Unknown recipients resolve to the domain catch-all when configured; otherwise
  the endpoint returns `406`. Missing receiving configuration, unknown domains,
  and bad signatures return `401`.
- Malformed multipart, missing raw MIME, oversize messages and other permanent
  faults return `406` so SendGrid stops retrying; transient faults return `500`.

## Attestation

The envelope sender reported to the approval workflow comes from the signed
`envelope.from` field, which the ECDSA signature covers. SendGrid's envelope
sender is therefore **provider-attested**, unlike Mailgun, Cloudflare and
Postmark. See [../SECURITY.md](../SECURITY.md).

## Verification

Send an email to an address on the receiving domain, then check it appears in the
inbox via the UI or `GET /v1/messages`. If it does not arrive:

- confirm the MX record resolves to `mx.sendgrid.net`;
- confirm the parse setting's destination URL matches `DEDICATED_RECEIVER_URL`
  (falling back to `BASE_URL`);
- confirm **POST the raw, full MIME message** is ticked;
- confirm the security policy with signature verification is attached and its
  public key matches the domain's receiving configuration;
- confirm the domain's **Receiving** provider is configured and the inbox exists.
