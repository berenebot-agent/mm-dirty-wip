# MailMoose ↔ OpenClaw connector WIP

Status: design + starter implementation only. Nothing outside this folder is wired into MailMoose.

## Goal

Add **OpenClaw** as an inbox-level MailMoose connector with the same security property as Hermes Relay:

- OpenClaw initiates an outbound authenticated WebSocket to MailMoose.
- No inbound port or webhook is required on the OpenClaw host.
- MailMoose remains the durable email source of truth.
- New mail is replayable and acknowledged only after the OpenClaw side accepts it.
- Replies travel back over the same outbound connection and use MailMoose's normal send/reply path.
- One email thread maps to one OpenClaw conversation target.

## Key implementation choice

Do **not** build a second relay transport.

The current Hermes Relay already provides the mechanics required by OpenClaw:

- `GET /relay` authenticated WebSocket
- HMAC upgrade token derived from gateway id + secret
- hello/descriptor handshake
- durable `inbound` frames with `bufferId`
- `inbound_ack`
- `outbound` send requests and `outbound_result`
- replay after disconnect
- connector delivery logging
- inbox-scoped outbound role (owner or assistant)

OpenClaw should be a **new connector type in the product model/UI** while reusing that transport contract.

## Folder contents

### `openclaw-plugin/`

Starter OpenClaw plugin package. It contains:

- package and OpenClaw manifests
- a channel registration scaffold
- an outbound MailMoose relay client
- HMAC WebSocket authentication matching MailMoose's current relay
- reconnect/backoff
- inbound frame acknowledgement
- outbound send/result correlation
- an explicit adapter seam for OpenClaw's current inbound runtime API

The transport half is intentionally concrete. The final OpenClaw inbound-normalisation call is kept in one small function because the plugin SDK is experimental and should be pinned/tested against the target OpenClaw release before publishing.

### `mailmoose-dropin/`

Implementation notes and small drop-in helpers for the MailMoose side. They are not applied to main code.

## Proposed user experience

### From Inbox Settings → Connectors

1. Click **Add connector**.
2. Choose **OpenClaw**.
3. MailMoose creates an inbox-scoped relay credential.
4. The completion screen shows:
   - the OpenClaw plugin install command
   - a ready-to-paste OpenClaw config block
5. OpenClaw connects outbound to MailMoose.
6. The connector appears beside Hermes/Webhook in the inbox connector list.

### From Add Client

OpenClaw may also appear as a quick-start option beside Hermes and Webhook. Selecting it hands off to the same inbox connector flow; it never creates an account-level client.

## Connector model

Product-visible kinds:

- `api` — account-level client
- `hermes` — inbox connector, outbound relay
- `openclaw` — inbox connector, outbound relay
- `webhook` — inbox connector, MailMoose pushes HTTP

For the first implementation, OpenClaw can share the existing relay persistence and wire protocol with Hermes. The smallest migration is to add a connector-kind discriminator to the existing relay connection row. A later cleanup can rename Hermes-specific internal tables/types to generic relay names if worthwhile.

## OpenClaw install target

Published experience:

```bash
openclaw plugins install clawhub:mailmoose
```

Then paste the generated `channels.mailmoose` config block.

During development:

```bash
openclaw plugins install -l ./openclaw-plugin
openclaw plugins inspect mailmoose --runtime --json
```

## Security requirements

- OpenClaw never needs a publicly reachable webhook.
- Relay secret must remain secret and be encrypted at rest by MailMoose.
- Upgrade tokens are short lived and HMAC authenticated, matching the existing relay.
- Do not log the relay secret or generated config.
- Keep sender provenance untrusted. Email From is not an authenticated OpenClaw user identity.
- Preserve MailMoose's existing allowed-sender warning and outbound role choice.
- Deleting the connector must invalidate/close its live socket.
- Acknowledgement happens only after OpenClaw has accepted the inbound event for dispatch.
- Reconnect must replay from the last durable acknowledgement.

## Session mapping

Default mapping:

```text
MailMoose inbox + email thread id
            ↓
OpenClaw channel target
            ↓
one continuing OpenClaw session/conversation
```

The MailMoose thread id is the connector target used for replies. This preserves conversational context while keeping unrelated email threads isolated.

## Current OpenClaw SDK note

As of 2026-10-02, OpenClaw's plugin SDK supports channel plugins and long-running Gateway services. The SDK is explicitly experimental, so pin the tested minimum OpenClaw version before publishing the plugin to ClawHub.

## Definition of done for promotion into main

- OpenClaw connector is selectable in both connector entry paths.
- Generated config works on a clean supported OpenClaw installation.
- Plugin opens only outbound network connections.
- Inbound email invokes the selected OpenClaw agent/session.
- OpenClaw replies are sent through MailMoose on the original email thread.
- Disconnect/reconnect replays unacked email exactly as Hermes Relay does.
- Delete immediately revokes the socket.
- Assistant role creates approval-required drafts; Owner sends directly.
- Connector list, settings, logs and delete actions match Hermes/Webhook UI conventions.
- Unit/integration tests cover auth token generation, reconnect, replay/ack and send correlation.
