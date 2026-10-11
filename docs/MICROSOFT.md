# Microsoft standalone inboxes

Microsoft Graph connects one whole personal or shared mailbox. The provider
owns messages; MailMoose retains encrypted credentials, folder/message/thread
metadata, stable provider identifiers, incremental cursors and delivery state.
Bodies and attachments are fetched into bounded transient files on demand.

## Operator setup

Create an Entra public-client app in the dedicated publisher tenant. For the
shared Quick Connect registration, choose work/school accounts in any directory
and personal Microsoft accounts. Add the **Mobile and desktop applications**
redirect URI `http://localhost`. No client secret is used.

Configure the deployment:

```text
MICROSOFT_CLIENT_ID=<application-client-id>
MICROSOFT_TENANT_ID=common
```

The production shared app ID can be supplied at rollout. Until configured, the
wizard explains that Microsoft setup is unavailable. Self-hosters can use their
own public-client registration through the same environment settings; use a
tenant ID/domain instead of `common` when restricting to that tenant.

Delegated permissions: `User.Read`, `Mail.ReadWrite`, `Mail.Send`, and
`offline_access`. Shared connections additionally request
`Mail.ReadWrite.Shared` and `Mail.Send.Shared`. Tenant consent policy may require
administrator approval. Exchange mailbox delegation must also grant access to
the selected shared mailbox; sending requires Send As or Send on Behalf rights.

Existing connections retain their original client ID and tenant when defaults
change. Keep their app registrations operational for refresh/reconnection.

## Connecting

Choose Microsoft in the standalone wizard, choose Personal or Shared, and supply
the shared address when applicable. Sign in and approve delegated access.
Microsoft redirects to `http://localhost`; a browser connection failure is
expected because no local listener runs. Copy the complete final address-bar URL
and paste it into MailMoose. Use another desktop browser if the URL is hidden.
The attempt expires after ten minutes and completion is single-use.

The callback is sensitive. MailMoose validates the exact localhost destination,
state and PKCE verifier and does not log or retain its code or pasted URL.
Tokens are encrypted using `APP_ENCRYPTION_KEY` and bound to account/inbox.

Each connection is restricted to its selected mailbox. Shared delegated scopes
can grant broader upstream access, but MailMoose never exposes another mailbox
or a general Graph proxy. Existing per-inbox Read/Assistant/Owner roles apply;
there is no folder permission subdivision. Reconnect uses the original identity
and mailbox. Shared operations never fall back to the user's personal mailbox.

## Operations and detection

Folders retain provider IDs and hierarchy; special roles use well-known Graph
identifiers rather than translated names. Categories map to message labels.
Immutable message IDs preserve local references through folder moves. The
ordinary Archive folder is supported; the separate Online Archive mailbox is
outside this release.

Remote-draft handoff remains the default; approval/direct-send use the existing
workflow. Replies use native Graph reply drafts when the original message can
be resolved uniquely. Graph manages Sent Items; MailMoose does not append a
duplicate copy. Microsoft may apply tenant-specific shared Sent Items rules.
Send acceptance is not proof of delivery. Unknown send outcomes are ambiguous
and must not be blindly retried.

Graph MIME import is not trusted to preserve recipients. Before submission,
MailMoose explicitly sets To/Cc/Bcc from the composed envelope and reads the
draft back to verify all three lists. A mismatch stops submission. Bcc comes
from the structured envelope for direct sends (the transmitted MIME intentionally
omits it). Remote-draft handoffs similarly set and verify the reviewed MIME
recipients before reporting the draft as published. Failures after draft creation
are treated as ambiguous so an automatic retry cannot create duplicate drafts.

Detection uses per-folder delta cursors for personal and shared
mailboxes. Metadata backfill is resumable and does not emit historical arrivals.
Live reads and on-demand detection use Graph; proactive delivery uses existing
remote workers. Search uses Graph and explicitly reports partial completeness
for provider search, which has a capped result set.

### Automatic push setup

Own-mailbox connections (Outlook.com and Microsoft 365) automatically subscribe
to Inbox changes once the initial metadata baseline completes. Existing eligible
connections are provisioned by the worker too. The existing delegated permissions
are sufficient; no client secret or additional service is required.

The public HTTPS callback is `/internal/ingest/microsoft/notifications`, using
`Config.ReceiverURL()`: `DEDICATED_RECEIVER_URL` when supplied, otherwise
`BASE_URL`. It is registered on both the optional dedicated inbound listener and
the core listener. Route this path through the reverse proxy to an active
listener, permitting Microsoft's POST validation requests and notifications.
URL selection is independent of listener enablement. Local HTTP-only deployments
continue polling. The localhost OAuth redirect is unrelated to this public URL.

Notifications contain change hints, not message bodies. A per-subscription
random `clientState` is encrypted at rest; callbacks match its SHA-256 hash and
the subscription ID before persisting a coalesced SQLite sync request. The
callback acknowledges only after persistence, then wakes the existing worker.
Delta sync and the normal durable arrival/event pipeline fetch and deliver mail;
duplicate notifications cannot create duplicate arrival events. Microsoft
controls notification latency, so delivery is near-real-time rather than a
guaranteed instant SLA.

Subscriptions last six days and renew with less than a day remaining. Lifecycle
notifications trigger reauthorization, subscription recreation, or delta catch-up.
Queued work survives restarts; lost subscription-create responses are reconciled
against Graph's subscription list. Inbox deletion/disable retains only an
encrypted cleanup grant until the subscription is removed or expires, then
deletes that state. Ordinary delta polling remains active as a recovery fallback.

Connection settings show push setup/active/retrying status; the API realtime
capability is true only while a recorded push subscription is active and unexpired.
Shared delegated mailboxes retain polling: Graph does not support change
subscriptions with delegated shared-mailbox permissions. Shared push would
require a separately approved application-permission integration.

## Rollout acceptance

Deterministic fixtures cover OAuth, mailbox boundaries, immutable IDs, provider
continuations, mutations and ambiguous send handling. After registering the app,
verify with real Outlook personal, Microsoft 365 personal and delegated shared
mailboxes: connection, refresh, Outlook-originated changes, attachments, native
reply drafts, shared sending rights and Sent Items behaviour.
