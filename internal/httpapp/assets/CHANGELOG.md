# Changelog

All notable changes to MailMoose are documented in this file. The format is
based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the
project aims to follow [Semantic Versioning](https://semver.org/spec/v2.0.0/).

## [Unreleased]

### Added

- Trash: deleting a message moves it to Trash instead of erasing it. Trashed
  messages are hidden from lists, search, threads and unread counts but keep
  their raw MIME, attachments and storage accounting. `POST
  /v1/messages/{id}/restore` returns a message to the mailbox; `DELETE
  /v1/messages/{id}/purge` erases a trashed message permanently; `POST
  /v1/inboxes/{id}/trash/empty` empties an inbox's Trash; `DELETE
  /v1/outbox/{id}` now moves a pending/failed send to Trash. The list filter
  `trashed=true` selects the Trash view.
- Per-account trash retention: `GET`/`PATCH /v1/account/settings` reads and
  writes `trash_retention_days` (default 0, meaning keep trash until emptied by
  hand). The maintenance sweep purges trashed messages older than the window and
  unlinks their raw files.
- Durable `message.trashed`, `message.restored` and `message.purged` events
  (streamed over SSE/long-poll; not relayed over Hermes).
- Trash UI: a Trash folder with Restore and Delete forever actions, an Empty
  trash button, and a Trash retention field on the Account page.
- Time zone display preference: an account default and a per-user override on
  the Account page, chosen from the full IANA list with type-ahead. `GET`/`PATCH
  /v1/account/settings` reads and writes the account `timezone` (an IANA name;
  empty means UTC). Stored and API timestamps remain UTC; the setting changes
  only how times are shown in the web interface. The IANA database is embedded
  via the Go standard library's `time/tzdata`, so conversions work on hosts
  without a system tzdata tree.

### Changed

- Mailbox views now use a left-hand sidebar: Compose stays at the top and the
  folders (Inbox, Drafts, Sent, Outbox, Trash, Spam) run down the side with
  their counts. The sidebar stacks above the content on narrow screens.
- Right-hand mail actions are now icons: mark read / mark unread, move to
  trash, restore and delete forever, in both the message list and the message
  view. The duplicate Trash folder tab is also removed.
- The mailbox sidebar lists the inbox's labels under a Labels heading (between
  Outbox and Trash), each with an unread count; selecting a label filters the
  message list to messages carrying it. Reply, Reply all and Forward are now
  icons in the message view (Reply all pre-fills the original sender and the
  other recipients, excluding the mailbox's own addresses). The inbox address in
  the header is click-to-copy and shows a
  brief "Copied to clipboard" confirmation that clears itself.
- Labels may no longer contain `/` or `\` (path separators); existing labels
  with those characters still render and filter. The web UI now uses the full
  window width instead of a fixed 1180px column.

- Removing an inbox now opens a confirmation dialog that displays the full
  email address and requires typing it back, matching the domain delete flow.
  Removing a client opens a dialog that names the client and type and requires
  an explicit confirmation click.

- Removed the dormant `messages.is_archived` column and the `archived` field on
  `PATCH /v1/messages/{id}`; archive is superseded by Trash.

### Added

- Inbound messages persist the transport-supplied SMTP envelope sender
  (`envelope_from`) alongside the canonical original envelope recipient
  (`envelope_recipient`); both are exposed on the normalized message. A missing
  sender stays empty and is never inferred from the MIME `From` header.
- Forward webhooks carry the original envelope metadata in
  `X-MailMoose-Envelope-From` / `X-MailMoose-Envelope-To` (RFC 3986
  percent-encoded UTF-8), with the raw MIME body unchanged.
- System administrator, account Admins, and non-admin mailbox operators. The
  system administrator (configured with `ADMIN_EMAIL` / `ADMIN_PASSWORD`) has an
  Admin page listing accounts, from which they invite a new person as a separate
  account. An account Admin manages **mailbox operators** (Owner of selected
  inboxes) and the account's **mailer** on the Account page; invitees set their
  own password from a single-use, expiring link that is sent through the normal
  outbound queue or copied directly.
- Per-account mailer: each account sends its invitations from one of its own
  mailboxes, so no account ever sends from another's.
- The system administrator's Admin page shows each account's storage usage and
  quota and can edit the quota, with `0` meaning unlimited.
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
  login is preserved. If `ADMIN_EMAIL` already belongs to an existing user, that
  user is adopted in place and forced to account Admin and system Admin rather
  than failing startup.
- The system administrator's Admin page lists accounts (and pending
  new-account invitations) instead of a raw invitation table, matching the
  mailbox-operator UX; operator invitations no longer appear there.
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
- Webhook delivery now skips mail that is currently Spam, internal, or has since
  been deleted, recording a terminal `skipped` delivery and advancing the cursor
  so such an event cannot block the head of a client's queue; releasing a
  message from Spam makes it deliverable again.
- A delivery attempt is recorded in the sending log when it starts, before the
  provider call, so an interrupted send (restart, crash or dropped connection)
  is visible as `Sending…` or `Interrupted` instead of leaving the message
  silently looping as pending with an empty log. The outbox shows an in-flight
  message as `Sending…`.
- Direct MX delivery no longer applies a single 45-second deadline to the whole
  SMTP transaction, so a large message is not cut off mid-upload; and a message
  the remote accepted is not reported as failed because the follow-up `QUIT`
  did not complete.

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
