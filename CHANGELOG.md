# Changelog

All notable changes to MailMoose are documented here. This project follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

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

### Changed

- The Remote SMTP receiving guide now states what a reverse proxy in front of a
  separate receiver has to do: the session endpoint speaks HTTP/2 only, the
  request body must be streamed rather than buffered, and a cleartext receiver
  admits sessions only from a trusted proxy peer.
- The guide no longer tells an operator to pick a Remote receiver from an
  installation setting that the web interface does not offer: an
  installation-wide remote receiver is configured with `PUT /v1/admin/mx`, and
  the account's own receiver is configured in the Remote MX panel.

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
