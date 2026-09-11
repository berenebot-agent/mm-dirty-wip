package model

import (
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
}

type User struct {
	ID        string    `json:"id"`
	AccountID string    `json:"account_id"`
	Email     string    `json:"email"`
	IsAdmin   bool      `json:"is_admin"`
	CreatedAt time.Time `json:"created_at"`
}

type Domain struct {
	ID                string    `json:"id"`
	AccountID         string    `json:"account_id"`
	Name              string    `json:"name"`
	CatchAllInboxID   string    `json:"catch_all_inbox_id,omitempty"`
	SendingProvider   string    `json:"sending_provider"`
	ReceivingProvider string    `json:"receiving_provider"`
	CreatedAt         time.Time `json:"created_at"`
}

type Inbox struct {
	ID             string   `json:"id"`
	AccountID      string   `json:"account_id"`
	DomainID       string   `json:"domain_id"`
	LocalPart      string   `json:"local_part"`
	Address        string   `json:"address"`
	DisplayName    string   `json:"display_name"`
	Enabled        bool     `json:"enabled"`
	AllowedSenders []string `json:"allowed_senders,omitempty"`
	// SenderRestricted enables the allow-list. When false, any sender is
	// accepted and AllowedSenders is ignored; when true, only AllowedSenders
	// (and the approver) are accepted.
	SenderRestricted bool `json:"sender_restricted,omitempty"`
	// ApproverEmail optionally nominates a person who may authorize draft
	// sends by email. When set, the approver address is always accepted as an
	// inbound sender for this inbox regardless of AllowedSenders.
	ApproverEmail string    `json:"approver_email,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
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
	// Client is the denormalized snapshot of the API key / Hermes credential
	// that sent an outbound message. It is empty for inbound mail and for sends
	// with no credential (for example an email-approved send).
	Client   string `json:"client,omitempty"`
	ClientID string `json:"-"`
	// Internal marks workflow mail (an approval-request email carrying a
	// one-time approval token) that is queued in an inbox but is not mailbox
	// content. It is hidden from every read surface so the token it carries is
	// only ever seen by the nominated approver.
	Internal       bool       `json:"-"`
	ReceivedAt     *time.Time `json:"received_at,omitempty"`
	SentAt         *time.Time `json:"sent_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	Read           bool       `json:"read"`
	Archived       bool       `json:"archived"`
	HasAttachments bool       `json:"has_attachments"`
	SizeBytes      int64      `json:"size_bytes"`
	RawPath        string     `json:"-"`
	// Outbox state. Status is one of "pending", "sent" or "failed".
	Status    string `json:"status,omitempty"`
	Attempts  int    `json:"attempts,omitempty"`
	LastError string `json:"last_error,omitempty"`
	NextRetry string `json:"next_retry,omitempty"`
	IdemKey   string `json:"-"`
	// Blocked marks a synthetic Message built for the admin Recent messages log.
	// Blocked mail is never stored in the messages table; see BlockedMessage.
	Blocked bool `json:"blocked,omitempty"`
	// Approval marks a synthetic Message built for the admin Recent messages log
	// from a consumed approval control message. Control mail is never stored in
	// the messages table; see ControlMessage.
	Approval bool `json:"approval,omitempty"`
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
)

type Draft struct {
	ID               string   `json:"id"`
	InboxID          string   `json:"inbox_id"`
	ReplyToMessageID string   `json:"reply_to_message_id,omitempty"`
	To               []string `json:"to"`
	CC               []string `json:"cc,omitempty"`
	BCC              []string `json:"bcc,omitempty"`
	Subject          string   `json:"subject"`
	Text             string   `json:"text"`
	HTML             string   `json:"html,omitempty"`
	// Status is one of DraftStatusDraft, DraftStatusPendingApproval or
	// DraftStatusRejected.
	Status string `json:"status,omitempty"`
	// SendRequest is the active or most recent workflow request, populated on
	// retrieval; it is not stored on the draft row.
	SendRequest *DraftSendRequest `json:"send_request,omitempty"`
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
	ApprovalMessageID string     `json:"approval_message_id,omitempty"`
	DecidedAt         *time.Time `json:"decided_at,omitempty"`
	DecisionActor     string     `json:"decision_actor,omitempty"`
	DecisionActorID   string     `json:"decision_actor_id,omitempty"`
	DecisionMethod    string     `json:"decision_method,omitempty"`
	Feedback          string     `json:"feedback,omitempty"`
	MessageID         string     `json:"message_id,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
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
	AccountID    string
	UserID       string
	APIKeyID     string
	SessionHash  string
	Admin        bool
	MailboxRoles map[string]string
	ViaSession   bool
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
