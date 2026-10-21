# Changelog

All notable changes to MailMoose are documented here. This project follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- The Dial MX `receiver_urls` receiving field is documented as what it is: a
  comma-separated string of HTTPS base URLs, not an array. The reference named
  the field without its type, so a caller sending an array got a `400` whose
  message was the only place the required shape appeared.
- An account Admin key now holds the domain-wide sending grant it was already
  advertised: it may send as any address on a domain, but only from that
  domain's enabled catch-all inbox. `GET /v1/bootstrap` reports
  `send_any_in_domain` only where the send is honoured, and always with
  `send_any_domain` naming the domain.
- The dashboard's "Recent messages" list shows each outbound message once,
  with its current delivery status. The delivery log records every state
  transition of a send, so one message contributed two or more rows that read
  as repeated sends; the rows are now collapsed to the message's latest
  transition, and the row says whether it was sent, failed, is in flight or was
  interrupted, with the attempt count when the delivery took more than one
  attempt. The per-attempt history is unchanged on the domain's activity log.
- `GET /v1/messages` now honours the `folder` filter for a domain inbox, the
  same way the inbox-scoped route does. The filter was accepted and ignored, so
  the whole inbox came back instead of the folder's messages.
- Inbound mail refused because the account or the inbox storage quota is full
  now leaves a trace in the product. The refusal appears in the domain's
  receiving log (`GET /v1/admin/domains/{id}/receiving/deliveries`) and on the
  dashboard as a `blocked` row with the reason `storage quota exceeded`, so an
  operator can see mail being turned away by a full mailbox instead of finding
  it only in the container log. The refusal itself is unchanged: the sender
  still retries, and the mail is still delivered once there is room.

### Changed

- The Remote SMTP receiving guide now states what a reverse proxy in front of a
  separate receiver has to do: the session endpoint speaks HTTP/2 only, the
  request body must be streamed rather than buffered, and a cleartext receiver
  admits sessions only from a trusted proxy peer.
- The guide now names the route to an installation-wide remote receiver: the
  **Settings → Admin → MX receiver** card, or `PUT /v1/admin/mx` with a
  logged-in system administrator's session. The account's own receiver is
  configured in that account's Remote MX panel.

### Added

- The Admin page has an **MX receiver** card: the installation's shared
  direct-SMTP receiver can now be added in either mode — the receiver bundled
  with the deployment, or one the operator runs themselves (URL and bearer key).
  The mode was reachable only through `PUT /v1/admin/mx`, while the Compose
  files and the environment example already told operators to set it in the web
  interface.

## [0.1.0-beta.2] - 2026-10-20

- Release polish to address full github publish workflow.

## [0.1.0-beta.1] - 2026-10-20

Initial release of MailMoose: self-hosted email infrastructure for people and
AI agents. Run it as one container, manage inboxes on your domains or connect
existing mailboxes, and give agents scoped access to read and send mail—with
approval controls when you need them.

Includes a web interface and REST API; message search, threads, labels and
attachments; inbound and outbound email integrations; durable events; Hermes
Relay; and an OpenClaw connector (early alpha).

### Upgrade notes

- This beta starts from a clean database baseline. Existing pre-beta databases
  are not migrated automatically.
- The default Compose file leaves host port 25 commented out. Enable the
  `25:2525` mapping only when using an Included MX receiver.
- If `APP_ENCRYPTION_KEY` is unset, the first start creates a key file under
  `DATA_DIR`. Back up that key with your data, or set your own key before adding
  provider credentials.

Behaviour and APIs may change between beta releases.
