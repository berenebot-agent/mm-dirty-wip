# MailMoose API Reference

Generated from `internal/apispec`; do not edit by hand.

## Discovery

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/bootstrap | Discover key capabilities and accessible inboxes |  |
| GET | /v1/limits | Get server pagination, size and rate limits | read |
| GET | /v1/account/settings | Get account preferences (Owner/Admin) | owner |
| PATCH | /v1/account/settings | Update account preferences (Owner/Admin) | owner |

## Inboxes

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/inboxes | List inboxes | read |
| POST | /v1/inboxes | Create an inbox (Admin) | admin |
| GET | /v1/inboxes/{id} | Get an inbox | read |
| PATCH | /v1/inboxes/{id} | Update an inbox (display_name, enabled, allowed_senders, sender_restricted, approver_email, aliases, alias_names, default_sender) | owner |
| DELETE | /v1/inboxes/{id} | Delete an inbox (Admin) | admin |
| POST | /v1/inboxes/{id}/trash/empty | Empty an inbox's Trash (Owner) | owner |

## Identities

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/identities | List identities (openagent.email compat) | admin |
| POST | /v1/identities | Create an identity (Admin) | admin |
| DELETE | /v1/identities/{address} | Delete an identity (openagent.email compat) | admin |

## Messages

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/messages | List messages with filters (inbox, thread, label, from, to, unread, has_attachment, before, trashed) | read |
| GET | /v1/messages/wait | Long-poll for a new message | read |
| POST | /v1/messages/wait | Long-poll for a new message (compat) | read |
| GET | /v1/messages/{id} | Get a message | read |
| PATCH | /v1/messages/{id} | Update read/labels/spam state | assistant |
| DELETE | /v1/messages/{id} | Move a message to Trash (Assistant/Owner) | assistant |
| POST | /v1/messages/{id}/restore | Restore a trashed message (Assistant/Owner) | assistant |
| DELETE | /v1/messages/{id}/purge | Permanently delete a trashed message (Owner) | owner |
| POST | /v1/messages/{id}/seen | Mark a message seen (compat) | assistant |

## Attachments

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/messages/{id}/attachments | List message attachments | read |
| GET | /v1/attachments/{id} | Download an attachment | read |

## Threads

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/threads | List threads | read |
| GET | /v1/threads/{id} | Get a thread | read |
| GET | /v1/threads/{id}/messages | List messages in a thread | read |

## Search

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/search | Search messages (FTS5) | read |

## Labels

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/labels | List distinct labels in use | read |

## Events

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/events | Incremental event history | read |
| GET | /v1/events/wait | Long-poll for events | read |
| GET | /v1/events/stream | SSE event stream | read |

## Send

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| POST | /v1/send | Send email as an Owner | owner |
| POST | /v1/messages/{id}/reply | Reply to a message (Owner); optional sender chooses the From identity | owner |

## Drafts

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/drafts | List drafts (filter by inbox; supports before and limit) | assistant |
| POST | /v1/drafts | Create a draft | assistant |
| GET | /v1/drafts/{id} | Get a draft (includes attachments) | assistant |
| PATCH | /v1/drafts/{id} | Update a draft (partial) | assistant |
| DELETE | /v1/drafts/{id} | Delete a draft | assistant |
| POST | /v1/drafts/{id}/send | Send a draft (Owner) | owner |
| GET | /v1/drafts/{id}/attachments | List draft attachments | assistant |
| POST | /v1/drafts/{id}/attachments | Upload draft attachments (multipart field 'attachments') | assistant |
| GET | /v1/drafts/{id}/attachments/{attId} | Download a draft attachment | assistant |
| DELETE | /v1/drafts/{id}/attachments/{attId} | Delete a draft attachment | assistant |

## Send requests

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| POST | /v1/drafts/{id}/request-send | Request authorization to send a draft (Assistant) | assistant |
| POST | /v1/drafts/{id}/cancel-send-request | Cancel a pending send request (Assistant) | assistant |
| POST | /v1/drafts/{id}/approve | Approve and send a pending draft (Owner) | owner |
| POST | /v1/drafts/{id}/reject | Reject a pending send request (Owner) | owner |
| GET | /v1/drafts/{id}/send-request | Get the latest send request for a draft | assistant |
| GET | /v1/send-requests | List draft send requests (filter by inbox and active) | assistant |

## Outbox

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/outbox | List pending and failed outbound messages | read |
| POST | /v1/outbox/{id}/retry | Re-queue a failed outbound message | owner |
| DELETE | /v1/outbox/{id} | Cancel a pending send or discard a failed one | owner |

## Admin: domains

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/admin/domains | List domains (Admin) | admin |
| POST | /v1/admin/domains | Create a domain (Admin) | admin |
| PATCH | /v1/admin/domains/{id} | Update a domain (Admin) | admin |
| DELETE | /v1/admin/domains/{id} | Delete a domain (Admin) | admin |
| GET | /v1/admin/domains/{id}/sending | Get a domain's sending provider config (Admin) | admin |
| PUT | /v1/admin/domains/{id}/sending | Set a domain's sending provider config (Admin) | admin |
| DELETE | /v1/admin/domains/{id}/sending | Clear a domain's sending provider config (Admin) | admin |
| GET | /v1/admin/domains/{id}/receiving | Get a domain's receiving provider config (Admin) | admin |
| PUT | /v1/admin/domains/{id}/receiving | Set a domain's receiving provider config (Admin) | admin |
| DELETE | /v1/admin/domains/{id}/receiving | Clear a domain's receiving provider config (Admin) | admin |
| GET | /v1/admin/domains/{id}/sending/deliveries | List delivery attempts for a domain (Admin) | admin |
| GET | /v1/admin/domains/{id}/receiving/deliveries | List receiving activity for a domain (Admin) | admin |

## Admin: keys

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/admin/keys | List API keys (Admin) | admin |
| POST | /v1/admin/keys | Create an API key (Admin) | admin |
| DELETE | /v1/admin/keys/{id} | Revoke an API key (Admin) | admin |

## Admin: external aliases

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/admin/inboxes/{id}/external-aliases | List an inbox's external sending aliases (Admin) | admin |
| POST | /v1/admin/inboxes/{id}/external-aliases | Create an external sending alias (Admin) | admin |
| PATCH | /v1/admin/inboxes/{id}/external-aliases/{aliasID} | Update an external alias display name (Admin) | admin |
| DELETE | /v1/admin/inboxes/{id}/external-aliases/{aliasID} | Delete an external alias (Admin) | admin |
| GET | /v1/admin/inboxes/{id}/external-aliases/{aliasID}/sending | Get an external alias's sending connector (Admin) | admin |
| PUT | /v1/admin/inboxes/{id}/external-aliases/{aliasID}/sending | Set an external alias's sending connector (Admin) | admin |
| DELETE | /v1/admin/inboxes/{id}/external-aliases/{aliasID}/sending | Clear an external alias's sending connector (Admin) | admin |
| GET | /v1/admin/inboxes/{id}/external-aliases/{aliasID}/sending/deliveries | List delivery attempts for an external alias (Admin) | admin |

## Admin: Hermes

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/admin/hermes | List Hermes connections (Admin) | admin |
| POST | /v1/admin/hermes/enroll | Enroll a Hermes Relay connection (Admin) | admin |
| PUT | /v1/admin/hermes/{id} | Update a Hermes connection outbound role (Admin) | admin |
| DELETE | /v1/admin/hermes/{id} | Delete a Hermes connection (Admin) | admin |

## Admin: OpenClaw

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/admin/openclaw | List OpenClaw connections (Admin) | admin |
| POST | /v1/admin/openclaw/enroll | Create an OpenClaw relay connection (Admin) | admin |
| POST | /v1/admin/openclaw/setup-code | Mint an OpenClaw one-time setup code (Admin) | admin |
| PUT | /v1/admin/openclaw/{id} | Update an OpenClaw connection outbound role (Admin) | admin |
| DELETE | /v1/admin/openclaw/{id} | Delete an OpenClaw connection (Admin) | admin |

## Admin: MX

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/admin/mx | Get the installation MX receiver settings and live status (System admin session) | admin |
| PUT | /v1/admin/mx | Set the installation MX receiver settings (System admin session) | admin |
| DELETE | /v1/admin/mx | Clear the installation MX receiver settings (System admin session) | admin |

## Admin: clients

| Method | Path | Summary | Role |
| --- | --- | --- | --- |
| GET | /v1/admin/clients | List clients (Admin) | admin |
| GET | /v1/admin/clients/webhooks | List webhook clients (Admin) | admin |
| POST | /v1/admin/clients/webhooks | Create a webhook client (Admin) | admin |
| PUT | /v1/admin/clients/webhooks/{id} | Update a webhook client (Admin) | admin |
| DELETE | /v1/admin/clients/webhooks/{id} | Delete a webhook client (Admin) | admin |
| POST | /v1/admin/clients/webhooks/{id}/rotate | Rotate a webhook signing secret (Admin) | admin |
| POST | /v1/admin/clients/webhooks/{id}/enabled | Enable or pause a webhook client (Admin) | admin |

