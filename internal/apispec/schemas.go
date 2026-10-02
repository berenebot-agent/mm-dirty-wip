package apispec

// This file is the components.schemas inventory for the OpenAPI document. The
// shapes mirror the JSON produced and consumed by internal/httpapp: model
// types carry the response fields and the handler-local request structs carry
// the request fields. Keeping them here (rather than reflecting over model at
// runtime) keeps apispec a leaf package and makes the wire contract reviewable
// in one place.

// Ref returns an OpenAPI reference to a named component schema.
func Ref(name string) map[string]any {
	return map[string]any{"$ref": "#/components/schemas/" + name}
}

func str(desc string) map[string]any     { return typed("string", desc) }
func ts(desc string) map[string]any      { return typed("date-time", desc) }
func integer(desc string) map[string]any { return typed("integer", desc) }
func boolean(desc string) map[string]any { return typed("boolean", desc) }

// typed builds a schema with an optional format and description.
func typed(format, desc string) map[string]any {
	m := map[string]any{}
	switch format {
	case "date-time":
		m["type"] = "string"
		m["format"] = "date-time"
		m["description"] = desc
	default:
		m["type"] = format
		if desc != "" {
			m["description"] = desc
		}
	}
	return m
}

// obj builds an object schema with optional required property names.
func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

// arrayOf builds an array schema whose items are the named component.
func arrayOf(name, desc string) map[string]any {
	m := map[string]any{"type": "array", "items": Ref(name)}
	if desc != "" {
		m["description"] = desc
	}
	return m
}

// stringList is an array-of-string schema.
func stringList(desc string) map[string]any {
	m := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	if desc != "" {
		m["description"] = desc
	}
	return m
}

// stringOrArray accepts either a single string or an array of strings, matching
// the server's lenient recipient parsing.
func stringOrArray(desc string) map[string]any {
	return map[string]any{
		"description": desc,
		"oneOf": []any{
			map[string]any{"type": "string"},
			map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
	}
}

// stringMap is an object with string values.
func stringMap(desc string) map[string]any {
	m := map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}}
	if desc != "" {
		m["description"] = desc
	}
	return m
}

// Schemas returns the components.schemas map for the OpenAPI document.
func Schemas() map[string]any {
	s := map[string]any{}
	for name, def := range schemas {
		s[name] = def
	}
	return s
}

// attachmentFields are shared by send/reply/draft write bodies.
var attachmentSchema = obj(map[string]any{
	"filename":     str("Attachment file name."),
	"content_type": str("IANA media type."),
	"content":      str("Raw bytes, base64-encoded."),
}, "filename", "content")

var schemas = map[string]any{
	"Error": obj(map[string]any{
		"error": str("Human-readable error message."),
		"code":  str("Stable machine-readable error code."),
	}, "error", "code"),

	"Address": obj(map[string]any{
		"name":    str("Display name."),
		"address": str("Email address."),
	}, "address"),

	"Limits": obj(map[string]any{
		"page_size_default":    integer("Default page size when limit is omitted."),
		"page_size_max_list":   integer("Ceiling for list endpoints (messages, threads, search, outbox, send requests, delivery logs)."),
		"page_size_max_events": integer("Ceiling for events and drafts."),
		"message_bytes_max":    integer("Maximum raw message size in bytes."),
		"attachment_bytes_max": integer("Maximum attachment size in bytes."),
		"send_per_minute":      integer("Per-credential send rate limit."),
		"login_per_minute":     integer("Per-IP login attempt limit."),
		"storage_quota_bytes":  integer("Default account storage quota in bytes."),
	}),

	"Attachment": obj(map[string]any{
		"id":           str("Attachment id."),
		"message_id":   str("Owning message id."),
		"filename":     str("File name."),
		"content_type": str("IANA media type."),
		"size":         integer("Size in bytes."),
		"content_id":   str("MIME Content-ID for inline parts."),
	}),

	"Message": obj(map[string]any{
		"id":                  str("Message id."),
		"inbox_id":            str("Owning inbox id."),
		"thread_id":           str("Thread id."),
		"direction":           str("inbound or outbound."),
		"from":                Ref("Address"),
		"to":                  stringList("To recipients."),
		"cc":                  stringList("Cc recipients."),
		"bcc":                 stringList("Bcc recipients."),
		"subject":             str("Subject line."),
		"text":                str("Plain-text body."),
		"html":                str("Sanitized HTML body, when present."),
		"message_id":          str("RFC 5322 Message-ID."),
		"in_reply_to":         str("RFC 5322 In-Reply-To."),
		"references":          stringList("RFC 5322 References."),
		"provider":            str("Provider that carried the message."),
		"provider_message_id": str("Provider-assigned message id."),
		"envelope_to":         stringList("SMTP envelope recipients."),
		"envelope_from":       str("Transport-supplied SMTP envelope sender (MAIL FROM); empty when the transport supplied none. Never derived from the MIME From header."),
		"envelope_recipient":  str("Canonical original envelope recipient, which may be a catch-all or alias address."),
		"client":              str("Credential that sent an outbound message."),
		"is_spam":             boolean("Whether the message is classified as spam."),
		"spam_reason":         str("Bounded spam classification reason."),
		"received_at":         ts("Inbound receipt time."),
		"sent_at":             ts("Outbound delivery time."),
		"created_at":          ts("Creation time."),
		"read":                boolean("Read state."),
		"deleted_at":          ts("Set when the message is in Trash; absent otherwise."),
		"labels":              stringList("Free-text labels. Each is trimmed, at most 64 characters, and may not contain control characters, '/' or '\\'."),
		"has_attachments":     boolean("Whether the message has attachments."),
		"size_bytes":          integer("Stored raw size in bytes."),
		"status":              str("Outbox status: pending, sent or failed. In an outbox listing, sending is true while a delivery attempt is in flight."),
		"attempts":            integer("Delivery attempts."),
		"last_error":          str("Last delivery error."),
		"next_retry":          str("Next scheduled retry."),
	}),

	"MessageList":    arrayOf("Message", "Newest-first list of messages."),
	"AttachmentList": arrayOf("Attachment", "Message attachments."),
	"LabelList":      stringList("Distinct labels in use."),
	"IdentityList":   obj(map[string]any{"identities": arrayOf("Identity", "Accessible identities.")}, "identities"),

	"Inbox": obj(map[string]any{
		"id":                    str("Inbox id."),
		"account_id":            str("Owning account id."),
		"domain_id":             str("Owning domain id."),
		"local_part":            str("Local part."),
		"address":               str("Full address."),
		"display_name":          str("Display name."),
		"enabled":               boolean("Whether the inbox accepts mail."),
		"allowed_senders":       stringList("Allowed sender patterns when sender_restricted."),
		"sender_restricted":     boolean("Whether the allowed-senders list is enforced."),
		"require_authenticated": boolean("Whether MX mail must be authenticated."),
		"approver_email":        str("Nominated external approval address."),
		"aliases":               stringList("Alias addresses that deliver to this inbox."),
		"alias_names":           stringMap("Alias address to display name."),
		"default_sender":        str("Preselected From address."),
		"external_aliases":      arrayOf("ExternalAlias", "Send-only external aliases (Admin readers only)."),
		"created_at":            ts("Creation time."),
	}),
	"InboxList": arrayOf("Inbox", "Accessible inboxes."),
	"InboxCreate": obj(map[string]any{
		"domain_id":    str("Owning domain id."),
		"local_part":   str("Local part."),
		"localpart":    str("openagent.email compatibility alias for local_part."),
		"display_name": str("Display name."),
	}),
	"InboxPatch": obj(map[string]any{
		"display_name":          str("Display name."),
		"enabled":               boolean("Whether the inbox accepts mail."),
		"allowed_senders":       stringList("Allowed sender patterns when sender_restricted."),
		"sender_restricted":     boolean("Enforce the allowed-senders list."),
		"require_authenticated": boolean("Require authenticated MX mail."),
		"approver_email":        str("Nominated external approval address; empty clears."),
		"aliases":               stringList("Replace the inbox alias set."),
		"alias_names":           stringMap("Alias address to display name."),
		"default_sender":        str("Preselected From address; empty clears to the primary."),
	}),
	"MailboxPermissions": obj(map[string]any{
		"role":        str("Effective role for this inbox: read, assistant, owner or admin."),
		"can_read":    boolean("Whether the credential can read messages and threads."),
		"can_draft":   boolean("Whether the credential can create and edit drafts."),
		"can_send":    boolean("Whether the credential can send and reply."),
		"can_approve": boolean("Whether the credential can approve a pending draft send."),
	}, "role", "can_read", "can_draft", "can_send", "can_approve"),

	"Bootstrap": obj(map[string]any{
		"account_id":    str("Account id."),
		"admin":         boolean("Whether the credential is account admin."),
		"mailbox_roles": stringMap("Inbox id to role: read, assistant or owner."),
		"permissions":   map[string]any{"type": "object", "additionalProperties": Ref("MailboxPermissions")},
		"inboxes":       arrayOf("Inbox", "Accessible inboxes."),
		"events":        obj(map[string]any{"list": str(""), "wait": str(""), "stream": str("")}),
		"limits":        Ref("Limits"),
	}),

	"Identity": obj(map[string]any{
		"address":         str("Identity address."),
		"name":            str("Display name."),
		"createdAt":       ts("Creation time."),
		"pushContentTier": integer("openagent.email push tier."),
	}),
	"IdentityCreate": obj(map[string]any{
		"name":      str("Display name."),
		"localpart": str("Local part; generated when omitted."),
		"domain_id": str("Domain id; required only when the account has several."),
	}),
	"IdentityCreated": obj(map[string]any{
		"address":         str("Identity address."),
		"name":            str("Display name."),
		"pushContentTier": integer("openagent.email push tier."),
		"token":           str("One-time owner API key for the identity."),
	}),
	"Deleted": obj(map[string]any{"deleted": boolean("Always true on success.")}),

	"MessagePatch": obj(map[string]any{
		"read":   boolean("Set the read state."),
		"labels": stringList("Replace the full label set; empty clears. Each label is trimmed, at most 64 characters, and may not contain control characters, '/' or '\\'."),
		"spam":   boolean("Move into or out of Spam."),
	}),
	"SeenBody": obj(map[string]any{
		"address": str("Compatibility inbox address guard."),
		"seen":    boolean("Read state; defaults to true."),
	}),
	"SeenResult": obj(map[string]any{"id": str("Message id."), "seen": boolean("Applied read state.")}, "id", "seen"),

	"MessageWaitBody": obj(map[string]any{
		"after":           str("Durable cursor to wait past."),
		"inbox":           str("Scope to one inbox id."),
		"timeout":         integer("Seconds to wait."),
		"timeoutSec":      integer("openagent.email compatibility alias for timeout."),
		"address":         str("openagent.email compatibility inbox selection."),
		"fromContains":    str("Filter on a substring of the From address."),
		"subjectContains": str("Filter on a substring of the subject."),
		"include_spam":    boolean("Include spam."),
	}),

	"Thread": obj(map[string]any{
		"id":              str("Thread id."),
		"inbox_id":        str("Owning inbox id."),
		"subject":         str("Thread subject."),
		"message_count":   integer("Number of messages."),
		"last_message_at": ts("Newest message time."),
	}),
	"ThreadList": arrayOf("Thread", "Threads, newest activity first."),
	"ThreadDetail": obj(map[string]any{
		"id":            str("Thread id."),
		"inbox_id":      str("Owning inbox id."),
		"subject":       str("Thread subject."),
		"message_count": integer("Number of messages."),
		"messages":      arrayOf("Message", "Messages in the thread."),
	}),

	"Event": obj(map[string]any{
		"cursor":     str("Durable cursor usable with after."),
		"inbox_id":   str("Related inbox id."),
		"type":       str("Event type, e.g. message.received."),
		"entity_id":  str("Related entity id."),
		"payload":    map[string]any{"type": "object", "description": "Event-specific payload."},
		"created_at": ts("Event time."),
	}),
	"EventList": arrayOf("Event", "Oldest-first event page."),

	"SendAttachment": attachmentSchema,

	"SendBody": obj(map[string]any{
		"inbox_id":    str("Inbox id; required unless from identifies one accessible inbox or a non-admin key owns exactly one inbox."),
		"from":        str("Compatibility inbox address selector; optional when inbox_id is supplied."),
		"sender":      str("From identity: the inbox primary or one of its aliases."),
		"to":          stringOrArray("Required: at least one recipient; a single address or a list."),
		"cc":          stringOrArray("Optional Cc recipients; a single address or a list."),
		"bcc":         stringOrArray("Optional Bcc recipients; a single address or a list."),
		"subject":     str("Required and must be non-empty."),
		"text":        str("Plain-text body; text or html must be non-empty."),
		"html":        str("HTML body; text or html must be non-empty."),
		"attachments": map[string]any{"type": "array", "description": "Optional base64-encoded attachments.", "items": Ref("SendAttachment")},
	}, "to", "subject"),

	"ReplyBody": obj(map[string]any{
		"sender":      str("From identity: the inbox primary or one of its aliases."),
		"text":        str("Plain-text body."),
		"html":        str("HTML body."),
		"attachments": map[string]any{"type": "array", "items": Ref("SendAttachment")},
	}),

	"SendQueued": obj(map[string]any{
		"queued":              boolean("Whether the message entered the outbox."),
		"messageId":           str("RFC 5322 Message-ID."),
		"provider_message_id": str("Provider-assigned message id."),
		"message":             Ref("Message"),
	}),
	"ReplyResult": obj(map[string]any{
		"provider_message_id": str("Provider-assigned message id."),
		"message":             Ref("Message"),
	}),
	"ApproveResult": obj(map[string]any{
		"queued":    boolean("Whether the approved message entered the outbox."),
		"messageId": str("RFC 5322 Message-ID."),
		"message":   Ref("Message"),
	}),
	"DraftWriteResult": map[string]any{
		"description": "The saved draft, or the queued send when action=send.",
		"oneOf":       []any{Ref("Draft"), Ref("SendQueued")},
	},

	"Draft": obj(map[string]any{
		"id":                     str("Draft id."),
		"inbox_id":               str("Owning inbox id."),
		"reply_to_message_id":    str("Message being replied to."),
		"from_address":           str("Chosen From address."),
		"from_name":              str("Resolved From display name."),
		"from_external_alias_id": str("External alias id when the sender is one."),
		"to":                     stringList("To recipients."),
		"cc":                     stringList("Cc recipients."),
		"bcc":                    stringList("Bcc recipients."),
		"subject":                str("Subject line."),
		"text":                   str("Plain-text body."),
		"html":                   str("HTML body."),
		"status":                 str("draft, pending_approval or rejected."),
		"send_request":           Ref("SendRequest"),
		"attachments":            arrayOf("DraftAttachment", "Draft attachments."),
		"created_at":             ts("Creation time."),
		"updated_at":             ts("Last update time."),
	}),
	"DraftList": arrayOf("Draft", "Drafts, most recently updated first."),

	"DraftWrite": obj(map[string]any{
		"inbox_id":            str("Owning inbox id."),
		"reply_to_message_id": str("Message being replied to."),
		"from_address":        str("Chosen From address."),
		"sender":              str("Alias for from_address."),
		"from_name":           str("From display name (compatibility)."),
		"to":                  stringList("To recipients."),
		"cc":                  stringList("Cc recipients."),
		"bcc":                 stringList("Bcc recipients."),
		"subject":             str("Subject line."),
		"text":                str("Plain-text body."),
		"html":                str("HTML body."),
		"attachments":         map[string]any{"type": "array", "items": Ref("SendAttachment")},
		"action":              str("draft (default), request-send or send."),
		"external":            boolean("Accepted for request-send compatibility."),
		"id":                  str("Compatibility field, ignored."),
		"status":              str("Compatibility field, ignored."),
	}),

	"DraftAttachment": obj(map[string]any{
		"id":           str("Attachment id."),
		"draft_id":     str("Owning draft id."),
		"filename":     str("File name."),
		"content_type": str("IANA media type."),
		"size":         integer("Size in bytes."),
		"created_at":   ts("Upload time."),
	}),
	"DraftAttachmentList": arrayOf("DraftAttachment", "Draft attachments."),
	"DraftAttachmentUpload": obj(map[string]any{
		"attachments": map[string]any{"type": "array", "items": map[string]any{"type": "string", "format": "binary"}},
	}, "attachments"),

	"RequestSendBody": obj(map[string]any{
		"inbox_id":            str("Owning inbox id."),
		"reply_to_message_id": str("Message being replied to."),
		"from_address":        str("Chosen From address."),
		"sender":              str("Alias for from_address."),
		"to":                  stringList("To recipients."),
		"cc":                  stringList("Cc recipients."),
		"bcc":                 stringList("Bcc recipients."),
		"subject":             str("Subject line."),
		"text":                str("Plain-text body."),
		"html":                str("HTML body."),
		"attachments":         map[string]any{"type": "array", "items": Ref("SendAttachment")},
		"external":            boolean("Optional; only meaningful with a configured approver."),
	}),

	"FeedbackBody": obj(map[string]any{
		"feedback": str("Optional decision feedback for the agent."),
	}),

	"SendRequest": obj(map[string]any{
		"id":                      str("Request id."),
		"draft_id":                str("Draft id."),
		"inbox_id":                str("Owning inbox id."),
		"status":                  str("pending, approved, rejected, cancelled or expired."),
		"delivery_status":         str("none, pending, sent or failed."),
		"requested_at":            ts("Request time."),
		"requested_by":            str("Requesting principal label."),
		"requested_by_api_key_id": str("Requesting API key id."),
		"requested_by_user_id":    str("Requesting user id."),
		"approver_email":          str("Nominated approver address."),
		"token_expires_at":        ts("Approval token expiry."),
		"approval_workflow_id":    str("Correlating workflow id."),
		"notification_status":     str("none, queued, sent or failed."),
		"decided_at":              ts("Decision time."),
		"decision_actor":          str("Who decided."),
		"decision_actor_id":       str("Deciding principal id."),
		"decision_method":         str("ui, api or email."),
		"feedback":                str("Decision feedback."),
		"message_id":              str("Resulting message id."),
		"created_at":              ts("Creation time."),
		"updated_at":              ts("Last update time."),
	}),
	"SendRequestList": arrayOf("SendRequest", "Send requests, newest first."),

	"APIKey": obj(map[string]any{
		"id":         str("Key id."),
		"name":       str("Key name."),
		"prefix":     str("Display prefix."),
		"admin":      boolean("Whether the key is account admin."),
		"mailboxes":  stringMap("Inbox id to role."),
		"created_at": ts("Creation time."),
	}),
	"APIKeyList": arrayOf("APIKey", "API keys, never including the secret."),
	"KeyCreateBody": obj(map[string]any{
		"name":      str("Key name."),
		"admin":     boolean("Grant account admin."),
		"mailboxes": stringMap("Inbox id to role."),
	}, "name"),
	"KeyCreated": obj(map[string]any{
		"key":   Ref("APIKey"),
		"token": str("The plaintext token, returned once."),
	}, "key", "token"),

	"Domain": obj(map[string]any{
		"id":                 str("Domain id."),
		"account_id":         str("Owning account id."),
		"name":               str("Domain name."),
		"catch_all_inbox_id": str("Catch-all inbox id."),
		"sending_provider":   str("Configured sending provider."),
		"receiving_provider": str("Configured receiving provider."),
		"created_at":         ts("Creation time."),
	}),
	"DomainList":   arrayOf("Domain", "Account domains."),
	"DomainCreate": obj(map[string]any{"name": str("Domain name.")}, "name"),
	"DomainPatch":  obj(map[string]any{"catch_all_inbox_id": str("Catch-all inbox id; empty clears.")}),
	"Updated":      obj(map[string]any{"updated": boolean("Always true on success.")}),

	"DomainConfig": obj(map[string]any{
		"domain_id":   str("Domain id."),
		"configured":  boolean("Whether a provider is configured."),
		"provider":    str("Provider name."),
		"config":      map[string]any{"type": "object", "description": "Non-secret provider config fields."},
		"updated_at":  ts("Last config update."),
		"webhook_url": str("Public webhook URL for the receiving provider."),
		"generated":   stringMap("Secrets generated by this request, returned once."),
	}),
	"DomainSendingPut": obj(map[string]any{
		"provider": str("Provider name."),
		"config":   map[string]any{"type": "object", "description": "Provider config; secret fields are encrypted at rest."},
	}, "provider"),
	"DomainReceivingPut": obj(map[string]any{
		"provider":          str("Provider name."),
		"config":            map[string]any{"type": "object", "description": "Provider config; secret fields are encrypted at rest."},
		"regenerate_secret": boolean("Rotate the provider signing secret."),
	}, "provider"),

	"MXSettings": obj(map[string]any{
		"mode":                    str("Receiver mode: included (embedded edge) or remote (separate receiver)."),
		"url":                     str("Remote receiver origin; empty for included mode."),
		"bearer_key":              str("Always empty on a read; the credential is never returned."),
		"key_configured":          boolean("Whether a bearer credential is stored."),
		"ca":                      str("PEM CA bundle for a private remote receiver; empty uses system roots."),
		"hostname":                str("SMTP greeting hostname advertised by the included receiver."),
		"max_message_bytes":       integer("Included receiver per-message byte cap (0 uses the receiver default, 31457280)."),
		"max_staging_bytes":       integer("Included receiver in-memory staging cap across transactions (0 uses the receiver default, 268435456)."),
		"max_recipients":          integer("Included receiver recipients per transaction (0 uses the receiver default, 100)."),
		"max_connections":         integer("Included receiver concurrent connection cap (0 uses the receiver default, 256)."),
		"require_tls":             boolean("Included receiver refuses plaintext SMTP. Omitted when not applicable (remote); default off, and it requires a certificate."),
		"verify_spf":              boolean("Included receiver SPF verification. Omitted when not applicable (remote); when present, true or false is the operator's explicit choice. Default on."),
		"verify_dkim":             boolean("Included receiver DKIM verification. Default on."),
		"verify_dmarc":            boolean("Included receiver DMARC verification. Default on."),
		"dns_resolver":            str("Included receiver DNS resolver (host:port); empty uses the system resolver."),
		"dns_timeout_seconds":     integer("Included receiver DNS lookup timeout (0 uses the receiver default, 10)."),
		"read_timeout_seconds":    integer("Included receiver SMTP read timeout (0 uses the receiver default, 60)."),
		"write_timeout_seconds":   integer("Included receiver SMTP write timeout (0 uses the receiver default, 60)."),
		"data_timeout_seconds":    integer("Included receiver SMTP DATA timeout (0 uses the receiver default, 300)."),
		"smtp_tls_cert":           str("Included receiver STARTTLS certificate (PEM, public). Clearing it (with a blank key) removes the pair."),
		"smtp_tls_key":            str("Included receiver STARTTLS private key (PEM). Write-only: never returned by GET."),
		"smtp_tls_key_configured": boolean("Whether an SMTP STARTTLS private key is stored. Always present on a read; ignored on write."),
		"revision":                integer("Optimistic-concurrency token a save must echo."),
		"updated_at":              ts("Last update time."),
		"status":                  Ref("MXStatus"),
	}),
	"MXSettingsPut": obj(map[string]any{
		"mode":                  str("Required. included or remote."),
		"url":                   str("Required for remote; must be an HTTP(S) origin with no path, userinfo, query or fragment. Rejected for included."),
		"bearer_key":            str("Required for remote on a first save; a blank value retains the stored credential. Ignored for included (the core generates and retains one)."),
		"ca":                    str("Optional PEM CA bundle for a private remote receiver."),
		"hostname":              str("Included receiver SMTP greeting hostname."),
		"max_message_bytes":     integer("Included receiver per-message byte cap; at least 1 MiB when set."),
		"max_staging_bytes":     integer("Included receiver in-memory staging cap; at least the max message size when set."),
		"max_recipients":        integer("Included receiver recipients per transaction."),
		"max_connections":       integer("Included receiver concurrent connection cap."),
		"require_tls":           boolean("Included receiver refuses plaintext SMTP; requires a certificate and key. An explicit true/false, omit to leave the default (off)."),
		"verify_spf":            boolean("Included receiver SPF verification; an explicit true/false, omit to leave the default (on). A remote save carrying any verification toggle is rejected."),
		"verify_dkim":           boolean("Included receiver DKIM verification."),
		"verify_dmarc":          boolean("Included receiver DMARC verification."),
		"dns_resolver":          str("Included receiver DNS resolver (host:port)."),
		"dns_timeout_seconds":   integer("Included receiver DNS timeout in seconds."),
		"read_timeout_seconds":  integer("Included receiver SMTP read timeout in seconds."),
		"write_timeout_seconds": integer("Included receiver SMTP write timeout in seconds."),
		"data_timeout_seconds":  integer("Included receiver SMTP DATA timeout in seconds."),
		"smtp_tls_cert":         str("Included receiver STARTTLS certificate (PEM, public). Supply together with smtp_tls_key, or clear both."),
		"smtp_tls_key":          str("Included receiver STARTTLS private key (PEM). A blank value retains the stored key; the GET never returns it."),
		"revision":              integer("Current revision, or zero to create."),
	}, "mode"),
	"MXStatus": obj(map[string]any{
		"mode":               str("Configured receiver mode."),
		"configured":         boolean("Whether a receiver is configured."),
		"revision":           integer("Persisted revision the live state reflects."),
		"state":              str("disabled, unknown, standby, connecting, active, draining, failed or unavailable. active means the receiver completed its live session handshake; connecting means a change or session is still in progress, so readiness is never claimed prematurely."),
		"detail":             str("Human-readable failure or availability detail."),
		"included_supported": boolean("Whether this process can run the embedded (included) receiver."),
		"smtp_addr":          str("Bound SMTP listener for the included receiver."),
		"session_addr":       str("Bound loopback session listener for the included receiver."),
		"active_connections": integer("Live SMTP connections on the included receiver."),
	}),

	"ExternalAlias": obj(map[string]any{
		"id":           str("Alias id."),
		"inbox_id":     str("Owning inbox id."),
		"address":      str("Send-only address."),
		"display_name": str("Display name."),
		"provider":     str("Sending connector provider."),
		"configured":   boolean("Whether a sending connector is configured."),
		"revision":     integer("Config revision for compare-and-set."),
		"created_at":   ts("Creation time."),
		"updated_at":   ts("Last update time."),
	}),
	"ExternalAliasList": arrayOf("ExternalAlias", "An inbox's external aliases."),
	"ExternalAliasCreate": obj(map[string]any{
		"address":      str("Full address; immutable after creation."),
		"display_name": str("Optional display name."),
	}, "address"),
	"ExternalAliasPatch": obj(map[string]any{"display_name": str("New display name.")}),
	"ExternalAliasSending": obj(map[string]any{
		"alias_id":   str("Alias id."),
		"configured": boolean("Whether a connector is configured."),
		"provider":   str("Connector provider."),
		"config":     map[string]any{"type": "object", "description": "Non-secret connector config."},
	}),
	"ExternalAliasSendingPut": obj(map[string]any{
		"provider": str("Connector provider."),
		"config":   map[string]any{"type": "object", "description": "Connector config; secret fields are encrypted at rest."},
	}, "provider"),

	"DeliveryAttempt": obj(map[string]any{
		"external_alias_id":   str("External alias id, when applicable."),
		"id":                  integer("Attempt id (keyset cursor)."),
		"domain_id":           str("Attributed domain id."),
		"provider":            str("Provider at attempt time."),
		"message_id":          str("Related message id."),
		"workflow_id":         str("Related workflow id."),
		"attempt":             integer("Attempt number."),
		"status":              str("sent, failed, sending (in flight) or interrupted (an in-flight attempt abandoned before an outcome was recorded)."),
		"provider_message_id": str("Provider-assigned message id."),
		"error_text":          str("Failure detail."),
		"from_address":        str("Envelope From."),
		"to":                  stringList("Envelope recipients."),
		"subject":             str("Subject line."),
		"created_at":          ts("Attempt time."),
	}),
	"DeliveryAttemptList": arrayOf("DeliveryAttempt", "Delivery attempts, newest first."),

	"DomainLogEntry": obj(map[string]any{
		"kind":                str("sent, failed, received, blocked or approval."),
		"id":                  str("Row id."),
		"created_at":          ts("Event time."),
		"inbox_id":            str("Related inbox id."),
		"provider":            str("Provider."),
		"from_address":        str("Envelope From."),
		"to":                  stringList("Envelope recipients."),
		"subject":             str("Subject line."),
		"client":              str("Sending credential label."),
		"size_bytes":          integer("Message size."),
		"provider_message_id": str("Provider-assigned message id."),
		"attempt":             integer("Attempt number."),
		"status":              str("Attempt or delivery status."),
		"reason":              str("Block reason."),
		"error_text":          str("Failure detail."),
		"request_id":          str("Related approval request id."),
		"action":              str("Approval action."),
		"message_id":          str("Click-through message id."),
	}),
	"DomainLogList": arrayOf("DomainLogEntry", "Two-way domain activity, newest first."),

	"HermesConnection": obj(map[string]any{
		"ID":              str("Connection id."),
		"InboxID":         str("Owning inbox id."),
		"Name":            str("Connection name."),
		"GatewayID":       str("Relay gateway id."),
		"OutboundRole":    str("owner (send directly) or assistant (draft and request approval)."),
		"LastAckEventID":  integer("Last acknowledged event id."),
		"CreatedAt":       ts("Creation time."),
		"LastConnectedAt": ts("Last connection time."),
	}),
	"HermesConnectionList": arrayOf("HermesConnection", "Relay connections, never including secrets."),
	"HermesEnrollBody": obj(map[string]any{
		"inbox_id": str("Inbox to bind."),
		"name":     str("Connection name."),
	}, "inbox_id", "name"),
	"HermesEnrollment": obj(map[string]any{
		"gateway_id":    str("Minted gateway id."),
		"secret":        str("Gateway secret, returned once."),
		"delivery_key":  str("Delivery key, returned once."),
		"connector_url": str("Relay connector URL."),
		"env":           str("Ready-to-paste environment block."),
	}),
	"OpenClawEnrollment": obj(map[string]any{
		"gateway_id":    str("Minted gateway id."),
		"secret":        str("Relay secret, returned once."),
		"delivery_key":  str("Delivery key, returned once."),
		"connector_url": str("Relay connector URL."),
		"kind":          str("Connector kind (openclaw)."),
	}),
	"HermesUpdateBody": obj(map[string]any{"role": str("owner or assistant.")}, "role"),
	"HermesUpdateResult": obj(map[string]any{
		"status": str("ok."),
		"role":   str("Applied outbound role."),
	}),
	"OpenClawSetupCode": obj(map[string]any{
		"code":       str("One-time setup code, returned once."),
		"expires_in": integer("Seconds until the code expires."),
		"setup_url":  str("Claim URL carrying the code in its fragment."),
		"command":    str("Ready-to-paste openclaw channels add command."),
	}),

	"WebhookClient": obj(map[string]any{
		"ID":             str("Client id."),
		"InboxID":        str("Owning inbox id."),
		"Name":           str("Client name."),
		"URL":            str("HTTPS destination URL."),
		"Mode":           str("notify (small JSON event) or forward (raw MIME)."),
		"AuthMode":       str("signature (HMAC) or bearer."),
		"Enabled":        boolean("Whether delivery is active."),
		"LastAckEventID": integer("Last acknowledged event id."),
		"LastSuccessAt":  ts("Last successful delivery."),
		"LastError":      str("Most recent delivery error, if any."),
		"CreatedAt":      ts("Creation time."),
	}),
	"WebhookClientList": arrayOf("WebhookClient", "Webhook clients, never including secrets."),
	"ClientList": obj(map[string]any{
		"api_keys": arrayOf("APIKey", "API key clients, never including secrets."),
		"hermes":   arrayOf("HermesConnection", "Hermes relay clients, never including secrets."),
		"webhooks": arrayOf("WebhookClient", "Webhook clients, never including secrets."),
	}),
	"WebhookCreateBody": obj(map[string]any{
		"inbox_id":      str("Inbox to bind."),
		"name":          str("Client name."),
		"url":           str("HTTPS destination URL."),
		"mode":          str("notify or forward."),
		"auth":          str("signature or bearer."),
		"bearer_secret": str("Optional existing bearer token, without the Bearer prefix. Requires auth=bearer; omitted or empty generates a secret."),
	}, "inbox_id", "url", "mode", "auth"),
	"WebhookCreated": obj(map[string]any{
		"client": Ref("WebhookClient"),
		"secret": str("The signing or bearer secret, returned once."),
	}, "client", "secret"),
	"WebhookUpdateBody": obj(map[string]any{
		"name":          str("Client name."),
		"url":           str("HTTPS destination URL."),
		"mode":          str("notify or forward."),
		"auth":          str("signature or bearer."),
		"bearer_secret": str("Optional replacement bearer token, without the Bearer prefix. Requires auth=bearer; omitted or empty preserves the current secret."),
	}, "url", "mode", "auth"),
	"WebhookRotateResult":  obj(map[string]any{"secret": str("The new signing secret, returned once.")}, "secret"),
	"WebhookEnabledBody":   obj(map[string]any{"enabled": boolean("Whether delivery is active.")}, "enabled"),
	"WebhookEnabledResult": obj(map[string]any{"enabled": boolean("Applied enabled state.")}, "enabled"),
}
