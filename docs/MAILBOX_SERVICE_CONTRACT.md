# Mailbox service contract

Status: **implemented.** This document is the interface contract the common
mailbox surface (local/domain and remote/standalone) implements and that the
workflow, HTTP/API and UI layers depend on. It records the shared model, the
per-backend ownership, and the honest limits of the remote index. SQLite's exact
schema is described by the migrations; this file describes behaviour.

Authoritative tree: `repo/`. Every path below is relative to `repo/`.

## 1. Domain model (`internal/model`)

### Inbox kinds — `internal/model/mailbox.go`
- `InboxKindDomain = "domain"`, `InboxKindStandalone = "standalone"`.
- Immutable after creation; the store rejects any change (`store.ErrKindImmutable`).

### `model.Inbox` (`internal/model/model.go`)
- `Kind string` — one of the two kinds above.
- `DomainID string` — empty for a standalone inbox; `NULL` in the database.
- `LocalPart string` — empty for standalone; the address is self-contained.
- `Address string` — the full primary address for both kinds.
- `Namespace string` — the selected remote root (an explicit IMAP folder path)
  for standalone; the scope of every remote operation. The default is `INBOX`.
- `Remote *RemoteConnection` — non-secret remote binding description.
- `RemoteConfigured bool` — whether encrypted remote credentials are present.
- `RemoteSentCopyEnabled bool` / `RemoteSentCopyFolder string` — the standalone
  Sent-copy switch and its optional explicit destination.
- `Capabilities *Capabilities` — populated on retrieval.

### Namespaces and folder scope — the INBOX default is the personal namespace
- The default root (`NamespaceDefault = INBOX`, or an empty namespace) resolves
  to the server's **personal namespace** as discovered by the IMAP `NAMESPACE`
  command: the personal prefix and everything under it. When the server reports
  a non-empty personal prefix, only that prefix and its children are in scope.
- **Shared and Other-users namespaces are always excluded**, whatever the
  personal prefix — even when the personal prefix is empty (INBOX at the top
  level). The adapter records the server's `Other` and `Shared` namespace
  prefixes as exclusions and rejects any path under them first, so a single
  personal mailbox never mirrors or reaches a shared/upstream namespace.
- **When `NAMESPACE` is unsupported (or reports no personal descriptor)** there is
  no discoverable personal boundary, so the scope is deliberately **conservative**:
  only the explicit selected root (the default `INBOX`) and its children, never
  the whole login. An empty prefix is therefore not treated as "everything".
- An **explicit custom root** (any other folder path) is a strict root: it scopes
  the inbox to that folder **and its children only**. A path outside it is not
  addressable. This is the fallback when the server exposes no personal
  namespace, and the way to point an inbox at a single non-INBOX folder.
- Enforcement lives in the IMAP adapter's `RootScope.InScope`
  (`internal/transport/imap/adapter.go`, resolved in `folders.go`) for discovery,
  and in `RemoteMailboxService.folderInScope`
  (`internal/app/remote_mailbox_ops.go`) for every remote read and mutation.
- **Setup note / limitation:** the connector setup UI accepts a "sync root"
  namespace. A blank value means the personal namespace; a non-blank value is a
  literal folder path used as a strict root. On a server whose personal namespace
  is genuinely top-level (empty prefix) the conservative fallback still scopes to
  `INBOX` and its children unless the operator enters an explicit root, so a
  top-level sibling folder on such a server must be reached by setting that root
  (or mapping it by role via the connection-settings page).

### Common types — `internal/model/mailbox.go`
- `Folder{ID, AccountID, InboxID, Path, Name, ParentPath, Role, MessageCount,
  UnreadCount, Selectable, IsSystem, RoleLocked, Origin, CreatedAt, UpdatedAt}`
  — a folder is an explicit member of the inbox's single selected root; `Path`
  is the stable opaque hierarchical locator relative to the root. Roles are
  `FolderRole*` constants (`folder`, `inbox`, `sent`, `drafts`, `trash`, `spam`,
  `archive`, `outbox`, `label`). `Origin` is `local` (owned by the account:
  seeded system or operator-created) or `remote` (mirrored from the provider).
  `IsSystem` marks a protected seeded folder; `RoleLocked` marks an explicit
  operator role mapping that a reconcile must never re-infer. Folders are
  **not** aliases or labels: managed aliases belong to inboxes and labels are
  free-text message metadata, both independent of the folder tree.
- `RemoteLocator{FolderPath, UIDValidity, UID, MessageID}` — the remote routing
  address. `Empty()` reports a local record. A locator is valid only while its
  `UIDValidity` still matches the folder.
- `RemoteConnection{Host, Port, Username, Security, SMTP *RemoteSMTP}` and
  `RemoteSMTP{Host, Port, Username, Security}`. Security is `RemoteSecurityTLS`
  (implicit, default), `RemoteSecurityStartTLS`, or `RemoteSecurityPlain`.
  Plaintext is an **explicit operator choice**, never an automatic downgrade;
  there is no runtime fallback from a stronger to a weaker mode. A self-hosted
  deployment may refuse it (see the UI warnings and `docs/SELFHOSTING.md`).
- `Capabilities{...}` with `DomainCapabilities()` / `StandaloneCapabilities()`
  (`StandaloneCapabilities` sets `Sync=true`; `Outbound` is refined per inbox to
  whether an SMTP binding exists).
- `ListEnvelope[T]{Items, NextCursor, Completeness, Errors []InboxFailure}` —
  the common listing shape. `Completeness` describes whether the source could
  enumerate the whole result set and is **independent of pagination**: having a
  `NextCursor` does not make a listing "partial". `Errors` carries per-inbox
  failures (`model.InboxFailure{InboxID, Code, Message, Retryable}`, built via
  `NewInboxFailure`) so a multi-inbox listing can report one failure without
  failing the whole request; failures expose only the normalized `ErrKind*` code
  and a short safe message, never raw provider or store error text.
- `MailboxError{Kind, Message, Retryable, Cause}` with `NewMailboxError` and the
  `ErrKind*` vocabulary. Provider and store failures map onto it so workflow
  code never inspects a native error.

## 2. Persistence (`internal/store`)

The schema is defined by migrations `052` (additive standalone foundation and
the shared `inbox_folders` table) through `058`. Later migrations are additive
and never rewrite a historical migration's SQL. Key tables:

- `inboxes` — `kind`, `address`, `namespace`, `remote_*`, `smtp_*`,
  `remote_configured`, `remote_sent_copy_enabled`, `remote_sent_copy_folder`,
  `authoring_mode`, `notify_default_address`, and the remote index state
  (`remote_index_status/remote_indexed_at/remote_index_error`).
- `inbox_folders` — the single folder table for BOTH inbox kinds: `path`, `name`,
  `parent_path`, `role`, `selectable`, `is_system`, `origin`, `role_locked`,
  `remote_uid_validity`, `remote_indexed_at`, and opaque `remote_metadata_json`.
- `messages.mailbox_id` — a message's single folder membership; `NULL` is the
  implicit system Inbox bucket.
- `inbox_remote_credentials` — the encrypted IMAP and optional SMTP secrets
  under `APP_ENCRYPTION_KEY`.
- `inbox_remote_messages` (header/thread metadata cache only), `inbox_remote_labels`,
  `inbox_remote_cursors`, `inbox_remote_arrivals`, `inbox_remote_notifications`,
  `inbox_remote_actions`, `remote_sent_copies`, `assistant_handling_requests`,
  and the durable `pending_file_cleanup` queue.

### Folder and message membership (`internal/store/folders.go`)
- A message is in exactly one folder (`MoveMessageToFolder`); a move emits a
  durable `message.folder_changed` event. `EnsureSystemFolders` seeds the
  protected system folder set (Inbox, Sent, Drafts, Archive, Outbox, Spam,
  Trash) lazily per inbox of either kind.
- `SetFolderRole` maps an existing folder (possibly an arbitrarily-named remote
  folder such as "Old Mail") to a role and persists `role_locked=1`, so a later
  reconcile never resets it by re-inferring the role from the name. Roles map to
  at most one folder. A seeded system role is asserted, never reassigned.

### Standalone CRUD and remote input (`internal/store/standalone.go`)
- `CreateStandaloneInbox`, `ListStandaloneInboxes`, `SetInboxKind` (idempotent
  else `ErrKindImmutable`), `UpdateStandaloneRemote`, `SaveRemoteCredentials`,
  `GetRemoteCredentials`, `UpsertFolders`, `ListFolders`, `GetFolder`,
  `UpsertRemoteMessage`, `GetRemoteMessage`, `GetRemoteMessageByUID`,
  `ListRemoteMessages`, `DeleteRemoteMessage`, `SetRemoteSentCopy`.
- Validation helpers: `ValidateStandaloneAddress`, `FolderRoleForName`.
- `inbox_folders` / `inbox_remote_messages` operations return
  `ErrStandaloneRequired` when the inbox is a domain inbox.

### Sending (`internal/store/sending.go`)
External aliases are removed, so sending has one target shape per kind:
- `SendingTarget{DomainID string}` — the managed domain whose connector sends
  (domain inbox), resolved through `ResolveSendingTarget` /
  `SendingConfigForTarget` / `SendingConfigForMessage`.
- `SendingTarget{InboxID string}` — a standalone inbox's own remote SMTP
  binding, resolved through the injected `StandaloneSenderResolver`
  (`SetStandaloneSenderResolver`). When no resolver is installed the
  configuration is `ErrNoProvider`, so a standalone message is **queued and
  held** rather than attributed to a domain that does not exist.
- `resolveSendingTargetQuery` resolves a requested sender to the inbox primary
  or a managed `inbox_aliases` entry only (a standalone inbox sends as its own
  connected address only). A standalone request for any other sender is
  `ErrSenderNotAllowed`.

## 3. Application boundary (`internal/app`)

### Router, capability surface and config (`internal/app/mailbox_service.go`)
- `MailboxBackendKind` — `BackendLocal` (domain) / `BackendRemote` (standalone).
- `MailboxRoute{Inbox, Backend, Capabilities, Remote}`.
- `MailboxRouter` — `NewMailboxRouter` / `Route` / `RouteInternal` /
  `CapabilitiesOf` / `RemoteLocatorFor` / `BackendFor`. One store read, no
  remote I/O.
- `StandaloneMailboxService` — `CreateStandalone` (account **Admin** only),
  `ListStandalone`, `GetStandalone`, `ReconcileFolders`, `ListFolders`.
- `mapStoreError` normalizes store errors to `*model.MailboxError`.
- `ErrRemoteNotBound` / `ErrLocalOnly`.
- The `MailboxBackend` interface (`Kind`, `Capabilities`, `ListFolders`) and its
  `LocalBackend`/`RemoteBackend` implementations remain a thin capability
  surface. The full live remote implementation is `RemoteMailboxService` below;
  production dispatch happens in `internal/httpapp/mailbox_access.go`, which
  resolves an inbox to its backend once and routes every read/mutation.

### Live remote surface (`internal/app/remote_mailbox.go`,
`remote_mailbox_ops.go`, `remote_mailbox_more.go`)
`RemoteMailboxService` opens a bounded IMAP session per operation, caches only
header/thread/folder metadata, and answers the live operations:
- **Configuration:** `ConfigureStandaloneRemote` (non-secret fields plus
  encrypted secrets; a blank secret retains the stored one), `TestStandaloneRemote`
  (authenticate + resolve folder scope + optional SMTP AUTH check, no mailbox
  mutation), `ResolveRemote`, `InstallRemoteBridges`.
- **Folders:** live `DiscoverFolders`, `CreateRemoteFolder`,
  `RenameRemoteFolder`, `DeleteRemoteFolder` (refuses a non-empty folder),
  `SetRemoteFolderRole`.
- **Messages:** `ReconcileRemote`, `ListRemoteMessages`, `GetRemoteMessage`
  (best-effort live header refresh, never marks seen), `FetchRemoteRaw` /
  `FetchRemoteAttachment` (streamed to a transient temp file, bounded by the
  message-size cap, **never archived**), `SetRemoteRead`, `SetRemoteFlagged`,
  `MoveRemoteMessage`, `PurgeRemoteMessage` (UID-targeted `UID EXPUNGE`, owner
  only, only from the Trash-role folder), `ResolveRemoteReply`,
  `ResolveRemoteForward`.
- **The common single-message GET returns the live body, or `503`.**
  `GET /v1/inboxes/{id}/messages/{messageId}` hydrates the body live (fetch +
  parse, never archived), so a remote read matches a local read. There is **no
  offline/degraded fallback**: if the connector is unreachable or the body cannot
  be parsed, the request answers `503`, rather than silently serving cached
  metadata. List reads stay metadata-only.
- **Threads and search:** `ListRemoteThreads`, `GetRemoteThread`, `SearchRemote`,
  `ListRemoteLabels`, `SetRemoteLabels` / `AddRemoteLabels` / `RemoveRemoteLabels`.
- **Sending / copies:** `RemoteStandaloneSender.ResolveInboxSendingConfig` (the
  injected `StandaloneSenderResolver`), `RemoteHandoffPublisher` (append the
  frozen draft to the remote Drafts folder and verify),
  `RemoteSentCopyPublisher` + `CopyPendingSentCopies` (a durable queue copying a
  sent message into the remote Sent folder; it never re-sends). The copy job
  **owns an independent frozen copy of the raw MIME** for its whole lifetime, so
  trashing and purging the outbound message before the copy settles cannot make a
  pending/ambiguous copy unverifiable.

### Remote index completeness (honest limits)
`ReconcileRemote` refreshes the folder tree and each selectable folder's header
index **progressively**. Every pass refreshes the newest `remoteNewestRefresh`
(500) messages so recent mail and flag changes are current, and advances a
**persisted per-folder backfill cursor** (`inbox_folders.backfill_before_uid` /
`backfill_complete`) downward by one bounded batch of
`DefaultRemoteReconcileLimit` (2000) messages. A folder larger than one batch is
therefore **eventually indexed in full over successive passes — it is not
permanently truncated**. The persisted cursor is scoped to the folder's
UIDVALIDITY; a UIDVALIDITY change resets it.

The index `status` (`never_started`, `partial`, `complete`, `error`) is recorded
on the inbox and exposed via `GET /v1/inboxes/{id}/remote/refresh` and the
remote-config view. It is `partial` while any selectable folder's backfill has not
reached the bottom (or a pass was interrupted / a provider error occurred), and
`complete` only once every folder's backfill has reached the bottom. A scoped
read reconciles on demand when its cached view is stale, and paging through older
mail also drives one backfill batch so old mail is never permanently hidden.

**Hard ceiling — a distinct, honest limit:** the reconcile holds the folder's
**complete UID set** in memory to prune removals safely. That set is capped at
`remoteUIDSetCap = 500000` UIDs (four bytes each). A folder whose UID set exceeds
this cap is **not snapshotted in full**: the pass indexes the newest window only,
**skips prune**, and does **not** advance the backfill cursor or claim
completeness (the cursor is relative to the full set, which it does not have). Such
a folder remains `partial` and is never falsely reported done. This is not the
normal case — ordinary folders (even large ones) backfill progressively to
`complete`; the 500k UID cap is the extreme-size guard.

Removals are pruned only against a **complete** UID snapshot (`fullKnown`); a
partial or windowed view never deletes cached mail.

### Assistant handling modes, drafts, notify (`internal/app/draft_workflow.go`,
`draft_handoff.go`, `remote_worker.go`, `control.go`)
- **`MailMooseApproval`** (a domain inbox's preset and default): the in-product
  approval workflow; the approval email carries the one-time token and the frozen
  `From`.
- **`RemoteDraft`** (a standalone inbox's default): a one-way handoff that
  appends the frozen draft to the connected remote Drafts folder. MailMoose
  never sends it. A handoff notification (carrying the correlation header, no
  token) tells the human a draft is waiting.
- The mode is snapshotted onto each request at creation, so an inbox setting
  change never affects an in-flight request. A standalone inbox can be switched
  to `MailMooseApproval` (its approver identity is matched against the message
  `From`; see the accepted risk below). `RemoteDraft` is a standalone-only mode:
  a domain inbox cannot be set to it (the write is rejected, and a legacy domain
  row stored as `remote_draft` normalizes back to the kind default on read), and
  `requestRemoteDraft` requires `kind=standalone` and a mapped remote Drafts
  folder.
- **Notify**: defaults to the inbox's own connected address, with an optional
  per-inbox override, snapshotted per submitted request.
- **Publication, notification and Sent-copy are separate state machines.** A
  handoff's publication state (`pending`/`published`/`ambiguous`/`failed`) is
  independent of its notification state; the Sent-copy is a separate durable job
  that never re-runs the SMTP send. Publication never claims guaranteed
  exactly-once: an append whose result cannot be verified is `ambiguous`, not a
  blind retry.
- **A handoff's outcome is exposed on the Draft response and in a history
  listing.** `model.Draft.Handoff` (`AssistantHandlingRequest`) carries the
  immutable handoff id plus the publication and notification states, populated on
  draft retrieval (`internal/store/drafts.go`), and `GET
  /v1/inboxes/{id}/handoffs` lists the inbox's handoffs newest first, **retaining
  terminal records** (`published`, `ambiguous`, `failed`/`cancelled`) even after
  the local draft has been cleaned up. The handoff events
  (`draft.handoff_requested`/`_published`/`_ambiguous`/`_failed`/`_cancelled`/
  `_notification_sent`/`_notification_failed`) carry the same non-secret fields.
- **Cancel and retry are explicit, bounded, and never automatic.** A **pending**
  or **ambiguous** handoff can be **resolved** (`Store.CancelHandoff` / the UI
  action): it becomes `failed` with `last_error="cancelled"`, the draft returns
  to editable, and a `draft.handoff_cancelled` event is emitted. Allowing an
  ambiguous handoff to be resolved gives the operator an exit so its frozen draft
  is not trapped. An already-`published` handoff cannot be cancelled (`409`). A
  **retry** (UI `retry-handoff`) is a deliberate **re-request** that routes
  through the inbox's effective authoring mode (`RequestSend`), so it only
  produces a new handoff while the inbox is in `remote_draft` mode; it never
  re-appends blindly. In particular, an `ambiguous` append — one whose outcome
  could not be verified — is **never automatically retried**; the human resolves
  or re-requests it. A **transient verification failure** (a lookup error after
  an unconfirmed append) is *not* terminal: the handoff stays pending and the next
  pass re-verifies by lookup (never a blind re-append), bounded by
  `maxHandoffAppendAttempts`. A definitive not-found with no other copy is
  ambiguous; more than one remote match is ambiguous. A retry of a missing frozen
  file verifies by lookup instead of re-appending different bytes.
- **Effective-mode controls.** `GET /v1/inboxes/{id}/authoring` reports the
  effective `mode`, the kind `default_mode`, the notify override, whether the
  inbox `standalone`, and `approver_enabled` — which is true exactly when the
  effective mode is `mailmoose_approval` (a `remote_draft` inbox has no in-app
  approver, regardless of kind). A domain inbox is preset to `mailmoose_approval`:
  `PUT /authoring` rejects `remote_draft` for a domain inbox with `400`, and the
  UI hides the mode selector and notify override for a domain inbox rather than
  offering them.

### Remote detection and approval (`internal/app/remote_worker.go`, `control.go`)
- `RemoteWorker` watches each remote-configured standalone inbox (IDLE with a
  bounded polling fallback), records exactly one durable arrival per genuinely
  new message, and emits a durable event before any fan-out. The first pass
  establishes a baseline without emitting, so enabling the watcher never floods.
  Both reads are bounded and correct on a folder larger than one search page: the
  baseline reads the true maximum UID with a newest-first single-UID search, and
  each incremental pass fetches the oldest new UIDs with an IMAP-native
  `(cursor+1):*` search (`SearchQuery.AfterUID`), never the whole folder and never
  the oldest page in place of the newest.
- **Event entity ids for remote mail are the arrival id, and the canonical read
  resolves it.** A remote arrival event's `entity_id` is the durable arrival id,
  which is deliberately distinct from the cached-metadata id (detection is
  independent of, and may run before, the metadata reconcile). The canonical read
  surface resolves an arrival id to its metadata row — materializing the row from
  the arrival's durable header fields when the reconcile has not yet run — so a
  client that follows an event straight into `GET /v1/messages/{id}` (or the
  common read) always resolves the message. An arrival whose recorded UIDVALIDITY
  has since changed cannot be resolved and reports `not_found`.
- A standalone approval control message is handled by
  `Service.HandleRemoteApprovalControl`, which matches the message's `From`
  address directly against the nominated approver plus the single-use token.
  There is no provider envelope attestation on this path; see the accepted risk
  in `SECURITY.md`.

## 4. HTTP / API (`internal/httpapp`, `internal/apispec`)

- `internal/httpapp/mailbox_access.go` is the single local/remote dispatch: it
  resolves an inbox to a `mailboxBackend` (`routed` = standalone/remote) and
  every `/v1` read/mutation branches on capability, never on a transport type.
- Common routes (`internal/httpapp/standalone_api.go`) return the shared envelope
  `{items, next_cursor, completeness, errors}` for folders, messages, threads,
  search and labels, on both inbox kinds.
- **Account-wide listings merge into one globally ordered stream.**
  `internal/httpapp/mailbox_merge.go` merges the local store with every
  authorized remote inbox into a single date-sorted page (not per-inbox
  concatenation). The opaque cursor (`commonCursor`) records each source's own
  native progress plus the **global stable key** (`<RFC3339Nano>|<id>`) of the
  last returned item, so a later page resumes each source strictly after its own
  progress and drops anything not strictly older than the global key — no
  duplicates, no gaps, even when items interleave across sources or share a
  timestamp. The per-source key is the message/thread **id** (not a timestamp),
  because a timestamp is not unique and would drop same-second siblings on the
  next page. A source failure is recorded per-inbox and never fails the page.
- **Search and filtered lists continue correctly under local filtering.**
  Because the remote source cannot express every local filter (notably
  `has_attachment`), the merge and the scoped remote list pull successive raw
  pages until enough matches are collected or the source is exhausted, resuming
  from each returned item's own native cursor (`SearchRemoteResult.NextCursor`
  for search; the metadata id for a list). They never stop at the first
  filtered-out window, so older matching mail is not hidden behind a page of
  non-matches. When a bounded scan reaches its per-request page cap without
  finding a match, it carries a **scan continuation** (`commonCursor.Scan`, or the
  returned `next_cursor` for the scoped list) so the next request resumes deeper
  instead of silently omitting a match beyond the cap. Completeness is `partial`
  whenever a remote source participates or a failure occurs.
- Routes are registered in `internal/httpapp/server.go`; the API reference is a
  generated artifact (`make docs`) and `tests/unit/apispec` guards its counts.
  The external-alias UI/API/routes/schemas are removed with no compatibility
  shim.

## 5. Known limits (report, do not paper over)

- **Progressive backfill, with a 500k UID snapshot ceiling.** Ordinary large
  folders are backfilled to `complete` over successive passes via the persisted
  per-folder cursor; the 2000-message batch is a per-pass bound, **not** a
  permanent cap. The one hard ceiling is `remoteUIDSetCap = 500000`: a folder
  whose complete UID set exceeds it is indexed newest-window-only, prune is
  skipped, and it stays `partial` (never falsely `complete`). Do not describe a
  status that is `partial` as complete.
- **Remote bodies are transient, and the common GET has no offline fallback.**
  Only header/thread/folder metadata is cached. A body or attachment is fetched
  live per request and never archived; if the connector is unreachable the common
  single-message GET answers `503` rather than serving cached metadata as the
  body.
- **Shared/Other namespaces are excluded, and an empty-prefix namespace is
  conservative.** A personal scope never reaches the server's Shared or Other
  namespaces. On a server with no discoverable personal namespace the scope falls
  back to the explicit root (`INBOX`) and its children, so a top-level sibling
  requires setting an explicit root — see the setup note in §1.
- **Provider Trash expiry is the provider's concern.** The local retention
  sweeper touches only locally-persisted messages; a remote Trash folder's
  server-side expiry is never swept locally.
- **Deployment policy for `RemoteSecurityPlain`** is enforced by the runtime
  layer, not the model: the standalone UI warns before a plaintext binding is
  saved (on the create dialog and the connection-settings page), plain is never
  auto-selected, and a connector never downgrades at runtime.
