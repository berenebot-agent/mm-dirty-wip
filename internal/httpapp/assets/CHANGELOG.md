# Changelog

All notable changes to MailMoose are documented in this file. The format is
based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the
project aims to follow [Semantic Versioning](https://semver.org/spec/v2.0.0/).

## [Unreleased]

### Performance

- Standalone folder navigation reads cached messages immediately. Folder API
  listings return the cached tree and indexed counts without waiting for IMAP;
  initial discovery and progressive backfill run in coalesced background work.
  API message/thread reads retain first-use indexing and explicit remote refresh
  still waits for synchronization. Loading spinners identify navigation and
  background sync; read-only IMAP selection is reused within a session, and
  polling-client cancellation no longer interrupts durable arrival persistence.

### Added

- **Standalone mailboxes.** An inbox now has a `kind` (`domain` or
  `standalone`). A standalone mailbox owns an address independent of any managed
  domain and connects to an existing mailbox through a per-inbox remote IMAP
  (and optional SMTP) connector, with credentials encrypted under
  `APP_ENCRYPTION_KEY`. Transport security defaults to TLS; STARTTLS and
  **plain** are explicit operator choices (warned in the UI), and a connector
  never silently downgrades. Message/thread **metadata** is cached locally while
  bodies and attachments are fetched live and never archived. A standalone inbox
  is a first-class mailbox: it has the same folder tree (with role mapping that
  a reconcile never resets), labels, threads, search, replayable events, API
  keys and the same `/v1/inboxes/{id}/…` common surface as a domain inbox,
  dispatched through one local/remote boundary. Its own remote SMTP binding
  carries outbound send, and a durable job copies sent mail into its remote Sent
  folder (toggle-able, independently retried, never a re-send, and owning an
  independent frozen copy of the raw MIME). The common mailbox model adds public
  folder (with archive/outbox roles), remote-locator, listing-envelope (opaque
  cursor, result-set completeness independent of pagination, per-inbox errors)
  and normalized-error types. Remote indexing is **progressive** via a persisted
  per-folder backfill cursor, so an ordinary large folder reaches `complete` over
  successive passes (the 2000-message batch is a per-pass bound, not a cap); the
  one hard limit is the **500k-UID snapshot ceiling**, above which a folder is
  indexed newest-window-only, un-pruned and reported `partial`, never falsely
  complete. Account-wide listings merge the local store and remote inboxes into
  one globally ordered stream. See `docs/DECISIONS.md` D097 and
  `docs/MAILBOX_SERVICE_CONTRACT.md`.
- **Assistant authoring modes.** A request to send a draft is resolved per the
  inbox's authoring mode, snapshotted onto each request. **MailMoose approvals**
  (the domain inbox's preset) is the in-product approval workflow with
  tokenized email approval — a domain inbox cannot be switched to remote draft;
  the mode selector is not offered for one. **Remote draft handoff** (the
  standalone-inbox
  default) places the frozen draft one-way in the connected mailbox's remote
  Drafts folder for a human to send from their own client; MailMoose never sends
it, and a token-free notification tells the human it is waiting. Publication,
notification and the Sent-copy advance as separate state machines, and an
unverifiable remote append is reported ambiguous rather than retried blindly.
A handoff notification (and the frozen handoff draft echoed back) is excluded
from remote detection by a durable correlation lookup. Handoffs are inspectable
through `GET /v1/inboxes/{id}/handoffs` (terminal records retained after the
local draft is cleaned up) and can be explicitly cancelled while still pending
or explicitly re-requested; an ambiguous append is never retried automatically.
The authoring settings report the effective mode, whether the inbox is
standalone, and whether the approver is enabled for the effective mode. See
`docs/MAILBOX_SERVICE_CONTRACT.md`.
- **A common folder tree and labels for every inbox.** Both inbox kinds now use
  one hierarchical folder model with well-known roles (Inbox, Sent, Drafts,
  Archive, Outbox, Spam, Trash) and a single-folder message membership
  (`message.folder_changed` event). A role can be mapped to an arbitrarily-named
  provider folder and is persisted so a later sync never re-infers it. Folders
  are managed through the common API and UI; labels remain free-text metadata,
  distinct from folders.
- The Drafts folder now has the same checkbox selector and bulk bar as the other
  folders, so drafts can be deleted in bulk. Every mailbox folder (and Drafts)
  also gains a Gmail-style **select all N** escape hatch: checking the header box
  on a paginated folder offers to widen the selection to every matching item, and
  the bulk action then runs against the whole folder rather than just the page.
  The folder set is re-derived on the server from the folder and label, so the
  operation covers exactly what the banner promised, and a destructive bulk action
  over the whole folder names the full count in its confirmation. The Drafts list
  is now keyset-paginated like the other folders.
- SendGrid and Postmark can now receive mail (D096). Both deliver full raw MIME
  into the existing inbound pipeline, so no sending adapter is added. **SendGrid**
  verifies the Inbound Parse ECDSA webhook signature with the domain's public key
  before staging the raw `email` part; its envelope sender is signature-attested.
  **Postmark** is protected by HTTP Basic credentials MailMoose generates per
  domain and shows once as a ready-to-paste webhook URL, and it preserves binary
  `RawEmail` content byte-for-byte. Configure either under **Receiving** on the
  domain page; setup guides are in `docs/SENDGRID.md` and `docs/POSTMARK.md`.
  Postmark's unattested envelope sender is added to the accepted-risk note in
  `SECURITY.md`.
- Dial MX / Antler MX receiving now shows one status light per receiver. Each
  receiver proves both your domain's `_mailmoose-mx` TXT authority and that it is
  named in your MX records, so a receiver that is authorized but not routed
  reports **not in MX** instead of a separate DNS check (D095). A missing record
  is fixed inline in the receiving dialog — the exact record with a copy button,
  with the live check flipping it green as DNS propagates — instead of a separate
  repair step. The dashboard's aggregate light is green as soon as any receiver is
  fully ready.
- The web UI updates live without a manual refresh (D094). Open dashboard and
  inbox pages subscribe to a session-authenticated event stream and refresh the
  unread/pending-send counts, the inbox folder and label badges, and the Dial MX
  traffic lights as mail arrives, is read or moved, or as a receiver's readiness
  changes. Marking a message read/unread now emits a durable `message.state_changed`
  event, and a receiver status change is pushed as a transient `mx.health_changed`
  notification, so one tab (or an agent) keeps every other tab current. The tab
  closes the stream after 60 seconds hidden and reopens it on focus.
- The inbox message list and the draft send-requests card refresh in place as
  events arrive, so new mail and approvals appear without a reload. This is a
  deliberately conservative background refresh: counts and badges always update,
  but the list itself is only swapped when you are at the top of the page with
  nothing selected, no dialog open and no field focused, so it never jumps under
  you while reading or composing. An unchanged list is left untouched (D094).
- Inbox settings gain a **Clients & Access** tab (account Admin only) listing the
  API keys and mailbox users with access to that inbox, plus its pending
  invitations. An Admin can create a new inbox-scoped API key, grant an existing
  key or user access, change a key's role inline (Read/Assistant/Owner), remove a
  single inbox binding without revoking the key, invite a person, and revoke a
  pending invitation. Account Admin keys appear read-only (D089). Inviting an
  address that already belongs to an account member grants access directly
  instead of erroring; the administrator, an address already on the inbox, and a
  duplicate pending invitation each get a specific message. Role changes are
  staged and committed by the footer Save, alongside every other tab's edits in
  a single request.

### Fixed — release follow-up

- Deleting, restoring, purging or marking spam on a **standalone** inbox's
  message from the session UI no longer fails with "message not found". A
  standalone inbox's messages are cached remote metadata, not local rows, so the
  `/ui/messages/{id}/delete|restore|purge|spam` actions now resolve an opaque
  remote id to its owning inbox and drive the live remote folder (Trash, Inbox or
  Spam role) — matching the per-row actions the bulk bar and REST API already
  used. A genuinely unknown id still answers 404.
- The Dial MX / Antler MX receiving dialog no longer opens with every connector
  showing a "Pending" light when the dashboard already shows green. The live
  per-receiver statuses the dashboard light is computed from are embedded on the
  receiving form, so the status table opens already matching the light that was
  clicked, then reconciles on the immediate check.
- Outbound provider HTTP requests use a configurable overall timeout
  (`OUTBOUND_HTTP_TIMEOUT_SECONDS`, default 300s) instead of a hard-coded 30s, so
  large attachment batches no longer fail with a client timeout while awaiting
  response headers (D092).
- The outbox delivers concurrently through a bounded sender pool
  (`OUTBOUND_CONCURRENCY`, default 5, max 32), so a slow attachment send no longer
  stalls the rest of a batch (D093).
- Outbound delivery no longer reads the whole stored message into memory for HTTP
  providers; only raw-MIME transports (SMTP, Direct MX) load it, and HTTP adapters
  reconstruct just the attachment bytes they send (D093).
- A provider HTTP timeout while awaiting headers is now treated as an ambiguous
  send and failed terminally for Brevo and Mailgun (which have no idempotency
  key), preventing a retry from double-delivering; Resend keeps its idempotent
  retry (D092).
- SMTP and Direct MX DATA waits observe cancellation; slow webhook attempts
  have independent budgets and durable retry outcomes.
- Session-expiry login restores compose text, sender and uploads across sign-in
  methods, including after a failed password attempt. Expired-session upload
  parsing is size-bounded.
- Mailgun URL-encoded MIME streams to disk, and Resend rejects recipient
  overflow before signature-secret lookup instead of silently dropping recipients.
- SPF alignment remains available without a DMARC policy. Email-approved sends
  count toward account limits; idempotent replays do not.
- Provider errors use fixed actionable diagnostics without raw response text.
  UI flash errors are redacted; panic diagnostics contain types rather than values.
- Thread, outbox and send responses use sanitized HTML while preserving CID
  attachment references. Passkey partial-failure warnings remain visible.
- Receiver URL uniqueness is case-insensitive (migration 050); migration FK
  restoration failures discard the connection and fail startup.
- Deployment and connector instructions reflect explicit port-25 publication
  and removal of the unused delivery key.
- Dial MX receiver: a failed domain proof now cools down only the domain that
  failed, not the whole source IP, so a transient DNS or key failure on one
  domain no longer denies registration of a core's other domains. The refusal
  is a distinct `domain_cooldown` reason that the core surfaces as the amber
  "waiting to retry" state rather than red "rejected" (D091).
- Antler MX setup wizard: finishing no longer bounces back to the receivers step
  when no receiver is authorized at that moment, which could happen while a DNS
  record was still catching up even though the receivers showed green. Once the
  operator is past the receiver step, Finish saves and opens the status view,
  which reports the receiver authorizing/reconnecting until it reconnects —
  matching the key-rotation and DNS-repair flows.

### Changed

- Account and system-admin settings collapse into one **Settings** modal opened
  from the header, replacing the separate Account and Admin links. It reuses the
  inbox-settings dialog chrome and has URL-addressable **Personal**, **Account**
  and **Admin** tabs (each shown only to those permitted: everyone, account
  Admins, system Admins), so the former `/account` and `/admin` pages remain
  reachable and bookmarkable as tabs of a single modal. The Personal and Account
  tabs present their fields as horizontal rows with one **Save** in the footer,
  and each tab's edits are committed in a single request.
- Inbox settings: the first tab is renamed **Identity** (was Basic) and the
  **Clients & Access** tab moves up beside it. The footer Save now commits every
  tab's staged edits (including Clients & Access role changes) in one request,
  and the allow-list hint no longer states that the approver is always allowed.
- Outbound destination policy: `ALLOW_PRIVATE_OUTBOUND` now defaults to `true`,
  so a self-hosted instance may send through a private gateway, local relay or
  LAN MX receiver without extra configuration. A hosted operator that must
  confine outbound traffic to the public internet sets
  `ALLOW_PRIVATE_OUTBOUND=false`; that global policy now also governs the
  per-account Remote MX receiver, so a tenant's `allow_private` opt-in is
  honoured only when the operator allows private outbound (D086, amends D028).
- Dial MX / Antler MX setup: the published MX and `_mailmoose-mx` TXT checks now
  resolve DNS live on every refresh (bounded by a four-second timeout) and are no
  longer cached in the core. Editing a TXT record is reflected on the next poll or
  **Check now** instead of waiting out a one-minute TTL, so the domain-auth
  traffic light and the wizard agree with what the receivers actually resolve.
- Receiver logging: a domain proof attempt now emits a single INFO
  `dialmx domain auth` record (terminal phase, result, reason, duration and a
  folded `steps` string) instead of one INFO `dialmx domain proof` record per
  step. The per-step records remain available at `DIALMX_LOG_LEVEL=debug`, so the
  default INFO stream is one line per domain authentication or renewal.
- SMTP edge logging: `mx auth verification completed` is folded into
  `mx auth evidence` as its `duration_ms`, and that single record is now INFO, so
  each message logs one auth line rather than two.
- Inbound log coverage: mail rejected by an inbox's allowed-senders or
  authenticated-sender rule, and approval control mail consumed to drive a send
  decision, now each emit one terminal INFO line (`inbound blocked`,
  `inbound control mail`) on the container stream. Neither outcome leaves a
  message row, event or relay delivery, so previously a blocked or control
  message produced no record at all; both are logged on every retry.

### Changed

- The build-from-source `docker-compose.yml` (and the hardened
  `docker-compose.advanced.yml`) no longer publish host port 25 by default;
  uncomment the `:25` mapping only when the deployment is an Included MX
  receiver reached from the internet.
- Provider credentials (domain/alias sending and receiving configs, account and
  installation Remote MX, and the Dial MX private seed) are now bound to their
  owning row with AES-GCM additional authenticated data, so a ciphertext copied
  to another row fails to decrypt. Existing rows keep working via a versioned
  envelope with a legacy fallback.
- Direct-SMTP edge: single-mode receivers now log a startup warning when they
  accept cleartext from a non-loopback address (the bearer key and mail are then
  unencrypted), and `DIALMX_REQUIRE_TLS=true` opts into accepting cleartext only
  from loopback or `DIALMX_TRUSTED_PROXIES`. Cleartext stays allowed by default
  for self-hosting.
- Remote MX receiver URLs (installation and per-account) now share one policy:
  `http`/private/LAN origins are allowed by default and held to `https` plus
  public-routable only when the operator sets `ALLOW_PRIVATE_OUTBOUND=false`.

### Changed

- The relay connector no longer mints or returns a `deliveryKey`: it was stored
  and shown but never validated. The Hermes `.env` block, OpenClaw config block,
  enroll response, and the OpenClaw plugin schema/setup no longer carry it.
- The single-message and search JSON API endpoints now return the HTML body
  sanitised with the same policy as the web UI, so an agent/LLM consumer cannot
  receive script-bearing email markup.
- UI error responses redact internal engine/filesystem fault text while keeping
  deliberate validation messages, so no page leaks database or path detail.
- The encryption-key derivation cache is keyed by a hash of the input and capped,
  so the raw secret is not retained as a map key and the cache cannot grow
  without bound.

### Removed

- **External sending aliases.** The send-only identities on domains MailMoose
  does not manage are removed entirely: the store table, model, service, HTTP/UI
  and API specification are deleted, with no compatibility shim. A migration
  (053) discards their unsent drafts, attachments and queued sends and jobs and
  refunds the account and inbox storage counters, drops the attribution columns,
  and removes orphaned threads; sent history and its `from_address` attribution
  are kept, and managed-domain aliases and ordinary mail are untouched. Raw
  files are retired after the migration commits via a durable cleanup queue.
  Sending now resolves only to the inbox primary or a managed alias.

### Fixed

- Session expiry during a form submission: a cookie-authenticated POST with an
  expired session now returns to login with a "session expired" notice, and a
  compose/reply/forward body is preserved and restored after signing back in,
  instead of silently discarding the typed message.
- Passkey registration: choosing "use only this passkey" and failing to disable
  password sign-in now reports success with a warning (both methods stay
  enabled) instead of a hard error that invites a retry which reports the
  passkey as already registered.
- Domain and inbox deletion now require the typed confirmation server-side, not
  just in the browser, so a script cannot bypass the guard; a mismatch returns
  the user to the dashboard with an error banner.
- Adding a domain or inbox with invalid input now returns to the dashboard with
  a friendly error instead of a bare 400 page, so the user is not stranded.
- The Outbox now paginates ("Load older") instead of silently truncating at one
  page, so queued mail beyond the first page stays reachable.
- Invite acceptance: a failure while redeeming a setup token now logs the cause
  and shows a generic message instead of echoing the raw error to the anonymous
  visitor.
- Added a partial index for the inbound approval-token lookup and a unique
  partial index enforcing one physical receiver per account at the database,
  closing the read-then-write race in the account Remote MX check.
- SMTP outbound: a 5xx reply is now classed as a permanent failure (matching
  the Direct MX transport) instead of being retried for hours, and the
  connection deadline is refreshed per command and cleared before the body
  upload, so a large or slow message is not cut off after the remote accepted
  it (which could otherwise cause a duplicate on retry).
- Webhook delivery: retry backoff now includes deterministic +/-25% jitter, so
  many endpoints that failed together do not all retry in lockstep.
- Direct-SMTP edge: the core and the standalone receiver now sweep stale
  `mxdial-*` staging files left by a previous crash at startup (with a grace
  window), so they cannot accumulate and exhaust the disk.
- Relay outbound: in-flight outbound operations per socket are bounded, so a
  gateway cannot exhaust goroutines and SQLite capacity by bursting frames;
  excess outbound requests receive a bounded "too many in-flight" result.
- Provider errors: the provider response body is truncated to a short,
  single-line snippet before it is stored in a message's `last_error` and shown
  to mailbox readers, instead of up to 1 MiB of raw provider output.
- Direct-SMTP edge: a panic in a per-connection receiver job, the read-deadline
  watcher, the summary ticker, or an mxdial backend worker is now recovered and
  logged (mxdial reports a temporary failure) instead of crashing the process.
- Resend inbound: the recipient list on a webhook is capped, so one event cannot
  drive an unbounded number of per-recipient database lookups.
- Outbound send rate limit: the per-account limit is now enforced centrally in
  the send service, so it also applies to the reply, draft-send, UI and relay
  paths instead of only the `/v1/send` endpoint; the limit is no longer
  bypassable by choosing a different send route.
- Relay acknowledgements: a gateway that acknowledges a buffer id ahead of the
  event actually delivered can no longer advance the durable cursor over the
  events in between (silent mail loss); the cursor advances only to the event
  delivered.
- Mailgun urlencoded webhook: the raw body is now bounded and read without a
  second full copy, so an unauthenticated caller cannot force the amplified
  peak allocation; the accepted size still allows the worst-case
  percent-encoding of a full-size message.
- MX approval authentication: SPF alignment is now recorded on the evidence, and
  when a DMARC policy is published it is authoritative — an aligned SPF/DKIM
  pass that the policy rejects (for example a strict `adkim=s` From domain with
  only a relaxed DKIM pass) no longer authenticates the sender.
- Inbound dedup: the duplicate probe now compares the same normalized
  envelope recipient that is stored, so a re-delivered webhook with different
  address casing is recognised as a duplicate instead of failing the insert.
- API events: `/v1/events` and `/v1/events/stream` now reject a malformed
  `after` cursor with 400 instead of silently treating it as "from the
  beginning" and replaying the account's whole event history.
- Bulk actions taken in the Spam folder now return to the Spam view instead of
  the Inbox.
- API key revocation (single-key and account-wide) now updates the legacy and
  current tables in one transaction, so a crash can no longer leave them
  disagreeing.
- Domain/inbox purge now also removes the inbox's relay/webhook client rows and
  enroll tokens, matching domain purge, instead of orphaning them.
- Delivery-log joins are now account-scoped, so a delivery attempt can only
  render its own account's message/workflow fields.
- Cursors, attachment listing, and per-message read-state updates are now
  account-guarded at the query, not only by the prior lookup.
- Linkified bare `www.` hosts now default to `https://` instead of `http://`.
- Migration no longer leaves the pooled connection with foreign keys disabled if
  the post-migration re-enable fails, which could have silently skipped cascades.
- Dial MX / Antler MX status: an unreachable receiver no longer shows as a
  yellow "waiting" connector. The core fills the configured `smtp_hostname` when
  a receiver it cannot reach advertises none, so the connector row resolves to
  its real state and paints red ("Receiver unreachable"/"Reconnecting") with the
  failure reason instead of an indeterminate wait.
- Inbox settings: the Add and Edit inbox dialogs now share one full-height
  settings shell with a fixed size, a left section list and a single scrollable
  panel, so the dialog no longer resizes as sections change and the Save/Cancel
  actions stay in a fixed footer. Alias, external-alias and connector editors
  open inline inside the panel instead of as stacked popups.
- Delivery auto-actions: saving the inbox edit dialog from any tab other than
  Connectors no longer clears the inbox's auto-action policy. The controls live
  on the Connectors tab, and a save that does not carry them now leaves the
  policy untouched instead of reading the absent fields as "off".
- Delivery auto-actions: the inbox Connectors tab now has its own **Save
  auto-actions** control. Previously the control was unreachable — the tab hid
  the dialog's save button and nothing else posted those fields, so the policy
  could only be set by the API or the connector-create wizard.
- Passkeys: registration from the Account page now sends the CSRF token, so
  "Add a passkey" no longer fails with a 403. The login/registration UI is
  disabled with an explanatory message on insecure (non-HTTPS) origins.
- Passkeys: synced/backup-eligible credentials (iCloud Keychain, Google
  Password Manager) now sign in correctly. The stored backup-eligible and
  backup-state flags are restored on the credential before assertion
  verification, which previously failed every login with "Backup Eligible flag
  inconsistency".
- Passkeys: the system administrator can no longer disable password sign-in via
  the "only sign-in method" flow, and a config-driven credential rotation always
  leaves the break-glass password usable.
- Passkey store operations (`SetPasswordAuth`, `UpdateWebAuthnCredentialUse`)
  now run their read and write in one immediate transaction, closing a
  check-then-act race that could leave an account with no usable sign-in method.
- Passkeys now record audit events for add, rename and remove, and the account
  page shows last-used time and synced/device-only state plus a control to
  re-enable password sign-in.

### Added

- First-run setup: a fresh instance with no users now serves a `/setup` page
  where the first visitor creates the system administrator and is signed in. It
  is a one-shot claim — the route self-disables once any user exists — guarded
  by the pre-auth CSRF token, a same-origin check and a per-source rate limit.
  `ADMIN_EMAIL` / `ADMIN_PASSWORD` still pre-create the administrator at startup
  if you prefer deployment-time credentials. Supersedes the "no HTTP path to
  claim the instance" clause of D055 (see D085).
- Feedback: the signed-in header has a Feedback button that opens a panel
  linking to the project mailbox at mailmoose@hgolabs.com.
- Dashboard: each Dial MX / Antler MX domain in the Domains table now shows one
  aggregate connector light. It is binary: green when at least one receiver is
  both authorized and its hostname is published in the domain's MX records (so
  inbound mail will be accepted), red otherwise. A receiver that is connected
  but not routed by MX, and a receiver that has disconnected, been rejected, or
  become unreachable, all show red — green means mail will arrive now, not
  merely that a session exists. It answers "will mail arrive?" at a glance,
  while the receiving dialog keeps the per-connector detail.
- Antler MX: a zero-config hosted shared relay for direct-SMTP receiving.
  Select **Antler MX (Free SMTP Relay - no port forwards required)** as the
  domain's receiving provider, enter a contact email, and publish the shown MX
  and `_mailmoose-mx.<domain>` TXT records. No inbound port, receiver container
  or receiver-side credential is needed: the core dials out, and the existing
  DNS-anchored Ed25519 proof establishes domain authority. The hosted receiver
  set is resolved from a versioned manifest (embedded and fetched live at
  setup-save time, cached, with last-known-good and embedded fallbacks) and
  snapshotted per domain, so new capacity reaches new setups without a core
  upgrade while published MX records stay stable. The contact email and a
  generated setup id are logged by the receiver with the `dialmx domain auth`
  summary and per-recipient message records for usage accounting; they are
  metadata, never credentials. Dial MX **custom** service (manual receiver URLs)
  is unchanged.
- Domain receiving API/UI: a Dial MX setup now reports live per-receiver
  authentication `status`, cached published-record `dns` traffic lights (MX
  hostnames and TXT key) and copy-ready `instructions`, and the setup dialog
  renders the MX records, TXT record and lights.
- Standalone receiver: `DIALMX_BROWSER_REDIRECT_URL` redirects a browser
  visiting the receiver root to a landing page (302); API, health and readiness
  routes are unchanged.
- Standalone receiver: a shared-mode receiver can run behind a TLS-terminating
  reverse proxy with `DIALMX_TRUSTED_PROXIES` set to the proxy address, so the
  receiver holds no certificate. The session listener serves cleartext HTTP/2
  and admits a session only from that allowlist (or loopback), rejecting any
  other cleartext session with `426`; with no allowlist shared mode still
  requires TLS. The core continues to dial the proxy over `https` with hostname
  verification. SMTP stays direct so SPF and per-source limits see the real
  sender IP. The per-source connection and authentication caps are now
  operator-tunable (`MX_PER_IP_CONN_LIMIT`, `MX_PER_IP_CONN_WINDOW_MAX`,
  `MX_PER_IP_AUTH_CONCURRENT`, `MX_PER_IP_AUTH_WINDOW_MAX`).
- Per-inbox storage quotas: an inbox can carry an optional storage cap on top
  of the account quota (`storage_quota_bytes` on `PATCH /v1/inboxes/{id}`, Admin
  only). A positive value caps the inbox's stored bytes, `0` means explicitly
  unlimited, and clearing the field removes the cap so only the account quota
  applies. The cap counts messages in any direction or state (including Spam and
  Trash) plus drafts and draft attachments, is enforced in the same transaction
  that stores mail, and rejects new inbound/outbound mail with the existing
  storage-quota response once reached. Lowering the cap below current usage is
  allowed and only refuses new mail until space is freed. The inbox Quota tab
  (Add and Edit dialogs) sets it and shows usage, and the dashboard Size cell
  turns amber at 90% and red at the cap.
- Sign in to the web UI with a **non-admin mailbox API key**: the login page
  offers *Sign in with an API key* next to password and passkey. The browser
  session maps exactly that key's mailbox bindings (the operator view) and
  nothing more — no account Admin dashboard, no Admin plane, no installation
  management. Admin keys are rejected. The session is stored hashed in a new
  `key_sessions` table, capped at 24 hours, CSRF-protected, and refused on key
  revoke or rotate.
- Account page: the settings are now grouped into three labelled sections —
  "Your settings" (personal time zone, passkeys, email and password), "Account
  settings" (account name, default time zone, Trash retention) and "Account
  administration" (mailer, mailbox operators and invitations). The account-level
  settings and account administration are shown only to an account Admin, and
  the account name and default time zone are now enforced as Admin-only on the
  server.
- Trash: an inbox can override its account's Trash auto-purge window from the
  inbox Quota tab (`trash_retention_days` on `PATCH /v1/inboxes/{id}`). A
  non-negative value sets the override (0 keeps that inbox's trashed mail until
  purged by hand); clearing it inherits the account default. The retention sweep
  uses the inbox override when set.
- Passkeys (WebAuthn): sign in without a password using Touch ID, Windows
  Hello, or a security key. Register one or more passkeys from the Account
  page; the login page gains a "Sign in with a passkey" button using a
  usernameless (discoverable credential) flow. The system administrator may add
  passkeys alongside the deployment-managed `ADMIN_EMAIL`/`ADMIN_PASSWORD`
  break-glass login. A user cannot remove their last remaining sign-in method.
  Passkeys use the `go-webauthn/webauthn` library with no attestation
  requested, and preference for user verification where the authenticator
  supports it.
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

- Dial MX receiver and SMTP-edge logs now use the same compact, `[MX]`-tagged
  plain-text format as the core's `[Core]` lines instead of per-line JSON, so the
  included edge and the core interleave readably on one container stream. The
  JSON envelope (`schema_version`, `service`, `boot_id`, `event`) is dropped. The
  per-connection and per-session transport records (connection open/close,
  session opened/hello/closed, STARTTLS, reply-write) are now DEBUG and hidden by
  default; set `DIALMX_LOG_LEVEL=debug` to show them. Mail receipt, mail transfer,
  domain proofs and failures stay INFO.
- Delivery auto-actions now default to the **all connectors** trigger instead of
  **any connector**: a new inbox, an auto-action form that omits the field, and
  the connector-create wizard and Connectors tab controls all start on `all`, so
  mail is not marked read or moved to Trash until every connector on the inbox
  has delivered it. An inbox that already carries `any` keeps it — save the
  Connectors tab to switch.
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
- The mailbox and message lists reflow for narrow screens: on phones the
  header row is hidden and each message becomes a compact two-line row with
  the action icons beneath, and the dashboard, forms and top bar stack.

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
