# Changelog

All notable changes to MailMoose are documented here. This project follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- Copy buttons in the web interface are hidden when the interface is served over
  plain HTTP. A browser only exposes the clipboard API in a secure context
  (HTTPS or `localhost`), so a copy that cannot work is no longer offered — the
  value stays visible and selectable by hand.
- The shared copy message is now shown in front of an open dialog's backdrop
  instead of behind it.
- Passkey controls are hidden when the interface is served over plain HTTP. A
  browser only runs a WebAuthn ceremony in a secure context (HTTPS or
  `localhost`), so a passkey sign-in that cannot work is no longer offered, and
  the account page no longer explains the requirement in place of the
  controls. Registered passkeys, and the option to re-enable password sign-in,
  stay reachable.

### Fixed

- The inbox settings button no longer jumps between the far left of the mailbox
  toolbar and the right-hand button group. The Outbox has no bulk actions, so its
  settings button was the only control on that toolbar and rendered alone on the
  left, while every other mailbox view put it in the button group on the right.
  It now closes the button group on every mailbox view.

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

[Unreleased]: https://github.com/dellarb/mailmoose/compare/v0.1.0-beta.1...main
[0.1.0-beta.1]: https://github.com/dellarb/mailmoose/releases/tag/v0.1.0-beta.1
