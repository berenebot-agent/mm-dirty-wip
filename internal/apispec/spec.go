// Package apispec is the single source of truth for the MailMoose HTTP API
// surface. The agent guide, the OpenAPI document, the generated reference and
// the example clients are all rendered from this table, so no artifact drifts
// alone.
//
// It is a leaf package: it must not import internal/httpapp, internal/store or
// internal/app.
package apispec

import (
	"sort"
	"strings"
)

// Param documents one query parameter on an operation. Type is an OpenAPI
// primitive name ("string", "integer", "boolean"); "array" becomes an array of
// strings.
type Param struct {
	Name        string
	Type        string
	Required    bool
	Description string
}

// Route is one method+path operation in the authenticated /v1 API.
type Route struct {
	Method  string // GET, POST, PUT, PATCH, DELETE
	Path    string // /v1/... with {placeholders} in net/http ServeMux form
	Summary string // one line, imperative ("List inboxes")
	// Description carries the operational prose (body semantics, query
	// examples) that the summary cannot. It is rendered into the OpenAPI
	// operation and as a sub-line in the agent guide.
	Description string
	Role        string // "read", "assistant", "owner", "admin", or "" when any role
	Group       string // section heading: Inboxes, Messages, Threads, ...
	// Success is the status code of the operation's primary success response.
	// Zero is treated as 200.
	Success int
	// Auth names the authentication scheme for the operation. Empty means the
	// standard bearer API key (the default for the /v1 surface). "session" marks
	// an installation-management route that requires a logged-in system
	// administrator's cookie session and a CSRF token on writes; account bearer
	// keys are not accepted for it.
	Auth string
	// Request is the component schema name of the JSON request body, or "" when
	// the operation has no body. RequestContentType defaults to
	// application/json and is only set for the multipart upload.
	Request            string
	RequestContentType string
	// Response is the component schema name of the primary success body, or ""
	// when the success response carries no JSON (204, SSE, binary download).
	Response string
	// Query documents the operation's query parameters.
	Query []Param
}

// Status returns the effective primary success status code for the route.
func (r Route) Status() int {
	if r.Success == 0 {
		return 200
	}
	return r.Success
}

// routes is the canonical table, populated in documentation order. It is a
// package variable so tests can render fixtures through the same functions.
//
// The table covers the authenticated /v1 surface registered in
// internal/httpapp/server.go. Keep it in sync with the live registrations;
// Missing reports drift in either direction.
var routes = []Route{
	// Discovery.
	{Method: "GET", Path: "/v1/bootstrap", Summary: "Discover key capabilities and accessible inboxes", Role: "", Group: "Discovery"},
	{Method: "GET", Path: "/v1/limits", Summary: "Get server pagination, size and rate limits", Role: "read", Group: "Discovery"},
	{Method: "GET", Path: "/v1/account/settings", Summary: "Get account preferences (Owner/Admin)", Description: "Account-level mailbox preferences: trash_retention_days, the number of days a trashed message is kept before the maintenance sweep purges it permanently (0 keeps trashed mail until purged manually); and timezone, the account default display time zone as an IANA name (empty means UTC). timezone affects only how the web UI renders timestamps; API timestamps are always UTC.", Role: "owner", Group: "Discovery"},
	{Method: "PATCH", Path: "/v1/account/settings", Summary: "Update account preferences (Owner/Admin)", Description: "Accepts trash_retention_days (0 or a positive integer) and/or timezone (an IANA zone name, or an empty string for UTC).", Role: "owner", Group: "Discovery"},

	// Inboxes.
	{Method: "GET", Path: "/v1/inboxes", Summary: "List inboxes", Role: "read", Group: "Inboxes"},
	{Method: "POST", Path: "/v1/inboxes", Summary: "Create an inbox (Admin)", Role: "admin", Group: "Inboxes", Success: 201},
	{Method: "GET", Path: "/v1/inboxes/{id}", Summary: "Get an inbox", Role: "read", Group: "Inboxes"},
	{Method: "PATCH", Path: "/v1/inboxes/{id}", Summary: "Update an inbox (display_name, enabled, allowed_senders, sender_restricted, approver_email, aliases, alias_names, default_sender, trash_retention_days, storage_quota_bytes, auto_mark_read_on_delivery, auto_trash_after_delivery_hours, delivery_trigger)", Description: "aliases replaces the inbox's alias set; each entry is a full local@domain address on any domain the account owns. aliases route inbound mail to this inbox and may be chosen as the From address when sending; alias_names maps an alias to its optional sender display name (falling back to the inbox display_name). default_sender preselects the compose/reply From address (the inbox primary or one of its aliases); empty clears it to the primary. trash_retention_days overrides the account's Trash auto-purge window for this inbox: a non-negative integer sets it (0 keeps this inbox's trashed mail until purged by hand), null clears the override so the inbox inherits the account setting, and an absent field leaves it unchanged. storage_quota_bytes caps this inbox's stored bytes on top of the account quota (Admin only): a positive integer sets the cap, 0 means explicitly unlimited, null clears the cap so only the account quota applies, and an absent field leaves it unchanged; the cap may be set below current usage, after which new inbound and outbound mail is refused with 507 until usage falls. auto_mark_read_on_delivery marks a message read once a connector delivers it; auto_trash_after_delivery_hours sets how many hours after connector delivery a message is moved to Trash (positive integer to set, null to disable, absent to leave unchanged); delivery_trigger picks when these fire, any (first connector to deliver) or all (every connector that existed when the message arrived, the default on a new inbox). These delivery auto-actions apply to agent/relay connectors only; API keys never trigger them.", Role: "owner", Group: "Inboxes"},
	{Method: "DELETE", Path: "/v1/inboxes/{id}", Summary: "Delete an inbox (Admin)", Role: "admin", Group: "Inboxes", Success: 204},
	{Method: "POST", Path: "/v1/inboxes/{id}/trash/empty", Summary: "Empty an inbox's Trash (Owner)", Description: "Permanently purges every trashed message in the inbox, removing rows, raw MIME and storage accounting. Returns the purged count.", Role: "owner", Group: "Inboxes"},

	// Identities (openagent.email compatibility).
	{Method: "GET", Path: "/v1/identities", Summary: "List identities (openagent.email compat)", Role: "admin", Group: "Identities"},
	{Method: "POST", Path: "/v1/identities", Summary: "Create an identity (Admin)", Role: "admin", Group: "Identities", Success: 201},
	{Method: "DELETE", Path: "/v1/identities/{address}", Summary: "Delete an identity (openagent.email compat)", Role: "admin", Group: "Identities"},

	// Messages.
	{Method: "GET", Path: "/v1/messages", Summary: "List messages with filters (inbox, thread, label, from, to, unread, has_attachment, before, trashed)", Description: "Filters: inbox, thread, label (repeatable; messages must carry all listed labels), from, to, unread, has_attachment, before (keyset cursor) and limit. Spam is excluded by default; spam=true lists only Spam and include_spam=true includes it. Trashed messages are excluded by default; trashed=true lists only trashed messages (the Trash view). The address query parameter is an openagent.email compatibility alias for inbox.", Role: "read", Group: "Messages"},
	{Method: "GET", Path: "/v1/messages/wait", Summary: "Long-poll for a new message", Role: "read", Group: "Messages"},
	{Method: "POST", Path: "/v1/messages/wait", Summary: "Long-poll for a new message (compat)", Role: "read", Group: "Messages"},
	{Method: "GET", Path: "/v1/messages/{id}", Summary: "Get a message", Role: "read", Group: "Messages"},
	{Method: "PATCH", Path: "/v1/messages/{id}", Summary: "Update read/labels/spam state", Description: "Sets read, labels and/or spam. labels replaces the whole label set ([] clears; omit the field to leave it unchanged). Requires Assistant or Owner.", Role: "assistant", Group: "Messages"},
	{Method: "DELETE", Path: "/v1/messages/{id}", Summary: "Move a message to Trash (Assistant/Owner)", Description: "Soft-deletes the message: it is hidden from ordinary reads but retains its raw MIME, attachments and storage accounting until purged. Use the purge route to erase it permanently.", Role: "assistant", Group: "Messages", Success: 204},
	{Method: "POST", Path: "/v1/messages/{id}/restore", Summary: "Restore a trashed message (Assistant/Owner)", Description: "Clears the trashed state, returning the message to the mailbox.", Role: "assistant", Group: "Messages"},
	{Method: "DELETE", Path: "/v1/messages/{id}/purge", Summary: "Permanently delete a trashed message (Owner)", Description: "Erases the message row, raw MIME, attachments, FTS entry and storage accounting. The message must already be trashed.", Role: "owner", Group: "Messages", Success: 204},
	{Method: "POST", Path: "/v1/messages/{id}/seen", Summary: "Mark a message seen (compat)", Description: "Sets the message read state; requires Assistant or Owner.", Role: "assistant", Group: "Messages"},

	// Attachments.
	{Method: "GET", Path: "/v1/messages/{id}/attachments", Summary: "List message attachments", Role: "read", Group: "Attachments"},
	{Method: "GET", Path: "/v1/attachments/{id}", Summary: "Download an attachment", Role: "read", Group: "Attachments"},

	// Threads.
	{Method: "GET", Path: "/v1/threads", Summary: "List threads", Role: "read", Group: "Threads"},
	{Method: "GET", Path: "/v1/threads/{id}", Summary: "Get a thread", Role: "read", Group: "Threads"},
	{Method: "GET", Path: "/v1/threads/{id}/messages", Summary: "List messages in a thread", Role: "read", Group: "Threads"},

	// Search.
	{Method: "GET", Path: "/v1/search", Summary: "Search messages (FTS5)", Description: "FTS5 full-text search. Query parameters: q (required), inbox, label (repeatable), from, to, has_attachment, before and limit.", Role: "read", Group: "Search"},

	// Labels.
	{Method: "GET", Path: "/v1/labels", Summary: "List distinct labels in use", Description: "Distinct free-text labels currently in use; matching ignores case and surrounding whitespace.", Role: "read", Group: "Labels"},

	// Events.
	{Method: "GET", Path: "/v1/events", Summary: "Incremental event history", Description: "Durable events after after (a cursor such as evt_...), oldest first, up to limit (default 100); optionally scoped by inbox.", Role: "read", Group: "Events"},
	{Method: "GET", Path: "/v1/events/wait", Summary: "Long-poll for events", Description: "Long-polls until an event after after arrives or timeout seconds elapse (1-60, default 60); returns an empty list on timeout.", Role: "read", Group: "Events"},
	{Method: "GET", Path: "/v1/events/stream", Summary: "SSE event stream", Description: "Server-Sent Events. Backlog after after is delivered before live events; each id: line is a durable cursor usable with after.", Role: "read", Group: "Events"},

	// Send and reply.
	{Method: "POST", Path: "/v1/send", Summary: "Send email as an Owner", Description: "Enqueues into the outbox and returns immediately. Add ?wait=true to block until delivery. Accepts JSON attachments with filename, content_type and base64-encoded content fields. Optional sender chooses a From identity (the inbox primary or one of its aliases); the sending provider is resolved from that address's domain.", Role: "owner", Group: "Send"},
	{Method: "POST", Path: "/v1/messages/{id}/reply", Summary: "Reply to a message (Owner); optional sender chooses the From identity", Description: "Sends a reply in the message's thread. Accepts text, optional html, base64 attachments and sender. Add ?wait=true to block until delivery.", Role: "owner", Group: "Send", Success: 201},

	// Drafts.
	{Method: "GET", Path: "/v1/drafts", Summary: "List drafts (filter by inbox; supports before and limit)", Role: "assistant", Group: "Drafts"},
	{Method: "POST", Path: "/v1/drafts", Summary: "Create a draft", Description: "Accepts draft fields plus an optional base64 JSON attachments array and an action of draft (default), request-send (Assistant) or send (Owner), so a draft can be created, attached and submitted in one request. sender is accepted as an alias for from_address.", Role: "assistant", Group: "Drafts", Success: 201},
	{Method: "GET", Path: "/v1/drafts/{id}", Summary: "Get a draft (includes attachments)", Role: "assistant", Group: "Drafts"},
	{Method: "PATCH", Path: "/v1/drafts/{id}", Summary: "Update a draft (partial)", Description: "Only fields present in the body are changed; omitted fields are left untouched. Also accepts attachments and an action of draft, request-send or send.", Role: "assistant", Group: "Drafts"},
	{Method: "DELETE", Path: "/v1/drafts/{id}", Summary: "Delete a draft", Role: "assistant", Group: "Drafts", Success: 204},
	{Method: "POST", Path: "/v1/drafts/{id}/send", Summary: "Send a draft (Owner)", Description: "Copies the draft's fields and attachments into a new outbound message, then deletes the draft. An optional body overrides fields and appends attachments before sending. Returns provider_message_id like /v1/send.", Role: "owner", Group: "Drafts"},
	{Method: "GET", Path: "/v1/drafts/{id}/attachments", Summary: "List draft attachments", Role: "assistant", Group: "Drafts"},
	{Method: "POST", Path: "/v1/drafts/{id}/attachments", Summary: "Upload draft attachments (multipart field 'attachments')", Description: "Multipart upload; the file field name is attachments.", Role: "assistant", Group: "Drafts", Success: 201},
	{Method: "GET", Path: "/v1/drafts/{id}/attachments/{attId}", Summary: "Download a draft attachment", Role: "assistant", Group: "Drafts"},
	{Method: "DELETE", Path: "/v1/drafts/{id}/attachments/{attId}", Summary: "Delete a draft attachment", Role: "assistant", Group: "Drafts", Success: 204},

	// Send requests.
	{Method: "POST", Path: "/v1/drafts/{id}/request-send", Summary: "Request authorization to send a draft (Assistant)", Description: "Freezes the draft as pending_approval until the request is approved, rejected, cancelled or expired. An optional body overrides fields and appends attachments before freezing. A configured inbox approver makes the request external automatically (the approval email is sent to them); {\"external\": true} is optional and only errors when no approver is configured.", Role: "assistant", Group: "Send requests"},
	{Method: "POST", Path: "/v1/drafts/{id}/cancel-send-request", Summary: "Cancel a pending send request (Assistant)", Role: "assistant", Group: "Send requests"},
	{Method: "POST", Path: "/v1/drafts/{id}/approve", Summary: "Approve and send a pending draft (Owner)", Description: "Approves the exact frozen draft and enqueues it through the outbound flow. Accepts an optional feedback field.", Role: "owner", Group: "Send requests"},
	{Method: "POST", Path: "/v1/drafts/{id}/reject", Summary: "Reject a pending send request (Owner)", Description: "Marks the draft rejected and stores optional feedback for the agent.", Role: "owner", Group: "Send requests"},
	{Method: "GET", Path: "/v1/drafts/{id}/send-request", Summary: "Get the latest send request for a draft", Description: "Returns the workflow record even after the draft has been consumed by an approved send.", Role: "assistant", Group: "Send requests"},
	{Method: "GET", Path: "/v1/send-requests", Summary: "List draft send requests (filter by inbox and active)", Role: "assistant", Group: "Send requests"},

	// Outbox.
	{Method: "GET", Path: "/v1/outbox", Summary: "List pending and failed outbound messages", Role: "read", Group: "Outbox"},
	{Method: "POST", Path: "/v1/outbox/{id}/retry", Summary: "Re-queue a failed outbound message", Role: "owner", Group: "Outbox", Success: 204},
	{Method: "DELETE", Path: "/v1/outbox/{id}", Summary: "Cancel a pending send or discard a failed one", Role: "owner", Group: "Outbox", Success: 204},

	// Admin: domains.
	{Method: "GET", Path: "/v1/admin/domains", Summary: "List domains (Admin)", Role: "admin", Group: "Admin: domains"},
	{Method: "POST", Path: "/v1/admin/domains", Summary: "Create a domain (Admin)", Role: "admin", Group: "Admin: domains", Success: 201},
	{Method: "PATCH", Path: "/v1/admin/domains/{id}", Summary: "Update a domain (Admin)", Description: "Accepts catch_all_inbox_id only.", Role: "admin", Group: "Admin: domains"},
	{Method: "DELETE", Path: "/v1/admin/domains/{id}", Summary: "Delete a domain (Admin)", Role: "admin", Group: "Admin: domains", Success: 204},
	{Method: "GET", Path: "/v1/admin/domains/{id}/sending", Summary: "Get a domain's sending provider config (Admin)", Role: "admin", Group: "Admin: domains"},
	{Method: "PUT", Path: "/v1/admin/domains/{id}/sending", Summary: "Set a domain's sending provider config (Admin)", Role: "admin", Group: "Admin: domains"},
	{Method: "DELETE", Path: "/v1/admin/domains/{id}/sending", Summary: "Clear a domain's sending provider config (Admin)", Role: "admin", Group: "Admin: domains", Success: 204},
	{Method: "GET", Path: "/v1/admin/domains/{id}/receiving", Summary: "Get a domain's receiving provider config (Admin)", Description: "Returns the non-secret receiving config. For Dial MX (Antler MX or custom) it also returns key_id, public_key, txt_record, live per-receiver status, cached published-record DNS checks and copy-ready DNS instructions.", Role: "admin", Group: "Admin: domains"},
	{Method: "PUT", Path: "/v1/admin/domains/{id}/receiving", Summary: "Set a domain's receiving provider config (Admin)", Description: "Accepts provider, config, and regenerate_secret. Generated secrets are returned once in the response. The dialmx provider's config is {service, contact_email, receiver_urls, enforcement}: service is antler (zero-config Antler MX shared relay, the default) or custom; antler requires contact_email and resolves the hosted receiver set (supplying receiver_urls for antler is a 400), custom requires receiver_urls.", Role: "admin", Group: "Admin: domains"},
	{Method: "DELETE", Path: "/v1/admin/domains/{id}/receiving", Summary: "Clear a domain's receiving provider config (Admin)", Role: "admin", Group: "Admin: domains", Success: 204},
	{Method: "GET", Path: "/v1/admin/domains/{id}/sending/deliveries", Summary: "List delivery attempts for a domain (Admin)", Description: "Returns the per-attempt delivery log for a domain, newest first, with message_id linking to the message. Supports limit and before (keyset on attempt id).", Role: "admin", Group: "Admin: domains"},
	{Method: "GET", Path: "/v1/admin/domains/{id}/receiving/deliveries", Summary: "List receiving activity for a domain (Admin)", Description: "Returns the receiving side of a domain's two-way log: delivered inbound mail, blocked mail and consumed approval control mail, newest first. Supports limit and before (RFC3339 timestamp keyset on created_at).", Role: "admin", Group: "Admin: domains"},

	// Admin: keys.
	{Method: "GET", Path: "/v1/admin/keys", Summary: "List API keys (Admin)", Role: "admin", Group: "Admin: keys"},
	{Method: "POST", Path: "/v1/admin/keys", Summary: "Create an API key (Admin)", Role: "admin", Group: "Admin: keys", Success: 201},
	{Method: "DELETE", Path: "/v1/admin/keys/{id}", Summary: "Revoke an API key (Admin)", Role: "admin", Group: "Admin: keys", Success: 204},

	// Admin: external aliases.
	{Method: "GET", Path: "/v1/admin/inboxes/{id}/external-aliases", Summary: "List an inbox's external sending aliases (Admin)", Description: "Send-only identities on domains MailMoose does not manage. Never participate in inbound routing. Returns ids, addresses, display names, connector provider and configured status; never credentials.", Role: "admin", Group: "Admin: external aliases"},
	{Method: "POST", Path: "/v1/admin/inboxes/{id}/external-aliases", Summary: "Create an external sending alias (Admin)", Description: "Body: address (full local@domain, immutable after creation) and optional display_name. The address must not be the inbox primary or a managed inbox/alias in the account, and must be unique among the account's external aliases.", Role: "admin", Group: "Admin: external aliases", Success: 201},
	{Method: "PATCH", Path: "/v1/admin/inboxes/{id}/external-aliases/{aliasID}", Summary: "Update an external alias display name (Admin)", Description: "Accepts display_name only; the address is immutable.", Role: "admin", Group: "Admin: external aliases"},
	{Method: "DELETE", Path: "/v1/admin/inboxes/{id}/external-aliases/{aliasID}", Summary: "Delete an external alias (Admin)", Description: "Removes the alias and its connector, clears any default_sender that referenced it, and makes its queued messages fail permanently rather than fall back to a domain connector.", Role: "admin", Group: "Admin: external aliases", Success: 204},
	{Method: "GET", Path: "/v1/admin/inboxes/{id}/external-aliases/{aliasID}/sending", Summary: "Get an external alias's sending connector (Admin)", Role: "admin", Group: "Admin: external aliases"},
	{Method: "PUT", Path: "/v1/admin/inboxes/{id}/external-aliases/{aliasID}/sending", Summary: "Set an external alias's sending connector (Admin)", Description: "Accepts provider and config; same schema, secret retention and CAS revision semantics as a domain sending config. Requeues only that alias's pending sends.", Role: "admin", Group: "Admin: external aliases"},
	{Method: "DELETE", Path: "/v1/admin/inboxes/{id}/external-aliases/{aliasID}/sending", Summary: "Clear an external alias's sending connector (Admin)", Role: "admin", Group: "Admin: external aliases", Success: 204},
	{Method: "GET", Path: "/v1/admin/inboxes/{id}/external-aliases/{aliasID}/sending/deliveries", Summary: "List delivery attempts for an external alias (Admin)", Description: "Returns that alias's per-attempt delivery log, newest first. Supports limit and before (keyset on attempt id).", Role: "admin", Group: "Admin: external aliases"},

	// Admin: Hermes.
	{Method: "GET", Path: "/v1/admin/hermes", Summary: "List Hermes connections (Admin)", Description: "Returns connection metadata (id, inbox, name, gateway id, outbound role, last acknowledged event and connectivity); never secrets.", Role: "admin", Group: "Admin: Hermes"},
	{Method: "POST", Path: "/v1/admin/hermes/enroll", Summary: "Enroll a Hermes Relay connection (Admin)", Description: "Body: inbox_id and name. Mints a gateway id, secret and delivery key, returned once with the connector URL and an env block.", Role: "admin", Group: "Admin: Hermes", Success: 201},
	{Method: "PUT", Path: "/v1/admin/hermes/{id}", Summary: "Update a Hermes connection outbound role (Admin)", Description: "Body: role. owner lets the relay send directly; assistant makes it draft and request approval instead.", Role: "admin", Group: "Admin: Hermes"},
	{Method: "DELETE", Path: "/v1/admin/hermes/{id}", Summary: "Delete a Hermes connection (Admin)", Role: "admin", Group: "Admin: Hermes", Success: 204},
	{Method: "GET", Path: "/v1/admin/openclaw", Summary: "List OpenClaw connections (Admin)", Description: "Returns OpenClaw connector metadata (id, inbox, name, gateway id, outbound role, last acknowledged event and connectivity); never secrets.", Role: "admin", Group: "Admin: OpenClaw"},
	{Method: "POST", Path: "/v1/admin/openclaw/enroll", Summary: "Create an OpenClaw relay connection (Admin)", Description: "Body: inbox_id and name. Mints a gateway id, secret and delivery key, returned once with the connector URL. OpenClaw shares the authenticated replayable relay transport with Hermes.", Role: "admin", Group: "Admin: OpenClaw", Success: 201},
	{Method: "POST", Path: "/v1/admin/openclaw/setup-code", Summary: "Mint an OpenClaw one-time setup code (Admin)", Description: "Body: inbox_id and name. Returns a single-use code, its claim URL and a ready-to-paste openclaw channels add command. The code expires in 15 minutes.", Role: "admin", Group: "Admin: OpenClaw", Success: 201},
	{Method: "PUT", Path: "/v1/admin/openclaw/{id}", Summary: "Update an OpenClaw connection outbound role (Admin)", Description: "Body: role. owner lets the relay send directly; assistant makes it draft and request approval instead.", Role: "admin", Group: "Admin: OpenClaw"},
	{Method: "DELETE", Path: "/v1/admin/openclaw/{id}", Summary: "Delete an OpenClaw connection (Admin)", Role: "admin", Group: "Admin: OpenClaw", Success: 204},

	// Admin: MX. These installation-management routes authenticate with the
	// system administrator's cookie session, not a bearer API key: one receiver
	// serves every account, so an account-scoped key must never reach them.
	{Method: "GET", Path: "/v1/admin/mx", Summary: "Get the installation MX receiver settings and live status (System admin session)", Description: "Session-authenticated installation route: requires a logged-in system administrator's cookie session; account bearer API keys are not accepted. Returns the installation-wide MX receiver configuration with its secret redacted (key_configured reports whether a bearer credential is stored) plus a status object carrying the live receiver state. A configured receiver is one whose mode is included or remote. Readiness comes from status.state and is never claimed before the receiver is live: state is connecting while a change or session handshake is still in progress and active only once it completes.", Role: "admin", Group: "Admin: MX", Auth: "session"},
	{Method: "PUT", Path: "/v1/admin/mx", Summary: "Set the installation MX receiver settings (System admin session)", Description: "Session-authenticated installation route: requires a logged-in system administrator's cookie session and a CSRF token (X-CSRF-Token header or _csrf form field); account bearer API keys are not accepted. Body: mode (included or remote), url, bearer_key, ca, hostname, max_message_bytes, max_staging_bytes, max_recipients, max_connections, require_tls, verify_spf, verify_dkim, verify_dmarc, dns_resolver, dns_timeout_seconds, read_timeout_seconds, write_timeout_seconds, data_timeout_seconds, smtp_tls_cert, smtp_tls_key and revision. Included mode generates and retains the bearer key (a supplied key is ignored) and takes no url, and may set the hostname, SMTP/staging limits, STARTTLS certificate and private key, DNS resolver and timeouts, RequireTLS and the verification toggles; verification defaults on and RequireTLS defaults off, an explicit true/false is honoured. smtp_tls_key is write-only: the GET never returns it, a blank value retains the stored key, and clearing smtp_tls_cert (with a blank key) removes the pair. Remote mode requires a url and a bearer_key, where a blank key retains the stored one, and rejects included-only fields. The revision is an optimistic-concurrency token a save must echo; zero creates the configuration.", Role: "admin", Group: "Admin: MX", Auth: "session"},
	{Method: "DELETE", Path: "/v1/admin/mx", Summary: "Clear the installation MX receiver settings (System admin session)", Description: "Session-authenticated installation route: requires a logged-in system administrator's cookie session and a CSRF token; account bearer API keys are not accepted. Removes the configured receiver, preserving the initialized marker so the one-time environment import never re-fires. Requires the current revision as a query parameter.", Role: "admin", Group: "Admin: MX", Auth: "session", Success: 204},

	// Admin: Remote MX. Account-scoped, account-admin bearer API (unlike the
	// installation MX routes, which are system-administrator session routes).
	{Method: "GET", Path: "/v1/admin/account/mx", Summary: "Get the account Remote MX receiver settings and live status (Admin)", Description: "Returns the calling account's Remote MX receiver configuration with its bearer credential redacted (key_configured reports whether one is stored) plus a status object carrying the live receiver state. Remote MX is an account-owned standalone Dial MX receiver reached in single mode; one physical receiver belongs to exactly one account.", Role: "admin", Group: "Admin: MX"},
	{Method: "PUT", Path: "/v1/admin/account/mx", Summary: "Set the account Remote MX receiver settings (Admin)", Description: "Body: url, bearer_key, ca, allow_private and revision. url must be an HTTP or HTTPS origin; a public receiver must use https and a public-routable host, while allow_private permits a loopback/LAN receiver (only honoured when the operator allows private outbound). A blank bearer_key retains the stored credential. The receiver URL must not already be registered to another account. The revision is an optimistic-concurrency token a save must echo; zero creates the configuration.", Role: "admin", Group: "Admin: MX"},
	{Method: "DELETE", Path: "/v1/admin/account/mx", Summary: "Clear the account Remote MX receiver settings (Admin)", Description: "Removes the account's Remote MX receiver. Refused while one or more domains still receive through it. Requires the current revision as a query parameter.", Role: "admin", Group: "Admin: MX", Success: 204},

	// Admin: clients.
	{Method: "GET", Path: "/v1/admin/clients", Summary: "List clients (Admin)", Description: "Returns every client of the account grouped by type: API keys, Hermes relays and webhooks. Never includes secrets.", Role: "admin", Group: "Admin: clients"},
	{Method: "GET", Path: "/v1/admin/clients/webhooks", Summary: "List webhook clients (Admin)", Description: "Returns webhook delivery clients (id, inbox, name, url, mode, auth mode, enabled, cursor and status); never the signing secret.", Role: "admin", Group: "Admin: clients"},
	{Method: "POST", Path: "/v1/admin/clients/webhooks", Summary: "Create a webhook client (Admin)", Description: "Body: inbox_id, name, url (HTTPS), mode (notify|forward) and auth (signature|bearer). Optional bearer_secret supplies an existing token for bearer auth; otherwise a secret is generated. Returns the client and its secret once.", Role: "admin", Group: "Admin: clients", Success: 201},
	{Method: "PUT", Path: "/v1/admin/clients/webhooks/{id}", Summary: "Update a webhook client (Admin)", Description: "Body: name, url, mode and auth. Optional bearer_secret replaces the secret for bearer auth; omitted or empty preserves it. Configuration and secret updates are atomic; the delivery cursor is unchanged.", Role: "admin", Group: "Admin: clients"},
	{Method: "DELETE", Path: "/v1/admin/clients/webhooks/{id}", Summary: "Delete a webhook client (Admin)", Role: "admin", Group: "Admin: clients", Success: 204},
	{Method: "POST", Path: "/v1/admin/clients/webhooks/{id}/rotate", Summary: "Rotate a webhook signing secret (Admin)", Description: "Issues a new secret and returns it once; the previous secret stops signing immediately.", Role: "admin", Group: "Admin: clients"},
	{Method: "POST", Path: "/v1/admin/clients/webhooks/{id}/enabled", Summary: "Enable or pause a webhook client (Admin)", Description: "Body: enabled. Pausing preserves the delivery cursor.", Role: "admin", Group: "Admin: clients"},
}

// Routes returns the API table in documentation order with the wire contract
// (request body, success body, query parameters) applied from route_io.go.
func Routes() []Route { return decorate(routes) }

// Groups returns the distinct group headings in first-seen order.
func Groups() []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range routes {
		if r.Group == "" || seen[r.Group] {
			continue
		}
		seen[r.Group] = true
		out = append(out, r.Group)
	}
	return out
}

// Missing compares the bearer-authenticated table against the live bearer
// registrations. registered holds net/http ServeMux patterns of the form
// "METHOD /v1/path". It returns routes that are registered but absent from the
// table (undocumented) and bearer table entries with no registered route
// (phantom). Session-authenticated installation routes are compared separately
// by SessionMissing, because they are not registered by the bearer mechanism.
// A non-empty result is a documentation bug.
func Missing(registered []string) (undocumented, phantom []string) {
	reg := make(map[string]bool, len(registered))
	for _, p := range registered {
		reg[strings.TrimSpace(p)] = true
	}
	table := make(map[string]bool, len(routes))
	for _, r := range routes {
		if r.Auth == AuthSession {
			continue
		}
		table[strings.TrimSpace(r.Method+" "+r.Path)] = true
	}
	for p := range reg {
		if !table[p] {
			undocumented = append(undocumented, p)
		}
	}
	for p := range table {
		if !reg[p] {
			phantom = append(phantom, p)
		}
	}
	sort.Strings(undocumented)
	sort.Strings(phantom)
	return undocumented, phantom
}

// AuthSession is the Auth value for installation-management routes that require
// a logged-in system administrator's cookie session (with a CSRF token on
// writes) rather than a bearer API key.
const AuthSession = "session"

// SessionRoutes returns the session-authenticated route keys ("METHOD /path").
func SessionRoutes() []string {
	var out []string
	for _, r := range routes {
		if r.Auth == AuthSession {
			out = append(out, r.Method+" "+r.Path)
		}
	}
	return out
}

// SessionMissing compares the session-authenticated table entries against the
// live session registrations, mirroring Missing for the bearer surface.
func SessionMissing(registered []string) (undocumented, phantom []string) {
	reg := make(map[string]bool, len(registered))
	for _, p := range registered {
		reg[strings.TrimSpace(p)] = true
	}
	table := make(map[string]bool)
	for _, r := range routes {
		if r.Auth != AuthSession {
			continue
		}
		table[strings.TrimSpace(r.Method+" "+r.Path)] = true
	}
	for p := range reg {
		if !table[p] {
			undocumented = append(undocumented, p)
		}
	}
	for p := range table {
		if !reg[p] {
			phantom = append(phantom, p)
		}
	}
	sort.Strings(undocumented)
	sort.Strings(phantom)
	return undocumented, phantom
}
