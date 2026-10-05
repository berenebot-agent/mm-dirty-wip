# MailMoose — Inbox Connectors

Connectors are the **inbox-level** clients that talk to MailMoose: an agent
relay, a push webhook, or a plain API key. They are a different model from the
per-domain **receiving and sending providers** in [PROVIDERS.md](PROVIDERS.md):
a provider decides how mail enters and leaves a domain, while a connector
decides who is allowed to *act on* the mail in one inbox.

Every connector is a row in `clients`, scoped to one inbox of one account, and
carries its own outbound authority and delivery log.

| Kind | `clients.type` | Direction | Transport |
|---|---|---|---|
| API key | `api_key` | request/response | REST over HTTPS |
| Hermes Relay | `hermes` | dial-out | WebSocket (`/relay`) |
| OpenClaw | `openclaw` | dial-out | WebSocket (`/relay`) |
| Webhook | `webhook` | push-out | HTTPS POST |

Manage them in the inbox's **Connectors** tab, or from the dashboard **Clients**
panel (**Add Client**). The API surface is under `/v1/admin/*`; see
[API.md](API.md) §11–14.

---

## 1. Shared behaviour

### Outbound authority

Every connector that can send takes one of two roles:

- **`owner`** — sends directly, as if a human pressed send.
- **`assistant`** — cannot send. A send is converted into a draft and a send
  request, which a human approves (by email, or in the UI) before it leaves.

The role is set when the connector is created and changed later with
`PUT /v1/admin/{hermes,openclaw}/{id}` (`{"role": "..."}`). Existing connectors
default to `owner`.

This is the same boundary as the per-mailbox API-key roles `read` /
`assistant` / `owner`; a connector is effectively a mailbox-bound principal.

### The no-allow-list warning

An inbox may block senders that are not on its allow list. A connector on an
inbox with **no** allow list will answer mail from anyone on the internet.

MailMoose surfaces this as a warning in the create dialog and requires an
explicit acknowledgement before the connector is created:

> **This inbox has no allow list.** Your agent will respond to anyone who
> emails this inbox.

It is a warning rather than a block because the allow list is a *filter*, not
an identity check — the `From:` header can be spoofed. It narrows who reaches
the agent; it does not authenticate them. Set it anyway: an unlisted, agent-fronted
address is a prompt-injection surface that is trivially discoverable.

### What the agent sees

Both relay connectors deliver the same event shape. MailMoose normalises the
stored message into text, and **the sender is untrusted input**:

- the `From:` address is caller-asserted, never an authenticated identity;
- the envelope sender is relay-supplied, not provider-attested
  (see [SECURITY.md](../SECURITY.md));
- so a connector must treat an inbound email as external, unauthenticated
  input. The inbox allow list is the primary gate.

If the message has no plain-text part, the relay event body is the placeholder
`(HTML email; open the message to view content)` and the real content stays in
MailMoose. A connector that needs the body should fetch it over the API rather
than rely on the pushed text.

### Delivery-triggered auto-actions

An inbox may automatically act on mail once a connector has delivered it:

- **Mark read on delivery** — the message leaves the unread count as soon as a
  connector has received it.
- **Move to Trash after delivery** — after a configurable number of hours from
  the delivery instant, the message is moved to Trash, where the ordinary Trash
  retention then applies.

Both are off by default, are **per-inbox** (they apply to every connector on the
inbox), and are offered in the connector-create dialog and on the inbox's
Connectors tab, which carries its own **Save auto-actions** control (the rest of
the inbox edit dialog saves on its other tabs and never touches this policy). A
**trigger** decides when they fire: `any` (the first connector to deliver) or
`all` (every connector that existed when the message arrived has delivered — a
connector added later never pins older mail). `all` is the default, so mail is
neither marked read nor trashed until every connector on the inbox has seen it.

These actions apply to **agent and relay connectors only**. API keys and human
accounts never trigger them: a poll is not a delivery. Mail that is Spam,
internal workflow mail, or already trashed is never acted on. A message exposes
its delivery history (which connector delivered it, and when it becomes eligible
for auto-trash) on the ordinary read surfaces.

---

## 2. API keys

The baseline connector: a bearer token scoped to one or more mailboxes with a
role each, optionally account-wide admin.

```bash
POST /v1/admin/keys
{"name":"Reporting", "admin":false, "mailboxes":{"in_01K...":"read"}}
```

The plaintext token is returned **once** (prefix `mmm_`), and only its hash is
stored. Rotate or revoke from the Clients panel or `DELETE /v1/admin/keys/{id}`.

Use an API key for anything that polls or acts on its own schedule: a script, a
cron job, another agent runtime. Use a relay connector when you want live push
without running a web server.

---

## 3. Hermes Relay

For [Hermes](https://github.com/NousResearch/hermes) agents. Hermes **dials out**
to MailMoose, so the agent host needs no public inbound port, and MailMoose stays
the durable source of truth: unacknowledged mail is replayed after a reconnect.

### Create the connection

In the inbox's **Connectors** tab → **Add Connector** → **Hermes relay
connection**, choose the inbox and the outbound authority, and acknowledge the
no-allow-list warning if it applies. MailMoose mints the credentials and shows
them **once**:

```text
GATEWAY_RELAY_URL=https://mail.example.com
GATEWAY_RELAY_ID=gw-...
GATEWAY_RELAY_SECRET=...
GATEWAY_RELAY_DELIVERY_KEY=...
GATEWAY_RELAY_PLATFORMS=email
GATEWAY_RELAY_ALLOW_DIRECT_PLATFORMS=true
```

Paste that block into the Hermes host's `.env` and restart the gateway. The same
block is returned by `POST /v1/admin/hermes/enroll` (`{"inbox_id","name"}`) as
its `env` field, alongside `gateway_id`, `secret`, `delivery_key` and
`connector_url`.

### The wire contract

Hermes opens an authenticated WebSocket to `wss://<host>/relay` and sends a
`hello`. MailMoose replies with a descriptor, then pushes accepted mail as
inbound events; Hermes acknowledges each one, and the acknowledgement is what
advances the durable cursor. Replies go back over the same socket.

Because the cursor is durable and the ack is the receipt, a crash mid-turn
replays the message rather than losing it.

### One-time enrollment tokens (OpenClaw only)

`POST /relay/enroll` redeems a single-use enrollment token. **Only the OpenClaw
connector can mint one** — `CreateRelayEnrollCode` rejects every other kind, so
there is no way to obtain a Hermes enrollment token and `hermes gateway enroll`
cannot complete against this installation.

The Hermes path is the **direct `.env` block** above:
`POST /v1/admin/hermes/enroll` (or the Create key dialog) mints the credentials
outright and returns them once. There is no token-exchange step.

> An earlier revision of this document — and `D018`, `docs/API.md`,
> `docs/ACCEPTANCE_TESTS.md` and the README — described a Hermes
> enrollment-token flow via `hermes gateway enroll`. That flow is not wired up
> in this codebase: `hermes_enroll_tokens` exists and is kind-aware, and the
> store-level `CreateHermesEnrollToken` is exercised by tests, but nothing above
> the store issues a Hermes token. Use the direct block.

Relay is **reply-only**: it resolves a target to the latest inbound message in a
thread, so it cannot initiate a brand-new outbound email. To send fresh mail,
use `/v1/send` with an API key, or have the connector reply to a thread.

---

## 4. OpenClaw Connector

For [OpenClaw](https://github.com/openclaw/openclaw) agents. OpenClaw is a
**distinct connector kind** (`clients.type = 'openclaw'`, decision `D074`) that
reuses the same relay transport as Hermes. There is no second transport and no
`/openclaw-relay` route — the difference is product-facing: its own icon, its
own delivery log, its own revocation path.

### Install the plugin

The MailMoose channel plugin is **not yet published** to npm or ClawHub; the
source ships in this repository at `plugins/openclaw/openclaw-plugin/`.

```bash
cd plugins/openclaw/openclaw-plugin
npm ci && npm run build
npm pack --pack-destination /tmp
openclaw plugins install npm-pack:/tmp/mailmoose-openclaw-0.1.0.tgz --accept-capabilities
openclaw plugins inspect mailmoose --runtime --json
```

Requires **Node >= 24.16 < 25 || >= 26.1**; the published OpenClaw images
already satisfy this. Installing from a local directory instead of a packed
tarball additionally needs `--force`, because a local path is outside ClawHub
review.

### Onboard: one-time code

In the inbox's **Connectors** tab → **Add Connector** → **OpenClaw agent
connector**, choose the inbox and the outbound authority. Pick **One-time
code**, and MailMoose shows the command to run on the OpenClaw host:

```bash
openclaw channels add --channel mailmoose --code https://mail.example.com/#<one-time-code>
```

The URL origin is the MailMoose address; the `#` fragment carries the code. The
code expires after **15 minutes** and is single-use; it is stored only as a
hash. The OpenClaw host redeems it at `POST /relay/enroll`, which returns the
connector credentials plus the `kind` and `name` it created.

> **Install the plugin before running this.** The stock CLI validates
> `--channel` against a built-in list that does not contain `mailmoose`, so the
> command fails until the plugin is installed.

### Onboard: manual config block

For air-gapped installs, choose **Manual config block** instead and paste the
generated block into the OpenClaw host's config:

```json5
{
  "channels": {
    "mailmoose": {
      "enabled": true,
      "baseUrl": "https://mail.example.com",
      "gatewayId": "gw-oc-…",
      "secret": "…",
      "deliveryKey": "…"
    }
  }
}
```

Once connected, OpenClaw appears in the inbox's connector list with the same
Settings, Log and delete actions as a Hermes connection. One email thread maps
to one OpenClaw conversation; outbound targets use `thread:<mailmoose_thread_id>`.
Deleting the connector closes its live socket immediately.

---

## 5. Webhook

A webhook connector pushes mail to a URL you control. Unlike the relays, it is
**outbound only** — there is no reply path; use an API key for that.

The destination must be an **HTTPS URL** with no userinfo and no fragment.

### Payload modes

- **notify** — a small JSON body, `Content-Type: application/json`:

  ```json
  {"event":"message.received","cursor":"evt_123","inbox_id":"in_01K...","message_id":"msg_01K..."}
  ```

- **forward** — the raw stored MIME, byte-for-byte, `Content-Type: message/rfc822`.

### Headers

Every delivery carries:

```text
X-MailMoose-Event:      message.received | message.spam_state_changed
X-MailMoose-Delivery:   <client_id>:<cursor>
X-MailMoose-Message-Id: <message_id>
X-MailMoose-Cursor:     evt_<n>
```

A **forward** delivery additionally carries the transport envelope metadata read
from the persisted message, so it is identical on retries:

```text
X-MailMoose-Envelope-From: <percent-encoded original envelope sender, or empty>
X-MailMoose-Envelope-To:   <percent-encoded canonical original envelope recipient>
```

Values are percent-encoded UTF-8 using RFC 3986 escaping: a space is `%20`, a
literal plus is `%2B`, and `@` is `%40`. This is **not**
`application/x-www-form-urlencoded`, where `+` means a space. Decode strictly
and treat an unparseable value as absent. An empty envelope sender is sent as an
empty header rather than being omitted, so a receiver that requires one fails
closed instead of falling back to the spoofable MIME `From:`. The envelope
sender is **relay-supplied, not provider-attested** — record it, do not treat it
as authority.

### Authentication

Chosen per connector:

- **bearer** — `Authorization: Bearer <token>`, the same generated token shown
  once at creation or rotation.
- **signature** — `X-MailMoose-Signature: t=<unix>,v1=<hex>`, an HMAC-SHA256
  over `t + "." + <body>` keyed by the connector secret. Verify the timestamp
  is recent before trusting the body.

### Delivery semantics

Delivery is durable and inbox-ordered. HTTP 2xx acknowledges the delivery and
advances the cursor; failures retry with capped exponential backoff for the
retry window (`WEBHOOK_RETRY_WINDOW_DAYS`, default 7), after which the delivery
is marked failed and the cursor advances. Mail that is currently Spam, internal,
or has since been deleted is terminally **skipped** with no request, so it can
never block the queue.

A non-2xx response is retried, so a receiver must be **idempotent** — dedupe on
`X-MailMoose-Delivery` (it is stable across attempts for a given event).

---

## 6. Choosing a connector

| You want | Use |
|---|---|
| A script or cron to poll, send and manage mail | API key |
| A Hermes agent to receive and reply to mail live | Hermes Relay |
| An OpenClaw agent to receive and reply to mail live | OpenClaw connector |
| To push mail into your own service or queue | Webhook (`forward` for the raw message, `notify` to fetch it yourself) |
| An agent that may draft but never send unattended | Any connector with outbound authority `assistant` |

Relay connectors are the only ones that give an agent a live, replayable inbox
with no inbound port and no polling code.

## 7. Troubleshooting

- **`403 admin required`** — connector management is Admin-only. Check the
  `admin` flag in `GET /v1/bootstrap`.
- **The setup code is rejected** — it is single-use and expires after 15
  minutes. Mint a new one; a consumed or expired code answers `403`.
- **`openclaw channels add` fails with an unknown channel** — the plugin is not
  installed yet (§4).
- **Mail arrives but the body says `(HTML email; open the message to view
  content)`** — the message has no plain-text part. Fetch it over the API; the
  relay event is a notification, not the full message.
- **A webhook never fires** — the destination must be HTTPS, and Spam, internal
  and deleted mail is skipped by design. Check the connector's delivery log.
