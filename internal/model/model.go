package model

import (
	"encoding/json"
	"fmt"
	"net/mail"
	"strings"
	"time"
)

type Account struct {
	ID                string    `json:"id"`
	Name              string    `json:"name"`
	StorageQuotaBytes int64     `json:"storage_quota_bytes"`
	StorageUsedBytes  int64     `json:"storage_used_bytes"`
	CreatedAt         time.Time `json:"created_at"`
	// Timezone is the account's default display time zone (IANA name). Empty
	// means UTC. It affects only how the human web UI renders timestamps;
	// stored and API timestamps are always UTC.
	Timezone string `json:"timezone,omitempty"`
}

type User struct {
	ID        string `json:"id"`
	AccountID string `json:"account_id"`
	Email     string `json:"email"`
	// IsAdmin is the account-level Admin role: full access to that account's
	// domains, clients and mailboxes.
	IsAdmin bool `json:"is_admin"`
	// SystemAdmin is the installation-level role: provision accounts and
	// invitations. It does not by itself grant access to other accounts' mail.
	SystemAdmin bool `json:"system_admin"`
	// Roles is the user's per-inbox access when they are not an account Admin,
	// keyed by inbox id. It is populated on listings and is empty for Admins.
	Roles     map[string]string `json:"mailboxes,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	// Timezone is the user's display time zone override (IANA name). Empty
	// means "inherit the account default". UI-only; never exposed over the API.
	Timezone string `json:"-"`
	// PasswordEnabled reports whether password authentication is available for
	// this user. It is false for passkey-only users; passkeys remain a valid
	// login method regardless.
	PasswordEnabled bool `json:"-"`
}

// WebAuthnCredential is one registered passkey. CredentialID and PublicKey are
// the WebAuthn credential handle and public key exactly as produced by the
// authenticator; the raw credential handle is the browser-side lookup key and
// is unique across the installation.
type WebAuthnCredential struct {
	ID           string    `json:"id"`
	UserID       string    `json:"-"`
	CredentialID []byte    `json:"-"`
	PublicKey    []byte    `json:"-"`
	SignCount    uint32    `json:"-"`
	Transports   []string  `json:"-"`
	Name         string    `json:"name"`
	CreatedAt    time.Time `json:"created_at"`
	LastUsedAt   time.Time `json:"last_used_at,omitempty"`
	// BackupEligible reports whether the credential can be synced between
	// devices. It is fixed at registration and must be reconstructed on the
	// credential before the library verifies an assertion, which hard-fails on
	// a mismatch against the authenticator's flag.
	BackupEligible bool `json:"-"`
	// BackupState reports whether the credential is currently backed up. It
	// can change between ceremonies.
	BackupState bool `json:"-"`
	// AAGUID and AttestationType are stored for diagnostics/display only.
	AAGUID          string `json:"-"`
	AttestationType string `json:"-"`
}

// Invite kinds. An account_admin invite provisions a new, separate account
// with its own Admin; an operator invite grants Owner access to selected
// mailboxes of an existing account.
const (
	InviteKindAccountAdmin = "account_admin"
	InviteKindOperator     = "operator"
)

// Invite is a pending, expiring, single-use account or operator setup link.
// The plaintext setup token is never stored; only its hash is kept.
type Invite struct {
	ID          string     `json:"id"`
	AccountID   string     `json:"account_id"`
	AccountName string     `json:"account_name,omitempty"`
	Email       string     `json:"email"`
	Kind        string     `json:"kind"`
	InboxIDs    []string   `json:"inbox_ids,omitempty"`
	ExpiresAt   time.Time  `json:"expires_at"`
	AcceptedAt  *time.Time `json:"accepted_at,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// Pending reports whether the invite can still be redeemed.
func (i Invite) Pending(now time.Time) bool {
	return i.AcceptedAt == nil && i.RevokedAt == nil && now.Before(i.ExpiresAt)
}

type Domain struct {
	ID                string `json:"id"`
	AccountID         string `json:"account_id"`
	Name              string `json:"name"`
	CatchAllInboxID   string `json:"catch_all_inbox_id,omitempty"`
	SendingProvider   string `json:"sending_provider"`
	ReceivingProvider string `json:"receiving_provider"`
	// ParentDomainID is set when this domain is a subdomain of another domain
	// in the same account. Its own configuration is optional; when the matching
	// Inherit* flag is set and no own configuration exists, the effective
	// configuration is inherited from the nearest ancestor that has one.
	ParentDomainID   string `json:"parent_domain_id,omitempty"`
	ParentDomain     string `json:"parent_domain,omitempty"`
	InheritReceiving bool   `json:"inherit_receiving"`
	InheritSending   bool   `json:"inherit_sending"`
	// ReceivingInheritedFrom/SendingInheritedFrom name the ancestor a provider
	// was inherited from (empty when the provider is the domain's own). They are
	// derived for display; the configuration itself is resolved at read time.
	ReceivingInheritedFrom string    `json:"receiving_inherited_from,omitempty"`
	SendingInheritedFrom   string    `json:"sending_inherited_from,omitempty"`
	CreatedAt              time.Time `json:"created_at"`
}

// IsSubdomain reports whether the domain is a child of (a subdomain of) another
// configured domain.
func (d Domain) IsSubdomain() bool { return d.ParentDomainID != "" }

type Inbox struct {
	ID             string   `json:"id"`
	AccountID      string   `json:"account_id"`
	DomainID       string   `json:"domain_id"`
	LocalPart      string   `json:"local_part"`
	Address        string   `json:"address"`
	DisplayName    string   `json:"display_name"`
	Enabled        bool     `json:"enabled"`
	AllowedSenders []string `json:"allowed_senders,omitempty"`
	// ExternalAliases is the inbox's send-only external aliases, populated on
	// retrieval. It is exposed to Admin readers only and never carries secrets.
	ExternalAliases []ExternalAlias `json:"external_aliases,omitempty"`
	// SenderRestricted enables the allow-list. When false, any sender is
	// accepted and AllowedSenders is ignored; when true, only AllowedSenders
	// (and the approver) are accepted. The allow-list matches the RFC5322.From
	// address, which is spoofable; it is a filter, not authenticated trust.
	SenderRestricted bool `json:"sender_restricted,omitempty"`
	// RequireAuthenticated, when set, additionally requires that MX-delivered
	// mail be authenticated: the From domain must pass DMARC, or have an
	// aligned SPF or DKIM pass, as evaluated by the authenticated MX edge. It
	// has no effect on webhook providers, which carry no auth evidence, and is
	// only meaningful alongside SenderRestricted.
	RequireAuthenticated bool `json:"require_authenticated,omitempty"`
	// ApproverEmail optionally nominates a person who may authorize draft
	// sends by email. When set, the approver address is always accepted as an
	// inbound sender for this inbox regardless of AllowedSenders.
	ApproverEmail string `json:"approver_email,omitempty"`
	// Aliases are alternate addresses (full addresses, possibly on a different
	// domain of the same account) that deliver to this inbox and may also be
	// chosen as the From address when sending. They are address-to-inbox
	// mappings, not mailboxes.
	Aliases []string `json:"aliases,omitempty"`
	// AliasNames maps an alias address to its optional sender display name. An
	// address absent from the map (or with an empty name) falls back to the
	// inbox DisplayName when sending.
	AliasNames map[string]string `json:"alias_names,omitempty"`
	// DefaultSender is the full address compose/reply preselects as From. It is
	// the primary Address or one of Aliases; empty means the primary.
	DefaultSender string `json:"default_sender,omitempty"`
	// TrashRetentionDays overrides the account's Trash auto-purge window for
	// this inbox. Nil (absent) inherits the account setting; 0 keeps this
	// inbox's trashed mail until purged by hand even if the account auto-purges.
	TrashRetentionDays *int `json:"trash_retention_days,omitempty"`
	// StorageQuotaBytes caps this inbox's stored bytes, layered on the
	// account quota. Nil (absent) means the inbox has no cap of its own (the
	// account cap still applies); 0 means explicitly unlimited; a positive
	// value is the cap in bytes.
	StorageQuotaBytes *int64 `json:"storage_quota_bytes,omitempty"`
	// StorageUsedBytes is this inbox's maintained storage usage in bytes. It
	// is 0 for a new inbox and, for a pre-migration inbox, is computed on the
	// first read or write that needs it.
	StorageUsedBytes int64 `json:"storage_used_bytes"`
	// AutoMarkReadOnDelivery, when set, marks a message read once a connector
	// bound to this inbox has successfully delivered it. It is an agent/relay
	// convenience and defaults off; API keys never trigger it.
	AutoMarkReadOnDelivery bool `json:"auto_mark_read_on_delivery,omitempty"`
	// AutoTrashAfterDeliveryHours, when non-nil, moves a delivered message to
	// Trash that many hours after the delivery instant. Nil disables it. It
	// applies only to agent/relay connector deliveries.
	AutoTrashAfterDeliveryHours *int `json:"auto_trash_after_delivery_hours,omitempty"`
	// DeliveryTrigger selects when the auto-actions fire: "any" (first
	// connector to deliver) or "all" (every connector that existed when the
	// message arrived has delivered). A new inbox carries "all".
	DeliveryTrigger string    `json:"delivery_trigger,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

// ExternalAlias is a send-only identity: it never participates in inbound
// routing. This view carries provider metadata and connector status only; the
// encrypted credentials never leave the store. External aliases are an
// Admin-only capability.
type ExternalAlias struct {
	ID          string    `json:"id"`
	InboxID     string    `json:"inbox_id"`
	Address     string    `json:"address"`
	DisplayName string    `json:"display_name,omitempty"`
	Provider    string    `json:"provider"`
	Configured  bool      `json:"configured"`
	Revision    int64     `json:"revision"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// HasApprover reports whether the inbox has a configured external approver.
func (i Inbox) HasApprover() bool { return strings.TrimSpace(i.ApproverEmail) != "" }

// AllowsApprover reports whether address is the inbox's configured approver.
// The approver is always permitted to write to the inbox so an approval reply
// is never blocked by the sender allowlist.
func (i Inbox) AllowsApprover(address string) bool {
	if !i.HasApprover() {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(i.ApproverEmail), strings.ToLower(strings.TrimSpace(address)))
}

// AllowsInbound reports whether the inbox accepts inbound mail from address,
// treating the configured approver as always permitted.
func (i Inbox) AllowsInbound(address string) bool {
	return i.AllowsSender(address) || i.AllowsApprover(address)
}

// NormalizeAllowedSender validates and normalizes a single allowed-sender
// pattern. It accepts a bare email address, a domain wildcard (*@example.com)
// or a subdomain wildcard (*@*.example.com). An empty entry returns "".
func NormalizeAllowedSender(raw string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	if value == "" {
		return "", nil
	}
	if strings.HasPrefix(value, "*@") {
		domain := value[2:]
		if strings.HasPrefix(domain, "*.") {
			domain = domain[2:]
		}
		if domain == "" || strings.Contains(domain, "*") {
			return "", fmt.Errorf("invalid sender pattern: %s", raw)
		}
		probe := "x@" + domain
		addr, err := mail.ParseAddress(probe)
		if err != nil || !strings.EqualFold(addr.Address, probe) {
			return "", fmt.Errorf("invalid sender pattern: %s", raw)
		}
		return value, nil
	}
	if strings.Contains(value, "*") {
		return "", fmt.Errorf("invalid sender pattern: %s", raw)
	}
	addr, err := mail.ParseAddress(value)
	if err != nil || !strings.EqualFold(addr.Address, value) {
		return "", fmt.Errorf("invalid sender address: %s", raw)
	}
	return value, nil
}

// MatchAllowedSender reports whether address matches an allowed-sender pattern.
// Patterns are matched case-insensitively. A "*@domain" pattern matches any
// local part at exactly domain; a "*@*.domain" pattern matches any local part
// at a proper subdomain of domain (not the apex).
func MatchAllowedSender(pattern, address string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	address = strings.ToLower(strings.TrimSpace(address))
	if pattern == "" || address == "" {
		return false
	}
	if strings.HasPrefix(pattern, "*@") {
		at := strings.LastIndexByte(address, '@')
		if at < 0 {
			return false
		}
		addrDomain := address[at+1:]
		domain := pattern[2:]
		if strings.HasPrefix(domain, "*.") {
			return strings.HasSuffix(addrDomain, "."+domain[2:])
		}
		return addrDomain == domain
	}
	return pattern == address
}

// AllowsSender reports whether the inbox accepts inbound mail from address.
// When SenderRestricted is false every sender is accepted; when true, only a
// matching entry in AllowedSenders is accepted (an empty list blocks everyone
// except the approver).
func (i Inbox) AllowsSender(address string) bool {
	if !i.SenderRestricted {
		return true
	}
	for _, allowed := range i.AllowedSenders {
		if MatchAllowedSender(allowed, address) {
			return true
		}
	}
	return false
}

type Address struct {
	Name    string `json:"name,omitempty"`
	Address string `json:"address"`
}

// MaxLabelLength bounds a single free-text label so it cannot bloat rows or
// responses.
const MaxLabelLength = 64

// NormalizeLabel trims and collapses a free-text label to its stored form. It
// returns ok=false for empty labels, labels over MaxLabelLength, or labels
// containing control characters, '/' or '\' (path separators, which the label
// URLs cannot carry). Case is preserved for display; the stored column
// compares NOCASE so matching ignores case.
func NormalizeLabel(raw string) (string, bool) {
	v := strings.Join(strings.Fields(raw), " ")
	if v == "" || len([]rune(v)) > MaxLabelLength {
		return "", false
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' {
			return "", false
		}
	}
	return v, true
}

type Message struct {
	ID                string   `json:"id"`
	AccountID         string   `json:"-"`
	InboxID           string   `json:"inbox_id"`
	ThreadID          string   `json:"thread_id"`
	Direction         string   `json:"direction"`
	From              Address  `json:"from"`
	To                []string `json:"to"`
	CC                []string `json:"cc"`
	BCC               []string `json:"bcc,omitempty"`
	Subject           string   `json:"subject"`
	Text              string   `json:"text"`
	HTML              string   `json:"html,omitempty"`
	RFCMessageID      string   `json:"message_id,omitempty"`
	InReplyTo         string   `json:"in_reply_to,omitempty"`
	References        []string `json:"references,omitempty"`
	Provider          string   `json:"provider,omitempty"`
	ProviderMessageID string   `json:"provider_message_id,omitempty"`
	EnvelopeTo        []string `json:"envelope_to,omitempty"`
	// EnvelopeFrom is the transport-supplied SMTP envelope sender (MAIL FROM)
	// exactly as the receiving adapter observed it, or empty when the transport
	// supplied none. It is never derived from the MIME From header.
	EnvelopeFrom string `json:"envelope_from,omitempty"`
	// EnvelopeRecipient is the canonical original envelope recipient the
	// transport addressed, which may be a catch-all or alias address rather
	// than the resolved inbox address.
	EnvelopeRecipient string `json:"envelope_recipient,omitempty"`
	// Source names where the message came from. For an outbound message it is
	// the denormalized snapshot of the API key / Hermes credential that sent it,
	// empty for sends with no credential (for example an email-approved send).
	// For inbound mail it is the receiving source, for example
	// "Antler: antler1.hgolabs.com" or the webhook provider's name.
	Source   string `json:"source,omitempty"`
	ClientID string `json:"-"`
	// Internal marks workflow mail (an approval-request email carrying a
	// one-time approval token) that is queued in an inbox but is not mailbox
	// content. It is hidden from every read surface so the token it carries is
	// only ever seen by the nominated approver.
	Internal bool `json:"-"`
	// Spam marks an inbound message classified as Spam by the per-domain auth
	// policy. Spam is a computed view over this flag (never a separate table),
	// counts toward quota, and retains MIME, attachments and recovery.
	Spam bool `json:"is_spam"`
	// SpamReason is the bounded classification reason. AuthResults is the
	// bounded normalized authentication evidence supplied by the authenticated
	// MX edge; it is empty for provider webhook mail.
	SpamReason  string          `json:"spam_reason,omitempty"`
	AuthResults json.RawMessage `json:"auth_results,omitempty"`
	ReceivedAt  *time.Time      `json:"received_at,omitempty"`
	SentAt      *time.Time      `json:"sent_at,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	Read        bool            `json:"read"`
	// DeletedAt is set when the message has been moved to Trash. A trashed
	// message is hidden from every ordinary read surface but retains its row,
	// raw MIME, FTS entry, attachments and storage accounting until it is
	// permanently purged (explicitly, or by the per-account retention sweep).
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	// DeliveryActionDueAt is set once a message's inbox delivery auto-actions
	// have been satisfied: it is the instant at which the message becomes
	// eligible for the auto-trash sweep (delivery instant plus the inbox's
	// configured hours). It is nil when no auto-trash is configured or the
	// trigger is not yet satisfied.
	DeliveryActionDueAt *time.Time `json:"delivery_action_due_at,omitempty"`
	// Deliveries lists the connectors that have successfully delivered this
	// message, one entry per connector. It is populated on the message read
	// surfaces and is empty when nothing has delivered the message yet.
	Deliveries []MessageDelivery `json:"deliveries,omitempty"`
	// Labels are free-text tags on the message. There is no account catalogue;
	// matching ignores case (COLLATE NOCASE) and surrounding whitespace.
	Labels         []string `json:"labels,omitempty"`
	HasAttachments bool     `json:"has_attachments"`
	SizeBytes      int64    `json:"size_bytes"`
	RawPath        string   `json:"-"`
	// Outbox state. Status is one of "pending", "sent" or "failed".
	Status    string `json:"status,omitempty"`
	Attempts  int    `json:"attempts,omitempty"`
	LastError string `json:"last_error,omitempty"`
	NextRetry string `json:"next_retry,omitempty"`
	// Sending is set on outbox listings when a delivery attempt is currently in
	// flight (an outbound_delivery_log row with status "sending"), so the outbox
	// can distinguish an actively-sending message from one merely queued.
	Sending bool   `json:"sending,omitempty"`
	IdemKey string `json:"-"`
	// Blocked marks a synthetic Message built for the admin Recent messages log.
	// Blocked mail is never stored in the messages table; see BlockedMessage.
	Blocked bool `json:"blocked,omitempty"`
	// Approval marks a synthetic Message built for the admin Recent messages log
	// from a consumed approval control message. Control mail is never stored in
	// the messages table; see ControlMessage.
	Approval bool `json:"approval,omitempty"`
}

// MessageDelivery records that one connector (client) has successfully
// delivered a message. It is the durable evidence behind the inbox
// delivery-triggered auto-actions.
type MessageDelivery struct {
	ClientID    string    `json:"client_id"`
	DeliveredAt time.Time `json:"delivered_at"`
}

// BlockedMessage is a metadata-only record of inbound mail rejected by an
// inbox's allowed-senders list. It is deliberately separate from Message so it
// can never be reached by the API, relay or inbox views.
type BlockedMessage struct {
	ID         string     `json:"id"`
	AccountID  string     `json:"-"`
	InboxID    string     `json:"inbox_id"`
	From       Address    `json:"from"`
	To         []string   `json:"to"`
	Subject    string     `json:"subject"`
	SizeBytes  int64      `json:"size_bytes"`
	Reason     string     `json:"reason"`
	ReceivedAt *time.Time `json:"received_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

type Thread struct {
	ID            string    `json:"id"`
	InboxID       string    `json:"inbox_id"`
	Subject       string    `json:"subject"`
	MessageCount  int       `json:"message_count"`
	LastMessageAt time.Time `json:"last_message_at"`
}

type Attachment struct {
	ID          string `json:"id"`
	MessageID   string `json:"message_id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	PartIndex   int    `json:"-"`
	ContentID   string `json:"content_id,omitempty"`
}

type Event struct {
	ID        int64          `json:"-"`
	Cursor    string         `json:"cursor"`
	AccountID string         `json:"-"`
	InboxID   string         `json:"inbox_id,omitempty"`
	Type      string         `json:"type"`
	EntityID  string         `json:"entity_id,omitempty"`
	Payload   map[string]any `json:"payload,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	// Transient marks an event that is delivered only to live in-process
	// subscribers (the hub) and is never written to the events table: it has no
	// durable cursor and is not part of the account's event history. The SSE
	// stream uses this flag — never the zero cursor — to decide how to frame a
	// notification, so a durable event can never be mistaken for a transient one.
	Transient bool `json:"-"`
}

// Draft workflow states stored on drafts.status.
const (
	DraftStatusDraft           = "draft"
	DraftStatusPendingApproval = "pending_approval"
	DraftStatusRejected        = "rejected"
)

// Draft send-request decision states stored on draft_send_requests.status.
const (
	SendRequestPending   = "pending"
	SendRequestApproved  = "approved"
	SendRequestRejected  = "rejected"
	SendRequestCancelled = "cancelled"
	SendRequestExpired   = "expired"
)

// Draft send-request delivery states stored on draft_send_requests.delivery_status.
const (
	SendDeliveryNone    = "none"
	SendDeliveryPending = "pending"
	SendDeliverySent    = "sent"
	SendDeliveryFailed  = "failed"
)

// Workflow notification states stored on draft_send_requests.notification_status.
// They report whether the approval-request email was actually handed to the
// outbound path, so a request is only presented as "awaiting approval" once the
// approver has really been notified.
const (
	NotificationNone   = "none"
	NotificationQueued = "queued"
	NotificationSent   = "sent"
	NotificationFailed = "failed"
)

// Outbound workflow job states stored on outbound_workflow.status.
const (
	WorkflowPending = "pending"
	WorkflowSent    = "sent"
	WorkflowFailed  = "failed"
)

// WorkflowKindApprovalRequest is the kind of the draft approval-request email.
const WorkflowKindApprovalRequest = "approval_request"

// Decision methods recorded for a draft send request.
const (
	DecisionMethodUI    = "ui"
	DecisionMethodAPI   = "api"
	DecisionMethodEmail = "email"
)

// Durable draft workflow event types.
const (
	EventDraftSendRequested        = "draft.send_requested"
	EventDraftSendRequestCancelled = "draft.send_request_cancelled"
	EventDraftApproved             = "draft.approved"
	EventDraftRejected             = "draft.rejected"
	EventDraftSent                 = "draft.sent"
	EventDraftSendFailed           = "draft.send_failed"
	EventDraftApprovalExpired      = "draft.approval_expired"
	EventDraftNotificationSent     = "draft.notification_sent"
	EventDraftNotificationFailed   = "draft.notification_failed"
)

// Durable message event types.
const EventMessageLabelsChanged = "message.labels_changed"
const EventMessageSpamChanged = "message.spam_state_changed"

// EventMessageStateChanged is emitted when a message's read/unread state is
// changed, so the web UI can update unread counts and list styling live. It
// carries no assistant-scoped payload and is not relayed over Hermes or
// delivered to webhooks.
const EventMessageStateChanged = "message.state_changed"

// EventMXHealthChanged is a transient, non-persisted notification that a
// domain's receiver-status light may have changed. It is published only to the
// in-process hub (never written to the events table), so it carries no durable
// cursor and is not part of the account's event history.
const EventMXHealthChanged = "mx.health_changed"

// Trash lifecycle event types. A trashed message is hidden but retained;
// restored returns it to the mailbox; purged erases it permanently (removing
// its row, raw MIME, attachments and storage accounting).
const (
	EventMessageTrashed  = "message.trashed"
	EventMessageRestored = "message.restored"
	EventMessagePurged   = "message.purged"
)

type Draft struct {
	ID               string `json:"id"`
	InboxID          string `json:"inbox_id"`
	ReplyToMessageID string `json:"reply_to_message_id,omitempty"`
	// FromAddress is the chosen sender (the inbox primary or one of its
	// aliases). Empty means the inbox primary. It is frozen into the approval
	// fingerprint and used by the approved send. FromName is the display name
	// resolved for that sender at draft time. FromExternalAliasID names an
	// external sending alias by immutable id when the sender is one, so
	// deleting and recreating an alias with the same address cannot silently
	// rebind the draft.
	FromAddress         string   `json:"from_address,omitempty"`
	FromName            string   `json:"from_name,omitempty"`
	FromExternalAliasID string   `json:"from_external_alias_id,omitempty"`
	To                  []string `json:"to"`
	CC                  []string `json:"cc,omitempty"`
	BCC                 []string `json:"bcc,omitempty"`
	Subject             string   `json:"subject"`
	Text                string   `json:"text"`
	HTML                string   `json:"html,omitempty"`
	// Status is one of DraftStatusDraft, DraftStatusPendingApproval or
	// DraftStatusRejected.
	Status string `json:"status,omitempty"`
	// SendRequest is the active or most recent workflow request, populated on
	// retrieval; it is not stored on the draft row.
	SendRequest *DraftSendRequest `json:"send_request,omitempty"`
	// Attachments is the draft's attachment metadata, populated on retrieval;
	// it is not stored on the draft row.
	Attachments []DraftAttachment `json:"attachments,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// DraftSendRequest records an agent's request that a draft be authorized and
// sent. It is deliberately independent of the draft row so it survives the
// draft being consumed by a successful send.
type DraftSendRequest struct {
	ID                  string    `json:"id"`
	DraftID             string    `json:"draft_id"`
	InboxID             string    `json:"inbox_id"`
	Status              string    `json:"status"`
	DeliveryStatus      string    `json:"delivery_status"`
	ContentHash         string    `json:"-"`
	RequestedAt         time.Time `json:"requested_at"`
	RequestedBy         string    `json:"requested_by,omitempty"`
	RequestedByAPIKeyID string    `json:"requested_by_api_key_id,omitempty"`
	RequestedByUserID   string    `json:"requested_by_user_id,omitempty"`
	// External approval fields. ApproverEmail is the nominated address; the
	// token is never stored in plaintext, only its hash.
	ApproverEmail     string     `json:"approver_email,omitempty"`
	TokenHash         string     `json:"-"`
	TokenExpiresAt    *time.Time `json:"token_expires_at,omitempty"`
	ApprovalMessageID string     `json:"-"`
	// ApprovalWorkflowID links the request to its queued workflow job. It is an
	// opaque non-secret id, exposed so a client can correlate the notification.
	ApprovalWorkflowID string `json:"approval_workflow_id,omitempty"`
	// NotificationStatus reports whether the approval-request email was queued,
	// handed to the outbound path, or failed (NotificationNone|Queued|Sent|Failed).
	NotificationStatus string     `json:"notification_status,omitempty"`
	DecidedAt          *time.Time `json:"decided_at,omitempty"`
	DecisionActor      string     `json:"decision_actor,omitempty"`
	DecisionActorID    string     `json:"decision_actor_id,omitempty"`
	DecisionMethod     string     `json:"decision_method,omitempty"`
	Feedback           string     `json:"feedback,omitempty"`
	MessageID          string     `json:"message_id,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

// DraftAttachment is a file attached to a draft. Unlike message attachments
// (which are extracted from a stored MIME part), draft attachments are stored
// as raw files on disk and copied to the sent message on send.
type DraftAttachment struct {
	ID          string `json:"id"`
	DraftID     string `json:"draft_id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	// ContentHash is the hex SHA-256 of the attachment bytes. It is folded
	// into the frozen draft fingerprint so an approval binds to the exact
	// bytes reviewed.
	ContentHash string    `json:"-"`
	RawPath     string    `json:"-"`
	CreatedAt   time.Time `json:"created_at"`
}

type APIKey struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Prefix    string            `json:"prefix"`
	Admin     bool              `json:"admin"`
	Roles     map[string]string `json:"mailboxes,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

type Principal struct {
	AccountID   string
	UserID      string
	APIKeyID    string
	SessionHash string
	// Admin is the account-level Admin role for AccountID.
	Admin bool
	// SystemAdmin is the installation-level system-administrator role. It is
	// only ever set for a web-UI session, never for an API key.
	SystemAdmin  bool
	MailboxRoles map[string]string
	ViaSession   bool
	// Timezone is the effective display time zone (IANA name) for this session,
	// resolved from the user override then the account default. Empty means UTC.
	// It is only populated for web-UI sessions.
	Timezone string
}

// Scopes returns the revocation scopes a live connection for this principal is
// registered under, so revoking a credential can cancel it immediately.
func (p Principal) Scopes() []string {
	var scopes []string
	if p.APIKeyID != "" {
		scopes = append(scopes, "key:"+p.APIKeyID)
	}
	if p.SessionHash != "" {
		scopes = append(scopes, "sess:"+p.SessionHash)
	}
	if p.UserID != "" {
		scopes = append(scopes, "user:"+p.UserID)
	}
	return scopes
}

func (p Principal) Role(inboxID string) string {
	if p.Admin {
		return "owner"
	}
	return p.MailboxRoles[inboxID]
}
func (p Principal) CanRead(inboxID string) bool {
	r := p.Role(inboxID)
	return r == "read" || r == "assistant" || r == "owner"
}
func (p Principal) CanAssist(inboxID string) bool {
	r := p.Role(inboxID)
	return r == "assistant" || r == "owner"
}
func (p Principal) CanOwn(inboxID string) bool { return p.Role(inboxID) == "owner" }

// OwnsAccount reports whether the principal holds Owner on every mailbox it can
// reach (and at least one). Account-level preferences are exposed to such a
// principal in addition to an account Admin.
func (p Principal) OwnsAccount() bool {
	if p.Admin {
		return true
	}
	if len(p.MailboxRoles) == 0 {
		return false
	}
	for _, role := range p.MailboxRoles {
		if role != "owner" {
			return false
		}
	}
	return true
}
