package model

import "time"

// Per-inbox authoring modes. A standalone inbox defaults to RemoteDraft: requests
// to send are handed off one-way to the connected remote server's Drafts folder
// and are never sent by MailMoose. A domain inbox uses MailMooseApproval: the
// in-product approval workflow, with UI/API decisions and tokenized email
// approval. RemoteDraft is a standalone-inbox concept — it requires a connected
// remote Drafts folder — so it is preset (not selectable) for a domain inbox and
// rejected when stored on one.
//
// The mode is snapshotted onto each request at creation, so flipping the inbox
// setting never changes an in-flight request.
const (
	// AuthoringMailMooseApproval is the MailMoose local approval workflow.
	AuthoringMailMooseApproval = "mailmoose_approval"
	// AuthoringRemoteDraft is the one-way handoff to the connected remote Drafts.
	AuthoringRemoteDraft = "remote_draft"
)

// DefaultAuthoringMode returns the authoring mode an inbox kind defaults to:
// a standalone inbox hands off to its connected remote Drafts, a domain inbox
// uses the MailMoose approval workflow.
func DefaultAuthoringMode(kind string) string {
	if kind == InboxKindStandalone {
		return AuthoringRemoteDraft
	}
	return AuthoringMailMooseApproval
}

// ValidAuthoringMode reports whether a string is a known authoring mode.
func ValidAuthoringMode(mode string) bool {
	return mode == AuthoringMailMooseApproval || mode == AuthoringRemoteDraft
}

// AuthoringModeAllowedForKind reports whether a mode may be stored on an inbox
// of the given kind. RemoteDraft is meaningful only for a standalone inbox (a
// domain inbox has no connected remote Drafts folder), so a domain inbox is
// preset to MailMooseApproval rather than offering the selector.
func AuthoringModeAllowedForKind(kind, mode string) bool {
	if kind != InboxKindStandalone && mode == AuthoringRemoteDraft {
		return false
	}
	return ValidAuthoringMode(mode)
}

// NormalizeAuthoringMode returns the effective authoring mode for an inbox,
// defaulting from the inbox kind when the stored value is empty, unknown, or
// not allowed for the kind (a legacy domain row stored as remote_draft reads as
// the kind default), so the kind invariant holds on read as well as on write.
func NormalizeAuthoringMode(kind, stored string) string {
	if AuthoringModeAllowedForKind(kind, stored) {
		return stored
	}
	return DefaultAuthoringMode(kind)
}

// HandoffHeader is the RFC5322 header that carries the stable correlation id of a
// RemoteDraft handoff. It is set on the appended draft and searched back on the
// remote server to confirm an append whose outcome is uncertain. It is never an
// approval token.
const HandoffHeader = "X-MailMoose-Handoff-ID"

// Remote handoff publication states. They record whether the frozen draft was
// actually appended to the remote Drafts folder. Because a provider may not
// report an APPENDUID, a successful append whose result cannot be verified is
// Pending/Ambiguous, never claimed as guaranteed exactly-once.
const (
	// HandoffPending: the request is queued for publication, not yet attempted.
	HandoffPending = "pending"
	// HandoffPublished: the append was confirmed (either the server reported an
	// APPENDUID, or a lookup by handoff id/message-id found the appended draft).
	HandoffPublished = "published"
	// HandoffAmbiguous: the append outcome could not be determined (no APPENDUID
	// and the follow-up lookup was itself inconclusive). The draft may or may not
	// exist remotely; the state is explicit so no automatic re-append occurs.
	HandoffAmbiguous = "ambiguous"
	// HandoffFailed: the append failed terminally (a permanent provider error).
	HandoffFailed = "failed"
)

// Notification job kinds for the outbound_workflow queue. A RemoteDraft handoff
// enqueues a WorkflowKindHandoff job so the human can be told a draft was placed
// in their remote Drafts; it carries no token and is not the approval email. A
// MailMooseApproval request enqueues WorkflowKindApprovalRequest as before. The
// persisted workflow row records only the job kind (plus its non-secret request
// link); no token is ever stored on the handoff notification.
const (
	// WorkflowKindHandoff is the workflow job kind for a RemoteDraft handoff
	// notification. It carries no approval token.
	WorkflowKindHandoff = "draft_handoff"
)

// Durable RemoteDraft handoff event types. They mirror the draft workflow events
// but are scoped to the handoff record: a handoff is a one-way publication, not
// an approval, so its events never imply a send decision.
const (
	// EventDraftHandoffRequested: an assistant requested a RemoteDraft handoff.
	EventDraftHandoffRequested = "draft.handoff_requested"
	// EventDraftHandoffPublished: the frozen draft was appended to the remote
	// Drafts folder (confirmed).
	EventDraftHandoffPublished = "draft.handoff_published"
	// EventDraftHandoffAmbiguous: the append outcome could not be verified; the
	// draft may or may not exist remotely. No automatic re-append follows.
	EventDraftHandoffAmbiguous = "draft.handoff_ambiguous"
	// EventDraftHandoffFailed: the handoff failed terminally.
	EventDraftHandoffFailed = "draft.handoff_failed"
	// EventDraftHandoffCancelled: an outstanding handoff was withdrawn before
	// publication.
	EventDraftHandoffCancelled = "draft.handoff_cancelled"
	// EventDraftHandoffNotificationSent: the handoff notification was handed to
	// the outbound path.
	EventDraftHandoffNotificationSent = "draft.handoff_notification_sent"
	// EventDraftHandoffNotificationFailed: the handoff notification failed
	// terminally. Publication is unaffected.
	EventDraftHandoffNotificationFailed = "draft.handoff_notification_failed"
)

// AssistantHandlingRequest is the durable record of a RemoteDraft handoff. It is
// deliberately separate from DraftSendRequest so publication state, remote
// correlation and notification state are tracked independently of the approval
// request model, and so the existing approval request row is never widened.
//
// A handling record is immutable in its frozen facts (mode, content hash, inbox,
// draft, handoff id, message id, remote folder): only the publication and
// notification state machines advance.
type AssistantHandlingRequest struct {
	ID      string `json:"id"`
	InboxID string `json:"inbox_id"`
	DraftID string `json:"draft_id"`
	// Mode is the authoring mode snapshotted at request time. It is one of the
	// Authoring* constants; a later inbox setting change never affects this row.
	Mode string `json:"mode"`
	// ContentHash is the fingerprint of the exact frozen draft and attachments
	// that were handed off.
	ContentHash string `json:"-"`
	// HandoffID is the stable correlation id placed in the handoff header. It is
	// opaque and non-secret and is exposed so a client can correlate the draft.
	HandoffID string `json:"handoff_id,omitempty"`
	// MessageID is the RFC5322 Message-ID assigned to the frozen draft. It is the
	// fallback remote locator when UIDVALIDITY changes.
	MessageID string `json:"message_id,omitempty"`
	// RemoteFolder is the resolved remote folder the draft was appended to (the
	// connected server's Drafts folder).
	RemoteFolder string `json:"remote_folder,omitempty"`
	// RawPath is the data-dir-relative path of the frozen raw MIME that was
	// appended. It is immutable and is not serialized.
	RawPath string `json:"-"`
	// SizeBytes is the frozen raw MIME size.
	SizeBytes int64 `json:"-"`
	// Publication is the handoff publication state (Handoff* constants).
	Publication string `json:"publication"`
	// RemoteUID is the appended message's UID when the server reported one, else
	// zero.
	RemoteUID uint32 `json:"remote_uid,omitempty"`
	// NotificationStatus reports whether the handoff notification was queued,
	// handed to the outbound path, or failed. It advances independently of
	// publication: a handoff whose notification is unavailable is still
	// published.
	NotificationStatus string `json:"notification_status,omitempty"`
	// NotificationMessageID is the RFC5322 Message-ID of the generated handoff
	// notification email (distinct from the frozen draft's MessageID). It is
	// recorded so a notification that arrives back in the connected inbox is
	// excluded from remote detection by a durable lookup rather than a live body
	// fetch.
	NotificationMessageID string     `json:"-"`
	Attempts              int        `json:"attempts,omitempty"`
	LastError             string     `json:"last_error,omitempty"`
	RequestedAt           time.Time  `json:"requested_at"`
	PublishedAt           *time.Time `json:"published_at,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
}
