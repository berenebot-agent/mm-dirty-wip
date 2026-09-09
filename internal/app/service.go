package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	"gatehouse-mail/internal/transport/netutil"
	_ "gatehouse-mail/internal/transport/resend"
	smtpt "gatehouse-mail/internal/transport/smtp"
)

type Service struct {
	Config        config.Config
	Store         *store.Store
	Hub           *events.Hub
	Log           *slog.Logger
	EncryptionKey []byte
	unroutedLim   *rateLimiter
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
	netutil.SetHosted(cfg.Mode == "hosted")
	return &Service{Config: cfg, Store: st, Hub: hub, Log: slog.Default(), EncryptionKey: key, unroutedLim: newRateLimiter(1, time.Minute)}, nil
}

// auditUnrouted records a rejected unknown-recipient delivery, rate-limited per
// recipient so random spam cannot grow the audit log without bound.
func (s *Service) auditUnrouted(provider, recipient string) {
	if !s.unroutedLim.Allow(recipient) {
		return
	}
	s.Store.Audit(context.Background(), "", provider+".unrouted", recipient)
}

func (s *Service) messagePath() string {
	id := idgen.New("raw")
	return filepath.Join(s.Config.DataDir, "messages", id[4:6], id[6:8], id+".eml")
}

// ResolveInboundBinding implements transport.BindingResolver. It maps an
// envelope recipient to its domain and assigned receive credential, decrypts
// the provider configuration, and returns ErrInboundUnauthorized for unknown or
// unconfigured recipients so every rejection is uniform.
func (s *Service) ResolveInboundBinding(ctx context.Context, provider, recipient string) (transport.InboundBinding, error) {
	b, err := s.Store.ResolveInboundBinding(ctx, provider, recipient)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return transport.InboundBinding{}, transport.ErrInboundUnauthorized
		}
		return transport.InboundBinding{}, err
	}
	cfg, err := s.decryptConfig(b.EncryptedConfig)
	if err != nil {
		return transport.InboundBinding{}, err
	}
	return transport.InboundBinding{
		AccountID:    b.AccountID,
		DomainID:     b.DomainID,
		CredentialID: b.CredentialID,
		Provider:     b.Provider,
		Recipient:    b.Recipient,
		Config:       cfg,
	}, nil
}

// IngestInbound receives a provider webhook: the adapter authenticates it and
// stages the raw MIME, then the shared core persists it. HTTP request details
// and provider secrets stay outside the core.
func (s *Service) IngestInbound(ctx context.Context, provider string, r *http.Request) (model.Message, bool, error) {
	t, ok := transport.LookupInbound(provider)
	if !ok {
		return model.Message{}, false, fmt.Errorf("%w: %s", transport.ErrUnknownProvider, provider)
	}
	tmp := filepath.Join(s.Config.DataDir, "messages", ".tmp", idgen.New("in")+".eml")
	defer os.Remove(tmp)
	msg, binding, err := t.Receive(ctx, r, s, tmp, s.Config.MaxMessageBytes)
	if err != nil {
		return model.Message{}, false, err
	}
	return s.ingestStaged(ctx, provider, msg, binding)
}

// ingestStaged is the protocol-independent mailbox core. It resolves the inbox
// for the canonical envelope recipient, validates the authenticated binding,
// parses MIME, applies sender rules and quota, persists the message/event
// transactionally, then publishes the realtime event. A future SMTP ingress can
// call the same core with its own authorization context.
func (s *Service) ingestStaged(ctx context.Context, provider string, msg transport.InboundMessage, binding transport.InboundBinding) (model.Message, bool, error) {
	inbox, _, err := s.Store.ResolveRecipient(ctx, msg.Recipient)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.auditUnrouted(provider, msg.Recipient)
		}
		return model.Message{}, false, err
	}
	// The inbox must belong to the exact account/domain that was authenticated.
	// This also covers catch-all routing, where the inbox address differs from
	// the original recipient but the domain is the same.
	if inbox.AccountID != binding.AccountID || inbox.DomainID != binding.DomainID {
		return model.Message{}, false, transport.ErrInboundUnauthorized
	}
	parsed, err := mailparse.ParseFile(msg.RawPath)
	if err != nil {
		return model.Message{}, false, fmt.Errorf("parse MIME: %w", err)
	}
	if !inbox.AllowsSender(parsed.From.Address) {
		blockedFrom := model.Address{Name: parsed.From.Name, Address: parsed.From.Address}
		blockedAt := parsed.Date
		if blockedAt.IsZero() {
			blockedAt = time.Now().UTC()
		}
		bm, dup, err := s.Store.CommitBlockedInbound(ctx, store.BlockedRecord{
			AccountID: inbox.AccountID, InboxID: inbox.ID, Provider: provider,
			ProviderDeliveryID: msg.DeliveryID, EnvelopeRecipient: msg.Recipient,
			From: blockedFrom, To: parsed.To,
			Subject: parsed.Subject, Reason: "sender not allowed", SizeBytes: msg.Size, ReceivedAt: blockedAt,
		})
		if err != nil {
			return model.Message{}, false, err
		}
		return model.Message{ID: bm.ID, InboxID: bm.InboxID, Direction: "inbound", From: bm.From, To: bm.To, Subject: bm.Subject, SizeBytes: bm.SizeBytes, ReceivedAt: bm.ReceivedAt, CreatedAt: bm.CreatedAt, Blocked: true}, dup, nil
	}
	final := s.messagePath()
	if err = os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return model.Message{}, false, err
	}
	if err = os.Rename(msg.RawPath, final); err != nil {
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
	m, ev, dup, err := s.Store.CommitInbound(ctx, store.InboundRecord{Inbox: inbox, Provider: provider, ProviderDeliveryID: msg.DeliveryID, ProviderMessageID: firstNonEmpty(msg.ProviderMessageID, parsed.RFCMessageID), EnvelopeRecipient: msg.Recipient, RFCMessageID: parsed.RFCMessageID, InReplyTo: parsed.InReplyTo, References: parsed.References, From: from, To: parsed.To, CC: parsed.CC, EnvelopeTo: []string{msg.Recipient}, Subject: parsed.Subject, Text: parsed.Text, HTML: parsed.HTML, RawPath: filepath.ToSlash(rel), SizeBytes: msg.Size, ReceivedAt: received, Attachments: atts})
	if err != nil {
		_ = os.Remove(final)
		return model.Message{}, false, err
	}
	if dup {
		_ = os.Remove(final)
		return m, true, nil
	}
	s.Log.Info("inbound received", "message_id", m.ID, "from", m.From.Address, "to", m.To)
	s.Hub.Publish(ev)
	return m, false, nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// rateLimiter is a small in-process per-key limiter used to bound audit-log
// growth from repeated rejected deliveries.
type rateLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	m      map[string]*rateEntry
}
type rateEntry struct {
	start time.Time
	n     int
}

func newRateLimiter(max int, w time.Duration) *rateLimiter {
	if max <= 0 {
		max = 1 << 30
	}
	return &rateLimiter{max: max, window: w, m: map[string]*rateEntry{}}
}
func (l *rateLimiter) Allow(k string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	e := l.m[k]
	if e == nil || now.Sub(e.start) >= l.window {
		l.m[k] = &rateEntry{start: now, n: 1}
		return true
	}
	if e.n >= l.max {
		return false
	}
	e.n++
	return true
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
	return s.decryptConfig(c.EncryptedConfig)
}

// SaveInboundCredential creates or updates an account-owned receive
// credential. Provider identity is immutable on update: switching providers
// requires a new credential and a domain reassignment. Blank secret fields on
// update retain the stored value, and required secrets are validated before
// persistence.
func (s *Service) SaveInboundCredential(ctx context.Context, accountID, id, name, provider string, cfg any) (store.InboundCredential, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	t, ok := transport.LookupInbound(provider)
	if !ok {
		return store.InboundCredential{}, fmt.Errorf("%w: %s", transport.ErrUnknownProvider, provider)
	}
	fields := t.ConfigFields()
	var existing store.InboundCredential
	if id != "" {
		e, err := s.Store.GetInboundCredential(ctx, accountID, id)
		if err != nil {
			return store.InboundCredential{}, err
		}
		if e.Provider != provider {
			return store.InboundCredential{}, fmt.Errorf("%w: provider cannot be changed", store.ErrForbidden)
		}
		existing = e
	}
	values, _ := cfg.(map[string]any)
	if values == nil {
		values = map[string]any{}
	}
	for _, f := range fields {
		if !f.Required || !f.Secret {
			continue
		}
		if v, _ := values[f.Name].(string); strings.TrimSpace(v) != "" {
			continue
		}
		// Retain the stored secret when editing the same provider.
		if id != "" {
			if old, err := s.decryptConfig(existing.EncryptedConfig); err == nil {
				if ov, ok := old[f.Name]; ok {
					values[f.Name] = ov
					continue
				}
			}
		}
		return store.InboundCredential{}, fmt.Errorf("%s is required", f.Label)
	}
	if strings.TrimSpace(name) == "" {
		name = t.Description()
	}
	b, err := json.Marshal(values)
	if err != nil {
		return store.InboundCredential{}, err
	}
	enc, err := cryptox.Encrypt(s.EncryptionKey, b)
	if err != nil {
		return store.InboundCredential{}, err
	}
	return s.Store.SaveInboundCredential(ctx, accountID, id, name, provider, enc)
}

func (s *Service) DecryptInboundCredential(c store.InboundCredential) (map[string]any, error) {
	return s.decryptConfig(c.EncryptedConfig)
}

func (s *Service) decryptConfig(encrypted string) (map[string]any, error) {
	b, err := cryptox.Decrypt(s.EncryptionKey, encrypted)
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
	// DraftID, when set, consumes the draft in the same transaction that
	// enqueues the message. It is never accepted from API JSON.
	DraftID string `json:"-"`
}
type SendResult struct {
	Message           model.Message `json:"message"`
	ProviderMessageID string        `json:"provider_message_id"`
}

// Send enqueues an outbound message into the outbox and returns immediately
// with the pending message. The background worker delivers it. If idem is set,
// the idempotency key is reserved at enqueue time and completed on delivery.
func (s *Service) Send(ctx context.Context, p model.Principal, in SendInput, idem string) (result SendResult, err error) {
	if !p.CanOwn(in.InboxID) {
		return SendResult{}, store.ErrForbidden
	}
	if idem != "" {
		// Atomically claim the idempotency key before doing any work so two
		// concurrent requests with the same key cannot both enqueue.
		claimed, mid, rerr := s.Store.IdempotencyReserve(ctx, p.AccountID, idem, in.InboxID)
		if rerr != nil {
			if errors.Is(rerr, store.ErrConflict) {
				return SendResult{}, fmt.Errorf("idempotency key %q is already in flight", idem)
			}
			return SendResult{}, rerr
		}
		if !claimed {
			m, gerr := s.Store.GetMessageByID(ctx, p.AccountID, mid)
			if gerr != nil {
				return SendResult{}, gerr
			}
			// The key is account-wide, so a replay must be scoped to the
			// mailbox it was used for and the caller must own that mailbox.
			if m.InboxID != in.InboxID || !p.CanOwn(m.InboxID) {
				return SendResult{}, store.ErrConflict
			}
			return SendResult{Message: m, ProviderMessageID: m.ProviderMessageID}, nil
		}
		// Release the reservation on any failure so a retry can re-send.
		defer func() {
			if err != nil {
				_ = s.Store.IdempotencyRelease(ctx, p.AccountID, idem)
			}
		}()
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
	// Providers reject messages with no body, so reject empty-body sends up
	// front rather than enqueuing a message that can never be delivered.
	if strings.TrimSpace(in.Text) == "" && strings.TrimSpace(in.HTML) == "" {
		return SendResult{}, fmt.Errorf("message body is required")
	}
	// A missing provider is not fatal: the message is queued and the outbox
	// worker holds it until a provider is assigned to the domain (or account).
	cred, credErr := s.Store.DomainOutboundCredential(ctx, p.AccountID, inbox.DomainID)
	queuedReason := ""
	if credErr != nil {
		if !errors.Is(credErr, store.ErrNoProvider) {
			return SendResult{}, credErr
		}
		queuedReason = "no outbound provider configured for this domain"
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
	// Persist the raw MIME and enqueue the pending message. Delivery happens
	// later in the worker.
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
	m, _, err := s.Store.CommitOutbound(ctx, store.OutboundRecord{Inbox: inbox, Provider: cred.Provider, RFCMessageID: msgID, InReplyTo: inReply, References: refs, From: model.Address{Name: inbox.DisplayName, Address: inbox.Address}, To: to, CC: cleanAddresses(in.CC), BCC: cleanAddresses(in.BCC), Subject: subject, Text: in.Text, HTML: html, RawPath: filepath.ToSlash(rel), SizeBytes: int64(len(raw)), ThreadID: threadID, IdemKey: idem, LastError: queuedReason, DraftID: in.DraftID, Attachments: metadata})
	if err != nil {
		_ = os.Remove(path)
		return SendResult{}, err
	}
	result = SendResult{Message: m}
	return result, nil
}

// SendDraft enqueues a draft and consumes it atomically: the draft rows are
// deleted in the same transaction as the message insert, and its attachment
// files are removed afterwards. The caller supplies the message fields (which
// may include unsaved edits); the draft's saved attachments are added here.
func (s *Service) SendDraft(ctx context.Context, p model.Principal, draftID string, in SendInput, idem string) (SendResult, error) {
	d, err := s.Store.GetDraft(ctx, p, draftID)
	if err != nil {
		return SendResult{}, err
	}
	if !p.CanOwn(d.InboxID) {
		return SendResult{}, store.ErrForbidden
	}
	if in.InboxID == "" {
		in.InboxID = d.InboxID
	}
	if in.InboxID != d.InboxID {
		return SendResult{}, store.ErrForbidden
	}
	atts, err := s.Store.ListDraftAttachments(ctx, p, draftID)
	if err != nil {
		return SendResult{}, err
	}
	paths := make([]string, 0, len(atts))
	for _, a := range atts {
		data, rerr := os.ReadFile(filepath.Join(s.Config.DataDir, filepath.FromSlash(a.RawPath)))
		if rerr != nil {
			return SendResult{}, rerr
		}
		in.Attachments = append(in.Attachments, SendAttachment{Filename: a.Filename, ContentType: a.ContentType, Content: data})
		paths = append(paths, a.RawPath)
	}
	in.DraftID = draftID
	res, err := s.Send(ctx, p, in, idem)
	if err != nil {
		return SendResult{}, err
	}
	// The draft rows were consumed with the enqueue; remove the files now.
	for _, path := range paths {
		_ = os.Remove(filepath.Join(s.Config.DataDir, filepath.FromSlash(path)))
	}
	return res, nil
}

// Deliver performs the actual provider send for a pending outbound message and
// marks it sent or failed. It is called by the outbox worker with the owner of
// the claim so a stale worker cannot deliver a message re-claimed elsewhere.
func (s *Service) Deliver(ctx context.Context, accountID, msgID, owner string) error {
	m, err := s.Store.GetMessageByID(ctx, accountID, msgID)
	if err != nil {
		return err
	}
	if m.Direction != "outbound" || m.Status != "pending" {
		return nil
	}
	if owner != "" {
		current, err := s.Store.MessageClaimOwner(ctx, accountID, msgID)
		if err != nil {
			return err
		}
		if current != owner {
			return fmt.Errorf("message %s is claimed by another worker", msgID)
		}
	}
	// Outcome bookkeeping must survive cancellation of the delivery context but
	// stay bounded, so a failed send is always recorded.
	outcomeCtx, cancelOutcome := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancelOutcome()
	cred, err := s.Store.OutboundCredentialForMessage(ctx, m.AccountID, m.ID)
	if err != nil {
		if errors.Is(err, store.ErrNoProvider) {
			return s.hold(outcomeCtx, m)
		}
		return s.fail(outcomeCtx, m, err, "", "")
	}
	cfg, err := s.DecryptOutboundCredential(cred)
	if err != nil {
		return s.fail(outcomeCtx, m, err, cred.ID, cred.Provider)
	}
	raw, err := os.ReadFile(filepath.Join(s.Config.DataDir, filepath.FromSlash(m.RawPath)))
	if err != nil {
		return s.fail(outcomeCtx, m, err, cred.ID, cred.Provider)
	}
	provider, ok := transport.LookupOutbound(cred.Provider)
	if !ok {
		return s.fail(outcomeCtx, m, fmt.Errorf("%w: %s", transport.ErrUnknownProvider, cred.Provider), cred.ID, cred.Provider)
	}
	outbound := transport.OutboundMessage{
		FromName:    m.From.Name,
		FromAddress: m.From.Address,
		To:          m.To,
		CC:          m.CC,
		BCC:         m.BCC,
		Subject:     m.Subject,
		Text:        m.Text,
		HTML:        m.HTML,
		MessageID:   m.RFCMessageID,
		InReplyTo:   m.InReplyTo,
		References:  m.References,
		RawMIME:     raw,
	}
	// HTTP adapters build their request from structured fields, so the
	// attachment bytes must be reconstructed from the stored MIME. Transports
	// that send the raw MIME directly (SMTP) skip this.
	if !prefersRawMIME(provider) {
		atts, aerr := s.deliveryAttachments(m)
		if aerr != nil {
			return s.fail(outcomeCtx, m, aerr, cred.ID, cred.Provider)
		}
		outbound.Attachments = atts
	}
	providerResult, err := provider.Send(ctx, cfg, outbound)
	if err != nil {
		return s.fail(outcomeCtx, m, err, cred.ID, cred.Provider)
	}
	_, ev, err := s.Store.MarkSent(outcomeCtx, m.AccountID, m.ID, providerResult.ProviderMessageID, cred.ID, cred.Provider)
	if err != nil {
		return err
	}
	s.Log.Info("outbound sent", "message_id", m.ID, "from", m.From.Address, "to", m.To)
	s.Hub.Publish(ev)
	return nil
}

// hold defers a pending message that has no outbound provider yet, without
// counting an attempt. The message stays queued and delivers once a provider is
// assigned to its domain (or account).
func (s *Service) hold(ctx context.Context, m model.Message) error {
	return s.Store.HoldPending(ctx, m.AccountID, m.ID, "no outbound provider configured for this domain", time.Now().UTC().Add(5*time.Minute))
}

// fail records a failed delivery attempt with exponential backoff, returning
// the error so the worker can log it. credID and provider attribute the attempt
// to a credential when one was resolved; they are empty when no active
// credential was available.
func (s *Service) fail(ctx context.Context, m model.Message, err error, credID, provider string) error {
	// A permanent provider error will never succeed on retry, so fail the
	// message immediately instead of retrying with backoff.
	if transport.IsPermanent(err) {
		_, ferr := s.Store.MarkFailed(ctx, m.AccountID, m.ID, err.Error(), time.Time{}, 1, credID, provider)
		if ferr != nil {
			return ferr
		}
		return err
	}
	backoff := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 4 * time.Hour}
	attempt := m.Attempts
	if attempt < 0 || attempt >= len(backoff) {
		attempt = len(backoff) - 1
	}
	next := time.Now().UTC().Add(backoff[attempt])
	_, ferr := s.Store.MarkFailed(ctx, m.AccountID, m.ID, err.Error(), next, len(backoff)+1, credID, provider)
	if ferr != nil {
		return ferr
	}
	return err
}

// WaitForDelivery blocks until the message reaches a terminal state (sent or
// failed) or the timeout elapses. It is used by the synchronous ?wait=true path.
func (s *Service) WaitForDelivery(ctx context.Context, accountID, msgID string, timeout time.Duration) (model.Message, error) {
	deadline := time.Now().Add(timeout)
	for {
		m, err := s.Store.GetMessageByID(ctx, accountID, msgID)
		if err != nil {
			return model.Message{}, err
		}
		if m.Status == "sent" || m.Status == "failed" {
			return m, nil
		}
		if time.Now().After(deadline) {
			return m, fmt.Errorf("delivery timed out after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return m, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// AccountIDForMessage resolves the account id for a message id. The worker
// claims messages without a principal, so it needs a way to look up the account.
func (s *Service) AccountIDForMessage(ctx context.Context, msgID string) (string, error) {
	return s.Store.MessageAccountID(ctx, msgID)
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
	var out []SendAttachment
	err = mailparse.ExtractAllAttachments(path, func(a mailparse.Attachment, r io.Reader) error {
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, r); err != nil {
			return err
		}
		out = append(out, SendAttachment{Filename: a.Filename, ContentType: a.ContentType, Content: buf.Bytes()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func prefersRawMIME(t transport.OutboundTransport) bool {
	if r, ok := t.(transport.RawMIMEProvider); ok {
		return r.PreferRawMIME()
	}
	return false
}

// deliveryAttachments reconstructs attachment bytes from the persisted MIME so
// HTTP adapters send the same content the stored message describes.
func (s *Service) deliveryAttachments(m model.Message) ([]transport.OutboundAttachment, error) {
	path := filepath.Join(s.Config.DataDir, filepath.FromSlash(m.RawPath))
	var out []transport.OutboundAttachment
	err := mailparse.ExtractAllAttachments(path, func(a mailparse.Attachment, r io.Reader) error {
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, r); err != nil {
			return err
		}
		out = append(out, transport.OutboundAttachment{Filename: a.Filename, ContentType: a.ContentType, Content: buf.Bytes()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
