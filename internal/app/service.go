package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gatehouse-mail/internal/auth"
	"gatehouse-mail/internal/config"
	"gatehouse-mail/internal/cryptox"
	"gatehouse-mail/internal/events"
	"gatehouse-mail/internal/idgen"
	"gatehouse-mail/internal/mailparse"
	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
	"gatehouse-mail/internal/transport"
	_ "gatehouse-mail/internal/transport/brevo"
	_ "gatehouse-mail/internal/transport/cloudflare"
	_ "gatehouse-mail/internal/transport/mailgun"
	smtpt "gatehouse-mail/internal/transport/smtp"
)

type Service struct {
	Config        config.Config
	Store         *store.Store
	Hub           *events.Hub
	EncryptionKey []byte
}

func New(cfg config.Config, st *store.Store, hub *events.Hub) (*Service, error) {
	key, err := cryptox.DeriveKey(cfg.AppEncryptionKey)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Join(cfg.DataDir, "messages", ".tmp"), 0o700); err != nil {
		return nil, err
	}
	smtpt.SetHosted(cfg.Mode == "hosted")
	return &Service{Config: cfg, Store: st, Hub: hub, EncryptionKey: key}, nil
}

func (s *Service) messagePath() string {
	id := idgen.New("raw")
	return filepath.Join(s.Config.DataDir, "messages", id[4:6], id[6:8], id+".eml")
}

func (s *Service) inboundSecret(provider string) (string, error) {
	switch provider {
	case "mailgun":
		if s.Config.MailgunSigningKey == "" {
			return "", fmt.Errorf("MAILGUN_SIGNING_KEY is not configured")
		}
		return s.Config.MailgunSigningKey, nil
	case "cloudflare":
		if s.Config.CloudflareSecret == "" {
			return "", fmt.Errorf("CLOUDFLARE_WEBHOOK_SECRET is not configured")
		}
		return s.Config.CloudflareSecret, nil
	default:
		return "", fmt.Errorf("%w: %s", transport.ErrUnknownProvider, provider)
	}
}

// IngestInbound runs the shared provider-neutral ingest pipeline for any
// registered inbound transport: parse the provider webhook, verify
// authenticity with the provider scheme, resolve the recipient, parse the
// staged MIME, commit transactionally, then publish the realtime event.
func (s *Service) IngestInbound(ctx context.Context, provider string, r *http.Request) (model.Message, bool, error) {
	t, ok := transport.LookupInbound(provider)
	if !ok {
		return model.Message{}, false, fmt.Errorf("%w: %s", transport.ErrUnknownProvider, provider)
	}
	secret, err := s.inboundSecret(provider)
	if err != nil {
		return model.Message{}, false, err
	}
	tmp := filepath.Join(s.Config.DataDir, "messages", ".tmp", idgen.New("in")+".eml")
	defer os.Remove(tmp)
	msg, err := t.Parse(r, tmp, s.Config.MaxMessageBytes)
	if err != nil {
		return model.Message{}, false, err
	}
	if err = t.Verify(r, msg, secret); err != nil {
		return model.Message{}, false, err
	}
	recipient := msg.Recipient
	if a, e := mail.ParseAddress(recipient); e == nil {
		recipient = a.Address
	}
	inbox, _, err := s.Store.ResolveRecipient(ctx, recipient)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.Store.Audit(ctx, "", provider+".unrouted", recipient)
		}
		return model.Message{}, false, err
	}
	parsed, err := mailparse.ParseFile(tmp)
	if err != nil {
		return model.Message{}, false, fmt.Errorf("parse MIME: %w", err)
	}
	final := s.messagePath()
	if err = os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return model.Message{}, false, err
	}
	if err = os.Rename(tmp, final); err != nil {
		return model.Message{}, false, err
	}
	rel, _ := filepath.Rel(s.Config.DataDir, final)
	atts := make([]store.AttachmentInput, 0, len(parsed.Attachments))
	for _, a := range parsed.Attachments {
		atts = append(atts, store.AttachmentInput{Filename: a.Filename, ContentType: a.ContentType, ContentID: a.ContentID, Size: a.Size, PartIndex: a.PartIndex})
	}
	from := model.Address{Name: parsed.From.Name, Address: parsed.From.Address}
	received := parsed.Date
	if received.IsZero() {
		received = time.Now().UTC()
	}
	m, ev, dup, err := s.Store.CommitInbound(ctx, store.InboundRecord{Inbox: inbox, Provider: provider, ProviderDeliveryID: msg.DeliveryID, ProviderMessageID: firstNonEmpty(msg.ProviderMessageID, parsed.RFCMessageID), RFCMessageID: parsed.RFCMessageID, InReplyTo: parsed.InReplyTo, References: parsed.References, From: from, To: parsed.To, CC: parsed.CC, EnvelopeTo: []string{strings.ToLower(recipient)}, Subject: parsed.Subject, Text: parsed.Text, HTML: parsed.HTML, RawPath: filepath.ToSlash(rel), SizeBytes: msg.Size, ReceivedAt: received, Attachments: atts})
	if err != nil {
		_ = os.Remove(final)
		return model.Message{}, false, err
	}
	if dup {
		_ = os.Remove(final)
		return m, true, nil
	}
	s.Hub.Publish(ev)
	return m, false, nil
}

// IngestMailgun is the compat entry point for the legacy
// POST /internal/ingest/mailgun route.
func (s *Service) IngestMailgun(ctx context.Context, r *http.Request) (model.Message, bool, error) {
	return s.IngestInbound(ctx, "mailgun", r)
}
func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func (s *Service) SaveOutboundCredential(ctx context.Context, accountID, id, name, provider string, cfg any) (store.OutboundCredential, error) {
	t, ok := transport.LookupOutbound(provider)
	if !ok {
		return store.OutboundCredential{}, fmt.Errorf("unknown outbound provider %q", provider)
	}
	if strings.TrimSpace(name) == "" {
		name = t.Description()
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return store.OutboundCredential{}, err
	}
	enc, err := cryptox.Encrypt(s.EncryptionKey, b)
	if err != nil {
		return store.OutboundCredential{}, err
	}
	return s.Store.SaveOutboundCredential(ctx, accountID, id, name, provider, enc)
}
func (s *Service) DecryptOutboundCredential(c store.OutboundCredential) (map[string]any, error) {
	b, err := cryptox.Decrypt(s.EncryptionKey, c.EncryptedConfig)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err = json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateHermesRelay issues a relay connection's credentials directly and
// returns the plaintext secret and delivery key exactly once. The gateway id is
// generated here so the operator only has to paste the printed .env block.
func (s *Service) CreateHermesRelay(ctx context.Context, p model.Principal, inboxID, name string) (string, string, string, error) {
	if !p.Admin {
		return "", "", "", store.ErrForbidden
	}
	if _, err := s.Store.GetInboxInternal(ctx, p.AccountID, inboxID); err != nil {
		return "", "", "", err
	}
	rand, err := auth.RandomToken(6)
	if err != nil {
		return "", "", "", err
	}
	gatewayID := "gw-" + rand
	secret, err := auth.RandomToken(32)
	if err != nil {
		return "", "", "", err
	}
	deliveryKey, err := auth.RandomToken(32)
	if err != nil {
		return "", "", "", err
	}
	secEnc, err := cryptox.Encrypt(s.EncryptionKey, []byte(secret))
	if err != nil {
		return "", "", "", err
	}
	delEnc, err := cryptox.Encrypt(s.EncryptionKey, []byte(deliveryKey))
	if err != nil {
		return "", "", "", err
	}
	rec := store.EnrollRecord{AccountID: p.AccountID, InboxID: inboxID, Name: strings.TrimSpace(name)}
	if rec.Name == "" {
		rec.Name = "Hermes"
	}
	if _, err = s.Store.CreateHermesConnection(ctx, rec, gatewayID, secEnc, delEnc); err != nil {
		return "", "", "", err
	}
	return gatewayID, secret, deliveryKey, nil
}

type SendAttachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type,omitempty"`
	Content     []byte `json:"content"`
}

type SendInput struct {
	InboxID            string           `json:"inbox_id"`
	To                 []string         `json:"to,omitempty"`
	CC                 []string         `json:"cc,omitempty"`
	BCC                []string         `json:"bcc,omitempty"`
	Subject            string           `json:"subject"`
	Text               string           `json:"text"`
	HTML               string           `json:"html,omitempty"`
	ReplyToMessageID   string           `json:"reply_to_message_id,omitempty"`
	ForwardOfMessageID string           `json:"forward_of_message_id,omitempty"`
	Attachments        []SendAttachment `json:"attachments,omitempty"`
}
type SendResult struct {
	Message           model.Message `json:"message"`
	ProviderMessageID string        `json:"provider_message_id"`
}

func (s *Service) Send(ctx context.Context, p model.Principal, in SendInput, idem string) (SendResult, error) {
	if !p.CanOwn(in.InboxID) {
		return SendResult{}, store.ErrForbidden
	}
	if idem != "" {
		mid, res, found, err := s.Store.IdempotencyGet(ctx, p.AccountID, idem)
		if err != nil {
			return SendResult{}, err
		}
		if found {
			m, err := s.Store.GetMessageByID(ctx, p.AccountID, mid)
			if err != nil {
				return SendResult{}, err
			}
			var sr SendResult
			_ = json.Unmarshal([]byte(res), &sr)
			sr.Message = m
			return sr, nil
		}
	}
	inbox, err := s.Store.GetInboxInternal(ctx, p.AccountID, in.InboxID)
	if err != nil {
		return SendResult{}, err
	}
	var threadID, inReply string
	refs := []string{}
	to := cleanAddresses(in.To)
	subject := strings.TrimSpace(in.Subject)
	if in.ReplyToMessageID != "" {
		target, err := s.Store.GetMessageByID(ctx, p.AccountID, in.ReplyToMessageID)
		if err != nil {
			return SendResult{}, err
		}
		if target.InboxID != inbox.ID {
			return SendResult{}, store.ErrForbidden
		}
		threadID = target.ThreadID
		inReply = target.RFCMessageID
		refs = append(refs, target.References...)
		if target.RFCMessageID != "" {
			refs = appendUnique(refs, target.RFCMessageID)
		}
		if len(to) == 0 {
			if target.Direction == "inbound" {
				to = []string{target.From.Address}
			} else {
				to = append([]string{}, target.To...)
			}
		}
		if subject == "" {
			subject = ReplySubject(target.Subject)
		}
	}
	if in.ForwardOfMessageID != "" {
		target, err := s.Store.GetMessageByID(ctx, p.AccountID, in.ForwardOfMessageID)
		if err != nil {
			return SendResult{}, err
		}
		if target.InboxID != inbox.ID {
			return SendResult{}, store.ErrForbidden
		}
		if subject == "" {
			subject = ForwardSubject(target.Subject)
		}
		if strings.TrimSpace(in.Text) == "" {
			in.Text = forwardPrefix(target)
		} else {
			in.Text = strings.TrimRight(in.Text, "\n") + "\n\n" + forwardPrefix(target)
		}
		carried, err := s.forwardAttachments(ctx, p, target)
		if err != nil {
			return SendResult{}, err
		}
		in.Attachments = append(in.Attachments, carried...)
	}
	if len(to) == 0 {
		return SendResult{}, fmt.Errorf("recipient required")
	}
	cred, err := s.Store.ActiveOutboundCredential(ctx, p.AccountID)
	if err != nil {
		return SendResult{}, fmt.Errorf("outbound provider not configured for account")
	}
	cfg, err := s.DecryptOutboundCredential(cred)
	if err != nil {
		return SendResult{}, err
	}
	msgID := fmt.Sprintf("<%s@%s>", strings.TrimPrefix(idgen.New("msg"), "msg_"), strings.SplitN(inbox.Address, "@", 2)[1])
	now := time.Now().UTC()
	html := in.HTML
	attachments, err := outboundAttachments(in.Attachments)
	if err != nil {
		return SendResult{}, err
	}
	if size := attachmentsSize(attachments); size > s.Config.MaxMessageBytes {
		return SendResult{}, fmt.Errorf("attachments exceed maximum message size")
	}
	raw, err := mailparse.BuildMessage(mailparse.Address{Name: inbox.DisplayName, Address: inbox.Address}, to, cleanAddresses(in.CC), cleanAddresses(in.BCC), subject, in.Text, html, msgID, inReply, refs, now, attachmentParts(attachments))
	if err != nil {
		return SendResult{}, err
	}
	acc, err := s.Store.GetAccount(ctx, p.AccountID)
	if err != nil {
		return SendResult{}, err
	}
	if acc.StorageQuotaBytes > 0 && acc.StorageUsedBytes+int64(len(raw)) > acc.StorageQuotaBytes {
		return SendResult{}, store.ErrQuota
	}
	provider, ok := transport.LookupOutbound(cred.Provider)
	if !ok {
		return SendResult{}, fmt.Errorf("%w: %s", transport.ErrUnknownProvider, cred.Provider)
	}
	providerResult, err := provider.Send(ctx, cfg, transport.OutboundMessage{
		FromName:    inbox.DisplayName,
		FromAddress: inbox.Address,
		To:          to,
		CC:          cleanAddresses(in.CC),
		BCC:         cleanAddresses(in.BCC),
		Subject:     subject,
		Text:        in.Text,
		HTML:        html,
		MessageID:   msgID,
		InReplyTo:   inReply,
		References:  refs,
		RawMIME:     raw,
		Attachments: attachments,
	})
	if err != nil {
		return SendResult{}, err
	}
	providerID := providerResult.ProviderMessageID
	path := s.messagePath()
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return SendResult{}, err
	}
	if err = os.WriteFile(path, raw, 0o600); err != nil {
		return SendResult{}, err
	}
	rel, _ := filepath.Rel(s.Config.DataDir, path)
	metadata := make([]store.AttachmentInput, 0, len(attachments))
	for i, attachment := range attachments {
		metadata = append(metadata, store.AttachmentInput{Filename: attachment.Filename, ContentType: attachment.ContentType, Size: int64(len(attachment.Content)), PartIndex: i + 1})
	}
	m, ev, err := s.Store.CommitOutbound(ctx, store.OutboundRecord{Inbox: inbox, Provider: cred.Provider, ProviderMessageID: providerID, RFCMessageID: msgID, InReplyTo: inReply, References: refs, From: model.Address{Name: inbox.DisplayName, Address: inbox.Address}, To: to, CC: cleanAddresses(in.CC), Subject: subject, Text: in.Text, HTML: html, RawPath: filepath.ToSlash(rel), SizeBytes: int64(len(raw)), SentAt: now, ThreadID: threadID, Attachments: metadata})
	if err != nil {
		_ = os.Remove(path)
		return SendResult{}, err
	}
	s.Hub.Publish(ev)
	result := SendResult{Message: m, ProviderMessageID: providerID}
	if idem != "" {
		_ = s.Store.IdempotencyPut(ctx, p.AccountID, idem, m.ID, result)
	}
	return result, nil
}
func outboundAttachments(in []SendAttachment) ([]transport.OutboundAttachment, error) {
	out := make([]transport.OutboundAttachment, 0, len(in))
	for index, attachment := range in {
		filename := mailparse.SafeAttachmentFilename(attachment.Filename, index+1, attachment.ContentType)
		if len(attachment.Content) == 0 {
			return nil, fmt.Errorf("attachment %q is empty", filename)
		}
		contentType := strings.TrimSpace(attachment.ContentType)
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		out = append(out, transport.OutboundAttachment{Filename: filename, ContentType: contentType, Content: attachment.Content})
	}
	return out, nil
}

func attachmentsSize(in []transport.OutboundAttachment) int64 {
	var total int64
	for _, attachment := range in {
		total += int64(len(attachment.Content))
	}
	return total
}

func attachmentParts(in []transport.OutboundAttachment) []mailparse.Attachment {
	out := make([]mailparse.Attachment, 0, len(in))
	for _, attachment := range in {
		out = append(out, mailparse.Attachment{Filename: attachment.Filename, ContentType: attachment.ContentType, Content: attachment.Content})
	}
	return out
}

func cleanAddresses(in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, v := range in {
		if a, err := mail.ParseAddress(strings.TrimSpace(v)); err == nil {
			v = strings.ToLower(a.Address)
		} else {
			v = strings.ToLower(strings.TrimSpace(v))
		}
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
func appendUnique(in []string, v string) []string {
	for _, x := range in {
		if x == v {
			return in
		}
	}
	return append(in, v)
}
func ReplySubject(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(strings.ToLower(s), "re:") {
		return s
	}
	return "Re: " + s
}
func ForwardSubject(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(strings.ToLower(s), "fwd:") {
		return s
	}
	return "Fwd: " + s
}
func addressLine(a model.Address) string {
	if strings.TrimSpace(a.Name) == "" {
		return a.Address
	}
	return a.Name + " <" + a.Address + ">"
}
func forwardPrefix(m model.Message) string {
	var b strings.Builder
	b.WriteString("---------- Forwarded message ----------\n")
	b.WriteString("From: " + addressLine(m.From) + "\n")
	if len(m.To) > 0 {
		b.WriteString("To: " + strings.Join(m.To, ", ") + "\n")
	}
	if len(m.CC) > 0 {
		b.WriteString("Cc: " + strings.Join(m.CC, ", ") + "\n")
	}
	when := m.CreatedAt
	if m.ReceivedAt != nil {
		when = *m.ReceivedAt
	} else if m.SentAt != nil {
		when = *m.SentAt
	}
	b.WriteString("Date: " + when.Format(time.RFC1123Z) + "\n")
	b.WriteString("Subject: " + m.Subject + "\n\n")
	b.WriteString(m.Text)
	return b.String()
}
func (s *Service) forwardAttachments(ctx context.Context, p model.Principal, m model.Message) ([]SendAttachment, error) {
	meta, err := s.Store.ListAttachments(ctx, p, m.ID)
	if err != nil {
		return nil, err
	}
	if len(meta) == 0 {
		return nil, nil
	}
	path := filepath.Join(s.Config.DataDir, filepath.FromSlash(m.RawPath))
	out := make([]SendAttachment, 0, len(meta))
	for _, a := range meta {
		var buf bytes.Buffer
		if err := mailparse.ExtractAttachment(path, a.PartIndex, &buf); err != nil {
			return nil, err
		}
		out = append(out, SendAttachment{Filename: a.Filename, ContentType: a.ContentType, Content: buf.Bytes()})
	}
	return out, nil
}
