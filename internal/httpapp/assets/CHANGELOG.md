# Changelog

All notable changes to MailMoose are documented in this file. The format is
based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the
project aims to follow [Semantic Versioning](https://semver.org/spec/v2.0.0/).

## [Unreleased]

### Added

- System administrator, account Admins, and non-admin mailbox operators. The
  system administrator (configured with `ADMIN_EMAIL` / `ADMIN_PASSWORD`) has an
  Admin plane to select the system-mailer mailbox, invite a new person as a
  separate account, or invite a mailbox operator with Owner access to selected
  inboxes; invitees set their own password from a single-use, expiring link that
  is sent through the normal outbound queue or copied directly.
- Self-hosted Direct MX outbound delivery with DNS MX resolution, opportunistic
  STARTTLS, public-destination enforcement, and single-recipient delivery.

- Agent discovery surface: `/.well-known/mailmoose`, `/agent`, `/docs`,
  `/openapi.json` (request, response and query schemas plus a machine-readable
  `x-required-role`), `/v1/limits`, `/v1/health`, `/changelog`, and served
  Bash, Python and curl example clients.
- `GET /v1/messages/wait` and `GET /v1/events/wait` long-polling with durable
  `evt_` cursors.
- External sending aliases with per-alias sending connectors and display names.
- One-shot draft writes with inline and multipart attachments, plus a
  human-in-the-loop send-approval workflow including email approvals.
- Direct SMTP (MX) ingress as an embedded edge or a separate container, with
  SPF, DKIM and DMARC verification.

### Changed

- The first-run administrator configuration `INITIAL_ADMIN_EMAIL` /
  `INITIAL_ADMIN_PASSWORD` (`_FILE` supported) is renamed to `ADMIN_EMAIL` /
  `ADMIN_PASSWORD` and is now deployment-authoritative: while set it rotates the
  system administrator's stored login on every start; when unset the stored
  login is preserved.
- The dashboard inbox and client edit buttons now use a gear (settings) icon,
  an admin-only gear shortcut in the inbox view opens that inbox's settings
  directly, and action icons in the dashboard tables and activity links are
  slightly larger.
- Renamed Gatehouse Mail to MailMoose. This is breaking: the module path,
  binary and image names, and the discovery path
  (`/.well-known/gatehouse` → `/.well-known/mailmoose`) all changed.
- Send responses report `queued: false` once `?wait=true` has resolved.

### Fixed

- `/v1/messages/wait` no longer replays history: with no cursor it blocks for
  new mail, and a malformed cursor is rejected with `400`.
- Empty-subject sends are rejected with a descriptive `400` instead of failing
  later at the provider.
- `limit` values below 1 and non-integer `limit`/`timeout` query values are
  rejected with `400` instead of being silently defaulted.
- Reply threading matches the provider's wire Message-ID, so replies join the
  original conversation even when the provider rewrites `Message-ID`.
- Outbox delivery panics are contained instead of crashing the worker.
- Non-UTF-8 encoded message headers are decoded correctly.

### Security

- The events feed (`/v1/events`, `/wait`, `/stream`) withholds assistant-scoped
  approval fields from principals that can only read the inbox.
- MX approval control mail is deduplicated by a durable receipt, so a
  byte-identical signed replay cannot re-run an approval decision.
- MX approvals require the edge's trusted SPF/DKIM/DMARC evidence for the
  sender's domain.
- `TRUST_PROXY_HEADERS=true` and catch-all `/0` `TRUSTED_PROXIES` entries are
  refused at startup, since they let any caller choose its own rate-limit
  identity.
- Stored raw-MIME and attachment paths are containment-checked before any open,
  read or remove.
- Event streams and long-polls are bounded per credential.
- Unrecognised storage-engine and filesystem errors answer a generic `500`
  instead of leaking internal detail, and `Strict-Transport-Security` is sent
  when `FORCE_HTTPS=true`.
- `FORCE_HTTPS` redirects plaintext requests to HTTPS and reports HTTPS in
  discovery documents; only trusted proxies may assert `X-Forwarded-Proto`.
- MX edge credentials are 256-bit.

[Unreleased]: https://github.com/dellarb/mailmoose/commits/main
