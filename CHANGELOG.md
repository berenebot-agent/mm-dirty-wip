# Changelog

All notable changes to Mammoth Mail are documented here. This project follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- **The reader names system folders in sentence case.** The move chooser, the
  sidebar and the "Moved to …" notice show Inbox, Sent, Spam, Trash and the
  other system folders in our own wording, so an IMAP `INBOX` or an Outlook
  `Deleted Items` is no longer shown as the provider wrote it. A folder the
  account renamed, and every custom folder, keeps its own name. A Microsoft
  mailbox no longer lists an invented Archive destination: Graph answers the
  well-known name `archive` with an unrelated folder.
- **A message mutation answers with its result when it settles quickly.** A move,
  delete, label change, read/spam change, restore, purge or seen call now waits
  up to five seconds for its own action and returns the settled body (the
  message, `204`, or `{id,seen}`). A provider slower than that still returns
  `202` with the durable action and its `Location`, and `?wait=true` keeps its
  30-second bound, so a 23-second IMAP move no longer looks like a failed write
  to a client that reads straight back. The web UI stays asynchronous.
- **Re-issuing a move does not queue it twice.** While an identical move is
  pending or running, submitting it again returns the queued action instead of a
  second one that would apply the move again.
- **Deleting a folder that is already gone at the provider finishes locally.**
  A provider answering "no such folder" (a retry after the first delete landed,
  or an answer lost to a timeout) now prunes the cached row and reports success,
  so the folder disappears from `GET /folders` and its parent stops refusing
  deletion with `409`.
- **A confirmed folder delete always updates the local index.** The prune and the
  folder-tree mirror run on their own bounded context, so a client that gave up
  or a gateway that timed out cannot leave a deleted folder cached. A folder
  mutation with an unresolved provider outcome queues a reconcile instead of
  waiting for an operator refresh.
- **The Sent-placement backfill covers every inbox kind.** Migration `003` filed
  completed sends into the Sent folder only for domain inboxes; it now applies to
  a connected inbox's own Sent folder too, matching the placement every new send
  records. The statement changes data only, so the schema hash is unchanged.
- **The running build is reported.** `/healthz`, `/v1/health`, `/health` and
  `/.well-known/mailmoose` carry a `build` value (the commit passed as
  `MAILMOOSE_BUILD`, otherwise the build timestamp), so a deployed instance can
  be identified without shell access.
- **A folder keeps its id when it is renamed, or when a folder above it is
  renamed.** Renaming a parent used to re-mint the `fld_...` id of every folder
  beneath it, so a client holding a child id then got `404 folder not found`.
  The renamed folder and its whole subtree now keep their ids (only `path` and
  `parent_path` change), for every inbox kind: a domain inbox, Gmail, Outlook,
  IMAP and One. A Gmail label id and an Outlook folder entry id survive a rename
  at the provider, so they are reused rather than replaced; an IMAP rename
  carries its subtree in the local index first, since an IMAP mailbox has no
  stable folder identity. The rename response still returns only the renamed
  folder, so re-read `GET /v1/inboxes/{id}/folders` for descendant paths.

- **A domain inbox's folders report their real message counts.** The folder
  list only ever filled `message_count` and `unread_count` from the connected
  inbox's remote index, which holds nothing for a local mailbox, so every
  folder of a domain inbox reported `0` while its listing returned rows. Each
  role is now counted with the same membership rule the listing applies (Spam
  is spam-classified mail, Trash is trashed mail, Sent is completed outbound
  mail, and Inbox includes the unfiled bucket), so a count is the size of the
  view a client gets from `folder=<id>`.
- **An IMAP custom folder can be renamed.** A rename proposes a name that
  exists nowhere yet, and the scope check only admitted existing folders, so
  every rename — top-level or nested — answered `400` with "renamed folder
  would leave the mailbox scope". Rename now admits a new name by the same rule
  creation uses.
- **Creating a nested folder no longer hides its parent.** A folder create
  mirrored only the new path, and the folder reconcile replaces the remote set,
  so the empty parent (and every sibling) was pruned from the tree and could no
  longer be addressed. A create and a rename now re-read the discovered tree
  and reconcile it whole, so a parent survives its child and a rename carries
  its children.
- **A move to Trash lands without two extra IMAP round-trips.** The move
  re-selected the destination for a `UIDVALIDITY` the last sync already
  recorded, then opened a second connection to refresh the header of the
  message it had just written. Both are skipped: the cached tree supplies the
  destination validity (with a live `SELECT` only as fallback) and the moved
  message is reported from the move's own write.
- **An Outlook category can be renamed and deleted.** Both were refused with
  `409 unsupported_operation`, attributed to the provider, although Graph
  supports `PATCH` and `DELETE` on a master category. Both operations are now
  applied on the provider, addressed by Graph's category id, and the local
  catalogue follows; `rename_labels` and `delete_labels` are true for an
  Outlook inbox.
- **The legacy `folder=<path>` form finds the same mail as the id form.** A
  path selector (`folder=Spam`) was only resolved from a folder's stable
  `fld_…` id, so on a domain inbox it kept an exact-placement filter the
  classification never populates and answered 0 rows while `folder=<Spam id>`
  listed the mail. Folder selectors now resolve a legacy path to the same
  folder row on the inbox-scoped, account-wide, search, thread and Folder-tab
  reads, then apply the same membership rule; on an account-wide query without
  `inbox=` the path resolves only when one inbox uniquely holds it, while a
  path several inboxes share keeps its compatibility behaviour (connected
  inboxes narrowed by path).
- **The Spam folder lists its mail on a domain inbox.** `folder=<Spam id>`
  selected the folder by exact `mailbox_id` placement, but marking a message
  Spam records `is_spam` without filing it, so the folder looked empty while
  `spam=true` found the same message. The Spam folder now uses the Spam role's
  membership rule (spam-classified mail) on the API and in the reader's Folder
  tab, so the folder and the role selector agree.
- **The domain Sent folder lists its mail again.** `folder=<Sent id>` on a
  domain inbox returned an empty list for sends completed before explicit
  filing, because the folder filter matched the exact `mailbox_id` placement
  that those rows never had while every projection (including the listing the
  rows came from) still counts them as Sent by the role rule. All folder-scoped
  domain reads now select Sent by its membership rule — completed outbound mail
  in the inbox — through one shared mapping the account-wide, inbox-scoped,
  search and inbox-thread routes all apply.
- **A Gmail message deleted mid-pass no longer fails the whole sync.** Gmail
  drops a deleted message's history entry asynchronously, so a change window can
  still name a message that `messages.get` then answers 404 for. The deep index
  reconcile treated that as a failed pass, so it retried the same window on every
  pass and logged `remote sync ... status=error` each time (`gmail messages.get:
  Gmail could not find the resource`), while cheap detection already skipped it.
  Reconcile now drops the stale cached row and skips the message, as detection
  does, so the index completes and the error stops recurring.
- **A plain listing spans the whole mailbox on every inbox kind.** An unscoped
  listing is no longer narrowed to a single default folder on a connected
  mailbox: an Outlook inbox now returns the mail in every folder (user-created
  folders included) rather than only its Inbox, and Outlook, IMAP, Gmail and
  domain inboxes all apply the same Spam/Trash exclusion. A plain read is not a
  Trash or Spam view anywhere.
- **`folder=<fld_id>` and role filters compose the same way everywhere.** An
  explicit folder is the location and `trashed`/`spam`/`direction` narrow it,
  so `folder=<Sent>&direction=outbound` lists Sent, `folder=<Inbox>&trashed=true`
  lists nothing, and `folder=<Trash>&trashed=true` still lists Trash. A `fld_…`
  id is resolved to the provider's own folder for a connected mailbox instead of
  being passed through as a provider id, which returned no rows. An unknown
  folder id selects nothing.
- **A completed send is filed in the Sent folder it reports.** Domain system
  folders are seeded when the first send completes, so `folder=<Sent>` lists the
  message instead of an empty folder. Completed sends recorded before that
  filing began are now backfilled into the Sent folder on upgrade, so the
  stored placement agrees with the membership rule the reads apply.
- **Common mailbox contract before Beta 3.** Attachment listings now return
  descriptors with authenticated relative `download_url` values for both domain
  and connected mailboxes. Remote enumeration fetches size-limited MIME live,
  removes its temporary file and never marks mail read; provider/parse failures
  are errors rather than empty attachment lists. Existing local attachment IDs
  and download routes are preserved.
- Remote arrival handles now resolve for message mutations and inbox-scoped
  detail/content/attachment reads as well as the account-wide detail route.
  Mutation responses and polling locations use canonical message IDs; existing
  event payloads are unchanged. Inbox and account authorization still apply.
- Thread detail reports preview completeness and a paginated `messages_url`.
  Connected `thread` message filters span indexed folders instead of only the
  default Inbox. Ordinary thread listings exclude Spam and Trash unless explicitly
  selected. Existing legacy array routes retain their response shapes.
- OpenAPI now describes inbox kind/connection fields, attachment download URLs,
  action queue records, optional request bodies, settled delete/purge/seen
  responses and the inbox-label envelope. Repeated query encoding is corrected.
  Tag documentation makes clear that `tag:Name` is name-based, not an immutable
  object ID; a rename changes the tag on all messages carrying it.

- **Draft send and approval retries replay instead of returning 404.** A
  successful send consumes its draft, so a retry of the same request after a
  lost response previously failed at the draft lookup and never reached the
  idempotency check — a client following the retry contract could re-create
  the draft and send a duplicate. Both paths now match the key against the
  message it enqueued and return that message. The key's fingerprint now
  includes the draft's attachment bytes, so a key reused for different content
  still returns 409. See **D155** and `docs/API.md` §9.

### Changed

- **Pre-promotion contract cleanup:** hosted Tusk MX uses `service: "hosted"` rather than `"antler"`, and the receiving API returns `receiver_manifest` instead of `antler_manifest`. Existing encrypted domain configurations are normalised on read and written with neutral receiver metadata keys at their next save; snapshotted receiver endpoints and setup credentials are preserved. `/.well-known/mailmoose`, the old Antler diagram route and outgoing `X-MailMoose-*` webhook header aliases are removed. The Cloudflare inbound and historical draft handoff compatibility readers remain.

- **The Go module follows the renamed upstream:** `github.com/dellarb/mammothmail`.
  New passphrase-encrypted credentials use a repository-independent salt; existing
  credentials remain readable with the legacy salt. Direct 32-byte keys are
  unchanged. Back up data before upgrading: older binaries cannot decrypt new
  passphrase-encrypted credentials after a rollback.

- **The hosted relay is named Tusk MX.** Antler MX is renamed to **Tusk MX**
  everywhere it is shown: the receiving provider selector, the Dial MX setup
  wizard and status view, the dashboard and domain log labels
  (`Tusk: <host>`), and the docs. The new mail-flow diagram replaces the old
  one. The `dialmx` provider, its `"service": "antler"` value and the legacy
  manifest URL remain unchanged. Saved setups keep their receiver snapshots,
  DNS records and live sessions; new enrolments use the renamed receivers as
  described below. See **D082**.
- **The docs call the product Mammoth Mail throughout.** Every reference in
  `README.md` and `docs/` now uses the current name. Protocol and storage
  identifiers keep their old spelling, because they are what the running code
  actually emits and accepts: `X-MailMoose-*`, `/.well-known/mailmoose`,
  `MAILMOOSE_BUILD`, `_mailmoose-mx` and the `github.com/dellarb/mailmoose`
  module path (subsequently renamed as noted above).
- **New Tusk MX setups enrol with the renamed Tusk receivers.** The receiver
  set now points at `tusk1.hgolabs.com` and `tusk2.hgolabs.com` (receiver ids
  `tusk-1`/`tusk-2`), so a setup saved from here publishes the new MX and TXT
  records and mail flows through the renamed service. The manifest is the copy of
  record at `endpoints.json`; the pre-rename `antler-endpoints.json` is kept at
  the same path with the same receiver set, because older cores still fetch it
  from there and its absence would send them to their compiled-in defaults. A
  core that cannot fetch either manifest falls back to the embedded copy, which
  carries the same two receivers. Existing setups are unaffected: each keeps the
  receiver snapshot it enrolled with, so their published records and live
  sessions do not move.
- **Client-facing names are brand-free (API change explicitly approved).**
  Webhook and forward headers are now `X-Mail-*` (for example
  `X-Mail-Signature`), sent alongside the old `X-MailMoose-*` names during the
  transition. Cloudflare Worker inbound headers are `X-Mail-*` and the old
  names are still accepted. Agent discovery is
  `GET /.well-known/mail-api` (the old path remains an alias) and the auth
  realm is `mail-api`. The DNS proof record is `_mail-proof.<domain>`, and
  environment variables and scripts use `MAIL_*`. GHCR images are
  `mammothmail` and `mammothmail-remote-mx`. See **D161**.

- **Product renamed to Mammoth Mail (API change explicitly approved).** The
  approval authoring mode `mailmoose_approval` is now **`local_approval`**
  everywhere it appears in the API, including `GET`/`PUT
  /v1/inboxes/{id}/authoring`. Clients sending or reading the old value must
  switch. Stored values are migrated (migration 004). The DNS proof record
  moves from `_mailmoose-mx.<domain>` to **`_mail-proof.<domain>`** (D161):
  every domain must republish it. The display name, environment variables,
  client configuration path and script names change to Mammoth Mail /
  `mammothmail`. See **D161**.

- **Unsupported-operation error code (API change explicitly approved).**
  Unsupported mailbox operations keep HTTP **409**, but return
  `code: "unsupported_operation"` instead of `conflict`. Ordinary conflicts
  remain `conflict`. Queued-action failure/timeout responses now include
  `error` and `code` alongside their existing action handles. See **D156**.
- Inbox capabilities add `archive` (null-destination move) and `purge`
  (permanent deletion after Trash), independently of caller permissions and
  provider availability. `realtime` means provider push detection, not whether
  the Mammoth Mail event/SSE API is available. The Beta 3 onward v1 compatibility
  policy is documented in `docs/API.md`.

- **Outbound idempotency rejects mismatched reuse (API change explicitly
  approved).** Keys remain account-wide across send/reply/forward and draft
  sends/approvals, but are now bound to the send inputs, action/source, sender
  and attachments. Reusing a key for a different request returns **409 Conflict**
  (`code: "conflict"`) rather than silently returning an unrelated message.
  Identical retries still replay the original message without sending again.
  Legacy keys without a fingerprint fail closed with 409; reconcile their
  original messages before choosing a new key to avoid duplicate delivery.
  See **D154** and `docs/API.md` §9.

- **Common outbound API response (API change explicitly approved).** Send,
  reply, forward, draft-send, draft writes with `action: "send"`, and draft
  approval now return **200 OK** with the same `queued`, `messageId`,
  `provider_message_id` and `message` fields. **Reply previously returned
  201**; clients checking that status must accept 200 instead. Reply gains
  `queued`/`messageId`; approval gains `provider_message_id`. All these send
  paths support `?wait=true`, and `queued` reflects whether the returned
  message is still pending. Reply also accepts optional `cc`, `bcc` and a
  `subject` override. New `POST /v1/messages/{id}/forward` exposes the UI's
  existing forwarding behavior for local and standalone mailboxes: a new
  conversation containing the original plain text and attachments, with
  explicit recipients and optional compose fields. Bundled Python/Bash
  clients include `forward`. See **D153** and `docs/API.md` §9.

- **API Beta 3 — folders and labels are now two separate things (API change
  explicitly approved).** A message's location is its `folder_id`: one stable
  Mammoth Mail folder id from `GET /v1/inboxes/{id}/folders`, or `null`. Its
  labels are the additive `label_ids` (`tag:Name` only). A folder id is no
  longer a label id: the `folder:<id>` catalogue identity is removed, the
  exclusive/system label classification is gone, and `label_ids` no longer
  carries Inbox, Sent, Spam or Trash as labels — those are folders again. The
  legacy `labels`, `mailbox_id` and `folder_path` fields stay, and now
  describe the same canonical folder. Clients that filtered by a
  `folder:<id>` label must scope the folder with `folder=` instead; clients
  that read system classifications out of `label_ids` must read `folder_id`.
  This supersedes the unreleased label-first contract; the final model is
  recorded as **D151** in `docs/DECISIONS.md` and documented in
  `docs/API.md` §3b.

- Capability discovery is one surface: the inbox. `GET /v1/inboxes/{id}` now
  reports `create_folders`, `rename_folders`, `delete_folders`,
  `create_labels`, `rename_labels` and `delete_labels` beside the existing
  folder, move and label flags. A domain or Gmail inbox has system folders
  only and refuses folder creation, rename and delete; Outlook and IMAP
  inboxes keep their physical custom folders; an IMAP inbox has no canonical
  labels and refuses every label operation and label filter; Outlook
  categories can be created but not renamed or deleted. The label catalogue
  keeps its `capabilities` block for compatibility and still reports the
  label half of the same surface, including on an empty catalogue.

- Gmail mail now reports one canonical folder — the first of Trash, Spam,
  Drafts, Inbox, Sent that the message carries — instead of a set of overlapping
  system labels. A Gmail inbox's folder list is exactly those five system
  destinations: there is no virtual folder, no All Mail and no derived Archive
  view. `{"folder_id":null}` on a move means **archive**: a domain inbox files
  the message in its internal archive, which is not a canonical folder, so the
  message then reports `folder_id: null`; a Gmail inbox removes `INBOX` and
  keeps `SENT`, so an archived message still projects the Sent folder. An
  Outlook or IMAP message always lives in exactly one physical folder, so a
  null move is refused there. System folders stay protected, and Sent, Drafts
  and Outbox are never move destinations.

- A domain mailbox's legacy custom folders are mapped to additive labels
  without being flattened: the folder records keep their original full paths
  and the message relations that point at them, so the organisation can be
  restored, and the label mapping is deterministic, collision-safe (a taken
  name takes the first free numeric suffix) and idempotent through a mapping
  table. Mail in a legacy custom folder has no canonical folder — it reports
  `folder_id: null` and carries the folder's name as a label.

- The Microsoft connector now requests the `MailboxSettings.ReadWrite`
  delegated permission and creates an Outlook label as a native master
  category, recording it locally only after Graph accepts it. Renaming and
  deleting a category stay unsupported. **A Microsoft mailbox connected
  before this permission was added must be reconnected once** from its
  settings; a grant without the new permission is refused with `Microsoft
  mailbox permissions were not granted`. See decision **D152**.

- The domain Receiving dialog now keeps the two SMTP receivers apart. The
  bundled receiver's tuning — SMTP greeting hostname, limits, verification,
  network timeouts and STARTTLS — is shown only while Direct MX is selected, so
  it can no longer read as settings for a Remote MX receiver. The Remote MX
  panel carries the account receiver's URL and bearer key, and states that the
  receiver's own SMTP hostname, limits and verification are configured on the
  receiver. Remote MX uses the same single Save and Remove receiving controls
  as other providers: Save applies the receiver connection and domain settings;
  Remove receiving removes only this domain's selection, keeping shared settings.
- A Remote MX domain can now be given a per-domain authentication enforcement
  policy (moderate or hard) like a Direct MX domain, instead of always grading
  the receiver's evidence at moderate. The default is unchanged.
- The Receiving dialog now shows the account Remote MX receiver's status beside
  its connection, so a domain can be checked against the receiver it actually
  talks to.

- The provider guide and the self-hosting guide now say plainly that a Direct MX
  domain receives no mail until the installation's receiver is configured, and
  that selecting the provider in the dashboard does not configure the receiver.
  The provider list offers Direct MX before the receiver exists, so a domain
  that reads as set up may not be receiving yet.

- The dashboard now shows when a Direct MX domain is not receiving mail. Such a
  domain is flagged "Not receiving" in the domain list, and the Direct MX setup
  panel says that saving the receiving provider does not start the bundled SMTP
  receiver. The flag clears once the receiver is configured.

- Native search pagination is standardized on `before=<next_cursor>` for every
  inbox kind. The former standalone search `cursor` input is rejected with a
  migration hint. Clients must treat returned continuations as opaque strings,
  retain filters, and not treat `before` as a date. No route or message-schema
  replacement is introduced; explicit openagent.email compatibility stays separate.

- Inbox membership is now the same whether a folder is named explicitly or left
  implicit. Asking for the Inbox folder returns the messages that belong to it
  by default placement as well as those moved into it, across message search,
  threads, the local Inbox listing, its counts and its bulk actions. Outbound,
  spam and trashed mail stay out of the Inbox. An account-wide Inbox filter
  still does not reach another inbox's mail.

- Thread lists now accept repeated canonical `label_id` filters. Filters are
  intersected before thread counts and pagination are computed, for local and
  connected inboxes alike.

- The mailbox sidebar now separates the two. A **Folders** heading covers the
  system views, a **Custom folders** section lists the mailbox's physical
  custom folders with the native create, rename and delete controls, and a
  **Labels** section lists the additive classifications. A label never moves a
  message, so a mailbox without additive labels (IMAP) shows no label control
  at all, and a mailbox without custom folders (domain, Gmail) shows no folder
  control. The message reader gained a **Move to** control that files a
  message through the same folder action the API uses, so an archived message
  can be moved back to the Inbox.

### Fixed

- **A message mutation answers with its result when it settles quickly.** A move,
  delete, label change, read/spam change, restore, purge or seen call now waits
  up to five seconds for its own action and returns the settled body (the
  message, `204`, or `{id,seen}`). A provider slower than that still returns
  `202` with the durable action and its `Location`, and `?wait=true` keeps its
  30-second bound, so a 23-second IMAP move no longer looks like a failed write
  to a client that reads straight back. The web UI stays asynchronous.
- **Re-issuing a move does not queue it twice.** While an identical move is
  pending or running, submitting it again returns the queued action instead of a
  second one that would apply the move again.
- **Deleting a folder that is already gone at the provider finishes locally.**
  A provider answering "no such folder" (a retry after the first delete landed,
  or an answer lost to a timeout) now prunes the cached row and reports success,
  so the folder disappears from `GET /folders` and its parent stops refusing
  deletion with `409`.
- **A confirmed folder delete always updates the local index.** The prune and the
  folder-tree mirror run on their own bounded context, so a client that gave up
  or a gateway that timed out cannot leave a deleted folder cached. A folder
  mutation with an unresolved provider outcome queues a reconcile instead of
  waiting for an operator refresh.
- **The Sent-placement backfill covers every inbox kind.** Migration `003` filed
  completed sends into the Sent folder only for domain inboxes; it now applies to
  a connected inbox's own Sent folder too, matching the placement every new send
  records. The statement changes data only, so the schema hash is unchanged.
- **The running build is reported.** `/healthz`, `/v1/health`, `/health` and
  `/.well-known/mailmoose` carry a `build` value (the commit passed as
  `MAILMOOSE_BUILD`, otherwise the build timestamp), so a deployed instance can
  be identified without shell access.

- Message placement changes now publish durable lifecycle events consistently,
  and pending or failed outbound messages no longer appear in Sent. Connected
  message listings also honor `has_attachment=false`, as well as `true`.

- Restored beta-2 `unread_count` on connected-inbox thread lists and details,
  and included it consistently for domain inboxes and account-wide thread
  routes. Filtered lists count unread messages in the same matching subset as
  `message_count`, and all-read threads explicitly return `unread_count: 0`.

- Direct MX's DNS instruction now points the client domain at the hostname in
  Mammoth Mail's configured receiving URL (or public URL), not the separately
  configurable SMTP greeting hostname.
- Remote MX setup now saves the account connection, domain selection and spam
  policy in one submission. Validation errors reopen Receiving with non-secret
  inputs retained; a blank bearer key retains the saved credential. Unchanged
  connection settings are not rewritten when saving a domain's spam policy.
- The domain Receiving dialog no longer offers a Remote MX domain the shared
  Direct MX receiver's settings.
- The Direct MX panel now names both sides of the instruction — the domain whose
  MX records to publish and the receiver hostname to point them at.

- Connected mailbox search on an unknown folder now returns an empty page, like
  the connected message listing, instead of passing the nonexistent folder to
  the provider. Gmail in particular would otherwise surface its own
  invalid-label error to API clients.

- Domain and connected inbox listings now share message/thread/search handlers,
  filters and `before=<next_cursor>` pagination. Single-inbox thread/search
  continuations no longer disappear, standalone search respects `folder`, and
  local search respects `subject` and `unread`. Provider-native tie ordering is
  retained so a search continuation cannot skip equal-date results. Partial
  remote indexes are no longer advertised as complete by account-wide reads.
- Local attachment downloads now work through `/v1/messages/{id}/attachments/{part}`
  as well as the inbox-scoped and attachment-ID routes.
- Agent guidance, OpenAPI, examples and mailbox docs now describe the label
  catalogue, provider capabilities, authoring outcomes and current Empty Trash
  behavior. The Python client no longer fails at startup on duplicate `--table`
  arguments; both example clients show catalogue capabilities when scoped to an inbox.

- A remote-draft handoff whose destination folder does not exist now fails
  instead of ending up ambiguous. The remote server's refusal (`NO [TRYCREATE]
  No such folder`) is a definitive answer, but it was classified as a transient
  fault, so the product retried eight times over about half a minute and then
  settled the handoff as `ambiguous` — the one publication state an operator
  cannot act on, for an outcome where nothing had happened at all. The refusal
  is now terminal: the handoff settles as `failed` with the reason `the
  connected mailbox has no Drafts folder`, it is not retried, and the local
  draft is unfrozen so it can be edited or requested again. A server that
  refuses the operation for a reason of its own (a permission or policy
  refusal) is treated the same way. The API reference no longer claims such a
  handoff is refused at request time: it never was.
- The Receiving panel now names the HTTP status the receiver URL answered the
  session with, e.g. "receiver refused the session (HTTP 405)" or "receiver
  reported a server error (HTTP 502)". A proxy in front of a receiver answers in
  its own right, and the core cannot tell a proxy's answer from the receiver's,
  so the status is shown and nothing is attributed — but a self-hoster whose
  proxy answers an error no longer sees a bare "unreachable" or an endless
  "reconnecting" with nothing to match against the proxy's log. The status is
  also returned as `http_code` on the receiver status rows and on the account
  Remote MX status.
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
- Moving a message to a folder is now discoverable by an API client. The route
  `POST /v1/inboxes/{id}/messages/{messageId}/move` has always worked, but it
  was missing from the machine-readable contract, so it appeared in neither
  `GET /openapi.json` nor the agent guide at `GET /agent`, and a client that
  read those could not find a way to file a message out of a custom folder. The
  route is now in both, with its request body documented. The two answers that
  used to be dead ends name it: a folder-backed label cannot be removed (the
  message says where to move it instead), and a system folder such as the
  Inbox is not a label (the answer still is a `404`, but it says to use the
  move route).
- A stale or unknown pagination cursor is now refused instead of being
  guessed at. A `before` cursor that no longer resolves — because the message
  it named was deleted, or because it was never a cursor at all — used to be
  treated as an empty position, so the listing silently restarted from the
  newest message and a pager looped forever, or came back empty and looked
  like the end of the mailbox. The listing now fails with an `invalid cursor`
  error, for local and connected inboxes alike, across message, thread and
  search pages. The old `cursor` query parameter is refused the same way and
  the answer names `before`, the parameter that does work; the API reference
  already said search no longer accepted `cursor`.
- The dashboard no longer shows an outbound delivery that has no message record
  as a second, unopenable row in "Recent messages". Such a transition is an
  audit event rather than a mail, and it now gets its own "Unlinked outbound
  delivery events" table, so one send is one row and the events that cannot be
  attributed to a message stay visible for investigation instead of being
  inferred from the subject, recipient and timing, which two independently
  sent messages can share.

### Changed

- The Remote SMTP receiving guide now states what a reverse proxy in front of a
  separate receiver has to do: the session endpoint speaks HTTP/2 only, the
  request body must be streamed rather than buffered, and a cleartext receiver
  admits sessions only from a trusted proxy peer.
- Direct MX and Remote MX setup now live in their respective domain Receiving
  panels. Direct MX shows its shared-receiver notice before enforcement and
  receiver tuning; Remote MX describes the outbound receiver connection and
  links to its setup guide. The Direct MX dialog widens while that provider is
  selected.
- Direct MX no longer tells operators to publish an SPF record just to receive
  mail. SPF verification checks the incoming sender's domain; the receiving
  domain needs an MX record pointing to the receiver.

## [0.1.0-beta.2] - 2026-10-20

- Release polish to address full github publish workflow.

## [0.1.0-beta.1] - 2026-10-20

Initial release of Mammoth Mail: self-hosted email infrastructure for people and
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
