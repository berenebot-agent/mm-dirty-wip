package apispec

// This file is the components.schemas inventory for the OpenAPI document. The
// shapes mirror the JSON produced and consumed by internal/httpapp: explicit
// projections or model types carry response fields; handler-local structs carry
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

// commonEnvelopeSchema builds the shared paginated listing envelope whose items
// are the named component. Every common mailbox listing returns this shape so a
// client parses one pagination contract for every mailbox kind.
func commonEnvelopeSchema(itemName, desc string) map[string]any {
	m := obj(map[string]any{
		"items":        arrayOf(itemName, "This page's items."),
		"next_cursor":  str("Opaque keyset cursor for the following page; empty on the last page."),
		"completeness": str("Whether the source could enumerate the whole result set: complete, partial or unknown. Orthogonal to pagination."),
		"errors":       arrayOf("InboxFailure", "Per-inbox failures when a listing spans more than one inbox."),
	}, "items", "completeness")
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
	"MessageActionControl": obj(map[string]any{"action": str("retry or dismiss")}, "action"),
	"MessageActionResult":  obj(map[string]any{"queued": boolean("Whether the durable action is still pending."), "action": obj(map[string]any{"id": str("Action handle."), "inbox_id": str(""), "message_id": str(""), "state": str("pending, running, done, failed or ambiguous"), "error": str("Public failure detail."), "attempts": integer("Attempts made.")})}),
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
		"id":                     str("Message id."),
		"inbox_id":               str("Owning inbox id."),
		"inbox_kind":             str("Owning inbox kind: domain or standalone. Informational metadata; it does not select a different message schema."),
		"thread_id":              str("Thread id."),
		"direction":              map[string]any{"type": "string", "enum": []string{"inbound", "outbound", "unknown"}, "description": "Recorded direction wins; otherwise Sent evidence wins over Inbox evidence; drafts and unclassified folders are unknown. Evidence uses native labels/mapped roles, never a folder name or From-address guess."},
		"from":                   Ref("Address"),
		"to":                     stringList("To recipients."),
		"cc":                     stringList("Cc recipients."),
		"bcc":                    stringList("Bcc recipients."),
		"subject":                str("Subject line."),
		"text":                   str("Plain-text body on a detail read (possibly empty). Absent on message lists, search and thread-message listings."),
		"html":                   str("Sanitized HTML body on a detail read, when non-empty. Absent on metadata-only listings."),
		"message_id":             str("RFC 5322 Message-ID."),
		"in_reply_to":            str("RFC 5322 In-Reply-To."),
		"references":             stringList("RFC 5322 References."),
		"provider":               str("Mailbox provider: mailmoose for domain inboxes; google, microsoft, imap or one for connected inboxes. unknown only when mailbox identity cannot be resolved."),
		"message_transport":      str("Transport used to receive/send the message. For connected inboxes this equals provider. For domain inboxes it names the delivery transport, or unknown when unavailable."),
		"provider_message_id":    str("Provider-assigned message id."),
		"envelope_to":            stringList("SMTP envelope recipients."),
		"envelope_from":          str("Transport-supplied SMTP envelope sender (MAIL FROM); empty when the transport supplied none. Never derived from the MIME From header."),
		"envelope_recipient":     str("Canonical original envelope recipient, which may be a catch-all or alias address."),
		"source":                 str("Sending credential label or receiving source, when known."),
		"is_spam":                boolean("Whether the message is classified as spam; absent when no classification is known."),
		"spam_reason":            str("Bounded spam classification reason."),
		"auth_results":           map[string]any{"type": "object", "description": "Normalized authentication evidence, when available.", "additionalProperties": true},
		"received_at":            ts("Known receipt timestamp."),
		"sent_at":                ts("Known send timestamp, supplied by delivery or message metadata."),
		"created_at":             ts("Known message creation time; absent when unavailable. Never substituted with receipt or indexing time."),
		"read":                   boolean("Read state."),
		"deleted_at":             ts("Set when the message is in Trash; absent otherwise."),
		"delivery_action_due_at": ts("Eligibility time for the configured delivery auto-trash action, when set."),
		"deliveries":             map[string]any{"type": "array", "items": Ref("MessageDelivery")},
		"labels":                 stringList("Legacy additive label names only (free-text tags and provider categories). Prefer label_ids, which also carries folder-backed labels."),
		"label_ids":              stringList("Canonical unified label membership for the owning inbox's catalogue: tag:<Name> for additive labels and folder:<id> for folder-backed labels. Always present; empty when the message carries no label."),
		"has_attachments":        boolean("Whether the message has attachments."),
		"size_bytes":             integer("Known raw message size in bytes (reported by the mailbox when raw MIME is not stored)."),
		"mailbox_id":             str("Folder the message belongs to within its inbox; empty for the implicit system Inbox."),
		"folder_path":            str("Human-readable mailbox folder path, when known. A folder path need not have a local mailbox_id."),
		"status":                 str("Outbox status: pending, sent or failed. In an outbox listing, sending is true while a delivery attempt is in flight."),
		"attempts":               integer("Delivery attempts."),
		"last_error":             str("Last delivery error."),
		"next_retry":             str("Next scheduled retry."),
		"sending":                boolean("True while an outbound delivery attempt is in flight; absent otherwise."),
		"snippet":                str("Cached plain-text preview, at most 200 Unicode characters. Always a string; '-' means no usable preview is available. Generated for new local/IMAP mail without a historical back-generation job."),
		"flagged":                boolean("Flagged state, when known. Known false is included."),
		"answered":               boolean("Answered state, when known. Known false is included."),
		"draft":                  boolean("Draft state, when known. Known false is included."),
		"flags":                  stringList("Mailbox flags, when available."),
		"internal_date":          ts("Mailbox internal date, when known."),
		"indexed_at":             ts("Time cached metadata was indexed, when known."),
	}, "id", "inbox_id", "from", "to", "cc", "subject", "read", "has_attachments", "size_bytes", "provider", "message_transport", "direction", "snippet", "label_ids"),

	"MessageDelivery": obj(map[string]any{
		"client_id":    str("Connector credential id."),
		"delivered_at": ts("Successful connector delivery time."),
	}, "client_id", "delivered_at"),

	"MessageList":    arrayOf("Message", "Newest-first list of messages."),
	"AttachmentList": arrayOf("Attachment", "Message attachments."),
	"LabelList":      stringList("Distinct labels in use."),
	"MailboxLabel": obj(map[string]any{
		"id":         str("Label id: tag:Name or folder:<id>. Use it verbatim as label_id."),
		"name":       str("Human-readable label name."),
		"kind":       map[string]any{"type": "string", "enum": []string{"tag", "folder"}, "description": "tag (additive) or folder (move destination)."},
		"membership": map[string]any{"type": "string", "enum": []string{"multiple", "exclusive"}, "description": "multiple for tags or exclusive for folder-backed labels."},
		"source":     map[string]any{"type": "string", "enum": []string{"local", "provider"}, "description": "local for MailMoose-owned label metadata, provider for a provider-owned label or folder."},
		"folder_id":  str("Underlying folder id when applicable."),
		"can_remove": boolean("Can remove membership without a move."),
		"can_rename": boolean("Can rename label through the provider or local store."),
		"can_delete": boolean("Can delete label through the provider or local store."),
	}, "id", "name", "kind", "membership", "source", "can_remove", "can_rename", "can_delete"),
	"MailboxLabelCapabilities": obj(map[string]any{
		"can_create":        boolean("Whether this inbox accepts a new label."),
		"create_kind":       map[string]any{"type": "string", "enum": []string{"tag", "folder"}, "description": "Kind a created label takes: tag for additive labels, folder for an IMAP remote folder."},
		"create_membership": map[string]any{"type": "string", "enum": []string{"multiple", "exclusive"}, "description": "Membership a created label has. exclusive means applying it moves the message."},
		"can_rename":        boolean("Whether this inbox supports renaming labels through the provider or local store."),
		"can_delete":        boolean("Whether this inbox supports deleting labels through the provider or local store."),
	}, "can_create", "create_kind", "create_membership", "can_rename", "can_delete"),
	"MailboxLabelCatalog": obj(map[string]any{
		"items":        arrayOf("MailboxLabel", "This page's labels."),
		"next_cursor":  str("Opaque cursor for the following page; empty on the last page."),
		"completeness": str("Whether the source could enumerate every label: complete, partial or unknown."),
		"errors":       map[string]any{"type": "array", "items": Ref("InboxFailure"), "description": "Per-inbox failures when a listing spans more than one inbox."},
		"capabilities": Ref("MailboxLabelCapabilities"),
	}, "items", "completeness", "capabilities"),
	"MailboxLabelEnvelope": commonEnvelopeSchema("MailboxLabel", "Unified provider-aware label catalogue."),
	"MailboxLabelCreate":   obj(map[string]any{"name": str("New label name.")}, "name"),
	"MailboxLabelRename":   obj(map[string]any{"name": str("Replacement label name.")}, "name"),
	"MailboxLabelAction": obj(map[string]any{
		"name":      str("Free-text label name. Either name or label_id is required."),
		"label_id":  str("Explicit label identity, including folder-backed destinations."),
		"operation": map[string]any{"type": "string", "enum": []string{"add", "remove"}, "description": "add or remove."},
	}, "operation"),

	"IdentityList": obj(map[string]any{"identities": arrayOf("Identity", "Accessible identities.")}, "identities"),

	"InboxFailure": obj(map[string]any{
		"inbox_id":  str("Inbox id the failure relates to."),
		"code":      str("Stable, transport-independent classification (the MailboxError kind vocabulary)."),
		"message":   str("Short, safe description; never a raw provider or store error."),
		"retryable": boolean("Whether retrying the same operation may succeed without operator intervention."),
	}, "inbox_id", "code"),
	"Folder": obj(map[string]any{
		"id":            str("Folder id."),
		"inbox_id":      str("Owning inbox id."),
		"path":          str("Full hierarchical path; the stable opaque locator."),
		"name":          str("Display name of the final path segment."),
		"parent_path":   str("Parent folder path; empty for a top-level folder."),
		"role":          str("Well-known role: folder, inbox, sent, drafts, trash, spam, archive, outbox or label."),
		"message_count": integer("Messages in the folder."),
		"unread_count":  integer("Unread messages in the folder."),
		"selectable":    boolean("Whether the folder may be chosen as a sync target or move destination."),
		"is_system":     boolean("Whether the folder is a seeded, protected system folder."),
		"created_at":    ts("Creation time."),
		"updated_at":    ts("Last update time."),
	}, "id", "inbox_id", "path", "name", "role", "selectable"),
	"FolderCreate": obj(map[string]any{
		"path": str("Slash-separated folder path; required."),
		"name": str("Display name of the final segment; defaults from path."),
	}, "path"),
	"FolderRename":    obj(map[string]any{"name": str("New display name.")}, "name"),
	"FolderList":      commonEnvelopeSchema("Folder", "Folder tree in hierarchical order."),
	"MessageEnvelope": commonEnvelopeSchema("Message", "Message page."),
	"ThreadEnvelope":  commonEnvelopeSchema("Thread", "Thread page."),
	"RemoteConfig": obj(map[string]any{
		"host":              str("IMAP server hostname."),
		"port":              integer("IMAP port."),
		"username":          str("IMAP login identity."),
		"security":          str("Transport security: tls (default), starttls or plain."),
		"namespace":         str("Selected root folder the inbox syncs; defaults to INBOX."),
		"smtp_host":         str("Optional outbound SMTP host."),
		"smtp_port":         integer("Optional outbound SMTP port."),
		"smtp_username":     str("Optional outbound SMTP login identity."),
		"smtp_security":     str("Optional outbound SMTP transport security."),
		"imap_password_set": boolean("Whether an IMAP password is stored; the secret is never returned."),
		"smtp_password_set": boolean("Whether an SMTP password is stored; the secret is never returned."),
		"configured":        boolean("Whether the connector has a stored IMAP credential and host."),
		"capabilities":      Ref("Capabilities"),
		"missing_roles":     stringList("Special folder roles not yet mapped, so a client can prompt an explicit select-or-create."),
		"sent_copy_enabled": boolean("Whether a sent message is copied into the remote Sent folder."),
		"sent_copy_folder":  str("Optional explicit destination folder for the sent copy."),
	}),
	"RemoteConfigPut": obj(map[string]any{
		"host":              str("IMAP host."),
		"port":              integer("IMAP port."),
		"username":          str("IMAP username."),
		"security":          str("tls, starttls or plain."),
		"smtp_host":         str("Optional SMTP host."),
		"smtp_port":         integer("Optional SMTP port."),
		"smtp_username":     str("Optional SMTP username."),
		"smtp_security":     str("Optional SMTP security."),
		"clear_smtp":        boolean("Remove the outbound SMTP binding entirely."),
		"namespace":         str("Selected root folder; defaults to INBOX."),
		"imap_password":     str("IMAP password; blank retains the stored value."),
		"smtp_password":     str("SMTP password; blank retains the stored value."),
		"sent_copy_enabled": boolean("Copy a sent message into the remote Sent folder."),
		"sent_copy_folder":  str("Optional explicit destination folder for the sent copy."),
	}),
	"RemoteConfigTest": obj(map[string]any{
		"host":          str("IMAP host to test; blank uses the stored binding."),
		"port":          integer("IMAP port."),
		"username":      str("IMAP username."),
		"security":      str("tls, starttls or plain."),
		"smtp_host":     str("Optional SMTP host."),
		"smtp_port":     integer("Optional SMTP port."),
		"smtp_username": str("Optional SMTP username."),
		"smtp_security": str("Optional SMTP security."),
		"namespace":     str("Selected root folder."),
		"imap_password": str("IMAP password; blank uses the stored value."),
	}),
	"RemoteTestResult": obj(map[string]any{
		"ok":             boolean("Whether the connector authenticated and resolved a folder scope."),
		"root":           str("The resolved root folder."),
		"delimiter":      str("The server's hierarchy delimiter."),
		"personal":       boolean("Whether the root is the personal namespace."),
		"inbox_in_scope": boolean("Whether the INBOX is within the selected scope."),
	}, "ok"),
	"RemoteRefreshResult": obj(map[string]any{
		"status":     str("Index completeness after the reconcile: complete, partial, error or never_started."),
		"indexed_at": ts("When the reconcile completed."),
	}, "status"),
	"RemoteRoleMap": obj(map[string]any{
		"create": boolean("Create the conventional folder for the role when none exists."),
		"path":   str("An explicit folder name to create; must conventionally map to the role."),
	}),
	"RemoteRoleResult": obj(map[string]any{
		"role":      str("The special folder role."),
		"mapped":    boolean("Whether a folder now carries the role."),
		"path":      str("The folder path carrying the role."),
		"folder_id": str("The folder id carrying the role."),
		"created":   boolean("Whether the folder was created by this call."),
	}, "role", "mapped"),
	"AuthoringSettings": obj(map[string]any{
		"mode":              str("Effective authoring mode: mailmoose_approval or remote_draft. A domain inbox is always mailmoose_approval."),
		"default_mode":      str("The kind default mode."),
		"notify_address":    str("Effective notification address."),
		"notify_overridden": boolean("Whether an explicit per-inbox notify address is set."),
		"approver_enabled":  boolean("Whether the approver is enabled, based on the effective mode (mailmoose_approval)."),
		"approver_email":    str("Nominated external approval address."),
		"standalone":        boolean("Whether the inbox is a standalone (remote) mailbox."),
	}, "mode", "default_mode"),
	"AuthoringSettingsPut": obj(map[string]any{
		"mode":           str("mailmoose_approval or remote_draft; empty clears to the kind default. remote_draft is a standalone-only mode and is rejected for a domain inbox."),
		"notify_address": str("Notification address; empty clears the override to the connected address."),
	}),
	"Handoff": obj(map[string]any{
		"id":                  str("Handoff record id."),
		"inbox_id":            str("Owning inbox id."),
		"draft_id":            str("The source draft id (the local draft may have been cleaned up after publication)."),
		"mode":                str("Authoring mode snapshotted at request time (mailmoose_approval or remote_draft)."),
		"handoff_id":          str("Stable non-secret correlation id placed in the handoff header, exposed so a client can correlate it."),
		"message_id":          str("RFC5322 Message-ID assigned to the frozen draft."),
		"remote_folder":       str("Remote folder the draft was appended to."),
		"publication":         str("Publication state: pending, published, ambiguous or failed. Independent of notification and the Sent-copy."),
		"notification_status": str("Notification state: none, queued, sent or failed. Independent of publication."),
		"attempts":            integer("Append attempts."),
		"last_error":          str("Bounded, safe failure reason."),
		"requested_at":        ts("When the handoff was requested."),
		"published_at":        ts("When the handoff was confirmed published."),
		"created_at":          ts("Creation time."),
		"updated_at":          ts("Last update time."),
	}, "id", "inbox_id", "draft_id", "mode", "publication"),
	"HandoffList": commonEnvelopeSchema("Handoff", "RemoteDraft handoffs, newest first."),
	"Capabilities": obj(map[string]any{
		"folders":              boolean("Whether the mailbox exposes named folders."),
		"hierarchical_folders": boolean("Whether folder names may nest."),
		"move":                 boolean("Whether messages can be moved between folders."),
		"labels":               boolean("Whether the mailbox has free-text labels independent of folders."),
		"search":               boolean("Whether the server or local index supports search."),
		"drafts":               boolean("Whether the mailbox holds a server-side Drafts concept."),
		"outbound":             boolean("Whether the mailbox can send mail itself."),
		"realtime":             boolean("Whether the mailbox pushes change notifications."),
		"sync":                 boolean("Whether the mailbox is backed by a synchronised remote server."),
	}),

	"Inbox": obj(map[string]any{
		"id":                              str("Inbox id."),
		"account_id":                      str("Owning account id."),
		"domain_id":                       str("Owning domain id."),
		"local_part":                      str("Local part."),
		"address":                         str("Full address."),
		"display_name":                    str("Display name."),
		"enabled":                         boolean("Whether the inbox accepts mail."),
		"allowed_senders":                 stringList("Allowed sender patterns when sender_restricted."),
		"sender_restricted":               boolean("Whether the allowed-senders list is enforced."),
		"require_authenticated":           boolean("Whether MX mail must be authenticated."),
		"approver_email":                  str("Nominated external approval address."),
		"aliases":                         stringList("Alias addresses that deliver to this inbox."),
		"alias_names":                     stringMap("Alias address to display name."),
		"default_sender":                  str("Preselected From address."),
		"auto_mark_read_on_delivery":      boolean("Whether a connector delivery marks the message read."),
		"auto_trash_after_delivery_hours": integer("Hours after connector delivery at which a message is trashed; absent when disabled."),
		"delivery_trigger":                str("Delivery trigger for the auto-actions: any or all. Defaults to all."),
		"created_at":                      ts("Creation time."),
	}),
	"InboxList": arrayOf("Inbox", "Accessible inboxes."),
	"InboxCreate": obj(map[string]any{
		"kind":         str("Inbox kind: domain (default) or standalone."),
		"domain_id":    str("Owning domain id (domain kind)."),
		"local_part":   str("Local part (domain kind)."),
		"localpart":    str("openagent.email compatibility alias for local_part."),
		"display_name": str("Display name."),
		"address":      str("Full self-contained address (standalone kind)."),
		"namespace":    str("Selected remote root folder to sync (standalone kind); defaults to INBOX."),
		"remote": obj(map[string]any{
			"host":          str("IMAP host."),
			"port":          integer("IMAP port."),
			"username":      str("IMAP username."),
			"security":      str("tls (default), starttls or plain."),
			"smtp_host":     str("Optional SMTP host."),
			"smtp_port":     integer("Optional SMTP port."),
			"smtp_username": str("Optional SMTP username."),
			"smtp_security": str("Optional SMTP security."),
		}),
	}),
	"InboxPatch": obj(map[string]any{
		"display_name":                    str("Display name."),
		"enabled":                         boolean("Whether the inbox accepts mail."),
		"allowed_senders":                 stringList("Allowed sender patterns when sender_restricted."),
		"sender_restricted":               boolean("Enforce the allowed-senders list."),
		"require_authenticated":           boolean("Require authenticated MX mail."),
		"approver_email":                  str("Nominated external approval address; empty clears."),
		"aliases":                         stringList("Replace the inbox alias set."),
		"alias_names":                     stringMap("Alias address to display name."),
		"default_sender":                  str("Preselected From address; empty clears to the primary."),
		"auto_mark_read_on_delivery":      boolean("Mark a message read once a connector delivers it."),
		"auto_trash_after_delivery_hours": integer("Hours after connector delivery to move a message to Trash; null disables, absent leaves unchanged."),
		"delivery_trigger":                str("When auto-actions fire: any (first delivery) or all (every connector present at receipt, the default)."),
	}),
	"MailboxPermissions": obj(map[string]any{
		"role":               str("Effective role for this inbox: read, assistant, owner or admin."),
		"can_read":           boolean("Whether the credential can read messages and threads."),
		"can_draft":          boolean("Whether the credential can create and edit drafts."),
		"can_send":           boolean("Whether the credential can send and reply."),
		"can_approve":        boolean("Whether the credential can approve a pending draft send."),
		"send_any_in_domain": boolean("Whether the credential may set `sender` to any address on the inbox's domain, not only its primary or managed aliases."),
		"send_any_domain":    str("The domain `send_any_in_domain` applies to. Present only when it is true."),
	}, "role", "can_read", "can_draft", "can_send", "can_approve", "send_any_in_domain"),

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
		"to":          stringOrArray("Explicit reply recipients. Required when direction is unknown; otherwise defaults to the sender for inbound mail or the original To recipients for outbound mail."),
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
		"id":                  str("Draft id."),
		"inbox_id":            str("Owning inbox id."),
		"reply_to_message_id": str("Message being replied to."),
		"from_address":        str("Chosen From address."),
		"from_name":           str("Resolved From display name."),
		"to":                  stringList("To recipients."),
		"cc":                  stringList("Cc recipients."),
		"bcc":                 stringList("Bcc recipients."),
		"subject":             str("Subject line."),
		"text":                str("Plain-text body."),
		"html":                str("HTML body."),
		"status":              str("draft, pending_approval or rejected."),
		"send_request":        Ref("SendRequest"),
		"handoff":             Ref("Handoff"),
		"attachments":         arrayOf("DraftAttachment", "Draft attachments."),
		"created_at":          ts("Creation time."),
		"updated_at":          ts("Last update time."),
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
		"id":          str("Key id."),
		"name":        str("Key name."),
		"prefix":      str("Display prefix."),
		"admin":       boolean("Whether the key is account admin."),
		"mailboxes":   stringMap("Inbox id to role."),
		"domain_send": stringMap("Inbox id (a domain's catch-all) to whether the key holds the live domain-wide sending grant there."),
		"created_at":  ts("Creation time."),
	}),
	"APIKeyList": arrayOf("APIKey", "API keys, never including the secret."),
	"KeyCreateBody": obj(map[string]any{
		"name":        str("Key name."),
		"admin":       boolean("Grant account admin."),
		"mailboxes":   stringMap("Inbox id to role."),
		"domain_send": stringList("Inbox ids (a domain's catch-all) that hold the domain-wide sending grant, so the key may send as any address on the domain. Setting the grant forces Owner on that binding."),
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

	"AccountMXSettings": obj(map[string]any{
		"url":            str("Remote MX receiver origin (HTTP or HTTPS)."),
		"bearer_key":     str("Always empty on a read; the credential is never returned."),
		"key_configured": boolean("Whether a bearer credential is stored."),
		"ca":             str("PEM CA bundle for a private receiver; empty uses system roots."),
		"allow_private":  boolean("Whether the dialer may reach a loopback/LAN receiver. Only honoured when the operator allows private outbound (ALLOW_PRIVATE_OUTBOUND=true, the default)."),
		"revision":       integer("Optimistic-concurrency token a save must echo."),
		"status":         Ref("AccountMXStatus"),
	}),
	"AccountMXSettingsPut": obj(map[string]any{
		"url":           str("Required. HTTP or HTTPS origin with no path, userinfo, query or fragment. A public receiver must use https and a public-routable host; set allow_private for a loopback/LAN receiver."),
		"bearer_key":    str("Required on a first save; a blank value retains the stored credential. Must match the receiver's DIALMX_CORE_KEY."),
		"ca":            str("Optional PEM CA bundle for a private receiver."),
		"allow_private": boolean("Permit the dialer to reach a loopback/LAN receiver. Only honoured when the operator allows private outbound (ALLOW_PRIVATE_OUTBOUND=true, the default)."),
		"revision":      integer("Current revision, or zero to create."),
	}, "url"),
	"AccountMXStatus": obj(map[string]any{
		"configured": boolean("Whether a receiver is configured for the account."),
		"revision":   integer("Persisted revision the live state reflects."),
		"state":      str("disabled, connecting or active. active means the receiver completed its live session handshake; connecting means a change or session is still in progress, so readiness is never claimed prematurely."),
		"detail":     str("Human-readable failure or availability detail."),
	}),

	"DeliveryAttempt": obj(map[string]any{
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
		"source":              str("Sending credential label at attempt time."),
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
		"source":              str("Where the row came from: the sending credential for outbound mail, or the receiving source (for example an Antler receiver) for inbound mail."),
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
		"connector_url": str("Relay connector URL."),
		"env":           str("Ready-to-paste environment block."),
	}),
	"OpenClawEnrollment": obj(map[string]any{
		"gateway_id":    str("Minted gateway id."),
		"secret":        str("Relay secret, returned once."),
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
