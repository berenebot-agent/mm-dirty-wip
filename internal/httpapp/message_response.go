package httpapp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/htmlsanitize"
	"github.com/dellarb/mailmoose/internal/messagepreview"
	"github.com/dellarb/mailmoose/internal/model"
)

// messageResponse is the sole native API message shape. Availability, not inbox
// kind, controls optional fields. Storage and UI models are never wire contracts.
type messageResponse struct {
	ID                  string                  `json:"id"`
	InboxID             string                  `json:"inbox_id"`
	InboxKind           string                  `json:"inbox_kind,omitempty"`
	ThreadID            string                  `json:"thread_id,omitempty"`
	Direction           string                  `json:"direction"`
	From                model.Address           `json:"from"`
	To                  []string                `json:"to"`
	CC                  []string                `json:"cc"`
	BCC                 []string                `json:"bcc,omitempty"`
	Subject             string                  `json:"subject"`
	Text                *string                 `json:"text,omitempty"`
	HTML                string                  `json:"html,omitempty"`
	MessageID           string                  `json:"message_id,omitempty"`
	InReplyTo           string                  `json:"in_reply_to,omitempty"`
	References          []string                `json:"references,omitempty"`
	Provider            string                  `json:"provider"`
	MessageTransport    string                  `json:"message_transport"`
	ProviderMessageID   string                  `json:"provider_message_id,omitempty"`
	EnvelopeTo          []string                `json:"envelope_to,omitempty"`
	EnvelopeFrom        string                  `json:"envelope_from,omitempty"`
	EnvelopeRecipient   string                  `json:"envelope_recipient,omitempty"`
	Source              string                  `json:"source,omitempty"`
	Spam                *bool                   `json:"is_spam,omitempty"`
	SpamReason          string                  `json:"spam_reason,omitempty"`
	AuthResults         json.RawMessage         `json:"auth_results,omitempty"`
	ReceivedAt          *time.Time              `json:"received_at,omitempty"`
	SentAt              *time.Time              `json:"sent_at,omitempty"`
	CreatedAt           *time.Time              `json:"created_at,omitempty"`
	Read                bool                    `json:"read"`
	DeletedAt           *time.Time              `json:"deleted_at,omitempty"`
	DeliveryActionDueAt *time.Time              `json:"delivery_action_due_at,omitempty"`
	Deliveries          []model.MessageDelivery `json:"deliveries,omitempty"`
	// Labels is the legacy additive-name view: free-text tags and provider
	// category names. LabelIDs is the canonical unified membership view and adds
	// folder-backed labels, which is why both are projected.
	Labels         []string   `json:"labels,omitempty"`
	LabelIDs       []string   `json:"label_ids"`
	HasAttachments bool       `json:"has_attachments"`
	SizeBytes      int64      `json:"size_bytes"`
	MailboxID      string     `json:"mailbox_id,omitempty"`
	FolderPath     string     `json:"folder_path,omitempty"`
	Status         string     `json:"status,omitempty"`
	Attempts       int        `json:"attempts,omitempty"`
	LastError      string     `json:"last_error,omitempty"`
	NextRetry      string     `json:"next_retry,omitempty"`
	Sending        bool       `json:"sending,omitempty"`
	Snippet        string     `json:"snippet"`
	Flagged        *bool      `json:"flagged,omitempty"`
	Answered       *bool      `json:"answered,omitempty"`
	Draft          *bool      `json:"draft,omitempty"`
	Flags          []string   `json:"flags,omitempty"`
	InternalDate   *time.Time `json:"internal_date,omitempty"`
	IndexedAt      *time.Time `json:"indexed_at,omitempty"`
}

func messageResponseOf(m model.Message, body bool) messageResponse {
	out := messageResponse{
		ID: m.ID, InboxID: m.InboxID, ThreadID: m.ThreadID, Direction: model.MessageDirection(m.Direction, false, false, false),
		From: m.From, To: m.To, CC: m.CC, BCC: m.BCC, Subject: m.Subject,
		MessageID: m.RFCMessageID, InReplyTo: m.InReplyTo, References: m.References,
		MessageTransport: m.Provider, ProviderMessageID: m.ProviderMessageID, Snippet: m.Snippet,
		EnvelopeTo: m.EnvelopeTo, EnvelopeFrom: m.EnvelopeFrom, EnvelopeRecipient: m.EnvelopeRecipient,
		Source: m.Source, Spam: &m.Spam, SpamReason: m.SpamReason, AuthResults: m.AuthResults,
		ReceivedAt: m.ReceivedAt, SentAt: m.SentAt, Read: m.Read, DeletedAt: m.DeletedAt,
		DeliveryActionDueAt: m.DeliveryActionDueAt, Deliveries: m.Deliveries, Labels: m.Labels,
		HasAttachments: m.HasAttachments, SizeBytes: m.SizeBytes, MailboxID: m.MailboxID,
		FolderPath: m.FolderPath, Status: m.Status, Attempts: m.Attempts,
		LastError: m.LastError, NextRetry: m.NextRetry, Sending: m.Sending,
	}
	if out.To == nil {
		out.To = []string{}
	}
	if out.CC == nil {
		out.CC = []string{}
	}
	if !m.CreatedAt.IsZero() {
		out.CreatedAt = &m.CreatedAt
	}
	if body {
		out.Text = &m.Text
		out.HTML = htmlsanitize.Sanitize(m.HTML)
	}
	if meta := m.ReadMetadata; meta != nil {
		out.Snippet, out.Flagged, out.Answered, out.Draft = meta.Snippet, meta.Flagged, meta.Answered, meta.Draft
		out.Flags, out.InternalDate, out.IndexedAt = meta.Flags, meta.InternalDate, meta.IndexedAt
		out.CreatedAt, out.Spam = meta.CreatedAt, meta.Spam
	}
	out.Snippet = messagepreview.Text(out.Snippet)
	if out.Snippet == "" {
		out.Snippet = "-"
	}
	if out.MessageTransport == "" {
		out.MessageTransport = "unknown"
	}
	return out
}

// inboxIdentity is the per-inbox context the message projection needs: the
// backend that owns the message and the folder index that turns a message's
// membership into the unified label_ids the catalogue advertises. It is resolved
// once per inbox per response, never per message.
type inboxIdentity struct {
	kind     string
	provider string
	byID     map[string]model.Folder
	byPath   map[string]model.Folder
}

// messageResponses resolves inbox identity once per inbox, never per item. It
// does not fetch message bodies or expose any connection/credential data.
func (s *Server) messageResponses(r *http.Request, ms []model.Message, body bool) []messageResponse {
	out := make([]messageResponse, 0, len(ms))
	identities := map[string]inboxIdentity{}
	p := principal(r)
	for _, m := range ms {
		id, ok := identities[m.InboxID]
		if !ok {
			id.provider = "unknown"
			if box, err := s.Service.Store.GetInboxInternal(r.Context(), p.AccountID, m.InboxID); err == nil {
				id.kind = box.Kind
				id.provider = "mailmoose"
				if box.Kind == model.InboxKindStandalone {
					id.provider = "imap"
					switch {
					case s.Service.Store.IsOne(r.Context(), p.AccountID, box.ID):
						id.provider = "one"
					case s.remoteMailbox().IsGoogle(r.Context(), p.AccountID, box.ID):
						id.provider = "google"
					case s.remoteMailbox().IsMicrosoft(r.Context(), p.AccountID, box.ID):
						id.provider = "microsoft"
					case box.Remote != nil:
						id.provider = "imap"
					}
				}
				id.byID, id.byPath = s.labelFolderIndex(r.Context(), p, box)
			}
			identities[m.InboxID] = id
		}
		v := messageResponseOf(m, body)
		v.InboxKind, v.Provider = id.kind, id.provider
		if id.kind == model.InboxKindStandalone {
			v.MessageTransport = id.provider
		}
		v.LabelIDs = labelIDsFor(m, id)
		out = append(out, v)
	}
	return out
}

// labelFolderIndex indexes an inbox's label-eligible folders by id and by path so
// a message's single folder membership can be projected as a unified label id on
// every backend, including Microsoft's provider folder ids.
func (s *Server) labelFolderIndex(ctx context.Context, p model.Principal, box model.Inbox) (map[string]model.Folder, map[string]model.Folder) {
	folders, err := s.Service.Store.ListFolders(ctx, p.AccountID, box.ID)
	if err != nil {
		return nil, nil
	}
	byID := map[string]model.Folder{}
	byPath := map[string]model.Folder{}
	for _, f := range folders {
		if !labelEligibleFolder(f) {
			continue
		}
		byID[f.ID] = f
		byPath[f.Path] = f
	}
	return byID, byPath
}

// labelIDsFor composes the unified membership ids for one message. Additive
// names are name-addressed (tag:<Name>) exactly as the action API addresses them;
// folder membership is id-addressed (folder:<id>) exactly as the catalogue
// advertises it. A message has one folder membership, so at most one folder id is
// emitted.
func labelIDsFor(m model.Message, id inboxIdentity) []string {
	out := make([]string, 0, len(m.Labels)+1)
	for _, name := range m.Labels {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		out = appendUniqueFold(out, "tag:"+name)
	}
	folder, ok := id.byID[m.MailboxID]
	if !ok && m.FolderPath != "" {
		folder, ok = id.byPath[m.FolderPath]
	}
	if ok {
		out = appendUniqueFold(out, "folder:"+folder.ID)
	}
	return out
}

func (s *Server) messageResponse(r *http.Request, m model.Message) messageResponse {
	return s.messageResponses(r, []model.Message{m}, true)[0]
}
