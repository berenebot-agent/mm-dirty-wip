# Agent connectors

Connectors give AI agents and applications access to inboxes. Set them up from an inbox's **Connectors** tab or the **Clients** page. These are separate from [email providers](EMAIL_PROVIDERS.md), which handle receiving and sending mail.

## Choose a connector

| Connector | Use it for |
| --- | --- |
| **API key** | Scripts, applications, and agents using the REST API |
| **Hermes Relay** | Live email delivery and replies through Hermes |
| **OpenClaw** | Live email delivery and replies through OpenClaw |
| **Webhook** | Push new email to your own HTTPS endpoint |

## Permissions and approvals

API keys can have **Read**, **Assistant**, or **Owner** access to each inbox. Relay connectors use **Assistant** or **Owner** authority.

- **Read:** Read and search messages.
- **Assistant:** Read, update mail, and prepare drafts without sending directly.
- **Owner:** Send and reply without approval.

An Assistant's request to send depends on the inbox's authoring mode:

- **Mammoth Mail approval:** The draft waits for approval in Mammoth Mail or by email. This is the mode for domain inboxes.
- **Remote draft:** The draft is placed in the connected mailbox's Drafts folder for you to review and send. This is the default for standalone inboxes. No approval email is sent.

An inbox sender allow list reduces who can reach an agent, but is not proof of sender identity. Email sender addresses can be spoofed. Treat incoming email as untrusted input.

In **Connectors**, you can also enable **mark read on delivery** and **move to Trash after delivery**. Both are off by default. The default trigger waits for every connector present when the message arrived; choose *any* if the first successful delivery is sufficient. API-key reads do not count as connector delivery.

## API keys

Create a key in **Clients → Add Client**, assign an inbox and role, and copy the token when shown. It is shown once. One key can have different roles across several inboxes.

Use the key with the REST API:

```http
Authorization: Bearer mmm_...
```

An agent can discover its permissions and the available operations from:

```text
/.well-known/mail-api
/agent
/v1/bootstrap
/openapi.json
```

See [API.md](API.md) for the complete interface.

### Agent first-use checklist

Start with `/v1/bootstrap`: choose an accessible inbox and inspect its effective
permissions. Direct sending requires `can_send`; an Assistant should inspect
`/v1/inboxes/{id}/authoring` and use the draft workflow instead. A remote-draft
handoff places mail in the provider's Drafts folder; it is not a send or approval
notification.

Domain and connected inboxes share the native mailbox routes and message schema.
List, thread and search pagination always follows `next_cursor` by sending it as
`before`, with the same filters. Tokens are opaque, not timestamps. Respect
`completeness` and per-inbox `errors`; pagination does not prove the whole mailbox
has been indexed. Fetch message detail for bodies, since listings omit them.

Read `/v1/inboxes/{id}` for the mailbox `capabilities`: `folders`, `move`,
`create_folders`, `rename_folders`, `delete_folders`, `labels`,
`create_labels`, `rename_labels` and `delete_labels`. They say what the
mailbox supports; do not infer it from the inbox type. A message's location
is its `folder_id` (one Mammoth Mail folder id, or `null`) and its labels are
the additive `label_ids` (`tag:Name`, empty for IMAP) — a folder is never a
label. `/v1/inboxes/{id}/label-catalog` lists the labels themselves and
repeats the label half of the capabilities. The
[shared contract and capability matrix](API.md#shared-contract-and-provider-capabilities)
records deliberate provider differences. `/examples/python`, `/examples/bash`
and `/examples/curl` provide runnable examples; Python uses only the standard
library, while Bash needs `curl` and `jq`.

## Hermes Relay

Hermes connects *out* to Mammoth Mail over a WebSocket. No inbound port is required on the Hermes host. Unacknowledged messages are replayed when it reconnects.

1. **Copy the generated block** into your gateway's `.env` — usually `~/.hermes/.env`. Replace any existing `GATEWAY_RELAY_*` lines; only one relay connection can own a gateway.

   ```dotenv
   GATEWAY_RELAY_URL=https://mail.example.com
   GATEWAY_RELAY_ID=gw-...
   GATEWAY_RELAY_SECRET=...
   GATEWAY_RELAY_PLATFORMS=email
   GATEWAY_RELAY_ALLOW_DIRECT_PLATFORMS=true
   ```

   > **Optional but recommended — reduce extra emails**
   >
   > a. Set a home address alongside the relay lines in the same `.env`, so Hermes does not send correspondents a “No home channel is set” notice.
   >
   >    ```dotenv
   >    EMAIL_HOME_ADDRESS=you@yourdomain.com
   >    EMAIL_HOME_ADDRESS_NAME=Your Agent Name
   >    ```
   >
   > b. Turn off memory and self-improvement notices globally in `~/.hermes/config.yaml` (merge into any existing `display` section):
   >
   >    ```yaml
   >    display:
   >      memory_notifications: off
   >      platforms:
   >        email:
   >          suppress_warning_notifications: true
   >    ```

2. **Restart the Hermes gateway (required):** run `hermes gateway restart` to load the configuration changes.

Keep these setup instructions in step with the Add Client dialog's Hermes guide in `internal/httpapp/ui.go`.

The inbox's **Connectors** tab shows **Never connected** until a relay first
connects, then **Last connected** with the time its WebSocket connection was
established. This is connection history, not live online status or proof that
mail was handled successfully; use the delivery log for delivery outcomes.
The same history is shown for OpenClaw connectors. Abandoned manual setups can
be removed with the existing **Delete** action. If the one-time credentials are
lost, delete the connector and add a new one; update the gateway configuration
with the replacement credentials.

Hermes Relay receives and replies to existing threads. It cannot start a new outbound email over the relay. Use `/v1/send` with an API key for that.

## OpenClaw

The OpenClaw connector is an early alpha: the plugin and the connector are in
the early stages of testing, so expect rough edges and please report what breaks.

The OpenClaw channel plugin is included in this repository but is not yet published to npm or ClawHub. Install it on the OpenClaw host first:

```bash
cd plugins/openclaw/openclaw-plugin
npm ci && npm run build
npm pack --pack-destination /tmp
openclaw plugins install npm-pack:/tmp/mammothmail-openclaw-0.1.0.tgz --accept-capabilities
```

The plugin requires Node 24.16 or later in the supported 24.x or 26.x ranges. If installing from a local directory instead of a package, OpenClaw also requires `--force`.

In Mammoth Mail, choose **Add Connector → OpenClaw**, then **One-time code**. Run the generated command on the OpenClaw host:

```bash
openclaw channels add --channel mammothmail --code https://mail.example.com/#<one-time-code>
```

The plugin must be installed first. The code is single-use and expires after 15 minutes. The setup screen also offers a manual configuration block.

Generating a one-time code does not create a delivery connector until the host
redeems it. Unredeemed, unexpired codes appear under **Pending setup codes** in
that inbox's **Connectors** tab, with their name and expiry time. **Cancel setup**
invalidates the code immediately. Pending codes have no delivery log and do not
count as connectors for delivery-triggered automatic actions. After redemption,
the pending entry is replaced by the actual connector, which may still show
**Never connected** until the gateway opens its relay connection. Refresh the
page to see changes made by the gateway.

Expired codes no longer appear in the pending list and cannot be redeemed. If a
code expires or is lost, generate a new one with **Add Connector → OpenClaw →
One-time code**; cancel any previous, still-valid code you no longer need.

OpenClaw receives mail and replies using the same relay transport as Hermes, with its own connector identity and delivery log.

OpenClaw's “Learned” notice needs OpenClaw-side configuration; the Hermes settings above do not fix it.

## Webhooks

Webhooks push email to an HTTPS endpoint. They cannot reply or send; use a separate API key for that.

Choose the payload when creating the connector:

- **Notify:** A small JSON event with the inbox, message reference, and cursor. Fetch content separately through the API.
- **Forward:** The original MIME message as `message/rfc822`. For standalone mailboxes, the content is fetched live from the provider.

Each delivery carries `X-Mail-Event`, `X-Mail-Delivery`, and `X-Mail-Cursor`. Forward deliveries also include the original transport envelope addresses as percent-encoded `X-Mail-Envelope-From` and `X-Mail-Envelope-To` headers. The envelope sender is relay-supplied, not proof of identity.

Authentication is either a generated bearer token or an HMAC-SHA256 signature in `X-Mail-Signature`. Verify it at your endpoint.

Outbound webhooks now emit only the brand-neutral `X-Mail-*` headers (for example `X-Mail-Signature`). The old `X-MailMoose-*` header aliases are no longer sent.

### Verify the signature

The signing key is the connector secret itself, used as UTF-8 bytes. It looks like base64url but is not encoded: do not base64-decode it. Mammoth Mail signs the Unix timestamp, a literal `.`, then the exact request body it sends:

```text
v1 = hex(HMAC-SHA256(secret, t + "." + body))
```

This example uses only the Python standard library and applies to both payload modes:

```python
import hashlib
import hmac
import time


def verify(secret: str, header: str, body: bytes, tolerance: int = 300) -> bool:
    fields = {}
    for part in header.split(","):
        name, sep, value = part.strip().partition("=")
        if not sep or name in fields:
            return False  # malformed: missing '=' or repeated field
        fields[name] = value

    ts, sig = fields.get("t", ""), fields.get("v1", "")
    if not ts.isascii() or not ts.isdigit() or not sig.isascii():
        return False
    try:
        fresh = abs(time.time() - int(ts)) <= tolerance
    except ValueError:
        return False
    if not fresh:
        return False  # stale, or too far in the future

    expected = hmac.new(secret.encode(), ts.encode() + b"." + body, hashlib.sha256).digest()
    try:
        return hmac.compare_digest(expected, bytes.fromhex(sig))
    except ValueError:
        return False  # v1 is not hex
```

Verify before parsing anything, because the signature covers the raw bytes, not a re-serialised form:

```python
# Notify: body is JSON.
if not verify(secret, request.headers.get("X-Mail-Signature", ""), request.body):
    return 401
event = json.loads(request.body)

# Forward: body is the original MIME message (message/rfc822).
if not verify(secret, request.headers.get("X-Mail-Signature", ""), request.body):
    return 401
```

A receiver that rejects a timestamp older than its tolerance also rejects replayed deliveries; the tolerance is your choice.

### Delivery order and retries

Deliveries are strictly ordered per connector. The oldest undelivered event is attempted first, and while it waits for a retry, later events for that connector wait behind it. It stops blocking only when it is delivered with a 2xx, terminally failed once the retry window is exhausted, or skipped because the message is Spam, internal, or deleted. Other connectors are not blocked, including another webhook on the same inbox.

A 2xx response acknowledges the event. Other responses are retried with capped exponential backoff for the configured window. The window starts at the first attempt and defaults to seven days; set `WEBHOOK_RETRY_WINDOW_DAYS` to change it. **Deduplicate on `X-Mail-Delivery`**, because a retry repeats the same event and delivery ID.

For exact header encoding and the request contract, see the shared [contract fixture](../tests/fixtures/mammothmail-webhook-contract.json) and [API.md](API.md).

## Troubleshooting

- **No relay events:** Check the connector status, role, and delivery log. An HTML-only email may arrive with a placeholder; fetch the body through the API.
- **OpenClaw reports an unknown channel:** Install the Mammoth Mail plugin before running the setup command.
- **Webhook not firing:** Check that the destination is HTTPS, that the inbox has deliverable mail, and that the delivery log shows successful attempts.
