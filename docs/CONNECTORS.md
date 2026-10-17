# Agent connectors

Connectors give AI agents and applications access to inboxes. Set them up from an inbox's **Connectors** tab or the **Clients** page. These are separate from [email providers](EMAIL_EMAIL_PROVIDERS.md), which handle receiving and sending mail.

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

- **MailMoose approval:** The draft waits for approval in MailMoose or by email. This is the mode for domain inboxes.
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
/.well-known/mailmoose
/agent
/v1/bootstrap
/openapi.json
```

See [API.md](API.md) for the complete interface.

## Hermes Relay

Hermes connects *out* to MailMoose over a WebSocket. No inbound port is required on the Hermes host. Unacknowledged messages are replayed when it reconnects.

1. Open the inbox's **Connectors** tab, add a **Hermes relay connection**, and select its role.
2. Copy the generated environment block into your Hermes gateway's `.env` file.
3. Restart Hermes.

The generated block looks like:

```dotenv
GATEWAY_RELAY_URL=https://mail.example.com
GATEWAY_RELAY_ID=gw-...
GATEWAY_RELAY_SECRET=...
GATEWAY_RELAY_PLATFORMS=email
GATEWAY_RELAY_ALLOW_DIRECT_PLATFORMS=true
```

Hermes Relay receives and replies to existing threads. It cannot start a new outbound email over the relay. Use `/v1/send` with an API key for that.

## OpenClaw

The OpenClaw channel plugin is included in this repository but is not yet published to npm or ClawHub. Install it on the OpenClaw host first:

```bash
cd plugins/openclaw/openclaw-plugin
npm ci && npm run build
npm pack --pack-destination /tmp
openclaw plugins install npm-pack:/tmp/mailmoose-openclaw-0.1.0.tgz --accept-capabilities
```

The plugin requires Node 24.16 or later in the supported 24.x or 26.x ranges. If installing from a local directory instead of a package, OpenClaw also requires `--force`.

In MailMoose, choose **Add Connector → OpenClaw**, then **One-time code**. Run the generated command on the OpenClaw host:

```bash
openclaw channels add --channel mailmoose --code https://mail.example.com/#<one-time-code>
```

The plugin must be installed first. The code is single-use and expires after 15 minutes. The setup screen also offers a manual configuration block.

OpenClaw receives mail and replies using the same relay transport as Hermes, with its own connector identity and delivery log.

## Webhooks

Webhooks push email to an HTTPS endpoint. They cannot reply or send; use a separate API key for that.

Choose the payload when creating the connector:

- **Notify:** A small JSON event with the inbox, message reference, and cursor. Fetch content separately through the API.
- **Forward:** The original MIME message as `message/rfc822`. For standalone mailboxes, the content is fetched live from the provider.

Each delivery carries `X-MailMoose-Event`, `X-MailMoose-Delivery`, and `X-MailMoose-Cursor`. Forward deliveries also include the original transport envelope addresses as percent-encoded `X-MailMoose-Envelope-From` and `X-MailMoose-Envelope-To` headers. The envelope sender is relay-supplied, not proof of identity.

Authentication is either a generated bearer token or an HMAC-SHA256 signature in `X-MailMoose-Signature`. Verify it at your endpoint.

A 2xx response acknowledges the event. Other responses are retried for the configured window (seven days by default). **Deduplicate on `X-MailMoose-Delivery`**, because retries can repeat an event. Spam, internal, and deleted mail is skipped.

For exact header encoding and the request contract, see [the webhook contract](MAILMOOSE_CONTRACT.md) and [API.md](API.md).

## Troubleshooting

- **No relay events:** Check the connector status, role, and delivery log. An HTML-only email may arrive with a placeholder; fetch the body through the API.
- **OpenClaw reports an unknown channel:** Install the MailMoose plugin before running the setup command.
- **Webhook not firing:** Check that the destination is HTTPS, that the inbox has deliverable mail, and that the delivery log shows successful attempts.
