# Changelog

All notable changes to MailMoose are documented here. This project follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0-beta.1] - 2026-10-20

First public beta of MailMoose: self-hosted email infrastructure for people and
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
