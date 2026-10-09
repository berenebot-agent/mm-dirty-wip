# @mailmoose/openclaw

MailMoose email channel for [OpenClaw](https://github.com/openclaw/openclaw).

Your agent gets its own email inbox on a MailMoose instance. OpenClaw dials
out to the MailMoose relay over an authenticated WebSocket, receives new mail
as agent turns, and replies on the original email thread. No inbound port or
webhook is required on the OpenClaw host, and MailMoose stays the durable
source of truth: unacknowledged email is replayed after a reconnect.

## Requirements

- OpenClaw 2026.9.7 or newer
- A MailMoose inbox and an OpenClaw connector for it

## Install

```bash
openclaw plugins install clawhub:mailmoose
```

(The ClawHub package name is finalized at publication; until then install from
npm or a packed tarball.) During development:

```bash
openclaw plugins install -l ./openclaw-plugin
openclaw plugins inspect mailmoose --runtime --json
```

## Onboard

In MailMoose, open the inbox → **Connectors** → **Add Connector** → **OpenClaw**
and pick **One-time code**. Run the shown command on the OpenClaw host:

```bash
openclaw channels add --channel mailmoose --code https://mail.example.com/#<code>
```

The URL origin is the MailMoose address; the `#` fragment carries the
single-use code (valid 15 minutes). For air-gapped installs choose **Manual
config block** instead and paste the generated `channels.mailmoose` block:

```json5
{
  "channels": {
    "mailmoose": {
      "enabled": true,
      "baseUrl": "https://mail.example.com",
      "gatewayId": "gw-oc-…",
      "secret": "…"
    }
  }
}
```

## Configuration

| Key | Meaning |
| --- | --- |
| `baseUrl` | MailMoose base URL. |
| `gatewayId` | Connector gateway id. |
| `secret` | Relay authentication secret (sensitive). |
| `allowFrom` | Optional OpenClaw-side sender allowlist. MailMoose inbox allowlists remain the primary gate. |
| `dmPolicy` | `allowlist` (default), `open`, or `pairing`. |
| `agentId` | Optional agent to route this inbox to. |

One email thread maps to one OpenClaw conversation. Outbound targets use
`thread:<mailmoose_thread_id>`.

## Security

- All connectivity is outbound from the OpenClaw host.
- Email senders are untrusted input. MailMoose's inbox allow list is the
  primary sender gate; the plugin dispatches email as external,
  unauthenticated input and never treats the From header as an identity.
- The relay secret is shown once at setup and should be treated like any other
  channel credential.
