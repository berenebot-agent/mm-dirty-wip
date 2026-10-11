# Connect Google / Gmail

Use **Add inbox → Standalone → Google / Gmail → Next**. Each connected inbox
uses your own Google Cloud OAuth client. The Google mailbox remains the source
of truth. MailMoose caches headers, snippets, thread and label metadata, not a
default full-message archive. Bodies and attachments are fetched when opened.

## Create your Google app

1. Open [Google Cloud Console](https://console.cloud.google.com/) and sign in
   with the account you are connecting. Create a project (or select one) and
   keep it selected.
2. Enable the [Gmail API](https://console.cloud.google.com/apis/library/gmail.googleapis.com).
3. Open [Google Auth Platform](https://console.cloud.google.com/auth/overview).
   If you see **Get started**, click it, then set **App name** (any name, e.g.
   MailMoose), **User support email** and **Contact email** (your Google
   address), and **Audience**: choose **Internal** when the project and intended
   users belong to your Workspace organisation, otherwise choose **External**.
   Accept the policy and finish. If you chose External and remain in Testing,
   add your Google account as a test user.
4. Open [Data Access](https://console.cloud.google.com/auth/scopes) → **Add or
   remove scopes** and add `https://www.googleapis.com/auth/gmail.modify` (use
   manual scope entry if it is not listed), then save. This single scope permits
   reading, labels, read/unread, drafts and sending, including moving mail to
   Trash. No separate `gmail.send` scope is needed, and MailMoose does not
   request the broader permanent-delete scope.
5. Open [Clients](https://console.cloud.google.com/auth/clients) → **Create
   client**, choosing **Web application**. Leave **Authorised JavaScript
   origins** empty.
6. Under **Authorised redirect URIs**, add the exact URL shown in the MailMoose
   wizard. For example:

   ```text
   https://mail.example.com/ui/oauth/google/callback
   ```

   This is based on MailMoose's configured `BASE_URL`; spelling, scheme, port
   and path must match. JavaScript origins are not needed for this server-side
   authorization flow.
7. Enter the client ID and client secret in the wizard. Click **Connect Google**,
   select the mailbox account and approve access.

Google's browser return goes to your installation. Google servers do not need
inbound access: a private/VPN hostname works when your browser can reach it and
the URL meets Google's redirect registration rules. A normal HTTPS hostname is
required except for localhost/loopback HTTP when the browser and MailMoose share
the computer. A raw LAN IP, `.local` hostname or arbitrary HTTP origin is not a
registerable production callback. Set `BASE_URL` appropriately before setup.

## If the callback page fails

Keep the wizard tab open. After approving Google access, if the final MailMoose
page cannot load, copy the **complete final URL** from that tab's address bar.
Open **Callback failed? Paste the return URL** in the wizard and paste it.

The URL must be the same registered callback and contain the one-time code and
state. Connection attempts expire after ten minutes and are used once. A callback
already completed cannot be pasted again. This fallback does not bypass Google's
`redirect_uri_mismatch` checks and is not the discontinued Google OOB code flow.
Do not share the return URL. Application logs omit it; configure your reverse
proxy's access logs to omit callback query strings as well.

## Long-lived access and troubleshooting

- External apps left in **Testing** issue refresh tokens for Gmail scopes that
  expire after **seven days**. Configure the appropriate production audience for
  ongoing use. Publishing is distinct from verification; personal/internal uses
  may qualify for exemptions, while wider use of restricted Gmail scopes has
  Google's verification and assessment requirements.
- `redirect_uri_mismatch`: register the exact URL shown in the wizard, and use
  a Web application OAuth client.
- Access blocked by Workspace: ask your organisation administrator to allow the
  OAuth app and its requested Gmail permission.
- Missing offline access, revoked access, expired tokens or changed passwords:
  reconnect from the inbox settings' **Google / Gmail — reconnect account** link.
  Choose the same mailbox account. Reconnection preserves the inbox and stable IDs.
- Client secrets and tokens are encrypted with `APP_ENCRYPTION_KEY`. Back up the
  key separately from `/data`, as for other provider credentials. Secret fields
  and token values are never returned by configuration APIs.

## Mailbox behaviour

- Gmail system labels drive Inbox, Sent, Drafts, Spam and Trash views. Archive
  removes `INBOX` and preserves other labels. User-label views can overlap: one
  message has one stable MailMoose cache ID, regardless of how many labels it has.
- Label actions, read/star changes and other mutations are applied to Gmail.
  Permanent deletion is unavailable through this connector; use Gmail itself.
- Search executes against Gmail. List navigation uses cached metadata. Initial
  indexing is progressive and reports partial completeness until backfill ends.
  History API checks update external changes; an expired history cursor triggers
  a metadata resynchronization.
- Bodies and downloads are temporary, size-limited and removed after use. The
  existing HTML sanitizer and attachment download protections apply.
- Sending uses Gmail API and Gmail's own Sent copy. Assistant RemoteDraft handoff
  creates a Gmail draft for the human to review. Unknown publication outcomes
  are not blindly retried.
- Notification detection has a cursor separate from browsing/backfill. Initial
  connection establishes a baseline rather than notifying about existing mail.
  Proactive connectors use bounded polling; Google Cloud Pub/Sub push is not
  required for this connector.
- Normal OAuth connects the signed-in mailbox. Gmail UI delegation, domain-wide
  delegation and Google Groups require separate integrations.

References: [Web OAuth](https://developers.google.com/identity/protocols/oauth2/web-server),
[Gmail scopes](https://developers.google.com/workspace/gmail/api/auth/scopes),
[Gmail synchronization](https://developers.google.com/workspace/gmail/api/guides/sync).
