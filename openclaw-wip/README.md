# MailMoose ↔ OpenClaw connector

Status: **implemented**. The design in this folder led to the shipped OpenClaw
connector. The MailMoose side is live in the main tree; the OpenClaw plugin
lives in `openclaw-plugin/` and is intended for ClawHub/npm publication.

## What shipped

- **OpenClaw is a connector kind, not a second transport.** It reuses the
  authenticated, replayable relay (`GET /relay`, `POST /relay/enroll`) used by
  Hermes while remaining distinct in the product model
  (`clients.type = 'openclaw'`). See decision `D074` in
  `../docs/DECISIONS.md`.
- **Persistence.** Migration 042 widens the `clients.type` CHECK and adds
  `hermes_enroll_tokens.kind`; `internal/store/relay.go` is kind-aware
  (`RelayKind`, `CreateRelayEnrollToken`, `ListRelayConnections`).
- **Admin API.** `/v1/admin/openclaw` list/enroll/setup-code/update/delete,
  mirroring the Hermes routes.
- **UI.** OpenClaw appears beside Hermes and Webhook in the connector picker and
  the inbox connector list, with the same Settings, Log and delete actions and
  the same no-allow-list warning.
- **Onboarding.** The UI mints a 15-minute single-use setup code and shows
  `openclaw channels add --channel mailmoose --code <setup-url>`. The setup URL
  carries the MailMoose address in its origin and the code in its fragment. A
  manual `channels.mailmoose` config block remains for air-gapped installs.

## OpenClaw plugin

`openclaw-plugin/` is a channel plugin built on OpenClaw's plugin SDK 2026.9.7:

- one relay socket per account (`gateway.startAccount`) with reconnect/backoff;
- HMAC upgrade token matching `internal/hermesrelay/server.go`;
- inbound email dispatched through the shared OpenClaw turn kernel
  (`channelRuntime.inbound`), with the ack sent only after dispatch accepts the
  event, so a crash replays it;
- replies sent over the same socket, addressed as `thread:<thread_id>`;
- `src/setup.ts` redeems the one-time code at `POST /relay/enroll`.

Build and install proof:

```bash
cd openclaw-plugin
npm install
npm run build
npm pack --pack-destination /tmp
openclaw plugins install npm-pack:/tmp/mailmoose-openclaw-0.1.0.tgz --accept-capabilities
openclaw plugins inspect mailmoose --runtime --json
```

Unit tests: `npm test` (16 tests covering target grammar, token format, account
resolution and setup-code claim).

## Security properties

- OpenClaw never needs a publicly reachable webhook.
- The relay secret is encrypted at rest by MailMoose and shown once.
- Upgrade tokens are short-lived and HMAC-authenticated.
- Email From stays untrusted: the plugin dispatches senders as external input
  and MailMoose's inbox allow list is the primary gate (accepted risk `D058`).
- Deleting the connector closes its live socket; reconnect replays from the last
  acknowledged event.
