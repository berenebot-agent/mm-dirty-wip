# Changelog

All notable changes to MailMoose are documented here. This project follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

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
  MailMoose folder id from `GET /v1/inboxes/{id}/folders`, or `null`. Its
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

- Message placement changes now publish durable lifecycle events consistently,
  and pending or failed outbound messages no longer appear in Sent. Connected
  message listings also honor `has_attachment=false`, as well as `true`.

- Restored beta-2 `unread_count` on connected-inbox thread lists and details,
  and included it consistently for domain inboxes and account-wide thread
  routes. Filtered lists count unread messages in the same matching subset as
  `message_count`, and all-read threads explicitly return `unread_count: 0`.

- Direct MX's DNS instruction now points the client domain at the hostname in
  MailMoose's configured receiving URL (or public URL), not the separately
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
