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

> **Not currently offered.** The connector is implemented and unit-tested, but
> it has **not been verified against the real provider** — Twilio declined to
> activate a SendGrid account for testing (compliance refusal, ticket
> `#29899115`). SendGrid is therefore **removed from the receiving provider
> menu** until an end-to-end test passes. The setup below is retained as the
> path to verifying it; see [docs/SENDGRID.md](SENDGRID.md) for how to
> re-enable it.

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
DNS where possible. Operators must provide port 25 egress and publish SPF for the
sending IP. STARTTLS is used opportunistically when the receiving server
advertises it. Direct MX requires the operator to control the network identity
and reputation needed for direct Internet delivery.

### DKIM signing

Direct MX has no upstream provider to sign on your behalf, so MailMoose signs each
message itself. Signing is off by default; enable it in the domain's
**Sending → Direct MX** tab.

```json
{"helo": "mail.example.com", "dkim_enabled": "on"}
```

- `dkim_enabled` — `"off"` (default) or `"on"`. That is the whole configuration.
- `dkim_private_key` — generated by MailMoose (RSA-2048) when signing is switched
  on, stored encrypted, and never returned by a read. It is not an input: a value
  posted to it is ignored.
- `dkim_selector` / `dkim_domain` / `dkim_public_key` — derived and stored, but
  not settable. The selector is `mailmoose`; the signing domain is the domain mail
  is sent from, which is what keeps the signature aligned for DMARC.

The sending tab shows the record to publish as soon as signing is on:

- Type: `TXT`
- Name: `mailmoose._domainkey.<domain>` (for example
  `mailmoose._domainkey.example.com`)
- Value: `v=DKIM1; k=rsa; p=<public key>`

The public key is part of that record. MailMoose derives both the displayed
record and the signature from the same stored key, so they cannot disagree.

Messages are signed once, when they are queued, so the stored message carries the
signature that is actually transmitted. A retry after a failed attempt re-sends
the identical signed bytes rather than adding a second signature.

Turning signing off deletes the stored key. MailMoose does not check the published
record for you: a key that is generated but not published results in mail that
fails DKIM verification at the receiver.

Signing happens when a message is queued, so mail already queued before signing was
enabled keeps its existing (unsigned) stored message and is delivered unsigned.
Only messages queued after you enable signing carry a signature.

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


---

## Standalone mailbox providers

These connectors attach an existing Google or Microsoft mailbox to a MailMoose
standalone inbox. The provider mailbox remains the source of truth; see the
[mailbox service contract](MAILBOX_SERVICE_CONTRACT.md) for shared behavior.

## Google / Gmail


Use **Add inbox → Standalone inbox → Google / Gmail**. Each connected inbox
uses your own Google Cloud OAuth client. The Google mailbox remains the source
of truth. MailMoose caches headers, snippets, thread and label metadata, not a
default full-message archive. Bodies and attachments are fetched when opened.

### Create your Google app

1. Open [Google Cloud Console](https://console.cloud.google.com/) and sign in
   with the account you are connecting. Create a project (or select one) and
   keep it selected.
2. Enable the [Gmail API](https://console.cloud.google.com/apis/library/gmail.googleapis.com).
3. Open [Google Auth Platform](https://console.cloud.google.com/auth/overview).
   If you see **Get started**, click it, then set **App name** (any name, e.g.
   MailMoose), **User support email** and **Contact email** (your Google
   address), and **Audience**: choose **External** for a normal `@gmail.com`
   account. **Internal** is only available to Google Workspace organisations.
   Accept the policy and finish.
4. Keep the app in **Testing** and add your Google account as a test user.
   Open [Audience](https://console.cloud.google.com/auth/audience) and, under
   **Test users**, click **Add users** and add the exact address you are
   connecting. Do **not** click **Publish app** — for a restricted Gmail scope,
   an unverified External app in production is hard-blocked (see
   [Long-lived access](#long-lived-access-and-troubleshooting)).
5. Open [Data Access](https://console.cloud.google.com/auth/scopes) → **Add or
   remove scopes** and add `https://www.googleapis.com/auth/gmail.modify`,
   `openid`, and `https://www.googleapis.com/auth/userinfo.profile` (use manual
   scope entry if a scope is not listed), then save. The Gmail scope permits
   reading, labels, read/unread, drafts and sending, including moving mail to
   Trash. No separate `gmail.send` scope is needed, and MailMoose does not
   request the broader permanent-delete scope.
   The basic-profile scope lets MailMoose retrieve your Google account name for
   the inbox label. The OAuth request uses `openid profile` for this access.
6. Open [Clients](https://console.cloud.google.com/auth/clients) → **Create
   client**, choosing **Web application**. Leave **Authorised JavaScript
   origins** empty.
7. Under **Authorised redirect URIs**, add the exact URL shown in the MailMoose
   wizard. For example:

   ```text
   https://mail.example.com/ui/oauth/google/callback
   ```

   This is based on MailMoose's configured `BASE_URL`; spelling, scheme, port
   and path must match. JavaScript origins are not needed for this server-side
   authorization flow.
8. Enter the client ID and client secret in the wizard. Click **Connect Google**,
   select the mailbox account and approve access. Google will show a **"Google
   hasn't verified this app"** screen; click **Advanced → Go to <app name>** to
   continue.

Google's browser return goes to your installation. Google servers do not need
inbound access: a private/VPN hostname works when your browser can reach it and
the URL meets Google's redirect registration rules. A normal HTTPS hostname is
required except for localhost/loopback HTTP when the browser and MailMoose share
the computer. A raw LAN IP, `.local` hostname or arbitrary HTTP origin is not a
registerable production callback. Set `BASE_URL` appropriately before setup.

### Inbox display name

For a new inbox, an optional display name entered in the wizard takes precedence
over your Google account name. If Google does not supply a name, profile access
is declined, or the profile lookup fails, the mailbox still connects and the
email address is used as the fallback. This is the Google account profile name,
not a Gmail “Send mail as” name.

Existing connections need to reconnect to grant the added basic-profile access;
refreshing a token cannot add permission. On reconnect, a Google name replaces
only a label that still equals the inbox email address. Custom names are preserved,
and names are not continuously synchronized. This applies to direct Google OAuth,
not Gmail via One.

### If the callback page fails

Keep the wizard tab open. After approving Google access, if the final MailMoose
page cannot load, copy the **complete final URL** from that tab's address bar.
Open **Callback failed? Paste the return URL** in the wizard and paste it.

The URL must be the same registered callback and contain the one-time code and
state. Connection attempts expire after ten minutes and are used once. A callback
already completed cannot be pasted again. This fallback does not bypass Google's
`redirect_uri_mismatch` checks and is not the discontinued Google OOB code flow.
Do not share the return URL. Application logs omit it; configure your reverse
proxy's access logs to omit callback query strings as well.

### Long-lived access and troubleshooting

#### Personal Gmail: stay in Testing

For a normal `@gmail.com` account the OAuth app must be **External** (Internal is
Workspace-only) and should be left in **Testing** with your address added as a
test user. `gmail.modify` is a Google *restricted* scope, so:

- Apps that request restricted scopes in production must complete Google's
  verification (which includes a third-party security assessment). Until then,
  Google shows the **"Google hasn't verified this app"** warning in Testing —
  click **Advanced → Go to <app name>** to proceed.
- In Testing, authorizations and their refresh tokens expire **seven days** after
  consent. When MailMoose reports expired access, reconnect from the inbox
  settings' **Google / Gmail — reconnect account** link; it preserves the inbox
  and stable IDs.

Publishing an unverified External app does **not** remove the seven-day limit —
it replaces the click-through warning with a hard block (below). The only way to
remove the limit is to complete Google's verification, so for personal use the
seven-day reconnect is the intended trade-off.

#### Access blocked: has not completed the Google verification process

This error means the OAuth app is **In production** but has not completed
verification, so Google refuses the restricted Gmail scope outright. Revert it:

1. Open [Audience](https://console.cloud.google.com/auth/audience).
2. Under **Publishing status**, click **Back to testing**.
3. Ensure the address you sign in with is listed under **Test users**.
4. Save, wait a few minutes for propagation, then retry **Connect Google** and
   click through the **"Google hasn't verified this app"** screen.

#### Other issues

- `redirect_uri_mismatch`: register the exact URL shown in the wizard, and use
  a Web application OAuth client.
- `org_internal` / access blocked by Workspace: the app's audience is **Internal**
  but the account is outside the project's organisation. Use **External** for a
  personal `@gmail.com` account, or ask your organisation administrator to allow
  the OAuth app and its requested Gmail permission.
- Missing offline access, revoked access, expired tokens or changed passwords:
  reconnect from the inbox settings' **Google / Gmail — reconnect account** link.
  Choose the same mailbox account. Reconnection preserves the inbox and stable IDs.
- Client secrets and tokens are encrypted with `APP_ENCRYPTION_KEY`. Back up the
  key separately from `/data`, as for other provider credentials. Secret fields
  and token values are never returned by configuration APIs.

### Mailbox behaviour

- Gmail system labels drive Inbox, Sent, Drafts, Spam and Trash views. Archive
  removes `INBOX` and preserves other labels. User-label views can overlap: one
  message has one stable MailMoose cache ID, regardless of how many labels it has.
- Label actions, read/star changes and other mutations are applied to Gmail.
  Permanent deletion is unavailable through this connector; use Gmail itself.
- Search executes against Gmail. List navigation uses cached metadata. Initial
  indexing is progressive and reports partial completeness until backfill ends.
  History API checks update external changes; an expired history cursor triggers
  a metadata resynchronization.
- Bodies and downloads are temporary, size-limited and removed after use. The
  existing HTML sanitizer and attachment download protections apply.
- Sending uses Gmail API and Gmail's own Sent copy. Assistant RemoteDraft handoff
  creates a Gmail draft for the human to review. Unknown publication outcomes
  are not blindly retried.
- Notification detection has a cursor separate from browsing/backfill. Initial
  connection establishes a baseline rather than notifying about existing mail.
  Proactive connectors use bounded polling; Google Cloud Pub/Sub push is not
  required for this connector. The mailbox therefore reports
  `capabilities.realtime` as `false`: the poll interval is the delivery
  mechanism, not a safety net.
- Normal OAuth connects the signed-in mailbox. Gmail UI delegation, domain-wide
  delegation and Google Groups require separate integrations.

References: [Web OAuth](https://developers.google.com/identity/protocols/oauth2/web-server),
[Gmail scopes](https://developers.google.com/workspace/gmail/api/auth/scopes),
[Gmail synchronization](https://developers.google.com/workspace/gmail/api/guides/sync).
## Microsoft / Outlook


Microsoft Graph connects one whole personal or shared mailbox. The provider
owns messages; MailMoose retains encrypted credentials, folder/message/thread
metadata, stable provider identifiers, incremental cursors and delivery state.
Bodies and attachments are fetched into bounded transient files on demand.

### Operator setup

Create an Entra public-client app in the dedicated publisher tenant. For the
shared Quick Connect registration, choose work/school accounts in any directory
and personal Microsoft accounts. Add the **Mobile and desktop applications**
redirect URI `http://localhost`. No client secret is used.

Configure the deployment:

```text
MICROSOFT_CLIENT_ID=<application-client-id>
MICROSOFT_TENANT_ID=common
```

The production shared app ID can be supplied at rollout. Until configured, the
wizard explains that Microsoft setup is unavailable. Self-hosters can use their
own public-client registration through the same environment settings; use a
tenant ID/domain instead of `common` when restricting to that tenant.

Delegated permissions: `User.Read`, `Mail.ReadWrite`, `Mail.Send`, and
`offline_access`. Shared connections additionally request
`Mail.ReadWrite.Shared`, `Mail.Send.Shared` and `User.ReadBasic.All` (the last
only to enumerate candidate shared mailboxes in the wizard); all three must be
added to the app registration or the shared consent request fails. Tenant
consent policy may require administrator approval. Exchange mailbox delegation
must also grant access to the selected shared mailbox; sending requires Send As
or Send on Behalf rights.

Existing connections retain their original client ID and tenant when defaults
change. Keep their app registrations operational for refresh/reconnection.

### Connecting

Choose **Add inbox → Standalone inbox → Microsoft / Outlook**, then pick
**Connect Microsoft · Personal Inbox only** (signing in as the mailbox itself)
or **Connect Microsoft · Personal Inbox and Shared Inbox** (the extra shared
scopes are requested before sign-in, because Microsoft fixes granted scope in
the authorization request). Sign in and approve delegated access.
Microsoft redirects to `http://localhost`; a browser connection failure is
expected because no local listener runs. The sign-in opens in a centred
partial-screen window so this wizard stays visible behind it, and the wizard
flashes its paste prompt above the completion field as soon as that window is
open. Copy the complete final address-bar URL out of the sign-in window and
paste it into that field. Use another desktop browser if the URL is hidden.
The attempt expires after ten minutes and completion is single-use.

A shared connection then shows a **mailbox picker**: MailMoose lists candidate
shared mailboxes by enumerating a bounded window of the tenant directory and
probing each one's Inbox, so only mailboxes the account can actually open are
offered. Manual address entry is the fallback when the directory is unavailable
or the mailbox lies outside the enumerated window. The choice is validated
before the connection is stored. The inbox label defaults to the provider
display name (personal) or address (shared), and is editable from the inbox'
Identity tab later.

The callback is sensitive. MailMoose validates the exact localhost destination,
state and PKCE verifier and does not log or retain its code or pasted URL.
Tokens are encrypted using `APP_ENCRYPTION_KEY` and bound to account/inbox. The
exchanged token held between sign-in and mailbox choice is encrypted the same
way and is single-use.

Each connection is restricted to its selected mailbox. Shared delegated scopes
can grant broader upstream access, but MailMoose never exposes another mailbox
or a general Graph proxy. Existing per-inbox Read/Assistant/Owner roles apply;
there is no folder permission subdivision. Reconnect uses the original identity
and mailbox. Shared operations never fall back to the user's personal mailbox.

### Operations and detection

Folders retain provider IDs and hierarchy; special roles use well-known Graph
identifiers rather than translated names. Categories map to message labels.
Immutable message IDs preserve local references through folder moves. The
ordinary Archive folder is supported; the separate Online Archive mailbox is
outside this release.

Remote-draft handoff remains the default; approval/direct-send use the existing
workflow. Replies use native Graph reply drafts when the original message can
be resolved uniquely. Graph manages Sent Items; MailMoose does not append a
duplicate copy. Microsoft may apply tenant-specific shared Sent Items rules.
Send acceptance is not proof of delivery. Unknown send outcomes are ambiguous
and must not be blindly retried.

Graph MIME import is not trusted to preserve recipients. Before submission,
MailMoose explicitly sets To/Cc/Bcc from the composed envelope and reads the
draft back to verify all three lists. A mismatch stops submission. Bcc comes
from the structured envelope for direct sends (the transmitted MIME intentionally
omits it). Remote-draft handoffs similarly set and verify the reviewed MIME
recipients before reporting the draft as published. Failures after draft creation
are treated as ambiguous so an automatic retry cannot create duplicate drafts.

Detection uses per-folder delta cursors for personal and shared
mailboxes. Metadata backfill is resumable and does not emit historical arrivals.
Live reads and on-demand detection use Graph; proactive delivery uses existing
remote workers. Search uses Graph and explicitly reports partial completeness
for provider search, which has a capped result set.

#### Automatic push setup

Own-mailbox connections (Outlook.com and Microsoft 365) automatically subscribe
to Inbox changes once the initial metadata baseline completes. Existing eligible
connections are provisioned by the worker too. The existing delegated permissions
are sufficient; no client secret or additional service is required.

The public HTTPS callback is `/internal/ingest/microsoft/notifications`, using
`Config.ReceiverURL()`: `DEDICATED_RECEIVER_URL` when supplied, otherwise
`BASE_URL`. It is registered on both the optional dedicated inbound listener and
the core listener. Route this path through the reverse proxy to an active
listener, permitting Microsoft's POST validation requests and notifications.
URL selection is independent of listener enablement. Local HTTP-only deployments
continue polling. The localhost OAuth redirect is unrelated to this public URL.

Notifications contain change hints, not message bodies. A per-subscription
random `clientState` is encrypted at rest; callbacks match its SHA-256 hash and
the subscription ID before persisting a coalesced SQLite sync request. The
callback acknowledges only after persistence, then wakes the existing worker.
Delta sync and the normal durable arrival/event pipeline fetch and deliver mail;
duplicate notifications cannot create duplicate arrival events. Microsoft
controls notification latency, so delivery is near-real-time rather than a
guaranteed instant SLA.

An authenticated notification is a fetch hint, not an arrival, so it is not
logged at INFO: it persists durable work and wakes the push worker, nothing more.
A push-triggered delivery is visible as the worker's `remote sync` line with
`trigger=push`, followed by the arrival's `remote received` line; its durable
`message.received` event includes `provider=microsoft` and
`source=microsoft push`. Poll-detected Microsoft arrivals use `source=microsoft`;
IMAP and Gmail arrivals identify their own source. Unmatched notifications and
validation challenges are Debug-only, and callback `clientState` and validation
tokens are never logged. No UI activity-log entry is added for remote arrivals.

Subscriptions last six days and renew with less than a day remaining. Lifecycle
notifications trigger reauthorization, subscription recreation, or delta catch-up.
Queued work survives restarts; lost subscription-create responses are reconciled
against Graph's subscription list. Inbox deletion/disable retains only an
encrypted cleanup grant until the subscription is removed or expires, then
deletes that state.

Detection is split into a cheap quick pass and a deep reconcile. The quick pass
(the watcher poll and every on-demand read) applies only the Inbox folder's stored
delta cursor and does not enumerate the folder tree. The deep reconcile (every
folder, the folder tree, prune/backfill) runs on the inbox's own full-sync
cadence. While a subscription is active, the quick poll is dropped: a notification
wakes the quick pass immediately and only the full-sync safety-net remains, so a
missed or expired notification is still caught. If push is unavailable
(unconfigured receiver, shared mailboxes), ordinary quick delta polling applies.
In all cases client-driven freshness is coalesced onto the inbox's quick interval,
so several open tabs or polling clients cannot outpace the worker; a manual reload
always runs.

Connection settings show push setup/active/retrying status; the API realtime
capability is true only while a recorded push subscription is active and unexpired.
Shared delegated mailboxes retain polling: Graph does not support change
subscriptions with delegated shared-mailbox permissions. Shared push would
require a separately approved application-permission integration. The Identity
tab's **sync schedule** is therefore how new mail arrives for one: nothing beats
that poll, so the quick interval is the mailbox's real delivery latency.

### Rollout acceptance

Deterministic fixtures cover OAuth, mailbox boundaries, immutable IDs, provider
continuations, mutations and ambiguous send handling. After registering the app,
verify with real Outlook personal, Microsoft 365 personal and delegated shared
mailboxes: connection, refresh, Outlook-originated changes, attachments, native
reply drafts, shared sending rights and Sent Items behaviour.
