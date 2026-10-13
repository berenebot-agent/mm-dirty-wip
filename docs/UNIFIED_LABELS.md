# Unified Labels UI and API

MailMoose uses **Labels** as the single mailbox-organisation term. Backend folder
and tag representations are deliberately retained: renaming the model must not
silently change an IMAP MOVE into a local metadata update.

## Semantics

- **Multiple:** MailMoose-native labels, Gmail user labels and Microsoft Graph
  categories. A message may have several; Gmail labels and Graph categories are
  changed at the provider, not merely in a shadow local index.
- **Exclusive:** IMAP folders, Microsoft Graph mail folders and existing MailMoose
  local folders. Applying one moves the message. Removing one without a
  replacement is refused; the system Inbox, Sent, Trash etc stay in top-level
  navigation and retain their existing API/actions.
- Gmail's custom labels may also be indexed as remote folders. The unified
  catalogue exposes them only once as additive labels, using the existing Gmail
  label API when applying them.
- Existing IMAP-local metadata tags are preserved and remain visible in the
  legacy API. The unified writer does not create new IMAP-local tags, since those
  would not behave like remote folders.

## API

`GET /v1/inboxes/{id}/label-catalog` returns the standard envelope containing
`id`, `name`, `kind` (`tag` or `folder`), `membership` (`multiple` or
`exclusive`), `source`, `folder_id` when applicable, and `can_remove`.

`POST /v1/inboxes/{id}/messages/{messageId}/labels` takes:

```json
{"label_id":"tag:Invoices","operation":"add"}
```

or an exclusive destination such as:

```json
{"label_id":"folder:fld_123","operation":"add"}
```

The response uses the existing queued-action contract; `?wait=true` has the
usual waiting behaviour. Permission and mailbox ownership are checked before
the action is accepted. Unknown folder IDs cannot move messages across inboxes.
`remove` on an exclusive folder returns 409 rather than implicitly moving a
message. Newly named tags can be applied through `tag:<name>` for native,
Gmail or Microsoft mailboxes; on IMAP an existing remote folder must be chosen.

The existing `/folders`, `/labels` and `PATCH /v1/messages/{id}` routes remain
available to integrations and old clients. Migration 075 only adds a durable catalogue for empty native labels; there is no destructive conversion of message records.

## UI

There is one **Labels** section for custom tags and folder-backed labels. The
message view displays a selector that marks exclusive labels as moving the
message, while keeping free-text creation for providers that support additive
labels. System destinations remain separate. New remote folder-backed labels
continue to use the existing remote folder-management action; native labels can be created before they are applied to a message using a small durable catalogue (migration 075).

The action queue, live mailbox index and remote adapter still own mutations.
Provider failures therefore surface via the existing action-queue feedback.

`POST /v1/inboxes/{id}/label-catalog` accepts `{"name":"Invoices"}` and creates a durable additive native label. Connected Gmail mailboxes create a native provider label. IMAP and Microsoft folder-backed labels are created through the existing provider adapter, with exclusive membership. Existing labels are not migrated or deleted.
