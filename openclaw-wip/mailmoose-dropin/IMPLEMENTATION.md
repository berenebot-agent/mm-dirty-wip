# MailMoose-side implementation notes

These files are staged only. Do not apply them to main until the OpenClaw plugin's inbound adapter is proven against the pinned OpenClaw SDK.

## Recommended implementation

Treat OpenClaw as a **new connector kind backed by the existing relay transport**.

This keeps the user-visible model correct without duplicating WebSocket/auth/replay code.

## Current MailMoose seams to update when promoted

### 1. Persistence

Current relay persistence is Hermes-named. For the smallest safe change:

- add `connector_kind` to `hermes_connections`
- existing rows default to `hermes`
- accepted values initially: `hermes`, `openclaw`
- keep all existing connection ids, encrypted secrets, ack cursors and delivery logs

A later cleanup can rename the table/types to `relay_connections` if desired. Do not combine the rename with the first OpenClaw feature unless there is a compelling reason.

See `001_openclaw_connector_kind.sql`.

### 2. Relay server

Current file: `internal/hermesrelay/server.go`.

Prefer making the existing relay server connector-neutral internally rather than adding `/openclaw-relay`.

No new public route is needed:

```text
GET /relay
```

The current wire contract already fits the OpenClaw plugin:

- hello
- descriptor
- inbound + bufferId
- inbound_ack
- outbound send
- outbound_result
- ping

The only material server-side distinction is the stored connector kind used by admin/UI presentation and connector-specific generated setup instructions.

### 3. HTTP server

Current file: `internal/httpapp/server.go`.

Keep:

```text
POST /relay/enroll
GET  /relay
```

Do not add an inbound OpenClaw webhook.

If OpenClaw gets separate admin routes for clarity, they should operate on the same relay service/store boundary, for example:

```text
POST   /v1/admin/openclaw
PUT    /v1/admin/openclaw/{id}
DELETE /v1/admin/openclaw/{id}
```

A generic relay-connector API is also reasonable, but avoid a broad API refactor solely for this feature.

### 4. Connector create/edit UI

Current files:

- `internal/httpapp/ui.go`
- `internal/httpapp/assets/app.js`

Add `openclaw` beside `hermes` and `webhook` in the existing connector picker.

Expected flow:

```text
Add Connector
  → OpenClaw
  → choose inbox
  → choose outbound role (Owner / Assistant)
  → create
  → show plugin install command + generated config
```

The Add Client quick-start should also list OpenClaw and hand off to this same connector flow.

### 5. Inbox connector list

Render OpenClaw as its own connector chip/row kind:

```text
Hermes
OpenClaw
Webhook
```

It must use the same Settings / Log / red-X actions already used by other inbox connectors.

### 6. Generated setup

After creating the connector, show:

```bash
openclaw plugins install clawhub:mailmoose
```

and a copyable config block generated from the connection credentials.

`openclaw_bootstrap.go` contains a small helper suitable for moving into `internal/httpapp`.

### 7. Logs

Reuse the current connector delivery log and cursor/attempt semantics. OpenClaw should not get a second logging system.

### 8. Outbound permissions

Reuse the existing relay role semantics:

- **Owner** → direct MailMoose send/reply
- **Assistant** → draft + approval request

The OpenClaw connector should show the same sender-allowlist risk warning currently shown for Hermes.

### 9. Documentation

When promoted, update:

- `README.md`
- `docs/API-REFERENCE.md`
- `docs/ARCHITECTURE.md`
- `docs/DECISIONS.md`

Suggested decision wording:

> OpenClaw is an inbox-level outbound relay connector. It reuses the authenticated replayable Relay transport used by Hermes but remains a distinct connector kind in the product model. The OpenClaw host initiates all connectivity to MailMoose; no inbound OpenClaw webhook is required.

## Tests required before merge

1. Existing Hermes rows migrate to `connector_kind=hermes`.
2. OpenClaw creation cannot bind to another account's inbox.
3. Generated OpenClaw config contains the correct base URL, gateway id and secret.
4. Secrets never appear in list/detail/log responses after initial creation.
5. OpenClaw relay auth rejects bad HMAC and expired tokens.
6. Unacked event replays after disconnect.
7. ACK advances the durable cursor.
8. Delete closes the live OpenClaw socket.
9. Assistant role creates an approval-required draft.
10. Owner role replies on the source email thread.
11. Connector chips/settings/log/delete distinguish Hermes from OpenClaw.
12. Existing Hermes connector behavior remains unchanged.
