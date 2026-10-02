# MailMoose ↔ OpenClaw connector

Status: **implemented and verified end-to-end** against a live MailMoose and a
real OpenClaw Gateway. The MailMoose side is in the main tree; the OpenClaw
plugin lives in `openclaw-plugin/` and is **not yet published** to npm/ClawHub,
so this directory is currently the only source for it.

## Layout

```
plugins/openclaw/
  README.md            — this file
  openclaw-plugin/     — the channel plugin (TypeScript, @mailmoose/openclaw)
```

A local compose rig for running the plugin against a live MailMoose lives
**outside this repo**, at `test/openclaw/` in the project folder — it holds a
`.env` with a live provider key and is not a shipped artifact.

## What shipped

- **OpenClaw is a connector kind, not a second transport.** It reuses the
  authenticated, replayable relay (`GET /relay`, `POST /relay/enroll`) used by
  Hermes while remaining distinct in the product model
  (`clients.type = 'openclaw'`). See decision `D074` in
  `../../docs/DECISIONS.md`.
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
- HMAC upgrade token matching `internal/hermesrelay/server.go` (repo root).
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

Two caveats verified on a real install — both are open gaps, not documentation
subtleties:

- **Install from a local path needs `--force`.** `openclaw plugins install -l
  <dir> --accept-capabilities` alone is refused with *"Install cancelled; rerun
  with --force after reviewing the source"*, because a local path is outside
  ClawHub review. `npm-pack:` installs do not need it.
- **`openclaw channels add --channel mailmoose` only works once the plugin is
  installed.** The stock CLI validates `--channel` against a built-in list that
  does not contain `mailmoose`, so the install must come first. Ordering
  matters and is easy to get wrong from the connector UI's command alone.

Unit tests: `npm test` (16 tests covering target grammar, token format, account
resolution and setup-code claim). The SDK requires **Node >= 24.16 < 25 || >= 26.1**
— Node 22 fails at `openclaw`'s preinstall, and the published OpenClaw images
already satisfy this.

## Security properties

- OpenClaw never needs a publicly reachable webhook.
- The relay secret is encrypted at rest by MailMoose and shown once.
- Upgrade tokens are short-lived and HMAC-authenticated.
- Email From stays untrusted: the plugin dispatches senders as external input
  and MailMoose's inbox allow list is the primary gate (accepted risk `D058`).
- Deleting the connector closes its live socket; reconnect replays from the last
  acknowledged event.
