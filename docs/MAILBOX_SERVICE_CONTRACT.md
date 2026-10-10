# Mailbox service contract (Wave 0/1 foundation)

Status: **frozen for subsequent waves.** This document is the interface contract
the local, remote and workflow waves implement against. It records what already
exists (Wave 0/1) and the exact ownership of the pieces each later wave builds.
Do not change a signature here without updating this file and `DECISIONS.md`.

Authoritative tree: `repo/`. Every path below is relative to `repo/`.

## 1. Domain model (`internal/model`)

### Inbox kinds — `internal/model/mailbox.go`
- `InboxKindDomain = "domain"`, `InboxKindStandalone = "standalone"`.
- Immutable after creation; the store rejects any change (`store.ErrKindImmutable`).

### `model.Inbox` additions (`internal/model/model.go`)
- `Kind string` — one of the two kinds above.
- `DomainID string` — empty for a standalone inbox; `NULL` in the database.
- `LocalPart string` — empty for standalone; the address is self-contained.
- `Address string` — the full primary address for both kinds.
- `Namespace string` — the selected remote root (an explicit IMAP folder path,
  default `INBOX`) for standalone; the scope of every remote operation.
- `Remote *RemoteConnection` — non-secret remote binding description.
- `RemoteConfigured bool` — whether encrypted remote credentials are present.
- `Capabilities *Capabilities` — populated on retrieval.

### Common types — `internal/model/mailbox.go`
- `Folder{ID, AccountID, InboxID, Path, Name, ParentPath, Role, MessageCount,
  UnreadCount, Selectable, CreatedAt, UpdatedAt}` — a folder is an explicit
  member of the inbox's single selected root; `Path` is the stable opaque
  hierarchical locator relative to the root. Roles are `FolderRole*` constants
  (`folder`, `inbox`, `sent`, `drafts`, `trash`, `spam`, `archive`, `outbox`,
  `label`). Folders are **not** aliases or labels: managed aliases belong to
  inboxes and labels are free-text message metadata, both independent of the
  folder tree.
- `RemoteLocator{FolderPath, UIDValidity, UID, MessageID}` — the remote routing
  address. `Empty()` reports a local record. A locator is valid only while its
  `UIDValidity` still matches the folder.
- `RemoteConnection{Host, Port, Username, Security, SMTP *RemoteSMTP}` and
  `RemoteSMTP{Host, Port, Username, Security}`. Security is `RemoteSecurityTLS`
  (implicit, default), `RemoteSecurityStartTLS`, or `RemoteSecurityPlain`.
  Plaintext is an **explicit operator choice**, never an automatic downgrade;
  the deployment policy layer (not this model) decides whether a given
  deployment permits it, and a connector never falls back from a stronger to a
  weaker mode at runtime.
- `Capabilities{...}` with `DomainCapabilities()` / `StandaloneCapabilities()`.
- `ListEnvelope[T]{Items, NextCursor, Completeness, Errors []InboxFailure}` —
  the common listing shape. `Completeness` describes whether the source could
  enumerate the whole result set and is **independent of pagination**: having a
  `NextCursor` does not make a listing "partial". `Errors` carries per-inbox
  failures (`model.InboxFailure{InboxID, Code, Message, Retryable}`, built via
  `NewInboxFailure`) so a multi-inbox listing can report one failure without
  failing the whole request; failures expose only the normalized `ErrKind*`
  code and a short safe message, never raw provider or store error text.
- `MailboxError{Kind, Message, Retryable, Cause}` with `NewMailboxError` and the
  `ErrKind*` vocabulary. Provider and store failures map onto it so workflow
  code never inspects a native error.

## 2. Persistence (`internal/store`)

### Migrations
- `052` — additive standalone foundation. Rebuilds `inboxes` so `domain_id` is
  nullable and adds `kind`, `address`, `namespace`, `remote_*`, `smtp_*`,
  `remote_configured`; creates `inbox_remote_credentials`, `inbox_folders`,
  `inbox_remote_messages`, the durable `pending_file_cleanup` queue; partial
  unique index `idx_inboxes_standalone_address`.
- `053` — removes external sending aliases: drops `external_aliases`, the three
  attribution columns and their indexes; discards external-alias-specific unsent
  drafts, attachments and queued sends/jobs and refunds the account **and inbox**
  storage counters (including counters that were still NULL); keeps sent history
  and its delivery-log rows and `from_address` attribution; removes orphaned
  threads; preserves ordinary mail and managed aliases. File retirement is
  **post-commit**: raw paths are queued in `pending_file_cleanup` and unlinked by
  `store.sweepPendingFileCleanup` after the migration commits, so a rollback
  never leaves a live row pointing at a missing file. Partial-application is
  detected from the schema (alias table + each alias column), not from a single
  marker. Register migrations only after the latest version; never edit a
  historical migration (their SQL is frozen).

### Standalone CRUD — `internal/store/standalone.go`
- `CreateStandaloneInbox(ctx, accountID, StandaloneCreate) (model.Inbox, error)`
- `ListStandaloneInboxes(ctx, accountID) ([]model.Inbox, error)`
- `SetInboxKind(ctx, accountID, inboxID, kind) error` (idempotent; else `ErrKindImmutable`)
- `SaveRemoteCredentials(ctx, accountID, inboxID, encryptedIMAP, encryptedSMTP string, ConfigVersion) error`
- `GetRemoteCredentials(ctx, accountID, inboxID) (RemoteCredentials, error)`
- `UpsertFolders(ctx, accountID, inboxID, []FolderInput) ([]model.Folder, error)`
  — reconciles the folder set to exactly the input paths (insert/update/prune).
- `ListFolders`, `GetFolder`
- `UpsertRemoteMessage(ctx, accountID, inboxID, RemoteMessageInput) (RemoteMessage, error)`
  — upsert by `(inbox_id, folder_path, uid_validity, uid)`.
- `GetRemoteMessage`, `GetRemoteMessageByUID`, `ListRemoteMessages`, `DeleteRemoteMessage`
- Validation helpers: `ValidateStandaloneAddress`, `FolderRoleForName`.

`inbox_folders` / `inbox_remote_messages` operations return
`ErrStandaloneRequired` when the inbox is a domain inbox.

### Sending — `internal/store/sending.go`
External aliases are removed, so sending has a single target shape:
- `SendingTarget{DomainID string}` — the managed domain whose connector sends.
- `ResolveSendingTarget`, `SendingConfigForTarget`, `SendingConfigForMessage`.

`resolveSendingTargetQuery` resolves a requested sender to the inbox primary or a
managed `inbox_aliases` entry only.

## 3. Application boundary (`internal/app`)

### `internal/app/mailbox_service.go`
- `MailboxBackendKind` — `BackendLocal` (domain) / `BackendRemote` (standalone).
- `MailboxRoute{Inbox, Backend, Capabilities, Remote}`.
- `MailboxRouter` — `NewMailboxRouter`/`Route`/`RouteInternal`/`CapabilitiesOf`/
  `RemoteLocatorFor`/`BackendFor`. It performs one store read and no remote I/O.
- `MailboxBackend` — the practical interface downstream code depends on:
  `Kind()`, `Capabilities()`, `ListFolders(...) model.ListEnvelope[model.Folder]`.
  **Scaffolded now:** `LocalBackend` (domain; maps the inbox to a single INBOX
  folder entry), and `RemoteBackend` (`Kind`/`Capabilities` + `ListFolders` over
  the cached `inbox_folders`, reported as `CompletenessUnknown`). The adapter
  wave swaps `RemoteBackend` for a live-session implementation behind
  `BackendFor` and adds `Sync`, `OpenMessage` (live body/attachment by
  `RemoteLocator`), `MoveMessage`, `AppendRemoteDraft` and the SMTP send path.
  Until then those return `ErrKindUnsupported`, and no code should claim the
  remote service is implemented.
- `StandaloneMailboxService` — `CreateStandalone` (account **Admin** only),
  `ListStandalone`, `GetStandalone`, `ReconcileFolders`, `ListFolders`.
- `mapStoreError` normalizes store errors to `*model.MailboxError`.
- `ErrRemoteNotBound` / `ErrLocalOnly`.

### Assistant modes / drafts / notify (ownership for the workflow wave)
The local draft-authoring flow already exists in `internal/app/draft_workflow.go`
and `internal/app/service.go`. The workflow wave owns:
- **MailMoose approval mode** for a domain inbox: approval email carries the
  one-time token and the frozen `From`; for a standalone inbox the token + From
  are those of the remote identity. Uses `DraftSendRequest` as today.
- **Default one-way remote handoff mode**: append the draft to the standalone
  inbox's remote Drafts folder instead of requesting local approval. New store
  method (not yet present): `AppendRemoteDraft`.
- **Notify**: default to the connected address, optional override; snapshot per
  submitted request.
- **Pending publication/notification** must be modelled separately from SMTP
  success and the Sent-copy status. Extend `draft_send_requests` with a
  publication/notification status column; do not overload `delivery_status`.

## 4. HTTP / API (`internal/httpapp`, `internal/apispec`)
Routes are registered in `internal/httpapp/server.go`; the API reference is a
generated artifact (`make docs`) and `tests/unit/apispec` guards its counts. The
external-alias UI/API/routes/schemas are removed with no compatibility shim.

## 5. Deferred to later waves
- IMAP/SMTP remote adapter (`github.com/emersion/go-imap/v2`,
  `github.com/emersion/go-message`) behind the `MailboxBackend` interface; only
  then is the remote service actually implemented. `RemoteBackend` today is a
  cached-metadata scaffold.
- Deployment policy for `RemoteSecurityPlain`: the standalone UI must show an
  explicit warning before a plaintext remote binding is saved, and the runtime
  layer enforces any deployment-level refusal. Plain is never auto-selected.
- Standalone REST/UI for create/list/get and folder sync.
- Remote message read (live body/attachment fetch by `RemoteLocator`).
- The two assistant modes, notify defaults, and publication statuses above.
