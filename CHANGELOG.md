# Changelog

## Unreleased

- **Two published images, and one edge image for both receiver modes.** The
  `Dockerfile.mx` edge image and the standalone-receiver image were the same
  program under two names, differing only in the in-container binary filename;
  both are retired. The edge is now `mailmoose-remote-mx`, built from
  `dialmx/Dockerfile`, and `DIALMX_MODE=shared` or `DIALMX_MODE=single` picks
  the mode at runtime. Dev images published from this fork carry a `dirty-wip-`
  prefix (`dirty-wip-mailmoose`, `dirty-wip-mailmoose-remote-mx`) so they cannot
  be mistaken for release images. The embedded `mailmoose-mx` binary, the
  `_mailmoose-mx` TXT proof record and the session protocol names are unchanged.
  See `D137`.

- The publish workflow no longer claims that a push triggers it: it is
  manual-only (`workflow_dispatch`), and `release.yml` owns tagged releases.

- **The public database starts from one clean baseline.** All existing developer
  installations have completed the one-time transition, so the converter,
  maintenance image, historical fixture and transition instructions are retired.
  The public baseline, strict validation and future versioned migration runner
  remain unchanged. Retired credential tables and dual-writing are gone, and the
  core uses persisted Admin → MX receiver settings rather than legacy imports.

- The Antler MX setup wizard now offers Remove receiving as soon as it has
  stored the domain's receiving configuration. The receiving dialog's footer
  danger block is drawn (hidden) for every domain, so a setup started from
  nothing can be undone from the wizard that started it, instead of only after
  closing the dialog and reopening the domain's saved status view. A domain with
  no stored receiving configuration still shows no Remove receiving.

- A domain whose receiving setup was started and then cancelled no longer shows
  an amber "Add" beside its live receiver light. The Antler MX wizard stores the
  receiving provider on its first step (it has to, so the records and receivers
  can be checked while it runs), so cancelling part-way leaves the domain
  configured; the dashboard row now names that provider — "Antler MX", or
  "Dial MX" for a custom receiver set — as soon as the light snapshot lands,
  instead of keeping the pre-open "Add" until a manual reload.

- A receiving connector that the core can reach but that will not take the
  session no longer reads as "Reconnecting" forever. A receiver that answers and
  declines is now a settled failure — "Receiver refused the session", "Receiver
  speaks HTTP/1.1, not HTTP/2", "Wrong receiver mode" or "Receiver protocol
  mismatch" — in the Antler MX status dialog, the account Remote MX panel and
  the installation Direct MX panel. A receiver that is merely restarting (HTTP
  5xx) keeps retrying as before, and a receiver that has never answered is still
  "Receiver unreachable". The core keeps retrying a refused receiver, so
  redeploying or upgrading it recovers on its own.

- The OpenClaw connector now says plainly that it is an early alpha. The
  Add-connector dialog shows a short note between the Type and Name fields
  whenever OpenClaw is selected, and the connector guide says the same: the
  plugin and connector are still being tested, so expect rough edges.

- The Antler MX receiving wizard now opens with a short plain-language
  explanation of the service: a free hosted SMTP receiver for the domain,
  included as a receiving option, needing no extra container, no inbound SMTP
  ports and no exposure of the web interface. The same step shows the
  mail-flow diagram, which makes the key point that MailMoose only ever
  connects outbound to it. It also states plainly that Antler MX is a
  community service provided on a best-efforts basis, with no uptime
  guarantee; the provider guide carries the same sentence.

- A One / Gmail inbox no longer stays disconnected until someone saves its key
  again. The connection is marked unavailable only after three consecutive
  authorization failures, and the background sync keeps trying meanwhile — every
  attempt revalidates against One, and one One accepts clears the failure count
  and resumes syncing on its own. A single transient rejection can no longer
  strand a working inbox on a reconnect prompt, and a connection repaired in One
  recovers without touching MailMoose. Nothing but a removed or re-imported
  connection stops an attempt, so an unhealthy binding keeps earning and clearing
  its failures instead of being short-circuited.

- A One / Gmail connection is flagged as needing the operator only for a
  confirmed authorization rejection. A connector-level problem — One no longer
  exposing a mandatory Gmail action — is no longer recorded as needing attention,
  so a transient action-mapping hiccup can no longer strand a working key on a
  reconnect prompt that only a manual re-save could clear. A live message-body
  fetch that fails because the connection was revoked now returns the same
  reconnect verdict the banner uses, so the reader offers the in-place reconnect
  control instead of a bare "Could not load the message body" with no route to
  repair.

- The receiving published-record checks (Dial MX / Antler MX) now report one of
  four honest states — `ok`, `not_found`, `mismatch`, `dns_error` — and only `ok`
  is green. `pending` is gone (it implied work in progress that was not
  happening); an absent record reads `not_found` and a failed lookup reads
  `dns_error`, both red, because the operator must not proceed on a record that is
  wrong, absent, or unconfirmed. The checks now query absolute DNS names, so a
  brand-new domain's absent `_mailmoose-mx` record reads `not_found` reliably
  instead of drifting to a permanent "could not verify". DKIM keeps its advisory
  light: only a published-but-wrong record is red, an absent or unconfirmed one is
  grey.

- Remote-draft mode now just places the draft in the connected mailbox's Drafts
  folder. The web UI no longer shows any handoff surface (the "Draft handoffs"
  pane, per-draft handoff badges, and the cancel/retry controls are gone), no
  notification email is ever sent (any previously queued handoff notification is
  retired), an ambiguous or failed append unfreezes the local draft, and
  switching a standalone inbox to remote-draft clears any approver.

All notable changes to MailMoose are documented in this file. The format is
based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the
project aims to follow [Semantic Versioning](https://semver.org/spec/v2.0.0/).

## [Unreleased]

### Changed

- Strix security remediation (D133): alias writes now require account Admin in
  both API and store; existing delegated aliases keep working. Standalone IMAP
  and SMTP password retention is limited to an unchanged host, effective port,
  username and security; changed endpoints require re-entry, SMTP no longer
  inherits the IMAP secret, and binding/credential saves are atomic. Approval
  classification recognizes issued tokens rather than arbitrary marker-shaped
  text, and a separate managed approver inbox may receive its notification.
  Transient events obey inbox visibility/filtering, Read-only inbox responses
  omit the approver, account Remote MX receivers cannot cross account boundaries,
  and relay demotion applies to already-open sockets. Migration 079 adds the
  issued-token lookup index. Resend's sender-provenance limitation is documented
  as an accepted Low risk requiring a live token; private outbound remains
  enabled by default for self-hosting, with documentation corrected accordingly.

- `APP_ENCRYPTION_KEY` is no longer required to start. When it is unset,
  MailMoose reads `<DATA_DIR>/encryption_key` and generates it (0600, 64 hex
  characters) on first start, logging the path so the operator can back it up.
  A supplied key always wins and no file is written; an existing key file is
  never replaced. Set your own key before connecting providers if you want the
  key kept out of a `/data` backup — changing it later leaves stored provider
  credentials unreadable. See `docs/DECISIONS.md` `D128`.

- The One / Gmail setup screen now only creates inboxes. It no longer lists
  imported mailboxes or offers per-inbox connection management, and an
  already-imported mailbox is a plain, non-selectable "Already imported" row. A
  key is changed on the inbox that owns it: **Identity → Advanced → Reconnect
  with One** (or the red connection flag, which now opens that reconnect
  dialog immediately). The reconnect control is labelled for the connection it
  repairs — **Reconnect with One** — rather than tying One to Gmail. An address
  that is already configured is refused with the remedy — "delete the existing
  inbox before importing" — rather than a dead end, and reconnecting with a key
  that no longer exposes this inbox's original One connection says the same.
  "Delete connection" is gone: deleting the inbox is the only way to remove a One
  connection, and a mailbox user who cannot repair a connection is told to ask an
  administrator instead of being shown a control that would fail for them.

- The Antler MX contact-email field now states its purpose in the label itself —
  "used for important service messages only, no marketing" — and the separate
  explanatory sentence was dropped from both the setup wizard and the saved status
  view's settings panel, where it restated what the field labels already said. The
  authentication-enforcement label now reads "(spam)" to name what the setting
  acts on. The inherited-settings caveat is unchanged.

- Consolidated inbound and standalone mailbox setup guidance in
  `docs/EMAIL_PROVIDERS.md`, kept agent connector setup in `docs/CONNECTORS.md`, and
  added the operator-focused `docs/REMOTE_MX.md`. Archived the MX/Dial MX
  architecture material and One live-probe notes outside the repository.

- The Antler MX receiving dialog now shows a subdomain's MX **Name** as its
  fully-qualified name (`mail.example.com`) and reserves `@` for a two-label zone
  apex. It previously keyed the record name off the account's parent-domain link,
  so a subdomain added standalone — with no parent link — rendered `@`, which
  publishes the MX record on the parent zone and sends mail for the subdomain to
  the wrong domain. The name is now derived from the domain's own shape, so the
  wizard and the saved status view both get it right. Finishing the setup wizard
  also relabels the footer's close control from "Cancel" to "Close" as it lands on
  the saved status view, instead of leaving a stray "Cancel" beside an actionless
  panel.

- Consolidated the API contract and generated endpoint catalogue into
  `docs/API.md`, and brought the Google/Gmail and Microsoft standalone inbox
  setup guides together under `docs/EMAIL_PROVIDERS.md`. Removed the secondary
  metadata-sync status note and unified-labels guide from the shipped docs; their
  relevant API behavior remains described in `docs/API.md`.

- The dashboard's aggregate receiving light is drawn only for a Dial MX domain
  that actually names a receiver target. An Antler MX setup whose receiver set is
  empty — a manifest that advertised no receivers, or a setup that was started and
  abandoned after its configuration was saved — now renders **no dot** instead of a
  red "no inbound connector is ready / mail delivery is blocked". That red light was
  a phantom: it asserted mail was blocked for a domain with nothing to deliver to
  and nothing to fix. The live lights snapshot applies the same gate, so the
  dashboard's client tick cannot reintroduce the dot.

- The receiving published-record checks now use a four-state vocabulary that says
  what each state means: `ok` (published and matching), `not_found` (definitively
  not published), `mismatch` (published but wrong) and `dns_error` (the lookup
  could not be completed). The old `pending` is renamed to `not_found` because it
  implied work in progress, and `unavailable` to `dns_error` because it is a lookup
  failure. On the receiving side only `ok` is green: `not_found`, `mismatch` and
  `dns_error` are all red, since the operator must not proceed on a record that is
  absent, wrong, or unconfirmed. The amber that remains is the receiver-level
  extra-MX warning (a correct config with ambiguous routing) and the not-yet-run
  "Waiting" cell. The checks now resolve **absolute** DNS names, matching the
  receiver's own proofs, so an absent `_mailmoose-mx` record on a new domain reads
  `not_found` rather than being masked as a permanent lookup error by the
  resolver's search list. The DKIM sending-side light is unchanged in spirit: green
  only when published and matching, red only when a published record does not match
  the signing key, grey for absent or unconfirmed (mail still sends).

- A standalone inbox's connected **Drafts** folder is now pulled into its cached
  index immediately after a remote-draft handoff is confirmed placed, and its
  **Sent** folder immediately after a sent copy is confirmed. Previously the
  placed item stayed invisible to reads until the next scheduled full sync. The
  pull is folder-scoped, coalesced per inbox and folder, asynchronous and
  best-effort, so it never blocks or fails the write that triggered it; Gmail and
  Outlook connectors pull the equivalent label/folder.

- The inbox settings **Approver** tab's send-handling mode is labelled
  **MailMoose approval** and **Create draft** (previously the terse
  "MailMoose" / "Remote"), and the mode selector no longer flashes on a domain
  inbox before the dialog's script runs. Both are labelling/layout fixes: the
  underlying modes and their behaviour are unchanged.

- Messages now expose `label_ids`, the canonical unified label membership for the
  owning inbox: `tag:<Name>` for additive labels and `folder:<id>` for the single
  folder-backed label, always present and `[]` when empty. The older `labels`
  array remains as the additive-name view only. Label filtering now resolves
  `label` and `label_id` through one shared rule on every list and search route:
  repeated values AND together, a bare name and `tag:<Name>` are equivalent,
  `folder:<id>` requires an `inbox=` scope account-wide, and an inbox-scoped and
  account-wide query with the same filter return the same page and completeness.
  The label catalogue reports `capabilities` so an empty catalogue still describes
  the inbox, create and list descriptors no longer drift, and provider-owned
  label names (Gmail user labels, Outlook categories) may be hierarchical. OpenAPI
  documents the `202` queued-action response on async-default mutations.

### Added

- A key may hold a **domain-wide sending** grant on a domain's catch-all inbox,
  so it can send from any address on that domain (for example `billing@`) without
  a hand-made alias. The grant is live only while that inbox is the domain's
  enabled catch-all, and it is one state with Owner: turning it on pins the role
  to Owner, and dropping the role below Owner drops the grant. It is checked at
  send time, so revoking it or changing the catch-all stops a pending send.
  Migration 074 adds `client_inbox_bindings.send_any_in_domain`;
  `POST /v1/admin/keys` accepts a `domain_send` list. In the inbox's Clients &
  Access panel it is a segmented **Domain send** button leading the row, and the
  Clients tab's key dialog carries the same control on the catch-all inbox's row.
  Catch-all inboxes show a small catch-all marker after their address wherever
  they are listed. Agents discover the capability from `GET /v1/bootstrap`:
  `permissions[inbox_id].send_any_in_domain` is `true` (with `send_any_domain`
  naming the domain), the `/v1` capabilities list gains `send-any-in-domain`, and
  the `/agent` guide explains the widened `sender` rule. See `D115`.

### Removed

- The `GET /changelog` route and the `changelog` field it added to
  `/.well-known/mailmoose`. The changelog is a repository document; agents read
  it from the source repository rather than from a running instance, so the
  embedded copy under `internal/httpapp/assets/` and the `make changelog` sync
  step are gone with it.

### Fixed

- The inbox settings **Identity** tab lays its fields out as label-left /
  control-right rows, matching the rest of the app. **Display name**, **Email
  address** and **Dashboard group** previously rendered as bare label/input
  pairs, so a label could sit on the same line as its input and the inputs had
  inconsistent widths — on a standalone inbox the disabled **Email address**
  field was the browser's default narrow width while its neighbours filled the
  panel. All three are now `.setting-row` blocks that collapse to a single
  column on narrow screens; the Dashboard group hint moved under its label. No
  behaviour change: the field names, ids and the save path are unchanged.

- Adding a domain that already exists now reports **"a domain with that name
  already exists"** in the dashboard and as a `409` with that message from
  `POST /v1/admin/domains`, instead of the generic "Could not create it. Please
  try again." / `conflict` response. A duplicate name was a SQLite `UNIQUE`
  violation that the redaction layer classified as an engine fault; the store now
  maps it to a deliberate `store.ErrDomainNameConflict`. The message does not
  disclose which account owns the existing name.

- Clearing **Domain send** on the Clients tab's edit-client screen now persists.
  The control submits an explicit `1`/`0` rather than relying on an unchecked
  checkbox, which submits nothing and was indistinguishable server-side from an
  untouched row. The control also sits immediately left of the role ladder (it
  had been placed before the key name), so it reads as a permission and every
  role ladder stays aligned.

### Security

- Stored secrets are now always bound to the row that owns them. A webhook
  client's signing secret binds to its account and inbox, and a relay
  connection's secret and delivery key bind to its gateway id; a value that
  lacks the `v2.` binding prefix is refused on read rather than decrypted
  unbound. Both existing instances have been upgraded; only the current bound
  format is supported. The temporary legacy-secret migration tool and its
  operator instructions have been removed. See `D118`.
- Password login now accepts only an Argon2id hash. A pre-Argon2id
  `pbkdf2-sha256` row can no longer verify and needs a password reset; the
  break-glass `ADMIN_PASSWORD` path still works. See `D118`.

- A post-login `next` value containing an ASCII tab or newline could survive into
  the `Location` header, which a browser then strips to form an off-site
  redirect (`/\t/evil.com` → `//evil.com`); `safeNextPath` now rejects control
  characters and any value that is not a plain root-relative path.
- Session and CSRF cookies are now `Secure` whenever `BASE_URL` is `https://`,
  including behind a TLS-terminating proxy with no configured trusted proxy,
  where they were previously issued without the flag.
- The CSRF token for compose, reply and forward forms no longer travels in the
  form-action URL (which leaked it into proxy and access logs); it is read from
  the hidden `_csrf` field of the multipart body, or the `X-CSRF-Token` header.
- Draft recovery requires a real, unrevoked session expired within the last
  15 minutes and its form CSRF token; fabricated cookies are rejected before
  reading the body. All flash reads and consumes are owner-checked, including
  attachment reuse and one-time secrets. Drafts survive re-login only for the
  same user or API client. The store admits at most eight entries and 2 MiB per
  owner, with global ceilings of 64 entries and 8 MiB; capacity pressure rejects
  new entries rather than evicting another owner's data. Anonymous auth notices
  are bound to a signed browser cookie.
- All password hashing and verification paths now bound Argon2 admission to
  five seconds by default, including ordinary `CheckPassword` and callers that
  supply a nonpositive timeout. The four-slot memory ceiling is unchanged.
- Inbound admission rejects immediately with `429` and `Retry-After` when the
  shared pool is full, instead of queuing unauthenticated requests. A two-slot
  pool now limits each source to one slot. Body reads still precede verification
  for body-carried signatures and remain bounded by the server read timeout.
- Password login now carries a per-account throttle in addition to the per-IP
  one, and IPv6 rate-limit keys are masked to `/64` so a client cannot rotate
  within its prefix to exhaust the limiter.
- Unauthenticated password login acquires its Argon2 admission slot with a
  timeout and returns `503` rather than blocking indefinitely behind a login
  flood.
- Concurrent inbound webhook requests are capped per source address, so one
  caller can no longer hold every shared inbound slot before its signature is
  verified.
- Passkeys are offered only from a browser secure context. A plain-HTTP
  deployment reached by IP or hostname no longer shows the passkey sign-in
  button, and its account page explains that HTTPS is required instead of
  offering controls the browser would refuse to run.

### Changed

- A terminal outbound send failure (permanent provider rejection, ambiguous
  outcome, or exhausted retries) now lands in the inbox as a locally generated
  delivery-failure notice from `MAILER-DAEMON@<domain>` (`Undeliverable: …`) and
  is cleared from the outbox, instead of only appearing as a failed outbox row.
  Retryable failures are unchanged. See `D129`.

- Message metadata now has required `provider` (mailbox implementation) and
  `message_transport` fields; connected inboxes return the same connector name
  in both. Public IMAP UID/UIDVALIDITY fields are removed, including handoff,
  event-replay, webhook, and relay metadata. `direction` consistently reports
  inbound, outbound, or unknown using recorded direction and native label/mapped
  role evidence; unknown-direction replies require explicit recipients. New
  local and IMAP arrivals cache bounded previews, with `snippet: "-"` when no
  preview is available. IMAP preview fetching is background-only and historical
  mail is not back-generated. Migration 072 adds the preview cache/pending bit.

- Native message APIs now share one schema across domain, IMAP, Google,
  Microsoft, and Gmail-via-One inboxes and across inbox/account/thread/search
  routes. Optional remote metadata is preserved instead of being lost in the
  account-wide projection. Unknown direction, spam verdict, and creation time
  are omitted rather than invented. Lists and thread-message responses are
  metadata-only for every inbox; fetch a detail to read its body. Details retain
  sanitized HTML and live remote-body failure behavior. OpenAPI documents the
  stable required core, optional fields, and inbox/provider identity metadata.

- Failed durable inbox actions are now reconsidered automatically every ten
  minutes instead of waiting for a human. A retryable read/label change is
  requeued and retried; a move, delete, restore or purge is only retried when the
  message is still observed in its source folder, settles as done when it already
  reached the destination, and stays failed when its location is unknown. An
  action is never replayed ahead of a newer unsettled action for the same message,
  and uncertain (`ambiguous`) actions are never reconsidered.

- Durable inbox actions now use a single-worker round-robin scheduler, processing
  one eligible action per inbox per turn so a busy inbox's backlog cannot
  monopolize the queue. Same-message ordering and retry safety are preserved;
  individual slow actions still delay other inboxes while they execute.

### Removed

- Unreachable code reported by `deadcode` and `staticcheck -checks U1000` is
  gone. The `MailboxRouter`/`StandaloneMailboxService`/`MailboxBackend` seam in
  `internal/app` (plus its unit test) was never constructed by any workflow, HTTP
  or UI path; superseded remote-message handlers and form helpers in
  `internal/httpapp`; and the unreferenced `ListBlockedMessages`,
  `LatestMessageInThread`, `DeleteDraft`, `GetEvent`, `DeleteDomain`,
  `CreateHermesEnrollToken`, `ResolveDialMXRecipients` and `CreateReply` methods.
  No behaviour changes; the live paths are unchanged. See `D097`'s status update.
- `docs/IMPLEMENTATION_PLAN.md`, an unreferenced V1 phase plan.
- `tests/security-live-check.sh`, a one-off manual probe script from a security
  review round: it was not part of `tests/run.sh`, CI or any doc, and its checks
  are covered by `tests/unit/httpapp/security_fixes_test.go`.

### Changed

- Stored secrets are now always bound to the row that owns them (an AAD
  binding), so a value written in the older unbound format is refused on read.
  Only the current format is supported; the temporary upgrade tool has been
  retired after the two existing instances were upgraded.

### Added

- A Direct MX domain that signs outbound mail now shows a **published-DKIM-record
  indicator**: a traffic light in the dashboard Domains table's **Sending**
  column and the same dot next to the record in the sending dialog's "Publish
  this DNS record" panel. Green means the record is published and matches the
  signing key, red means a different key is published (a stale record from a
  rotation, or someone else's record on the name), and grey means not yet
  published or not confirmed. With signing off there is no light, no check and no
  lookup. The check is owned by the same shared DNS watcher as the receiving
  lights, so an open dialog polls it without ever blocking on a resolver. See
  `D113`.

- Direct MX outbound delivery can now sign messages with DKIM. Turn it on in the
  domain's **Sending → Direct MX** tab and MailMoose generates an RSA-2048 signing
  key and shows the exact TXT record to publish (`mailmoose._domainkey.<domain>`),
  inline in the same tab, with one-click copy. The private key is available behind
  a "Show private key" link, shown once and never rendered on the page. Messages
  are signed once, when they are queued, so the stored message carries the exact
  signature that is transmitted and a retry re-sends identical bytes rather than
  stacking a second signature. Turning signing off deletes the stored key. Only
  Direct MX is signed: every other provider signs with its own key or submits
  through its own authenticated infrastructure. See `D069`.

- A metadata sync row now reads **Downloading metadata N / ~M** with a live
  count and percentage while it runs. Gmail and One show their provider's
  message count as an approximate denominator (marked `~`, because the provider
  counts every label while MailMoose indexes a folder subset); the fraction is
  normalised so it can never exceed the total or stall below it, and it snaps to
  `M / M` on completion. Outlook and IMAP keep their authoritative totals. The
  queue row's dot and status also align to the mailbox name's line.

- Standalone mailbox metadata syncs now share a global concurrency limit,
  configurable with `REMOTE_SYNC_CONCURRENCY` (default `2`, range 1–32). This
  staggers startup indexing and also bounds simultaneous scheduled, push-triggered
  and manual syncs.

- Cloudflare receiving setup is a **full-page one-time wizard** again, instead of
  a two-step modal. After saving a Cloudflare receive path it opens a dedicated
  page (`/ui/domains/{id}/receiving/worker`) with the complete Worker code, a
  four-step Cloudflare walkthrough (create the Worker, paste and deploy, onboard
  Email Routing with its DNS records, and add one **catch-all Send to a Worker**
  rule), a **Copy code** button, a **Done** link, a **Regenerate secret** action,
  and a warning when the receiver host is not publicly reachable.

- The Cloudflare shared secret is now **optional**: leave it blank to have
  MailMoose generate one, or paste an existing secret to reuse a single Worker
  across several domains. Supplying a secret stores it verbatim and skips the
  generated-code page.

- The One / Gmail import screen now shows a best-effort **Messages** count per
  mailbox and offers a per-mailbox **Index history** window — **Everything**
  (default), **Last year**, **Last 90 days** or **Last 30 days**. A shorter
  window bounds that inbox's progressive metadata backfill, because a full
  build spends roughly one provider request per message against One's
  account-wide quota. The count is non-critical: a mailbox whose profile cannot
  be read shows **unknown** rather than a misleading zero.

- Provider connection health is now surfaced per inbox. A revoked or expired
  One/Gmail/Microsoft authorization renders a red flag in the dashboard's flags
  column, linked straight to the inbox's Identity tab, and a red prompt at the
  top of that tab offering **Reconnect**. The reconnect control sits under an
  **Advanced** disclosure so it does not read as a routine action. A **Gmail via
  One** inbox reconnects in place with a small **Save and check** API-key dialog
  (the key is verified against the inbox's original Gmail identity before it is
  stored); Google and Microsoft keep their OAuth wizard flows. Only a confirmed
  disconnect is flagged — a transient retryable error is not.

- Live inbox-wide metadata-sync progress for Gmail, One, Outlook and IMAP in the
  header Queue and at the top of every relevant folder view. Running work has a
  spinner; queued, waiting, paused and failed work stays visible with safe status.
  Counts update automatically, with indexed/total and percentages when a reliable
  provider total is known. Completed syncs disappear without a page reload.

- Standalone remote-mail arrivals now log a concise `remote received` line and
  include `provider` and `source` in their durable event payload. Source is
  persisted through asynchronous delivery: Microsoft delta polling reports
  `microsoft`, Graph-notification-triggered delivery reports `microsoft push`,
  and IMAP/Gmail report `imap`/`gmail`. Microsoft push notifications are not
  logged at INFO: each is a fetch hint, and a push-triggered delivery is already
  visible as a `remote sync` line with `trigger=push` followed by the
  `remote received` line. Unmatched notifications are Debug-only, and callback
  secrets are never logged.

- A global **status flag** in the header on the dashboard and inbox views.
  It is green "Live" while the event stream is connected, red "Disconnected"
  when the stream drops (reconnecting with backoff), amber "Sync problem" when
  the remote index reports an error, and a spinner "Loading…" while a UI
  background action is in flight (an IMAP sync, a remote message body load or a
  background list refresh). A dropped connection outranks Loading. The IMAP
  "Syncing messages…" strip and the "Updating messages…" refresh spinner are
  removed in favour of the flag; a failed background list refresh still appears
  in place with its Retry button, and the message-body loader is unchanged.
  Page navigation loading (link clicks and form submits) on these pages also
  reports through the flag instead of the floating "Loading…" banner, so no
  pop-up loading banner appears over the inbox view; other pages keep the
  banner.

- Microsoft / Outlook connections now have a **personal + shared** flow: the
  wizard offers a **Connect Microsoft · Personal Inbox only** and a **Connect
  Microsoft · Personal Inbox and Shared Inbox** button, and after sign-in the
  shared mailbox is picked from a list of mailboxes the signed-in account can
  actually open, with manual address entry as a fallback. The mailbox address is
  never typed before sign-in.

- The Google / Gmail and Microsoft / Outlook wizard steps label the inbox
  automatically — the provider's display name or the connected address — so the
  identity tab can rename it later.

### Changed

- The One / Gmail import screen is tidied: the mailbox table is a compact,
  vertically aligned table, and a whole row can be clicked to select or
  deselect it, highlighting the row. Each row's **Index history** control is
  disabled until that row is selected, so an unselected mailbox can never carry
  a history window.

- Folder management in the mailbox sidebar is simplified. The **Manage folders**
  disclosure is gone: custom folders now carry a small folder glyph that opens a
  dialog with **Rename** and **Delete** for that folder, and a **New folder**
  button sits at the bottom of the sidebar and opens a name field. Folder
  management controls are shown only to users who may change folders (Assistant
  or Owner).

- The Microsoft / Outlook sign-in now opens in a centred **partial-screen
  window** instead of a full browser tab, so the wizard stays visible behind it,
  and the wizard flashes a **Paste the resulting URL here** prompt above the
  completion field as soon as that window is open. The pasted URL is still the
  only way to finish, because the callback lands on a `http://localhost` URL
  where no listener runs. Google sign-in is unchanged.

- Adding a standalone inbox now picks the provider with one button per
  provider instead of a radio list. **Add inbox → Standalone inbox** shows
  **Google / Gmail**, **Microsoft / Outlook** and **IMAP + SMTP** as logo
  buttons; the Google and Microsoft buttons open that provider's setup step
  directly (no separate Next), and IMAP opens the in-dialog IMAP/SMTP form.
  The stale "(coming later)" Google and Microsoft buttons on that form are
  gone.

- The Google / Gmail and Microsoft / Outlook wizard steps are tidied: the
  provider logo heads the step, each connection form sits in a bordered box with
  its own heading, and the "Inbox label" prompt is gone — the inbox is labelled
  from the provider display name or the connected address, then renamed from its
  Identity tab. Picking **Personal Inbox and Shared Inbox** now replaces both
  halves of the Microsoft connect flow with the shared-mailbox picker.

- The **action queue** behind the header status flag is now a small bubble
  anchored under the flag instead of a large modal. It reads in the flag's own
  13px type, names each action in plain language ("Move to trash", "Delete
  permanently", "Mark read"), shows a plain status, lists failures first
  with Retry / Dismiss, and renders at most six rows with an "and N more" line,
  so a bulk delete of hundreds of messages no longer floods the pop-up.

### Fixed

- A standalone inbox's approval-request and draft-handoff notifications no longer
  fail before they reach the provider. The delivery-log attribution read the
  inbox's `domain_id` into a non-nullable string, so every workflow job for a
  standalone inbox (whose `domain_id` is `NULL`) failed with `converting NULL to
  string is unsupported` and its notification stayed **queued** forever while the
  handoff itself published normally. A standalone attempt is now recorded with no
  domain, exactly like a standalone message. See `D119`.
- A failed message action now reports the provider's actual reason instead of a
  bare **Action could not be completed**. A remote error normalised without a
  curated message (for example a refused folder move) previously had its cause
  discarded, so every failure looked identical in the action queue. See `D119`.
- Applying a folder-backed label to a message in an Outlook (Microsoft) inbox
  now moves it. The label API supplied the folder's local id as the destination,
  but the Microsoft move path resolved destinations by path only, so the move
  failed before reaching the provider with a generic error. A move destination
  is now resolved as a folder id **or** a path. See `D120`.
- The Antler MX status view and setup wizard no longer tell you to publish a
  record when the DNS lookup simply failed. A record that is definitely absent
  still shows its publish prompt, but a lookup that could not be completed reads
  **Unverified** with a retry notice, and a previously seen "additional MX
  servers" warning stays put instead of clearing while the resolver is down.
- The wizard's TXT check now agrees with the receiver: a record it calls
  **Matching** is one Antler accepts, so you no longer see a green record beside a
  receiver refusing with "key unavailable".
- Saving an Antler MX setup no longer re-points the domain at a different receiver
  set. A re-save keeps the receivers the domain's MX records already point at, so
  a manifest change during setup cannot leave you with records that no longer
  match. The setup wizard also shows a visible 10-second countdown while you are
  on its DNS or receivers step, and each suggested MX record carries its own
  status rather than sharing one.
- The saved Antler MX status view now lets you change the authentication
  enforcement policy and the contact email in place, in a **Settings** section
  below the status records. A subdomain that inherits its setup shows these
  read-only, naming the domain to edit.
- The dashboard's Antler MX light now clears an "extra MX records" warning as soon
  as the records are removed, instead of staying amber until a much later refresh.
  A status dialog also follows the shared live connection across a reconnect, and
  a domain with more than one dialog open updates the right one.

- A Dial MX / Antler MX published-record check that could not be completed no
  longer reads as "no record is published yet". A NODATA/NXDOMAIN answer stays
  `pending` (publish the record), while a timeout, SERVFAIL or refused lookup is
  the new `unavailable` state, which the wizard shows as **Unverified** with a
  retry notice instead of a publish instruction. A resolver outage therefore no
  longer tells an operator to create a record they may already have published
  correctly, and an `unavailable` MX check carries the previously observed
  extra-MX hosts forward, so a known amber warning cannot vanish because the
  resolver stopped answering.
- The core's local `_mailmoose-mx` TXT check now uses the receiver's own MM1
  validator (`mxwire.ParseDomainTXT`) instead of a second, looser parser. The old
  parser accepted records the receiver rejects (an extra field, a duplicate MM1
  record), so the status view could show a green "matching" record beside a
  receiver refusing with "key unavailable".

- Standalone mailbox bulk actions (IMAP, Microsoft, Gmail and One) now use the
  durable action queue instead of holding the browser request open for each
  remote operation. The notice reports accepted jobs, not completed moves, and
  separately counts submissions that could not be queued. Duplicate selected
  IDs are submitted once. Local mailbox bulk actions remain synchronous.
- A confirmed IMAP move without a reported destination UID is no longer marked
  failed when Message-ID lookup cannot resolve its new location. The stale source
  cache entry is removed and the next metadata reconcile indexes the destination;
  the action is completed rather than inviting another remote move.

- The message reader's action buttons (Reply, Reply all, Forward, mark unread,
  trash) are square and 36×36 like the mailbox toolbar. They had no width rule
  of their own, so the flex row squeezed the five of them into a cramped,
  uneven cluster.

- The mailbox toolbar's square buttons (settings, sync, mark read/unread,
  trash) are the same 36×36 in every view. A later `.bulkbar .icon-btn` rule
  carried the same CSS specificity as the sizing rule and won on source order,
  so the buttons were pinned to 30px with nothing stopping the flex row from
  shrinking them — which squashed them to slivers in the custom-folder view,
  whose toolbar also carries the Move select and button.

- An active Gmail or One metadata sync no longer reports **Waiting for provider
  request slot**. That label appeared for the whole sync because One paces
  requests at 15/minute, so it read as a fault when it was normal pacing. The row
  now shows the download in progress instead.

- Bulk actions taken from a custom folder view now stay in that folder and act
  on the right messages. Mark read/unread, Move to trash and Move used to drop
  the user back in the Inbox; "all N in this folder" acted on the Inbox's remote
  messages for a standalone (Microsoft/Gmail/IMAP) inbox, or was refused with
  "unknown folder" for a domain inbox. Renaming a folder also returns to the
  renamed folder, and creating one opens it.

- The header action queue now stays hidden until opened and closes correctly.
  Successful actions disappear from the popup as soon as the worker completes
  them, leaving only queued work and actions that need attention.

- The reload button on a standalone (IMAP) inbox no longer hides the newest
  messages. It submitted the page's outgoing "load older" cursor instead of the
  page it was on, so pressing reload advanced the list to an older page (and a
  manual browser refresh was needed to get back). Reload now preserves the
  current page exactly.

- The periodic/manual provider-sync log now names the mailbox by its address
  instead of the opaque inbox id, so an operator can tell which inbox a sync
  belongs to.

- Reconcile no longer risks deleting cached remote metadata against an
  incomplete view. A `UIDVALIDITY` change previously deleted every cached row
  for a folder before re-inserting only the fetched window, and a reduced/partial
  folder discovery pruned a folder together with its cached messages. Cached rows
  are now dropped only against a complete snapshot, so a transient reconcile
  cannot drop metadata. The reload button also no longer schedules the deep
  reconcile (the deep pass runs on its own cadence).

- A standalone (IMAP) inbox's message now opens in the same reader as any other
  message, so it exposes Reply, Reply all, Forward, Move to trash (and Restore /
  Delete forever in its Trash folder) instead of only "Mark unread". A reply or
  forward to a remote message no longer answers "message not found": the send
  handler resolves a remote id through the same local/remote boundary as the
  other message actions. Remote trash/spam state is read from the message's
  remote role folder. The reader also shows the message's attachments (fetched
  live by MIME part path with a secure `Content-Disposition: attachment`
  download) and its conversation thread from the cached remote index, and
  offers a plain-text alternative when the body has both HTML and text.

- Saving the Identity tab of a Microsoft- or Google-connected inbox no longer
  fails. Its connector fields are hidden and disabled, so the blank form was read
  as a connector change and sent through the live IMAP sign-in test, which has no
  IMAP credentials to test for a provider inbox: the save answered "IMAP
  connection failed: … credentials unavailable" and persisted nothing, not even
  the display name.

### Performance

- Opening a standalone message immediately renders cached metadata with a body
  spinner. The sanitized live body loads separately, with retry feedback on
  failure; remote images remain hidden by default.

- Standalone folder navigation reads cached messages immediately. Folder API
  listings return the cached tree and indexed counts without waiting for IMAP;
  initial discovery and progressive backfill run in coalesced background work.
  API message/thread reads retain first-use indexing and explicit remote refresh
  still waits for synchronization. Loading spinners identify navigation and
  background sync; read-only IMAP selection is reused within a session, and
  polling-client cancellation no longer interrupts durable arrival persistence.

- New standalone mail now appears in the inbox list immediately: a detection
  pass indexes the new INBOX headers into the read index rather than only
  recording an arrival event. The deep reconcile no longer runs for every inbox
  every minute; once a folder is fully backfilled, an unchanged folder costs a
  single `STATUS` read plus a newest-window header refresh instead of a full UID
  snapshot. The poll fallback uses `STATUS` instead of `EXAMINE`.

### Added

- A square **reload button** on a standalone (IMAP) inbox toolbar, shown to
  every user. It quick-syncs the current view — new-mail detection and a header
  refresh of the messages on screen — so new mail and read/flag changes appear
  without a page reload, and it never triggers a full reconcile. Requires only
  read access.

- Per-inbox **sync schedule** for standalone inboxes (Identity tab): how often
  to check for new mail and how often to run a full sync, both in whole minutes.
  Unset values inherit the defaults (1 minute / 15 minutes); the new-mail poll is
  floored at one minute. The schedule is shown for a provider-connected inbox too,
  not only a hand-configured IMAP one — a Microsoft delegated shared mailbox is
  poll-only, since Graph has no change subscriptions for it. It renders inline
  directly below the email address, with the two intervals side by side, rather
  than as a separate collapsible section. Entering 0 for the new-mail poll turns
  the quick poll off (the inbox relies on push and the full sync); blank inherits
  the one-minute default.

- A remote arrival's `source` now reports "imap push" when an IMAP IDLE mailbox
  update drove the detection pass that found it, matching the Microsoft Graph
  "microsoft push" source. The bare IDLE signal itself is logged at Debug now, so
  an operator sees a single "remote received" line per arrival.

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
  `docs/MAILBOX_BEHAVIOR.md`.
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
`docs/MAILBOX_BEHAVIOR.md`.
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
- Postmark can receive mail (D096) through the existing inbound pipeline.
  MailMoose generates HTTP Basic credentials per domain and shows the webhook
  URL once. The adapter preserves binary `RawEmail` content byte-for-byte.
  Configure it under **Receiving** on the domain page; see
  `docs/EMAIL_PROVIDERS.md#inbound-postmark`. Postmark's unattested envelope
  sender is recorded in the accepted-risk note in `SECURITY.md`.
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
- Dial MX / Antler MX status is now **self-reported, live over SSE**. The saved
  status dialog no longer polls DNS or shows a **Check now** button: it reconciles
  the instant a receiver's readiness changes (the transient `mx.health_changed`
  event), with a 60-second fallback refresh while it is open.
- Dial MX / Antler MX gains an **extra-MX warning**: when a domain with a ready
  Antler receiver also publishes MX records that are not Antler receivers, the
  dashboard aggregate light turns **amber** (individual connectors stay green) and
  the status dialog lists the offending servers with a suggestion to remove them,
  since mail may not be delivered via Antler. The warning is exposed as a top-level
  `unexpected_mx` field on the receiving response (independent of `instructions`),
  so a dialog opened from an amber dashboard row shows it immediately and keeps it
  current live.
- Dial MX / Antler MX published-record checks are now **owned by one background
  watcher** and shared, not re-run per render. Every 60 seconds it computes each
  Dial MX domain's `_mailmoose-mx` TXT check and MX routing check, diffs the verdict
  against the last one, and pushes a transient **`mx.dns_changed`** event only when
  it changes; both the dashboard light and an open status dialog read that one
  verdict, so they can never disagree and no render runs DNS. Because the receiver
  stops at its first failing gate (authority before routing) and so reports only the
  TXT reason, a domain whose TXT *and* MX are both wrong now shows **both** fix
  prompts: the status view is driven by the per-check DNS verdict — not the
  receiver's single reason — and each receiver row shows its specific Antler reason
  (`not in MX`, `key unavailable`, `authentication failed`, receiver unreachable, …)
  inline next to its traffic light instead of a generic "rejected".
- The Dial MX / Antler MX status view now opens **complete**: the receiving form
  embeds the copy-ready DNS records, the shared DNS checks and the extra-MX verdict
  alongside the connector and status sets, so the fix prompts (publish this TXT
  record / these MX records) and the "additional MX servers" warning paint on open —
  the same picture the dashboard light was drawn from — instead of appearing only
  after the first refresh lands.
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
# Google standalone inboxes

- Added standalone provider selection with IMAP/SMTP, Google/Gmail and a
  Microsoft/Outlook coming-next choice.
- Added guided BYO Google app setup, exact callback instructions, OAuth with
  encrypted offline tokens, and a validated callback-URL paste fallback.
- Added Gmail API operations, native labels and stable IDs, progressive metadata
  caching, History API updates, notification polling, sending and draft handoff.
- Added Google setup documentation and deterministic transport/OAuth fixtures.
# Durable mailbox action queue

- Domain and standalone message actions share async submission and `?wait=true`,
  matching send. Queued actions survive restart and execute in message order.
- The header status flag opens the account-wide queue; pending counts and a red
  exclamation identify work and failures. Retry/Dismiss are available in the UI
  and through `/v1/actions`.
- Read/unread, labels, spam, trash, restore, bulk move and purge—including Empty
  Trash—use the shared action worker. Optimistic views preserve provider locators.
- Existing clients expecting immediate mutations should add `?wait=true`.
