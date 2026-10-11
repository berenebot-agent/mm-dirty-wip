package model

import (
	"errors"
	"time"
)

// Inbox kinds. A domain inbox is a first-class mailbox attached to a managed
// domain of the account; it participates in the domain-centric inbound routing,
// alias and sending model exactly as before. A standalone inbox is not attached
// to any managed domain: it owns its own address, receives and sends through a
// per-inbox remote connector (IMAP/SMTP), and is otherwise a first-class mailbox
// with folders, labels, threads, events and API keys.
//
// The kind is recorded once at creation and is immutable; the store rejects any
// attempt to change it.
const (
	InboxKindDomain     = "domain"
	InboxKindStandalone = "standalone"
)

// Inbox namespace/root. A standalone inbox syncs exactly one selected root: an
// explicit IMAP folder path (its root for folder listing and every remote
// operation). The root is a literal path, not a login-wide mode; the operator
// chooses it during setup and it is the scope of everything the inbox sees. The
// default personal root is the INBOX.
const (
	// NamespaceINBOX is the default personal root: the INBOX folder and its
	// children only.
	NamespaceINBOX = "INBOX"
	// NamespaceDefault is the root applied when the operator supplies none.
	NamespaceDefault = NamespaceINBOX
)

// Folder roles. A folder the sync surface treats specially carries a role; every
// other folder is RoleFolder. Roles are advisory labels over a custom hierarchy:
// the remote folder name and hierarchy are always preserved, a role only tells
// the UI and workflow which well-known mailbox a folder represents. Labels are
// DISTINCT from folders: a label is free-text metadata on a message and is never
// a folder or a folder path.
const (
	FolderRoleFolder = "folder"
	FolderRoleInbox  = "inbox"
	FolderRoleSent   = "sent"
	FolderRoleDrafts = "drafts"
	FolderRoleTrash  = "trash"
	FolderRoleSpam   = "spam"
	// FolderRoleArchive is a well-known archive folder (for example Gmail's
	// "All Mail" or an IMAP "Archive"). It is a folder, kept distinct from the
	// free-text label namespace.
	FolderRoleArchive = "archive"
	// FolderRoleOutbox is a well-known outbound queue folder (for example an
	// IMAP "Outbox"), distinct from Sent.
	FolderRoleOutbox = "outbox"
	// FolderRoleLabel is reserved for a folder that a remote server actually
	// exposes as a label-like folder; it is still a folder, not a free-text
	// label. Where a provider has a real label store it maps to message labels,
	// never to a Folder.
	FolderRoleLabel = "label"
)

// Folder is a mailbox folder in a common, provider-neutral shape. A folder is an
// explicit member of the inbox's single selected root (see the Namespace*
// constants): the root and its descendants — and nothing outside it — are the
// mailbox's folders. Name is the display name of the final path segment; Path is
// the full hierarchical name (for example "Archive/2026") relative to the root
// and is the stable opaque locator. ParentPath is empty for a top-level folder.
// Folders are never aliases or labels: managed aliases belong to inboxes and
// labels are message metadata, both of which stay independent of this tree.
type Folder struct {
	ID           string `json:"id"`
	AccountID    string `json:"-"`
	InboxID      string `json:"inbox_id"`
	Path         string `json:"path"`
	Name         string `json:"name"`
	ParentPath   string `json:"parent_path,omitempty"`
	Role         string `json:"role"`
	MessageCount int64  `json:"message_count,omitempty"`
	UnreadCount  int64  `json:"unread_count,omitempty"`
	// Selectable reports whether the folder may be chosen as a sync target or
	// move destination. Non-selectable folders (for example an IMAP \Noselect
	// hierarchy node) are still listed so the hierarchy is complete.
	Selectable bool      `json:"selectable"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	// IsSystem marks a seeded system-role folder (Inbox, Sent, Drafts, Trash,
	// Spam, Outbox, Archive) whose role is protected: it cannot be arbitrarily
	// renamed or deleted, and its role is not inferred from a user-chosen name.
	// Custom folders created by an assistant have IsSystem=false.
	IsSystem bool `json:"is_system,omitempty"`
	// RoleLocked marks an explicit operator role mapping (SetFolderRole) that a
	// remote reconcile must never overwrite by re-inferring the role from the
	// folder's name. It lets an arbitrarily-named existing remote folder be
	// permanently mapped to a role (for example an "Old Mail" folder mapped to
	// Trash) without a later sync resetting it. It is false for a name-inferred or
	// seeded system role.
	RoleLocked bool `json:"role_locked,omitempty"`
	// Origin is where the folder came from: "local" for a seeded system or
	// operator-created folder owned by the account, or "remote" for a folder
	// mirrored from the connected provider. It distinguishes a real remote
	// mapping from a local folder that merely carries a role.
	Origin string `json:"origin,omitempty"`
}

// RemoteLocator names the remote position of a message or folder. It is the
// common address a local/remote operation routes on: a local store operation has
// an empty RemoteLocator, a remote (IMAP) operation carries the UIDVALIDITY and
// UID that identify a message within a folder regardless of how the folder was
// named locally or how the message was renamed remotely.
//
// UIDValidity scopes UID: some servers reassign UIDs when UIDVALIDITY changes, so
// a locator is only meaningful while its UIDValidity still matches the folder.
type RemoteLocator struct {
	FolderPath  string `json:"folder_path,omitempty"`
	UIDValidity uint32 `json:"uid_validity,omitempty"`
	UID         uint32 `json:"uid,omitempty"`
	// MessageID is the RFC5322 Message-ID when one is known. It is the fallback
	// locator when UIDValidity has changed and the UID is no longer trustworthy.
	MessageID string `json:"message_id,omitempty"`
}

// Empty reports whether the locator names no remote position (a local record).
func (l RemoteLocator) Empty() bool { return l.FolderPath == "" && l.UID == 0 && l.UIDValidity == 0 }

// RemoteConnection carries the operator-supplied, non-secret description of a
// remote mail server binding. Secrets are never held here; they live encrypted
// in the store keyed by InboxID.
type RemoteConnection struct {
	// Host is the IMAP server hostname; Port its port.
	Host string `json:"host,omitempty"`
	Port int    `json:"port,omitempty"`
	// Username is the login identity on the remote server (usually the full
	// address for a hosted provider).
	Username string `json:"username,omitempty"`
	// Security is the transport security mode: "tls" (implicit TLS, the default),
	// "starttls", or "plain" (no transport security). Plaintext is an explicit,
	// per-inbox operator choice, not an automatic downgrade: the runtime policy
	// layer decides whether a given deployment permits it (see
	// RemoteSecurityPlain). A connection never silently falls back to less
	// security than the configured mode.
	Security string `json:"security,omitempty"`
	// SMTP, when set, is an optional outbound server bound to the same inbox.
	SMTP *RemoteSMTP `json:"smtp,omitempty"`
}

// RemoteSMTP describes the optional outbound server of a standalone inbox.
type RemoteSMTP struct {
	Host     string `json:"host,omitempty"`
	Port     int    `json:"port,omitempty"`
	Username string `json:"username,omitempty"`
	Security string `json:"security,omitempty"`
}

// Remote transport security modes. TLS (implicit) is the default and STARTTLS
// is offered for compatibility. Plaintext is an explicit mode an operator may
// select for a self-hosted deployment; it is never chosen automatically and a
// connector never falls back from a stronger to a weaker mode at runtime. A
// deployment policy (checked by the runtime layer, not by this model) may refuse
// plaintext outright.
const (
	RemoteSecurityTLS          = "tls"
	RemoteSecurityStartTLS     = "starttls"
	RemoteSecurityPlain        = "plain"
	RemoteSecurityDefault      = RemoteSecurityTLS
	RemoteDefaultIMAPPort      = 993
	RemoteDefaultSMTPPort      = 465
	RemoteDefaultIMAPStartPort = 143
	RemoteDefaultSMTPStartPort = 587
	// RemoteDefaultIMAPPlainPort/RemoteDefaultSMTPPlainPort are the conventional
	// cleartext ports. They are used only when the operator selects plain.
	RemoteDefaultIMAPPlainPort = 143
	RemoteDefaultSMTPPlainPort = 25
)

// Remote sync cadence defaults and bounds for a standalone inbox. The quick
// interval governs the fallback poll when IDLE is unavailable; the full interval
// governs the deep index reconcile. Both are clamped to their minimum so a
// misconfigured value cannot hammer the remote server. IDLE-capable servers
// still deliver new mail instantly regardless of the quick interval.
const (
	RemotePollSecondsDefault     = 60
	RemotePollSecondsMin         = 15
	RemoteFullSyncMinutesDefault = 15
	RemoteFullSyncMinutesMin     = 1
)

// NormalizeRemotePollSeconds resolves a per-inbox quick-poll override to an
// effective value, applying the default and the minimum bound. A nil or
// non-positive value uses the default.
func NormalizeRemotePollSeconds(v *int) int {
	if v == nil || *v <= 0 {
		return RemotePollSecondsDefault
	}
	if *v < RemotePollSecondsMin {
		return RemotePollSecondsMin
	}
	return *v
}

// NormalizeRemoteFullSyncMinutes resolves a per-inbox full-sync override to an
// effective value, applying the default and the minimum bound.
func NormalizeRemoteFullSyncMinutes(v *int) int {
	if v == nil || *v <= 0 {
		return RemoteFullSyncMinutesDefault
	}
	if *v < RemoteFullSyncMinutesMin {
		return RemoteFullSyncMinutesMin
	}
	return *v
}

// Capabilities describes what a mailbox can do, so workflow and UI code can
// branch on a declared surface rather than on the inbox kind. A domain inbox and
// a standalone inbox differ in which capabilities are true.
type Capabilities struct {
	// Folders reports whether the mailbox exposes named folders.
	Folders bool `json:"folders"`
	// HierarchicalFolders reports whether folder names may carry a hierarchy
	// separator and nest.
	HierarchicalFolders bool `json:"hierarchical_folders"`
	// Move reports whether messages can be moved between folders.
	Move bool `json:"move"`
	// Labels reports whether the mailbox has free-text labels independent of
	// folders.
	Labels bool `json:"labels"`
	// Search reports whether the server (or local index) supports search.
	Search bool `json:"search"`
	// Drafts reports whether the mailbox holds a server-side Drafts concept the
	// assistant can hand off to.
	Drafts bool `json:"drafts"`
	// Outbound reports whether the mailbox can send mail itself (a configured
	// connector).
	Outbound bool `json:"outbound"`
	// Realtime reports whether the mailbox pushes change notifications.
	Realtime bool `json:"realtime"`
	// Sync reports whether the mailbox is backed by a remote server that is
	// synchronised (standalone only).
	Sync bool `json:"sync"`
}

// DomainCapabilities is the declared capability set of a domain inbox.
func DomainCapabilities() Capabilities {
	return Capabilities{Folders: true, Move: true, Labels: true, Search: true, Drafts: true, Outbound: true, Realtime: true}
}

// StandaloneCapabilities is the declared capability set of a standalone inbox
// backed by a remote IMAP account. The exact booleans a given server reports are
// refined per connection at handshake time; this is the default surface.
func StandaloneCapabilities() Capabilities {
	return Capabilities{Folders: true, HierarchicalFolders: true, Move: true, Labels: true, Search: true, Drafts: true, Outbound: true, Realtime: true, Sync: true}
}

// ListCompleteness reports whether a listing's result/index is complete, which
// is independent of pagination. A paginated listing that still has another page
// (NextCursor set) is not thereby "incomplete": completeness describes whether
// the source could enumerate the whole result set at all, for example a local
// index that is fully built versus a remote server that returned only a partial
// or best-effort view.
type ListCompleteness string

const (
	// CompletenessComplete means the source enumerated the entire result set; the
	// consumer may trust that the union of all pages is everything available.
	CompletenessComplete ListCompleteness = "complete"
	// CompletenessPartial means the source could only enumerate part of the result
	// set (for example a timed-out or best-effort remote search). More may exist
	// beyond the enumerated set even once every page has been fetched.
	CompletenessPartial ListCompleteness = "partial"
	// CompletenessUnknown means the source could not report its extent.
	CompletenessUnknown ListCompleteness = "unknown"
)

// ListEnvelope is the common shape of every paginated mailbox listing. Items hold
// the page's payload; NextCursor is an opaque token to request the following page
// and is empty when this page is the last one. Completeness describes the result
// set as a whole and is orthogonal to NextCursor. Errors carries per-inbox
// failures when a listing spans more than one inbox, so one failing mailbox does
// not fail the whole request.
type ListEnvelope[T any] struct {
	Items        []T              `json:"items"`
	NextCursor   string           `json:"next_cursor,omitempty"`
	Completeness ListCompleteness `json:"completeness"`
	Errors       []InboxFailure   `json:"errors,omitempty"`
}

// InboxFailure identifies a per-inbox failure inside an otherwise successful
// listing. Code is a stable, transport-independent classification (the
// MailboxError ErrKind* vocabulary) and Message is a short, safe description;
// raw provider or store error text is never placed here, so a listing response
// cannot leak credentials, hostnames or internal paths.
type InboxFailure struct {
	InboxID   string `json:"inbox_id"`
	Code      string `json:"code"`
	Message   string `json:"message,omitempty"`
	Retryable bool   `json:"retryable,omitempty"`
}

// NewInboxFailure builds a per-inbox failure from a normalized mailbox error,
// projecting only the safe fields and never the wrapped cause.
func NewInboxFailure(inboxID string, err error) InboxFailure {
	f := InboxFailure{InboxID: inboxID}
	var mb *MailboxError
	if errors.As(err, &mb) {
		f.Code, f.Message, f.Retryable = mb.Kind, mb.Message, mb.Retryable
		return f
	}
	f.Code = ErrKindInternal
	return f
}

// MailboxError is a normalized mailbox failure. Kind is a stable, transport-
// independent classification (see the Err* constants) and Message is safe to
// surface to a human. Providers and the local store map their native errors onto
// this type so workflow code never inspects a provider error directly.
type MailboxError struct {
	Kind    string `json:"kind"`
	Message string `json:"message,omitempty"`
	// Retryable reports whether retrying the same operation may succeed without
	// operator intervention.
	Retryable bool `json:"retryable,omitempty"`
	// Cause is the wrapped underlying error and is never serialized.
	Cause error `json:"-"`
}

func (e *MailboxError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return e.Kind
}

func (e *MailboxError) Unwrap() error { return e.Cause }

// MailboxError kinds. They are the vocabulary workflow and UI code branches on.
const (
	ErrKindNotFound    = "not_found"
	ErrKindForbidden   = "forbidden"
	ErrKindConflict    = "conflict"
	ErrKindInvalid     = "invalid"
	ErrKindUnavailable = "unavailable"
	ErrKindAuth        = "auth"
	ErrKindQuota       = "quota"
	ErrKindUnsupported = "unsupported"
	ErrKindRetryable   = "retryable"
	ErrKindInternal    = "internal"
)

// NewMailboxError builds a normalized mailbox error.
func NewMailboxError(kind, message string, retryable bool, cause error) *MailboxError {
	return &MailboxError{Kind: kind, Message: message, Retryable: retryable, Cause: cause}
}
