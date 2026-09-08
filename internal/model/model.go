package model

import "time"

type Account struct {
	ID string `json:"id"`
	Name string `json:"name"`
	StorageQuotaBytes int64 `json:"storage_quota_bytes"`
	StorageUsedBytes int64 `json:"storage_used_bytes"`
	CreatedAt time.Time `json:"created_at"`
}

type User struct {
	ID string `json:"id"`
	AccountID string `json:"account_id"`
	Email string `json:"email"`
	IsAdmin bool `json:"is_admin"`
	CreatedAt time.Time `json:"created_at"`
}

type Domain struct {
	ID string `json:"id"`
	AccountID string `json:"account_id"`
	Name string `json:"name"`
	CatchAllInboxID string `json:"catch_all_inbox_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type Inbox struct {
	ID string `json:"id"`
	AccountID string `json:"account_id"`
	DomainID string `json:"domain_id"`
	LocalPart string `json:"local_part"`
	Address string `json:"address"`
	DisplayName string `json:"display_name"`
	Enabled bool `json:"enabled"`
	OutboundCredentialID string `json:"outbound_credential_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type Address struct { Name string `json:"name,omitempty"`; Address string `json:"address"` }

type Message struct {
	ID string `json:"id"`
	AccountID string `json:"-"`
	InboxID string `json:"inbox_id"`
	ThreadID string `json:"thread_id"`
	Direction string `json:"direction"`
	From Address `json:"from"`
	To []string `json:"to"`
	CC []string `json:"cc"`
	Subject string `json:"subject"`
	Text string `json:"text"`
	HTML string `json:"html,omitempty"`
	RFCMessageID string `json:"message_id,omitempty"`
	InReplyTo string `json:"in_reply_to,omitempty"`
	References []string `json:"references,omitempty"`
	Provider string `json:"provider,omitempty"`
	ProviderMessageID string `json:"provider_message_id,omitempty"`
	EnvelopeTo []string `json:"envelope_to,omitempty"`
	ReceivedAt *time.Time `json:"received_at,omitempty"`
	SentAt *time.Time `json:"sent_at,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Read bool `json:"read"`
	Archived bool `json:"archived"`
	HasAttachments bool `json:"has_attachments"`
	SizeBytes int64 `json:"size_bytes"`
	RawPath string `json:"-"`
}

type Thread struct {
	ID string `json:"id"`
	InboxID string `json:"inbox_id"`
	Subject string `json:"subject"`
	MessageCount int `json:"message_count"`
	LastMessageAt time.Time `json:"last_message_at"`
}

type Attachment struct {
	ID string `json:"id"`
	MessageID string `json:"message_id"`
	Filename string `json:"filename"`
	ContentType string `json:"content_type"`
	Size int64 `json:"size"`
	PartIndex int `json:"-"`
	ContentID string `json:"content_id,omitempty"`
}

type Event struct {
	ID int64 `json:"-"`
	Cursor string `json:"cursor"`
	AccountID string `json:"-"`
	InboxID string `json:"inbox_id,omitempty"`
	Type string `json:"type"`
	EntityID string `json:"entity_id,omitempty"`
	Payload map[string]any `json:"payload,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type Draft struct {
	ID string `json:"id"`
	InboxID string `json:"inbox_id"`
	ReplyToMessageID string `json:"reply_to_message_id,omitempty"`
	To []string `json:"to"`
	CC []string `json:"cc,omitempty"`
	BCC []string `json:"bcc,omitempty"`
	Subject string `json:"subject"`
	Text string `json:"text"`
	HTML string `json:"html,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type APIKey struct {
	ID string `json:"id"`
	Name string `json:"name"`
	Prefix string `json:"prefix"`
	Admin bool `json:"admin"`
	Roles map[string]string `json:"mailboxes,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type Principal struct {
	AccountID string
	UserID string
	APIKeyID string
	Admin bool
	MailboxRoles map[string]string
	ViaSession bool
}

func (p Principal) Role(inboxID string) string {
	if p.Admin { return "owner" }
	return p.MailboxRoles[inboxID]
}
func (p Principal) CanRead(inboxID string) bool { r:=p.Role(inboxID); return r=="read"||r=="assistant"||r=="owner" }
func (p Principal) CanAssist(inboxID string) bool { r:=p.Role(inboxID); return r=="assistant"||r=="owner" }
func (p Principal) CanOwn(inboxID string) bool { return p.Role(inboxID)=="owner" }
