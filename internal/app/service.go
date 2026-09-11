package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/mail"
	"os"
	"path/filepath"
	"strconv"
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
	_ "gatehouse-mail/internal/transport/smtp"
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
	netutil.SetRequirePublic(cfg.RequirePublicOutbound())
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
	inbox, route, err := s.Store.ResolveRecipient(ctx, msg.Recipient)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.auditUnrouted(provider, msg.Recipient)
		}
		return model.Message{}, false, err
	}
	// The resolved inbox must belong to the account that was authenticated.
	// Exact and catch-all matches must also stay on the authenticated domain;
	// only an explicit alias may route across domains within the same account.
	if inbox.AccountID != binding.AccountID {
		return model.Message{}, false, transport.ErrInboundUnauthorized
	}
	if route != store.RouteAlias && inbox.DomainID != binding.DomainID {
		return model.Message{}, false, transport.ErrInboundUnauthorized
	}
	parsed, err := mailparse.ParseFile(msg.RawPath)
	if err != nil {
		return model.Message{}, false, fmt.Errorf("parse MIME: %w", err)
	}
	// A strict approval control subject is consumed as workflow input before
	// ordinary delivery, so the token never becomes mailbox content. It is
	// handed to the control handler regardless of the sender allow-list because
	// the handler validates the live token and the exact stored approver, and
	// because an inbox's approver setting may have changed after the request was
	// created. Invalid control mail is consumed too; only its outcome is
	// recorded.
	if looksLikeControl(parsed.Subject) {
		return model.Message{}, false, s.handleControlMessage(ctx, provider, msg, inbox, parsed)
	}
	if !inbox.AllowsInbound(parsed.From.Address) {
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

// ErrInvalidConfig wraps every user-supplied provider configuration validation
// error. HTTP callers map it to a 400 response; any other error returned by the
// Save methods is an internal fault and must be redacted.
var ErrInvalidConfig = errors.New("invalid config")

// invalidConfig builds a validation error without ever including a secret
// value: only field labels and option names are reported.
func invalidConfig(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidConfig, fmt.Sprintf(format, args...))
}

func normalizeProvider(provider string) string {
	return strings.ToLower(strings.TrimSpace(provider))
}

// unknownProvider marks an unrecognised provider as both a validation fault and
// a transport lookup failure so either sentinel maps to the same 400 response.
func unknownProvider(provider string) error {
	return fmt.Errorf("%w: %w %q", ErrInvalidConfig, transport.ErrUnknownProvider, provider)
}

func outboundConfigFields(provider string) ([]transport.ConfigField, error) {
	t, ok := transport.LookupOutbound(provider)
	if !ok {
		return nil, unknownProvider(provider)
	}
	schema, ok := t.(transport.ConfigSchemaProvider)
	if !ok {
		return nil, fmt.Errorf("outbound provider %q has no configuration schema", provider)
	}
	return schema.ConfigFields(), nil
}

func inboundConfigFields(provider string) ([]transport.ConfigField, error) {
	t, ok := transport.LookupInbound(provider)
	if !ok {
		return nil, unknownProvider(provider)
	}
	schema, ok := t.(transport.ConfigSchemaProvider)
	if !ok {
		return nil, fmt.Errorf("inbound provider %q has no configuration schema", provider)
	}
	return schema.ConfigFields(), nil
}

// SaveDomainSendingConfig validates, encrypts and persists the single optional
// sending configuration for a domain. Same-provider blank secret fields retain
// their stored value; every non-secret field is a whole-config value, so an
// omitted field takes the provider default or is a required-field error (never
// a merge). A provider change never reuses old fields or secrets. CAS protects
// against concurrent rotation: on conflict store.ErrConflict is returned and
// the caller must reload rather than overwrite.
func (s *Service) SaveDomainSendingConfig(ctx context.Context, accountID, domainID, provider string, cfg map[string]any) (store.DomainSendingConfig, error) {
	// Resolve the domain and any current config before touching the provider
	// schema: a missing or foreign domain must be ErrNotFound even when the
	// requested provider is unknown.
	existing, exists, err := s.getSendingConfig(ctx, accountID, domainID)
	if err != nil {
		return store.DomainSendingConfig{}, err
	}
	provider = normalizeProvider(provider)
	fields, err := outboundConfigFields(provider)
	if err != nil {
		return store.DomainSendingConfig{}, err
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	sameProvider := exists && strings.EqualFold(existing.Provider, provider)
	var old map[string]any
	if sameProvider {
		if old, err = s.DecryptDomainSendingConfig(existing); err != nil {
			return store.DomainSendingConfig{}, err
		}
	}
	merged, err := validateConfig(fields, cfg, old, sameProvider)
	if err != nil {
		return store.DomainSendingConfig{}, err
	}
	if err := s.validateProviderBase(merged); err != nil {
		return store.DomainSendingConfig{}, err
	}
	enc, err := s.encryptConfig(merged)
	if err != nil {
		return store.DomainSendingConfig{}, err
	}
	expected := store.ConfigVersion{}
	if exists {
		expected = store.ConfigVersion{ID: existing.ID, Revision: existing.Revision}
	}
	return s.Store.SaveDomainSendingConfig(ctx, accountID, domainID, provider, enc, expected)
}

// SaveDomainReceivingConfig validates, encrypts and persists the single
// optional receiving configuration for a domain. Generated secrets (Cloudflare
// Worker shared secret) are minted only for a new config, a provider change, or
// an explicit regenerate=true for the currently configured provider, and are
// returned exactly once after the config is durably saved.
func (s *Service) SaveDomainReceivingConfig(ctx context.Context, accountID, domainID, provider string, cfg map[string]any, regenerate bool) (store.DomainReceivingConfig, map[string]string, error) {
	// Resolve the domain and any current config before touching the provider
	// schema: a missing or foreign domain must be ErrNotFound even when the
	// requested provider is unknown.
	existing, exists, err := s.getReceivingConfig(ctx, accountID, domainID)
	if err != nil {
		return store.DomainReceivingConfig{}, nil, err
	}
	provider = normalizeProvider(provider)
	fields, err := inboundConfigFields(provider)
	if err != nil {
		return store.DomainReceivingConfig{}, nil, err
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	sameProvider := exists && strings.EqualFold(existing.Provider, provider)
	var old map[string]any
	if sameProvider {
		if old, err = s.DecryptDomainReceivingConfig(existing); err != nil {
			return store.DomainReceivingConfig{}, nil, err
		}
	}
	generated, err := applyGeneratedSecrets(fields, cfg, sameProvider, regenerate)
	if err != nil {
		return store.DomainReceivingConfig{}, nil, err
	}
	merged, err := validateConfig(fields, cfg, old, sameProvider)
	if err != nil {
		return store.DomainReceivingConfig{}, nil, err
	}
	if err := s.validateProviderBase(merged); err != nil {
		return store.DomainReceivingConfig{}, nil, err
	}
	enc, err := s.encryptConfig(merged)
	if err != nil {
		return store.DomainReceivingConfig{}, nil, err
	}
	expected := store.ConfigVersion{}
	if exists {
		expected = store.ConfigVersion{ID: existing.ID, Revision: existing.Revision}
	}
	saved, err := s.Store.SaveDomainReceivingConfig(ctx, accountID, domainID, provider, enc, expected)
	if err != nil {
		return store.DomainReceivingConfig{}, nil, err
	}
	if len(generated) == 0 {
		return saved, nil, nil
	}
	return saved, generated, nil
}

func (s *Service) DecryptDomainSendingConfig(c store.DomainSendingConfig) (map[string]any, error) {
	return s.decryptConfig(c.EncryptedConfig)
}

func (s *Service) DecryptDomainReceivingConfig(c store.DomainReceivingConfig) (map[string]any, error) {
	return s.decryptConfig(c.EncryptedConfig)
}

// getSendingConfig distinguishes "domain has no sending config" (ErrNoProvider)
// from "domain does not exist or is foreign to the account" (other error).
func (s *Service) getSendingConfig(ctx context.Context, accountID, domainID string) (store.DomainSendingConfig, bool, error) {
	c, err := s.Store.GetDomainSendingConfig(ctx, accountID, domainID)
	if err == nil {
		return c, true, nil
	}
	if errors.Is(err, store.ErrNoProvider) {
		return store.DomainSendingConfig{}, false, nil
	}
	return store.DomainSendingConfig{}, false, err
}

func (s *Service) getReceivingConfig(ctx context.Context, accountID, domainID string) (store.DomainReceivingConfig, bool, error) {
	c, err := s.Store.GetDomainReceivingConfig(ctx, accountID, domainID)
	if err == nil {
		return c, true, nil
	}
	if errors.Is(err, store.ErrNoProvider) {
		return store.DomainReceivingConfig{}, false, nil
	}
	return store.DomainReceivingConfig{}, false, err
}

// applyGeneratedSecrets inserts a fresh generated secret for each schema field
// marked Generated that has no effective value. It retains the stored secret on
// a same-provider save unless regenerate is requested and rejects regenerate
// requests that are not for the currently configured provider or that also
// supply a value. The returned map is only surfaced after successful storage.
func applyGeneratedSecrets(fields []transport.ConfigField, cfg map[string]any, sameProvider, regenerate bool) (map[string]string, error) {
	var generatedFields []transport.ConfigField
	for _, f := range fields {
		if f.Generated {
			generatedFields = append(generatedFields, f)
		}
	}
	if len(generatedFields) == 0 {
		if regenerate {
			return nil, invalidConfig("provider has no generated secret to regenerate")
		}
		return nil, nil
	}
	if regenerate && !sameProvider {
		return nil, invalidConfig("cannot regenerate a secret for an unconfigured provider")
	}
	generated := map[string]string{}
	for _, f := range generatedFields {
		raw, present := cfg[f.Name]
		value, isString := raw.(string)
		if present && !isString {
			return nil, invalidConfig("%s must be a string", f.Label)
		}
		if strings.TrimSpace(value) != "" {
			if regenerate {
				return nil, invalidConfig("%s cannot be supplied when regenerating", f.Label)
			}
			continue
		}
		if regenerate || !sameProvider {
			secret, err := auth.RandomToken(32)
			if err != nil {
				return nil, err
			}
			cfg[f.Name] = secret
			generated[f.Name] = secret
		}
	}
	return generated, nil
}

// validateProviderBase rejects a provider API base that is not HTTPS or names a
// non-public IP literal when public-destination enforcement is active. Host
// names are resolved and checked at dial time, not here.
func (s *Service) validateProviderBase(values map[string]any) error {
	base, _ := values["api_base"].(string)
	if strings.TrimSpace(base) == "" {
		return nil
	}
	if err := netutil.ValidateBaseURL(base); err != nil {
		return invalidConfig("Base URL: %s", err.Error())
	}
	return nil
}

// validateConfig expands incoming values against a provider schema. existing
// holds the prior decrypted values for same-provider secret retention (nil when
// there is no prior same-provider config). It rejects unknown keys, wrong
// types, invalid select options and non-integral numbers before returning a
// complete, encrypted-ready config. Secret values never appear in errors.
func validateConfig(fields []transport.ConfigField, incoming, existing map[string]any, keepSecrets bool) (map[string]any, error) {
	known := make(map[string]transport.ConfigField, len(fields))
	for _, f := range fields {
		known[f.Name] = f
	}
	for key := range incoming {
		if _, ok := known[key]; !ok {
			return nil, invalidConfig("unknown option %q", key)
		}
	}
	out := make(map[string]any, len(fields))
	for _, f := range fields {
		raw, present := incoming[f.Name]
		if f.Secret {
			value, isString := raw.(string)
			if present && !isString {
				return nil, invalidConfig("%s must be a string", f.Label)
			}
			if !present || strings.TrimSpace(value) == "" {
				if keepSecrets && existing != nil {
					if old, ok := existing[f.Name]; ok {
						if ov, _ := old.(string); strings.TrimSpace(ov) != "" {
							out[f.Name] = ov
							continue
						}
					}
				}
				if f.Required {
					return nil, invalidConfig("%s is required", f.Label)
				}
				continue
			}
			out[f.Name] = value
			continue
		}
		value, err := resolveNonSecret(f, raw, present)
		if err != nil {
			return nil, err
		}
		if value != nil {
			out[f.Name] = value
		}
	}
	return out, nil
}

// resolveNonSecret applies whole-config semantics to a non-secret field: a
// missing or blank value takes the provider default, is a required-field error,
// or is omitted.
func resolveNonSecret(f transport.ConfigField, raw any, present bool) (any, error) {
	if present {
		if str, ok := raw.(string); ok && strings.TrimSpace(str) == "" {
			present = false
		}
	}
	if !present {
		if strings.TrimSpace(f.Default) != "" {
			return defaultFieldValue(f)
		}
		if f.Required {
			return nil, invalidConfig("%s is required", f.Label)
		}
		return nil, nil
	}
	return coerceFieldValue(f, raw)
}

func defaultFieldValue(f transport.ConfigField) (any, error) {
	if f.Type == "number" {
		n, err := strconv.Atoi(strings.TrimSpace(f.Default))
		if err != nil {
			return nil, fmt.Errorf("provider default for %s is not a number", f.Name)
		}
		return n, nil
	}
	return strings.TrimSpace(f.Default), nil
}

func coerceFieldValue(f transport.ConfigField, raw any) (any, error) {
	switch f.Type {
	case "number":
		n, err := wholeNumber(raw)
		if err != nil {
			return nil, invalidConfig("%s must be a whole number", f.Label)
		}
		if f.Name == "port" && (n < 1 || n > 65535) {
			return nil, invalidConfig("%s must be between 1 and 65535", f.Label)
		}
		return n, nil
	case "select":
		value, ok := raw.(string)
		if !ok {
			return nil, invalidConfig("%s must be a string", f.Label)
		}
		value = strings.TrimSpace(value)
		for _, opt := range f.Options {
			if opt.Value == value {
				return value, nil
			}
		}
		return nil, invalidConfig("invalid value for %s", f.Label)
	default:
		value, ok := raw.(string)
		if !ok {
			return nil, invalidConfig("%s must be a string", f.Label)
		}
		return strings.TrimSpace(value), nil
	}
}

func wholeNumber(raw any) (int, error) {
	switch v := raw.(type) {
	case int:
		return v, nil
	case int32:
		return int(v), nil
	case int64:
		return int(v), nil
	case float32:
		return wholeFloat(float64(v))
	case float64:
		return wholeFloat(v)
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, err
		}
		return int(n), nil
	default:
		return 0, fmt.Errorf("not a number")
	}
}

func wholeFloat(v float64) (int, error) {
	if math.IsNaN(v) || math.IsInf(v, 0) || v != math.Trunc(v) {
		return 0, fmt.Errorf("not a whole number")
	}
	return int(v), nil
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

func (s *Service) encryptConfig(values map[string]any) (string, error) {
	b, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	return cryptox.Encrypt(s.EncryptionKey, b)
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
	// SendRequestID, when set, atomically authorizes the referenced draft send
	// request as part of this send. It is never accepted from API JSON.
	SendRequestID    string `json:"-"`
	DecisionActor    string `json:"-"`
	DecisionActorID  string `json:"-"`
	DecisionMethod   string `json:"-"`
	DecisionFeedback string `json:"-"`
	// ClientLabel/ClientID snapshot the credential that enqueued the message.
	// They are resolved from the principal and never accepted from API JSON.
	ClientLabel string `json:"-"`
	ClientID    string `json:"-"`
}

// clientIdentity resolves the client recorded on a sent message. A web-UI
// session is recorded as "UI"; an API key or Hermes credential uses its stable
// name; a principal with no credential (for example an email approval) records
// nothing.
func clientIdentity(p model.Principal, actor store.ActorIdentity) (label, id string) {
	if p.ViaSession {
		return "UI", p.UserID
	}
	if p.APIKeyID != "" {
		return actor.Label, actor.APIKeyID
	}
	return "", ""
}

// setClient fills the client fields of a send from the request principal.
func (s *Service) setClient(ctx context.Context, p model.Principal, in *SendInput) error {
	actor, err := s.Store.ActorIdentity(ctx, p)
	if err != nil {
		return err
	}
	in.ClientLabel, in.ClientID = clientIdentity(p, actor)
	return nil
}

type SendResult struct {
	Message           model.Message `json:"message"`
	ProviderMessageID string        `json:"provider_message_id"`
}

// Send enqueues an outbound message into the outbox and returns immediately
// with the pending message. The background worker delivers it. If idem is set,
// the idempotency key is reserved at enqueue time and completed on delivery.
func (s *Service) Send(ctx context.Context, p model.Principal, in SendInput, idem string) (SendResult, error) {
	if !p.CanOwn(in.InboxID) {
		return SendResult{}, store.ErrForbidden
	}
	if err := s.setClient(ctx, p, &in); err != nil {
		return SendResult{}, err
	}
	return s.send(ctx, p.AccountID, in, idem)
}

// send is the principal-free outbound core. The external email approval path
// calls it after validating the token and approver; mailbox ownership was
// already established by the send request itself.
func (s *Service) send(ctx context.Context, accountID string, in SendInput, idem string) (result SendResult, err error) {
	if idem != "" {
		// Atomically claim the idempotency key before doing any work so two
		// concurrent requests with the same key cannot both enqueue.
		claimed, mid, rerr := s.Store.IdempotencyReserve(ctx, accountID, idem, in.InboxID)
		if rerr != nil {
			if errors.Is(rerr, store.ErrConflict) {
				return SendResult{}, fmt.Errorf("idempotency key %q is already in flight", idem)
			}
			return SendResult{}, rerr
		}
		if !claimed {
			m, gerr := s.Store.GetMessageByID(ctx, accountID, mid)
			if gerr != nil {
				return SendResult{}, gerr
			}
			// The key is account-wide, so a replay must be scoped to the
			// mailbox it was used for.
			if m.InboxID != in.InboxID {
				return SendResult{}, store.ErrConflict
			}
			return SendResult{Message: m, ProviderMessageID: m.ProviderMessageID}, nil
		}
		// Release the reservation on any failure so a retry can re-send.
		defer func() {
			if err != nil {
				_ = s.Store.IdempotencyRelease(ctx, accountID, idem)
			}
		}()
	}
	inbox, err := s.Store.GetInboxInternal(ctx, accountID, in.InboxID)
	if err != nil {
		return SendResult{}, err
	}
	var threadID, inReply string
	refs := []string{}
	to, err := cleanAddresses(in.To)
	if err != nil {
		return SendResult{}, err
	}
	cc, err := cleanAddresses(in.CC)
	if err != nil {
		return SendResult{}, err
	}
	bcc, err := cleanAddresses(in.BCC)
	if err != nil {
		return SendResult{}, err
	}
	subject := strings.TrimSpace(in.Subject)
	if in.ReplyToMessageID != "" {
		target, err := s.Store.GetMessageByID(ctx, accountID, in.ReplyToMessageID)
		if err != nil {
			return SendResult{}, err
		}
		if target.InboxID != inbox.ID || target.Internal {
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
		target, err := s.Store.GetMessageByID(ctx, accountID, in.ForwardOfMessageID)
		if err != nil {
			return SendResult{}, err
		}
		if target.InboxID != inbox.ID || target.Internal {
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
		carried, err := s.forwardAttachments(ctx, accountID, target)
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
	// worker holds it until a provider is configured for the domain.
	sending, cfgErr := s.Store.GetDomainSendingConfig(ctx, accountID, inbox.DomainID)
	queuedReason := ""
	if cfgErr != nil {
		if !errors.Is(cfgErr, store.ErrNoProvider) {
			return SendResult{}, cfgErr
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
	raw, err := mailparse.BuildMessage(mailparse.Address{Name: inbox.DisplayName, Address: inbox.Address}, to, cc, bcc, subject, in.Text, html, msgID, inReply, refs, now, attachmentParts(attachments))
	if err != nil {
		return SendResult{}, err
	}
	acc, err := s.Store.GetAccount(ctx, accountID)
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
	m, draftEvent, err := s.Store.CommitOutbound(ctx, store.OutboundRecord{Inbox: inbox, Provider: sending.Provider, RFCMessageID: msgID, InReplyTo: inReply, References: refs, From: model.Address{Name: inbox.DisplayName, Address: inbox.Address}, To: to, CC: cc, BCC: bcc, Subject: subject, Text: in.Text, HTML: html, RawPath: filepath.ToSlash(rel), SizeBytes: int64(len(raw)), ThreadID: threadID, IdemKey: idem, LastError: queuedReason, DraftID: in.DraftID, ClientLabel: in.ClientLabel, ClientID: in.ClientID, Attachments: metadata, SendRequestID: in.SendRequestID, DecisionActor: in.DecisionActor, DecisionActorID: in.DecisionActorID, DecisionMethod: in.DecisionMethod, DecisionFeedback: in.DecisionFeedback})
	if err != nil {
		_ = os.Remove(path)
		return SendResult{}, err
	}
	if draftEvent.Type != "" {
		s.Hub.Publish(draftEvent)
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
	// A direct owner send of a pending draft authorizes the outstanding request
	// as it enqueues; the approval and the message land in one transaction.
	if in.SendRequestID == "" && d.SendRequest != nil && d.SendRequest.Status == model.SendRequestPending {
		actor, aerr := s.Store.ActorIdentity(ctx, p)
		if aerr != nil {
			return SendResult{}, aerr
		}
		in.SendRequestID = d.SendRequest.ID
		in.DecisionActor = actor.Label
		in.DecisionActorID = actor.ID()
		if p.ViaSession {
			in.DecisionMethod = model.DecisionMethodUI
		} else {
			in.DecisionMethod = model.DecisionMethodAPI
		}
	}
	if err := s.setClient(ctx, p, &in); err != nil {
		return SendResult{}, err
	}
	return s.sendDraftCore(ctx, p.AccountID, d, in, idem)
}

// sendDraftCore adds the draft's stored attachments to the send and enqueues it
// without a principal. It is shared by the owner send path and the validated
// external approval path. The draft is consumed atomically at enqueue.
func (s *Service) sendDraftCore(ctx context.Context, accountID string, d model.Draft, in SendInput, idem string) (SendResult, error) {
	atts, err := s.Store.ListDraftAttachmentsInternal(ctx, accountID, d.ID)
	if err != nil {
		return SendResult{}, err
	}
	paths := make([]string, 0, len(atts))
	for _, a := range atts {
		data, rerr := os.ReadFile(filepath.Join(s.Config.DataDir, filepath.FromSlash(a.RawPath)))
		if rerr != nil {
			return SendResult{}, rerr
		}
		// The frozen fingerprint must match the bytes actually sent, even for a
		// direct owner send of a draft whose file changed out of band.
		if strings.TrimSpace(a.ContentHash) != "" {
			sum := sha256.Sum256(data)
			if !equalTokenHash(a.ContentHash, hex.EncodeToString(sum[:])) {
				return SendResult{}, store.ErrConflict
			}
		}
		in.Attachments = append(in.Attachments, SendAttachment{Filename: a.Filename, ContentType: a.ContentType, Content: data})
		paths = append(paths, a.RawPath)
	}
	in.DraftID = d.ID
	res, err := s.send(ctx, accountID, in, idem)
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
	sending, err := s.Store.DomainSendingConfigForMessage(ctx, m.AccountID, m.ID)
	if err != nil {
		if errors.Is(err, store.ErrNoProvider) {
			return s.hold(outcomeCtx, m)
		}
		return s.fail(outcomeCtx, m, err, "")
	}
	cfg, err := s.DecryptDomainSendingConfig(sending)
	if err != nil {
		return s.fail(outcomeCtx, m, err, sending.Provider)
	}
	raw, err := os.ReadFile(filepath.Join(s.Config.DataDir, filepath.FromSlash(m.RawPath)))
	if err != nil {
		return s.fail(outcomeCtx, m, err, sending.Provider)
	}
	provider, ok := transport.LookupOutbound(sending.Provider)
	if !ok {
		return s.fail(outcomeCtx, m, fmt.Errorf("%w: %s", transport.ErrUnknownProvider, sending.Provider), sending.Provider)
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
			return s.fail(outcomeCtx, m, aerr, sending.Provider)
		}
		outbound.Attachments = atts
	}
	providerResult, err := provider.Send(ctx, cfg, outbound)
	if err != nil {
		return s.fail(outcomeCtx, m, err, sending.Provider)
	}
	_, events, err := s.Store.MarkSent(outcomeCtx, m.AccountID, m.ID, providerResult.ProviderMessageID, sending.Provider)
	if err != nil {
		return err
	}
	s.Log.Info("outbound sent", "message_id", m.ID, "from", m.From.Address, "to", m.To)
	for _, ev := range events {
		s.Hub.Publish(ev)
	}
	return nil
}

// hold defers a pending message that has no outbound provider yet, without
// counting an attempt. The message stays queued and delivers once a provider is
// assigned to its domain (or account).
func (s *Service) hold(ctx context.Context, m model.Message) error {
	return s.Store.HoldPending(ctx, m.AccountID, m.ID, "no outbound provider configured for this domain", time.Now().UTC().Add(5*time.Minute))
}

// fail records a failed delivery attempt with exponential backoff, returning
// the error so the worker can log it. provider attributes the attempt to the
// provider snapshot actually used; it is empty when no config was available.
// The attempt's domain is derived inside the store from the message and inbox,
// so the config may be deleted while a send is in flight without losing the
// outcome.
func (s *Service) fail(ctx context.Context, m model.Message, err error, provider string) error {
	// A permanent provider error will never succeed on retry, so fail the
	// message immediately instead of retrying with backoff.
	if transport.IsPermanent(err) {
		_, events, ferr := s.Store.MarkFailed(ctx, m.AccountID, m.ID, err.Error(), time.Time{}, 1, provider)
		if ferr != nil {
			return ferr
		}
		for _, ev := range events {
			s.Hub.Publish(ev)
		}
		return err
	}
	backoff := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 4 * time.Hour}
	attempt := m.Attempts
	if attempt < 0 || attempt >= len(backoff) {
		attempt = len(backoff) - 1
	}
	next := time.Now().UTC().Add(backoff[attempt])
	_, events, ferr := s.Store.MarkFailed(ctx, m.AccountID, m.ID, err.Error(), next, len(backoff)+1, provider)
	if ferr != nil {
		return ferr
	}
	for _, ev := range events {
		s.Hub.Publish(ev)
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

func cleanAddresses(in []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, v := range in {
		v = strings.TrimSpace(v)
		// Reject CR/LF and other control characters up front. Falling back to
		// the raw value would let a caller smuggle extra MIME headers into the
		// outbound message (header injection).
		if mailparse.HasControlChars(v) {
			return nil, fmt.Errorf("address contains control characters")
		}
		if a, err := mail.ParseAddress(v); err == nil {
			v = strings.ToLower(a.Address)
		} else {
			v = strings.ToLower(v)
		}
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out, nil
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
func (s *Service) forwardAttachments(ctx context.Context, accountID string, m model.Message) ([]SendAttachment, error) {
	meta, err := s.Store.ListAttachmentsInternal(ctx, accountID, m.ID)
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
