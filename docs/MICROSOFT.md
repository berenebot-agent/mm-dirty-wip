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

Initial detection uses per-folder delta polling for personal and shared
mailboxes. Metadata backfill is resumable and does not emit historical arrivals.
Live reads and on-demand detection use Graph; proactive delivery uses existing
remote workers. Search uses Graph and explicitly reports partial completeness
for provider search, which has a capped result set.

Personal Graph push subscriptions are deferred. When added, their callback URL
must use `Config.ReceiverURL()`: `DEDICATED_RECEIVER_URL` when supplied, otherwise
`BASE_URL`. Callback routes belong in the shared inbound registration, available
on the optional dedicated listener and the core listener, matching other inbound
providers. URL selection is independent of listener enablement; the reverse
proxy must route it correctly. Shared delegated mailboxes retain delta polling.
The localhost OAuth redirect is unrelated to this public push URL.

## Rollout acceptance

Deterministic fixtures cover OAuth, mailbox boundaries, immutable IDs, provider
continuations, mutations and ambiguous send handling. After registering the app,
verify with real Outlook personal, Microsoft 365 personal and delegated shared
mailboxes: connection, refresh, Outlook-originated changes, attachments, native
reply drafts, shared sending rights and Sent Items behaviour.
