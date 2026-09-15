// Package apispec is the single source of truth for the Gatehouse Mail HTTP API
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

	// Inboxes.
	{Method: "GET", Path: "/v1/inboxes", Summary: "List inboxes", Role: "read", Group: "Inboxes"},
	{Method: "POST", Path: "/v1/inboxes", Summary: "Create an inbox (Admin)", Role: "admin", Group: "Inboxes", Success: 201},
	{Method: "GET", Path: "/v1/inboxes/{id}", Summary: "Get an inbox", Role: "read", Group: "Inboxes"},
	{Method: "PATCH", Path: "/v1/inboxes/{id}", Summary: "Update an inbox (display_name, enabled, allowed_senders, sender_restricted, approver_email, aliases, alias_names, default_sender)", Description: "aliases replaces the inbox's alias set; each entry is a full local@domain address on any domain the account owns. aliases route inbound mail to this inbox and may be chosen as the From address when sending; alias_names maps an alias address to its optional sender display name (falling back to the inbox display_name). default_sender preselects the compose/reply From address (the inbox primary or one of its aliases); empty clears it to the primary.", Role: "owner", Group: "Inboxes"},
	{Method: "DELETE", Path: "/v1/inboxes/{id}", Summary: "Delete an inbox (Admin)", Role: "admin", Group: "Inboxes", Success: 204},

	// Identities (openagent.email compatibility).
	{Method: "GET", Path: "/v1/identities", Summary: "List identities (openagent.email compat)", Role: "admin", Group: "Identities"},
	{Method: "POST", Path: "/v1/identities", Summary: "Create an identity (Admin)", Role: "admin", Group: "Identities", Success: 201},
	{Method: "DELETE", Path: "/v1/identities/{address}", Summary: "Delete an identity (openagent.email compat)", Role: "admin", Group: "Identities"},

	// Messages.
	{Method: "GET", Path: "/v1/messages", Summary: "List messages with filters (inbox, thread, label, from, to, unread, has_attachment, before)", Description: "Filters: inbox, thread, label (repeatable; messages must carry all listed labels), from, to, unread, has_attachment, before (keyset cursor) and limit. Spam is excluded by default; spam=true lists only Spam and include_spam=true includes it. The address query parameter is an openagent.email compatibility alias for inbox.", Role: "read", Group: "Messages"},
	{Method: "GET", Path: "/v1/messages/wait", Summary: "Long-poll for a new message", Role: "read", Group: "Messages"},
	{Method: "POST", Path: "/v1/messages/wait", Summary: "Long-poll for a new message (compat)", Role: "read", Group: "Messages"},
	{Method: "GET", Path: "/v1/messages/{id}", Summary: "Get a message", Role: "read", Group: "Messages"},
	{Method: "PATCH", Path: "/v1/messages/{id}", Summary: "Update read/archived/labels state", Description: "Sets read, archived, labels and/or spam. labels replaces the whole label set ([] clears; omit the field to leave it unchanged). Requires Assistant or Owner.", Role: "assistant", Group: "Messages"},
	{Method: "DELETE", Path: "/v1/messages/{id}", Summary: "Delete a message (Assistant/Owner)", Role: "assistant", Group: "Messages", Success: 204},
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
	{Method: "GET", Path: "/v1/admin/domains/{id}/receiving", Summary: "Get a domain's receiving provider config (Admin)", Role: "admin", Group: "Admin: domains"},
	{Method: "PUT", Path: "/v1/admin/domains/{id}/receiving", Summary: "Set a domain's receiving provider config (Admin)", Description: "Accepts provider, config, and regenerate_secret. Generated secrets are returned once in the response.", Role: "admin", Group: "Admin: domains"},
	{Method: "DELETE", Path: "/v1/admin/domains/{id}/receiving", Summary: "Clear a domain's receiving provider config (Admin)", Role: "admin", Group: "Admin: domains", Success: 204},
	{Method: "GET", Path: "/v1/admin/domains/{id}/sending/deliveries", Summary: "List delivery attempts for a domain (Admin)", Description: "Returns the per-attempt delivery log for a domain, newest first, with message_id linking to the message. Supports limit and before (keyset on attempt id).", Role: "admin", Group: "Admin: domains"},
	{Method: "GET", Path: "/v1/admin/domains/{id}/receiving/deliveries", Summary: "List receiving activity for a domain (Admin)", Description: "Returns the receiving side of a domain's two-way log: delivered inbound mail, blocked mail and consumed approval control mail, newest first. Supports limit and before (RFC3339 timestamp keyset on created_at).", Role: "admin", Group: "Admin: domains"},

	// Admin: keys.
	{Method: "GET", Path: "/v1/admin/keys", Summary: "List API keys (Admin)", Role: "admin", Group: "Admin: keys"},
	{Method: "POST", Path: "/v1/admin/keys", Summary: "Create an API key (Admin)", Role: "admin", Group: "Admin: keys", Success: 201},
	{Method: "DELETE", Path: "/v1/admin/keys/{id}", Summary: "Revoke an API key (Admin)", Role: "admin", Group: "Admin: keys", Success: 204},

	// Admin: external aliases.
	{Method: "GET", Path: "/v1/admin/inboxes/{id}/external-aliases", Summary: "List an inbox's external sending aliases (Admin, self-hosted)", Description: "Send-only identities on domains Gatehouse does not manage. Never participate in inbound routing. Returns ids, addresses, display names, connector provider and configured status; never credentials.", Role: "admin", Group: "Admin: external aliases"},
	{Method: "POST", Path: "/v1/admin/inboxes/{id}/external-aliases", Summary: "Create an external sending alias (Admin, self-hosted)", Description: "Body: address (full local@domain, immutable after creation) and optional display_name. The address must not be the inbox primary or a managed inbox/alias in the account, and must be unique among the account's external aliases.", Role: "admin", Group: "Admin: external aliases", Success: 201},
	{Method: "PATCH", Path: "/v1/admin/inboxes/{id}/external-aliases/{aliasID}", Summary: "Update an external alias display name (Admin, self-hosted)", Description: "Accepts display_name only; the address is immutable.", Role: "admin", Group: "Admin: external aliases"},
	{Method: "DELETE", Path: "/v1/admin/inboxes/{id}/external-aliases/{aliasID}", Summary: "Delete an external alias (Admin, self-hosted)", Description: "Removes the alias and its connector, clears any default_sender that referenced it, and makes its queued messages fail permanently rather than fall back to a domain connector.", Role: "admin", Group: "Admin: external aliases", Success: 204},
	{Method: "GET", Path: "/v1/admin/inboxes/{id}/external-aliases/{aliasID}/sending", Summary: "Get an external alias's sending connector (Admin, self-hosted)", Role: "admin", Group: "Admin: external aliases"},
	{Method: "PUT", Path: "/v1/admin/inboxes/{id}/external-aliases/{aliasID}/sending", Summary: "Set an external alias's sending connector (Admin, self-hosted)", Description: "Accepts provider and config; same schema, secret retention and CAS revision semantics as a domain sending config. Requeues only that alias's pending sends.", Role: "admin", Group: "Admin: external aliases"},
	{Method: "DELETE", Path: "/v1/admin/inboxes/{id}/external-aliases/{aliasID}/sending", Summary: "Clear an external alias's sending connector (Admin, self-hosted)", Role: "admin", Group: "Admin: external aliases", Success: 204},
	{Method: "GET", Path: "/v1/admin/inboxes/{id}/external-aliases/{aliasID}/sending/deliveries", Summary: "List delivery attempts for an external alias (Admin, self-hosted)", Description: "Returns that alias's per-attempt delivery log, newest first. Supports limit and before (keyset on attempt id).", Role: "admin", Group: "Admin: external aliases"},

	// Admin: Hermes.
	{Method: "GET", Path: "/v1/admin/hermes", Summary: "List Hermes connections (Admin)", Description: "Returns connection metadata (id, inbox, name, gateway id, outbound role, last acknowledged event and connectivity); never secrets.", Role: "admin", Group: "Admin: Hermes"},
	{Method: "POST", Path: "/v1/admin/hermes/enroll", Summary: "Enroll a Hermes Relay connection (Admin)", Description: "Body: inbox_id and name. Mints a gateway id, secret and delivery key, returned once with the connector URL and an env block.", Role: "admin", Group: "Admin: Hermes", Success: 201},
	{Method: "PUT", Path: "/v1/admin/hermes/{id}", Summary: "Update a Hermes connection outbound role (Admin)", Description: "Body: role. owner lets the relay send directly; assistant makes it draft and request approval instead.", Role: "admin", Group: "Admin: Hermes"},
	{Method: "DELETE", Path: "/v1/admin/hermes/{id}", Summary: "Delete a Hermes connection (Admin)", Role: "admin", Group: "Admin: Hermes", Success: 204},
}

// Routes returns the API table in documentation order.
func Routes() []Route { return routes }

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

// Missing compares the table against the live registrations. registered holds
// net/http ServeMux patterns of the form "METHOD /v1/path". It returns routes
// that are registered but absent from the table (undocumented) and table
// entries with no registered route (phantom). A non-empty result is a
// documentation bug.
func Missing(registered []string) (undocumented, phantom []string) {
	reg := make(map[string]bool, len(registered))
	for _, p := range registered {
		reg[strings.TrimSpace(p)] = true
	}
	table := make(map[string]bool, len(routes))
	for _, r := range routes {
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
