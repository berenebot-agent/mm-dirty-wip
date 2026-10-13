# Unified Labels

**Labels** is the single organisational term in the MailMoose UI. System
destinations (Inbox, Drafts, Sent, Outbox, Spam and Trash) remain at the top
of the mailbox sidebar; a compact Labels section follows with a single "+"
button.

Messages have a **single free-text Add label field**, with HTML datalist
suggestions for known labels. There is no Apply Label selector and no separate
Move-to-folder dropdown. Applying an existing folder-backed label moves the
message, and the suggestion explicitly says so. Unrecognised free-text names
create an additive label on MailMoose-native, Gmail, One/Gmail and Microsoft
mailboxes. On IMAP, labels are remote folders: create them in the sidebar first
and type the existing folder name to move a message.

## Provider semantics

| Provider | Label type | Multiple | Label creation |
|---|---|---|---|
| MailMoose-native | Local metadata | Yes | Persistent empty labels or free-text |
| IMAP | Remote folder | Exclusive | Remote folder, via Labels "+" |
| Google Gmail | Native user label | Yes | Gmail provider API |
| One/Gmail | Native Gmail via One passthrough | Yes | Gmail provider API |
| Microsoft Graph | Category or remote folder | Categories yes; folders exclusive | Free-text category or folder via legacy API |

Outlook categories and folders remain distinct internally. If they share a
name, typing it selects the category; API clients can explicitly use a
`folder:` ID to move. Gmail user-label membership is multiple even though
the remote index also exposes labels as folder-like views. An IMAP legacy
local-only tag is not presented as a supported new label, but remains stored and
can be removed by an existing legacy client.

## Canonical API

- `GET /v1/inboxes/{id}/label-catalog`: capabilities and membership. The
  response carries `capabilities` (`can_create`, `create_kind`,
  `create_membership`, `can_rename`, `can_delete`) so an **empty catalogue** still
  describes what the inbox supports without the client branching on backend.
- `POST /v1/inboxes/{id}/label-catalog` body `{"name":"Invoices"}`: create. The
  returned descriptor is the same projection the listing returns.
- `PATCH /v1/inboxes/{id}/label-catalog/{labelId}` body `{"name":"New"}`: rename if supported.
- `DELETE /v1/inboxes/{id}/label-catalog/{labelId}`: delete if supported.
- `POST /v1/inboxes/{id}/messages/{messageId}/labels?wait=true` body
  `{"name":"Invoices","operation":"add"}` or
  `{"label_id":"folder:ID","operation":"add"}`. Set `operation` to
  `"remove"` for additive label removal.

### Message membership

Every message carries `label_ids`: the canonical unified membership for its
inbox's catalogue. Additive labels are name-addressed (`tag:<Name>`), exactly as
the action API addresses them; the single folder-backed membership is
id-addressed (`folder:<id>`), exactly as the catalogue advertises it. It is
always present and `[]` when the message carries no label. The older `labels`
array remains as the legacy additive-name view only and omits folder-backed
labels; prefer `label_ids`.

### Filtering a list or search by label

`label` (legacy name) and `label_id` (catalogue id) are both accepted and
resolved by one shared rule on every listing and search route:

- repeated values **AND** together (a message must carry every requested label);
- `tag:<Name>` and a bare name are equivalent and select an additive label;
- `folder:<id>` selects a folder-backed label and requires an `inbox=` scope on
  the account-wide routes, because folder membership is single-inbox state;
- at most one `folder:` id may be requested, because a message has one folder
  membership;
- a label id that does not name a folder of the scoped inbox is `404`, never a
  silently widened query.

An inbox-scoped and an account-wide query with the same resolved filter return
the same page and the same `completeness`.

Label operations are queued as *deltas*, not entire label-array snapshots.
This avoids overlapping additions overwriting each other. Folder-backed labels
create a move action; attempting to remove an exclusive folder label without
another destination is rejected. A provider-owned label name (a Gmail user label
or an Outlook category) may be hierarchical and is validated by the provider
rule, not the MailMoose-native rule that rejects `/` and `\`.

`/v1/inboxes/{id}/folders`, the older flat `/v1/inboxes/{id}/labels`,
`PATCH /v1/messages/{id}`, and the old folder CRUD routes remain for
compatibility and backend provider configuration. Do not remove folder IDs or
remote-role mappings, which are required by IMAP and Microsoft.

Capabilities communicate supported mutation types. Outlook categories cannot
currently be globally renamed or deleted through Graph using the existing
adapter; those catalogue operations are not advertised as supported.

Schema migration 075 stores empty native label names; existing labels and
folder locations are not destructively converted.
