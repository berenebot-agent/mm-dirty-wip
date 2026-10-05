# Cloudflare Email Routing inbound

This guide sets up Cloudflare Email Routing so incoming mail reaches your
MailMoose instance over the generic inbound webhook.

Email Routing cannot POST directly to an arbitrary URL - it delivers each
message to a Cloudflare Worker. MailMoose generates a ready-to-paste
Worker that streams the raw MIME and the envelope metadata to MailMoose.

Flow:

```text
Internet email -> Cloudflare MX -> Email Routing -> Worker -> HTTPS POST
    -> /internal/ingest/cloudflare -> logical inbox
```

## 1. Add the receiving configuration in MailMoose

Inbound provider secrets are no longer environment variables, and there is no
separate credential to create and then assign. Receiving is configured directly
on the domain:

1. On the dashboard, click **Receiving** for the domain you receive on.
2. Choose **Cloudflare Worker** and save the form.
3. MailMoose generates the shared secret, stores it encrypted on that domain,
   and shows the complete Worker code (the generated
   secret is already embedded) and the Cloudflare steps below in a one-time
   dialog. The generated
   secret is shown only once; use **Regenerate secret** in the domain's
   receiving dialog if you lose it, then paste the new Worker code.
4. Repeat for each domain you receive on. Each domain has its own configuration;
   a normal re-save keeps its existing secret.

The generated Worker uses `DEDICATED_RECEIVER_URL`, falling back to `BASE_URL`
when omitted or blank (even when the dedicated listener is enabled). That hostname must resolve to a
**public** IP: Cloudflare Workers cannot fetch private addresses (`10.x`,
`192.168.x`, `172.16-31.x`, `127.x`) and fail with error `1002`. If the setup
page shows a reachability warning, set `DEDICATED_RECEIVER_URL` (or fix its DNS
record) to a public hostname and regenerate. Keep `BASE_URL` set to the UI origin
so passkeys use the correct domain.

You can also use the REST API. Omitting the secret lets MailMoose generate one,
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
  - `X-MailMoose-Recipient` (required) - the envelope recipient; selects the
    domain and its receiving configuration. It is a routing hint and grants no
    authority until the bearer matches.
  - `X-MailMoose-Envelope-From` (optional for ordinary mail, **required for
    draft-approval control mail**) - the envelope sender. The generated Worker
    sends `message.from`; a custom Worker must set it or approval replies fail
    closed.
  - `X-MailMoose-Delivery-ID` (optional, bounded) - a stable id used for
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
4. Click **Create application** -> **Start with Hello World** -> **Deploy**.
5. Open the new Worker and click **Edit code**.
6. Replace the default stub with the generated code from the dialog.
7. Click **Deploy** (top-right) to publish the new code.

## 4. Enable Email Routing for your domain

1. In the same account, go to the **dashboard home**.
2. Select your domain from **Domains** (e.g. `example.com`).
3. In the left sidebar under **Email**, click **Email Routing**.
4. Onboard the domain and add the **DNS records** Cloudflare lists (the MX
   record, and any SPF/TXT records it shows). Cloudflare-managed domains apply
   these automatically.
5. Follow the on-screen verification; Email Routing shows **Active** when
   done.

## 5. Add a routing rule that sends mail to the Worker

MailMoose's model is built around cheap, unlimited logical inbox identities, so
the recommended setup is **one catch-all rule for the whole receiving domain,
not one rule per address**. MailMoose then routes every local part internally
(to an inbox, an alias, or the domain catch-all), and adding a new inbox never
requires touching Cloudflare again.

1. Still under **Email Routing**, open the **Routing rules** tab.
2. Edit the **catch-all** rule (the `@` local part, i.e. all addresses). If no
   catch-all rule exists yet, create one.
3. Choose **Send to a Worker**.
4. For **Action**, select your Worker (e.g. `oa-mailmoose`) from the
   "Send to a Worker" dropdown.
5. **Enable** the rule and click **Save**.

The server routes each local part according to the domain catch-all setting: a
matching inbox or alias is delivered there, and anything else goes to the
domain's configured catch-all inbox.

### Subdomains: onboard each one, reuse the one Worker

Cloudflare stores the catch-all rule on the **apex** domain only, and the
dashboard's "match every address" selector lists only apex domains. To receive
on a subdomain you must onboard it explicitly: under **Email Routing** open the
domain's **Settings**, and under **Subdomains** add the subdomain (for example
`agent.example.com`). Cloudflare writes the MX and SPF records for it; mail to
the subdomain is then handled by the same zone catch-all and delivered to the
same Worker. Catch-all entries in Wrangler's `addresses` field do not support
subdomains (`*@sub.example.com` fails with error `2062`).

Because a single Worker serves the whole zone, you do not need a second Worker
(or a second secret) for a subdomain:

- In MailMoose, add the subdomain as a domain. When it is a subdomain of a
  domain that already has a receiving configuration, MailMoose detects the
  parent and offers to reuse its receiving configuration (and sending
  configuration, if configured). This is on by default; untick it to configure
  the subdomain separately. The subdomain keeps its own inboxes, aliases and
  catch-all, so `foo@agent.example.com` is a distinct address from
  `foo@example.com`.
- The one Worker forwards `message.to`, so the server knows which subdomain the
  mail was for; the bearer is the parent's shared secret.
- You can later give a subdomain its own receiver, or clear the inherited one,
  from the subdomain's row in the **Domains** list. Rotating the parent's secret
  updates every inheriting subdomain at once.

Sending is separate: to send or reply `From: @agent.example.com`, onboard the
subdomain for Cloudflare Email Sending (its own SPF/DKIM), independently of
receiving.

If you cannot dedicate a full domain/subdomain to MailMoose, per-address rules
remain available as a fallback: repeat steps 2–6 with a **Custom email address**
matching a specific inbox (e.g. `hermes@example.com`). Prefer the catch-all
whenever possible.

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
