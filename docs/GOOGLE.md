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
   address), and **Audience**: choose **External** for a normal `@gmail.com`
   account. **Internal** is only available to Google Workspace organisations.
   Accept the policy and finish.
4. Keep the app in **Testing** and add your Google account as a test user.
   Open [Audience](https://console.cloud.google.com/auth/audience) and, under
   **Test users**, click **Add users** and add the exact address you are
   connecting. Do **not** click **Publish app** — for a restricted Gmail scope,
   an unverified External app in production is hard-blocked (see
   [Long-lived access](#long-lived-access-and-troubleshooting)).
5. Open [Data Access](https://console.cloud.google.com/auth/scopes) → **Add or
   remove scopes** and add `https://www.googleapis.com/auth/gmail.modify` (use
   manual scope entry if it is not listed), then save. This single scope permits
   reading, labels, read/unread, drafts and sending, including moving mail to
   Trash. No separate `gmail.send` scope is needed, and MailMoose does not
   request the broader permanent-delete scope.
6. Open [Clients](https://console.cloud.google.com/auth/clients) → **Create
   client**, choosing **Web application**. Leave **Authorised JavaScript
   origins** empty.
7. Under **Authorised redirect URIs**, add the exact URL shown in the MailMoose
   wizard. For example:

   ```text
   https://mail.example.com/ui/oauth/google/callback
   ```

   This is based on MailMoose's configured `BASE_URL`; spelling, scheme, port
   and path must match. JavaScript origins are not needed for this server-side
   authorization flow.
8. Enter the client ID and client secret in the wizard. Click **Connect Google**,
   select the mailbox account and approve access. Google will show a **"Google
   hasn't verified this app"** screen; click **Advanced → Go to <app name>** to
   continue.

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

### Personal Gmail: stay in Testing

For a normal `@gmail.com` account the OAuth app must be **External** (Internal is
Workspace-only) and should be left in **Testing** with your address added as a
test user. `gmail.modify` is a Google *restricted* scope, so:

- Apps that request restricted scopes in production must complete Google's
  verification (which includes a third-party security assessment). Until then,
  Google shows the **"Google hasn't verified this app"** warning in Testing —
  click **Advanced → Go to <app name>** to proceed.
- In Testing, authorizations and their refresh tokens expire **seven days** after
  consent. When MailMoose reports expired access, reconnect from the inbox
  settings' **Google / Gmail — reconnect account** link; it preserves the inbox
  and stable IDs.

Publishing an unverified External app does **not** remove the seven-day limit —
it replaces the click-through warning with a hard block (below). The only way to
remove the limit is to complete Google's verification, so for personal use the
seven-day reconnect is the intended trade-off.

### Access blocked: has not completed the Google verification process

This error means the OAuth app is **In production** but has not completed
verification, so Google refuses the restricted Gmail scope outright. Revert it:

1. Open [Audience](https://console.cloud.google.com/auth/audience).
2. Under **Publishing status**, click **Back to testing**.
3. Ensure the address you sign in with is listed under **Test users**.
4. Save, wait a few minutes for propagation, then retry **Connect Google** and
   click through the **"Google hasn't verified this app"** screen.

### Other issues

- `redirect_uri_mismatch`: register the exact URL shown in the wizard, and use
  a Web application OAuth client.
- `org_internal` / access blocked by Workspace: the app's audience is **Internal**
  but the account is outside the project's organisation. Use **External** for a
  personal `@gmail.com` account, or ask your organisation administrator to allow
  the OAuth app and its requested Gmail permission.
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
