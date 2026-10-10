package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
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

	"github.com/dellarb/mailmoose/internal/auth"
	"github.com/dellarb/mailmoose/internal/config"
	"github.com/dellarb/mailmoose/internal/cryptox"
	"github.com/dellarb/mailmoose/internal/events"
	"github.com/dellarb/mailmoose/internal/idgen"
	"github.com/dellarb/mailmoose/internal/mailparse"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/internal/safepath"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport"
	_ "github.com/dellarb/mailmoose/internal/transport/brevo"
	_ "github.com/dellarb/mailmoose/internal/transport/cloudflare"
	_ "github.com/dellarb/mailmoose/internal/transport/mailgun"
	_ "github.com/dellarb/mailmoose/internal/transport/mx"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
	"github.com/dellarb/mailmoose/internal/transport/netutil"
	_ "github.com/dellarb/mailmoose/internal/transport/postmark"
	_ "github.com/dellarb/mailmoose/internal/transport/remotemx"
	_ "github.com/dellarb/mailmoose/internal/transport/resend"
	// SendGrid is deliberately NOT registered: its inbound connector is complete
	// and unit-tested, but the provider account could not be obtained for
	// end-to-end verification (Twilio compliance refused activation), so it is
	// hidden from the receiving provider menus until it can be tested. The
	// adapter and its tests are untouched — re-enable by uncommenting this
	// import and restoring "sendgrid" in the provider-order test's want list.
	// _ "github.com/dellarb/mailmoose/internal/transport/sendgrid"
	_ "github.com/dellarb/mailmoose/internal/transport/smtp"
)

// RemoteMessageResolver resolves a remote (standalone) message as a reply or
// forward source for a send that originates in the common mailbox boundary. A
// remote message's body lives only on the server, so a forward fetches and parses
// it transiently; the returned Text/attachments are used to build the outbound
// message and are never archived.
type RemoteMessageResolver interface {
	// ResolveRemoteReply resolves a remote message's header metadata as a
	// model.Message suitable for a reply (its ThreadKey, Message-ID, References
	// and From/To). It returns ok=false when the id is not a remote message of the
	// account.
	ResolveRemoteReply(ctx context.Context, accountID, id string) (model.Message, bool, error)
	// ResolveRemoteForward resolves a remote message for a forward, fetching and
	// parsing its body transiently so Text/HTML and attachments can be carried.
	// It returns ok=false when the id is not a remote message of the account.
	ResolveRemoteForward(ctx context.Context, accountID, id string) (model.Message, []SendAttachment, bool, error)
}

type Service struct {
	Config config.Config
	Store  *store.Store
	Hub    *events.Hub
	DialMX *mxdial.Manager
	// AntlerEndpoints resolves the Antler MX receiver manifest at setup-save
	// time. cmd/server installs the live GitHub resolver; when nil (tests) the
	// embedded manifest is used.
	AntlerEndpoints AntlerEndpointResolver
	// MXRuntime is the process-owned MX receiver controller. cmd/server sets it
	// before serving; when nil (tests, and deployments that wire no receiver)
	// settings persist and reconcile on the next start, and status reports the
	// persisted configuration without a live state.
	MXRuntime MXReceiverRuntime
	// RemoteMXRuntime is the process-owned controller for per-account Remote MX
	// receivers. cmd/server sets it before serving; when nil (tests) settings
	// persist and reconcile on the next start and no live state is reported.
	RemoteMXRuntime AccountMXReceiverRuntime
	// OutboxWaker is the outbox worker's wake surface. cmd/server sets it after
	// constructing the worker; when nil (tests, or a wiring that delivers only on
	// the periodic poll) a send simply enqueues and delivery waits for the next
	// tick. Wake requests an immediate delivery pass and never blocks.
	OutboxWaker   OutboxWaker
	Log           *slog.Logger
	EncryptionKey []byte
	// HandoffPublisher publishes a RemoteDraft handoff to a standalone inbox's
	// connected remote server (append to Drafts + verify). InstallRemoteBridges
	// installs the production publisher at startup; when nil, a handoff is created
	// and queued but its publication is held until a publisher is injected, and the
	// notification still reports that publication is unavailable rather than
	// failing the request. It is never a global: one bridge per Service.
	HandoffPublisher HandoffPublisher
	// RemoteForwarder fetches a detected remote arrival's raw MIME for the
	// demand-based webhook/Hermes forward. It is installed once at startup by
	// cmd/server (the remote bridge), so the webhook and relay workers can forward
	// a remote message without speaking IMAP. It is never a global: one bridge per
	// Service.
	RemoteForwarder RemoteForwarder
	// RemoteDetection drives an on-demand remote-arrival detection pass for an
	// inbox and reports its pending approvals. cmd/server installs the process
	// RemoteWorker; when nil (tests, or a deployment that wires no watcher) the
	// HTTP agent's on-demand poll reports that remote detection is unavailable.
	// It is never a global: one controller per Service.
	RemoteDetection RemoteDetection
	// RemoteMessages resolves a remote (standalone) message for a reply/forward
	// source when it is not a locally-persisted message. cmd/server installs the
	// remote bridge; when nil a reply/forward to a remote message is not
	// resolvable and the send is refused as not found. It is never a global.
	RemoteMessages RemoteMessageResolver
	// encryptionKeys holds the primary key first and any legacy derivation
	// after it, so decrypting pre-upgrade ciphertext still works.
	encryptionKeys  [][]byte
	unroutedLim     *rateLimiter
	sendLim         *rateLimiter
	dialMXIngestSem chan struct{}
}

// AntlerEndpointResolver resolves the current Antler MX receiver set. It is
// satisfied by *mxdial.AntlerResolver and stubbed in tests.
type AntlerEndpointResolver interface {
	Receivers(ctx context.Context) ([]mxdial.AntlerReceiver, error)
}

// OutboxWaker is the outbox worker's wake surface. It is satisfied by
// *OutboxWorker and stubbed in tests.
type OutboxWaker interface {
	Wake()
}

// wakeOutbox nudges the attached outbox worker, if any, after a successful
// enqueue so delivery starts immediately instead of waiting for the next poll
// tick. It never blocks.
func (s *Service) wakeOutbox() {
	if s.OutboxWaker != nil {
		s.OutboxWaker.Wake()
	}
}

func New(cfg config.Config, st *store.Store, hub *events.Hub) (*Service, error) {
	key, keys, err := cryptox.DeriveKeys(cfg.AppEncryptionKey)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Join(cfg.DataDir, "messages", ".tmp"), 0o700); err != nil {
		return nil, err
	}
	netutil.SetRequirePublic(cfg.RequirePublicOutbound())
	netutil.SetOutboundHTTPTimeout(cfg.OutboundHTTPTimeout)
	concurrency := cfg.InboundConcurrency
	if concurrency < 1 {
		concurrency = 32
	}
	return &Service{Config: cfg, Store: st, Hub: hub, Log: slog.Default(), EncryptionKey: key, encryptionKeys: keys, unroutedLim: newRateLimiter(1, time.Minute), sendLim: newRateLimiter(cfg.SendLimitPerMinute, time.Minute), dialMXIngestSem: make(chan struct{}, concurrency)}, nil
}

// providerSourceLabel returns the human source label for a webhook provider
// that carries no receiver identity. It prefers the registered transport's short
// description and falls back to the provider name so the activity log always
// shows something usable.
func providerSourceLabel(provider string) string {
	if t, ok := transport.LookupInbound(provider); ok {
		if d := strings.TrimSpace(t.Description()); d != "" {
			return d
		}
	}
	return provider
}

// outboundProviderLabel returns the human label for an outbound provider. It
// prefers the registered transport's short description and falls back to the
// provider name so a log line always shows something usable.
func outboundProviderLabel(provider string) string {
	if t, ok := transport.LookupOutbound(provider); ok {
		if d := strings.TrimSpace(t.Description()); d != "" {
			return d
		}
	}
	return provider
}

// auditUnrouted records a rejected unknown-recipient delivery. Coalescing is
// keyed on the receiving domain rather than the full recipient, so random local
// parts on a MailMoose-controlled domain cannot each produce an audit row; the
// offending recipient is still recorded in the detail for operational
// visibility.
func (s *Service) auditUnrouted(provider, recipient string) {
	key := provider + "|" + domainOf(recipient)
	if !s.unroutedLim.Allow(key) {
		return
	}
	s.Store.Audit(context.Background(), "", provider+".unrouted", recipient)
}

func (s *Service) messagePath() string {
	id := idgen.New("raw")
	return filepath.Join(s.Config.DataDir, "messages", id[4:6], id[6:8], id+".eml")
}

// mimeLimits returns the configured MIME traversal bounds so operators can tune
// them rather than relying on package hard-coded defaults.
func (s *Service) mimeLimits() mailparse.Limits {
	return mailparse.Limits{MaxDepth: s.Config.MaxMIMEDepth, MaxParts: s.Config.MaxMIMEParts}
}

// MaxMultipartParts implements transport.MultipartLimitProvider so inbound
// adapters honour the configured cap.
func (s *Service) MaxMultipartParts() int { return s.Config.MaxMultipartParts }

// workflowPath returns a fresh path for workflow (system) mail raw MIME. It is
// kept under its own tree so mailbox cleanup never touches it and retention can
// be swept independently.
func (s *Service) workflowPath() string {
	id := idgen.New("raw")
	return filepath.Join(s.Config.DataDir, "workflow", id[4:6], id[6:8], id+".eml")
}

// dataPath resolves a stored relative path beneath DataDir through the shared
// containment guard, so a raw path that escaped the data root (today only
// possible by a future code path storing a client-supplied value) cannot turn
// a read or remove into arbitrary file access.
func (s *Service) dataPath(rel string) (string, error) {
	return safepath.Join(s.Config.DataDir, rel)
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
	cfg, err := s.decryptConfig(configAAD(b.AccountID, b.ConfigDomainID), b.EncryptedConfig)
	if err != nil {
		return transport.InboundBinding{}, err
	}
	return transport.InboundBinding{
		AccountID:      b.AccountID,
		DomainID:       b.DomainID,
		CredentialID:   b.CredentialID,
		Provider:       b.Provider,
		Recipient:      b.Recipient,
		Config:         cfg,
		ConfigDomainID: b.ConfigDomainID,
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
// for each envelope recipient, validates the authenticated binding, parses
// MIME, applies sender rules and quota, persists the message/event
// transactionally, then publishes the realtime event. A future SMTP ingress can
// call the same core with its own authorization context.
//
// A provider event may address several MailMoose recipients (Resend carries the
// full To list). Each distinct inbox is delivered to once, under its own
// sender-allow-list and quota rules; a delivery to one inbox never suppresses
// or duplicates another.
func (s *Service) ingestStaged(ctx context.Context, provider string, msg transport.InboundMessage, binding transport.InboundBinding) (model.Message, bool, error) {
	parsed, err := mailparse.ParseFile(msg.RawPath, s.mimeLimits())
	if err != nil {
		return model.Message{}, false, fmt.Errorf("parse MIME: %w", err)
	}
	targets := msg.Recipients
	if len(targets) == 0 {
		targets = []string{msg.Recipient}
	}
	single := len(targets) == 1

	var (
		first     model.Message
		anyDup    bool
		delivered int
		firstErr  error
	)
	seen := map[string]bool{}
	for _, rcpt := range targets {
		rcpt = strings.TrimSpace(rcpt)
		if rcpt == "" {
			continue
		}
		inbox, route, rerr := s.Store.ResolveRecipient(ctx, rcpt)
		if rerr != nil {
			if errors.Is(rerr, store.ErrNotFound) {
				s.auditUnrouted(provider, rcpt)
			}
			if firstErr == nil {
				firstErr = rerr
			}
			continue
		}
		// The resolved inbox must belong to the account that was authenticated.
		// Exact and catch-all matches must also stay on the authenticated
		// domain; only an explicit alias may route across domains within the
		// same account. An extra recipient on another domain is not covered by
		// the signature we verified, so it is rejected for that target only.
		if inbox.AccountID != binding.AccountID || (route != store.RouteAlias && inbox.DomainID != binding.DomainID) {
			if firstErr == nil {
				firstErr = transport.ErrInboundUnauthorized
			}
			continue
		}
		if seen[inbox.ID] {
			continue
		}
		seen[inbox.ID] = true
		target := msg
		target.Recipient = rcpt
		m, dup, derr := s.deliverStaged(ctx, provider, target, inbox, parsed, single, nil)
		if derr != nil {
			if firstErr == nil {
				firstErr = derr
			}
			continue
		}
		if delivered == 0 {
			first = m
		}
		if dup {
			anyDup = true
		}
		delivered++
	}
	if delivered == 0 {
		if firstErr != nil {
			return model.Message{}, false, firstErr
		}
		return model.Message{}, false, store.ErrNotFound
	}
	return first, anyDup, nil
}

// mxDeliverAuth carries the MX-only disposition through the shared delivery
// primitive without widening the transport type for webhook providers.
type mxDeliverAuth struct {
	Spam        bool
	Reason      string
	AuthJSON    string
	Fingerprint string
	ReceiptTTL  time.Duration
	// Authenticated reports whether the edge's evidence establishes that the
	// From domain is authenticated (a DMARC pass, or an aligned SPF/DKIM pass).
	// It gates the inbox's MX-only authenticated-sender requirement.
	Authenticated bool
}

func (m *mxDeliverAuth) authenticated() bool { return m != nil && m.Authenticated }

func (m *mxDeliverAuth) spam() bool { return m != nil && m.Spam }
func (m *mxDeliverAuth) reason() string {
	if m == nil {
		return ""
	}
	return m.Reason
}
func (m *mxDeliverAuth) authJSON() string {
	if m == nil {
		return ""
	}
	return m.AuthJSON
}
func (m *mxDeliverAuth) fingerprint() string {
	if m == nil {
		return ""
	}
	return m.Fingerprint
}
func (m *mxDeliverAuth) receiptTTL() time.Duration {
	if m == nil {
		return 0
	}
	return m.ReceiptTTL
}

// deliverStaged persists one recipient's copy of an already-parsed inbound
// message, applying the control-mail, allow-list and quota rules for that inbox.
// When single is true the staged temp file is moved into place (the common
// case); for fan-out a copy is made so each inbox owns its raw MIME. mx carries
// the optional MX auth-policy disposition; it is nil for provider webhook mail.
func (s *Service) deliverStaged(ctx context.Context, provider string, msg transport.InboundMessage, inbox model.Inbox, parsed mailparse.Parsed, single bool, mx *mxDeliverAuth) (model.Message, bool, error) {
	// The transport-supplied envelope sender is untrusted relay metadata. Bound
	// it once here so both the approval control path and the persisted value
	// (message row and forward webhook headers) see the same safe value: over
	// the address-length limit or carrying control characters, it is discarded
	// rather than truncated, so malformed metadata can neither authorize an
	// approval nor be echoed downstream.
	msg.EnvelopeFrom = normalizeEnvelopeSender(msg.EnvelopeFrom)
	// The activity-log source is the concrete receiver a transport supplied
	// (MX/Dial MX); a webhook provider has no receiver selection, so fall back
	// to its display name. Snapshotting it here means both the control-record
	// and the ordinary message/blocked paths label the same source.
	if msg.Source == "" {
		msg.Source = providerSourceLabel(provider)
	}
	// A strict approval control subject, or a reply quoting the approval email's
	// [GH-REQUEST:<token>] reference line, is consumed as workflow input before
	// ordinary delivery, so the token never becomes mailbox content. It is
	// handed to the control handler regardless of the sender allow-list because
	// the handler validates the live token and the exact stored approver, and
	// because an inbox's approver setting may have changed after the request was
	// created. Invalid control mail is consumed too; only its outcome is
	// recorded.
	if looksLikeControl(parsed.Subject) || looksLikeControlReply(parsed) {
		return model.Message{}, false, s.handleControlMessage(ctx, provider, msg, inbox, parsed, mx.authenticated())
	}
	// The allow-list matches the spoofable RFC5322.From address. When the inbox
	// additionally requires an authenticated sender, MX mail whose edge evidence
	// does not establish From-domain authentication is blocked. Webhook
	// providers carry no auth evidence, so the requirement does not apply to
	// them (mx is nil / not trusted).
	blockReason := ""
	if !inbox.AllowsInbound(parsed.From.Address) {
		blockReason = "sender not allowed"
	} else if inbox.RequireAuthenticated && provider == mxProvider && !mx.authenticated() {
		blockReason = "sender not authenticated"
	}
	if blockReason != "" {
		blockedFrom := model.Address{Name: parsed.From.Name, Address: parsed.From.Address}
		blockedAt := parsed.Date
		if blockedAt.IsZero() {
			blockedAt = time.Now().UTC()
		}
		bm, dup, err := s.Store.CommitBlockedInbound(ctx, store.BlockedRecord{
			AccountID: inbox.AccountID, InboxID: inbox.ID, Provider: provider,
			Source: msg.Source, ProviderDeliveryID: msg.DeliveryID, EnvelopeRecipient: msg.Recipient,
			From: blockedFrom, To: parsed.To,
			Subject: parsed.Subject, Reason: blockReason, SizeBytes: msg.Size, ReceivedAt: blockedAt,
		})
		if err != nil {
			return model.Message{}, false, err
		}
		// A blocked delivery is still a terminal per-message outcome and is
		// logged at INFO (every retry, like the delivered path) so the container
		// stream shows why a message never reached the inbox: the block leaves
		// no message row, event or relay delivery to observe otherwise.
		s.Log.Info("inbound blocked", "message_id", bm.ID, "from", bm.From.Address, "to", bm.To, "provider", msg.Source, "reason", blockReason)
		return model.Message{ID: bm.ID, InboxID: bm.InboxID, Direction: "inbound", From: bm.From, To: bm.To, Subject: bm.Subject, SizeBytes: bm.SizeBytes, ReceivedAt: bm.ReceivedAt, CreatedAt: bm.CreatedAt, Blocked: true}, dup, nil
	}
	final := s.messagePath()
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return model.Message{}, false, err
	}
	if single {
		if err := os.Rename(msg.RawPath, final); err != nil {
			return model.Message{}, false, err
		}
	} else if err := copyFile(msg.RawPath, final); err != nil {
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
	m, ev, dup, err := s.Store.CommitInbound(ctx, store.InboundRecord{Inbox: inbox, Provider: provider, Source: msg.Source, ProviderDeliveryID: msg.DeliveryID, ProviderMessageID: firstNonEmpty(msg.ProviderMessageID, parsed.RFCMessageID), EnvelopeRecipient: msg.Recipient, EnvelopeFrom: msg.EnvelopeFrom, RFCMessageID: parsed.RFCMessageID, InReplyTo: parsed.InReplyTo, References: parsed.References, From: from, To: parsed.To, CC: parsed.CC, EnvelopeTo: []string{msg.Recipient}, Subject: parsed.Subject, Text: parsed.Text, HTML: parsed.HTML, RawPath: filepath.ToSlash(rel), SizeBytes: msg.Size, ReceivedAt: received, Attachments: atts, Spam: mx.spam(), SpamReason: mx.reason(), AuthResults: mx.authJSON(), DeliveryFingerprint: mx.fingerprint(), ReceiptTTL: mx.receiptTTL()})
	if err != nil {
		_ = os.Remove(final)
		return model.Message{}, false, err
	}
	if dup {
		_ = os.Remove(final)
		return m, true, nil
	}
	s.Log.Info("inbound received", "message_id", m.ID, "from", m.From.Address, "to", m.To, "provider", msg.Source)
	s.Hub.Publish(ev)
	return m, false, nil
}

// copyFile copies src to dst with 0600 permissions, used to give each
// fan-out recipient its own raw MIME copy.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// maxEnvelopeSenderLength bounds the transport-supplied envelope sender to the
// maximum length of an email address (RFC 5321 4.5.3.1.3, 254 octets). A value
// beyond it is discarded rather than truncated.
const maxEnvelopeSenderLength = 254

// normalizeEnvelopeSender returns the transport-supplied envelope sender as
// bounded safe metadata, or the empty string when it is absent or malformed. It
// never derives a value from the MIME headers: a null return path and a
// provider that passes no sender both stay empty.
func normalizeEnvelopeSender(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" || len(v) > maxEnvelopeSenderLength {
		return ""
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return v
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

// ErrReplyFromSpam is returned when a send or draft would use a Spam message as
// its reply source. The message must be released from Spam first.
var ErrReplyFromSpam = errors.New("message is in spam; release it before replying")

// ErrRateLimited is returned when an account exceeds its outbound send rate.
// Every send path funnels through Send/SendDraft, so enforcing it here (rather
// than at one HTTP route) means a caller cannot bypass the limit by using the
// reply, draft-send, UI or relay paths instead of the send endpoint.
var ErrRateLimited = errors.New("send rate limit exceeded")

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
	enc, err := s.encryptConfig(configAAD(accountID, domainID), merged)
	if err != nil {
		return store.DomainSendingConfig{}, err
	}
	expected := store.ConfigVersion{}
	if exists {
		expected = store.ConfigVersion{ID: existing.ID, Revision: existing.Revision}
	}
	saved, err := s.Store.SaveDomainSendingConfig(ctx, accountID, domainID, provider, enc, expected)
	if err != nil {
		return store.DomainSendingConfig{}, err
	}
	// A new or rotated sender is a reason to retry every pending message for the
	// domain immediately with a fresh attempt budget, rather than leaving them
	// waiting on the no-provider hold or a backoff earned against the old config.
	if _, rerr := s.Store.RequeuePendingWorkflowForDomain(ctx, accountID, domainID); rerr != nil {
		s.Log.Warn("requeue pending workflow after sending config save", "domain_id", domainID, "error", rerr)
	}
	if n, rerr := s.Store.RequeuePendingForDomain(ctx, accountID, domainID); rerr != nil {
		s.Log.Warn("requeue pending mail after sending config save", "domain_id", domainID, "error", rerr)
	} else if n > 0 {
		s.Log.Info("requeued pending mail after sending config save", "domain_id", domainID, "messages", n)
	}
	return saved, nil
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
	if provider == "dialmx" {
		return s.saveDialMXReceivingConfig(ctx, accountID, domainID, provider, cfg, existing, exists, regenerate)
	}
	if provider == RemoteMXProvider {
		return s.saveRemoteMXReceivingConfig(ctx, accountID, domainID, provider, existing, exists, regenerate)
	}
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
	enc, err := s.encryptConfig(configAAD(accountID, domainID), merged)
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

func (s *Service) saveDialMXReceivingConfig(ctx context.Context, accountID, domainID, provider string, cfg map[string]any, existing store.DomainReceivingConfig, exists, regenerate bool) (store.DomainReceivingConfig, map[string]string, error) {
	if regenerate {
		return store.DomainReceivingConfig{}, nil, invalidConfig("use the domain credential rotation action")
	}
	domain, err := s.Store.GetDomain(ctx, accountID, domainID)
	if err != nil {
		return store.DomainReceivingConfig{}, nil, err
	}
	if _, err := mxwire.CanonicalDomain(domain.Name); err != nil {
		return store.DomainReceivingConfig{}, nil, invalidConfig("Dial MX requires a DNS domain name (use punycode for international names)")
	}
	var old map[string]any
	if exists && strings.EqualFold(existing.Provider, provider) {
		if old, err = s.DecryptDomainReceivingConfig(existing); err != nil {
			return store.DomainReceivingConfig{}, nil, err
		}
	}
	// The service must be read before schema defaulting: a legacy save that
	// supplies only receiver_urls is custom, never the Antler default.
	service := dialMXService(cfg, old)
	merged, err := validateConfig(mxdial.Transport{}.ConfigFields(), cfg, nil, false)
	if err != nil {
		return store.DomainReceivingConfig{}, nil, err
	}
	if service == mxdial.ServiceAntler {
		// The Antler MX receiver set is resolved from the live manifest, never
		// supplied by the caller. Reject an explicit receiver_urls rather than
		// silently discarding it, so a client cannot believe it pointed the
		// domain at a chosen receiver.
		if raw, ok := merged["receiver_urls"].(string); ok && strings.TrimSpace(raw) != "" {
			return store.DomainReceivingConfig{}, nil, invalidConfig("receiver URLs cannot be supplied for the Antler MX service")
		}
		if err := s.fillAntlerReceivingConfig(ctx, merged, old); err != nil {
			return store.DomainReceivingConfig{}, nil, err
		}
	} else {
		urlsRaw, _ := merged["receiver_urls"].(string)
		urls, err := validateDialMXReceiverURLs(urlsRaw)
		if err != nil {
			return store.DomainReceivingConfig{}, nil, invalidConfig("%s", err.Error())
		}
		merged["service"] = mxdial.ServiceCustom
		merged["receiver_urls"] = strings.Join(urls, ",")
		delete(merged, "contact_email")
		delete(merged, "setup_id")
		delete(merged, "antler_receivers")
	}
	enc, err := s.encryptConfig(configAAD(accountID, domainID), merged)
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
	if _, err = s.EnsureDialMXCredential(ctx, accountID, domainID); err != nil {
		return store.DomainReceivingConfig{}, nil, err
	}
	if s.DialMX != nil {
		s.DialMX.Wake()
	}
	return saved, nil, nil
}

// saveRemoteMXReceivingConfig selects the account-owned Remote MX receiver for a
// domain. The receiver itself is configured once per account under
// /v1/admin/account/mx; a domain only references it by selecting the provider, so
// the stored config is empty. It requires the account to have a receiver
// configured (otherwise the domain would silently accept nothing).
func (s *Service) saveRemoteMXReceivingConfig(ctx context.Context, accountID, domainID, provider string, existing store.DomainReceivingConfig, exists, regenerate bool) (store.DomainReceivingConfig, map[string]string, error) {
	if regenerate {
		return store.DomainReceivingConfig{}, nil, invalidConfig("Remote MX has no per-domain secret to regenerate")
	}
	if _, err := s.Store.GetDomain(ctx, accountID, domainID); err != nil {
		return store.DomainReceivingConfig{}, nil, err
	}
	receiver, err := s.Store.GetAccountMXReceiver(ctx, accountID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.DomainReceivingConfig{}, nil, invalidConfig("configure this account's Remote MX receiver before selecting it for a domain")
		}
		return store.DomainReceivingConfig{}, nil, err
	}
	if receiver.ReceiverURL == "" {
		return store.DomainReceivingConfig{}, nil, invalidConfig("configure this account's Remote MX receiver before selecting it for a domain")
	}
	encrypted, err := s.encryptConfig(configAAD(accountID, domainID), map[string]any{})
	if err != nil {
		return store.DomainReceivingConfig{}, nil, err
	}
	expected := store.ConfigVersion{}
	if exists {
		expected = store.ConfigVersion{ID: existing.ID, Revision: existing.Revision}
	}
	saved, err := s.Store.SaveDomainReceivingConfig(ctx, accountID, domainID, provider, encrypted, expected)
	if err != nil {
		return store.DomainReceivingConfig{}, nil, err
	}
	s.wakeRemoteMXRuntime()
	return saved, nil, nil
}

// dialMXService resolves the effective service for a Dial MX save. Precedence:
// an explicit incoming service choice, then the stored choice, then incoming or
// stored receiver URLs (a legacy client that knows only receiver_urls is
// custom), and finally the Antler default. The explicit choice must win over
// receiver URLs because an Antler form carries its receiver snapshot in the
// same field the custom service uses.
func dialMXService(incoming, old map[string]any) string {
	if v, _ := incoming["service"].(string); v == mxdial.ServiceAntler || v == mxdial.ServiceCustom {
		return v
	}
	if old != nil {
		if v, _ := old["service"].(string); v == mxdial.ServiceAntler || v == mxdial.ServiceCustom {
			return v
		}
	}
	if v, _ := incoming["receiver_urls"].(string); strings.TrimSpace(v) != "" {
		return mxdial.ServiceCustom
	}
	if old != nil {
		if v, _ := old["receiver_urls"].(string); strings.TrimSpace(v) != "" {
			return mxdial.ServiceCustom
		}
	}
	return mxdial.ServiceAntler
}

// fillAntlerReceivingConfig validates the contact email, resolves the live
// Antler MX receiver set and snapshots it into the configuration. The snapshot
// is what the DNS instructions and MX-record checks read, so a later manifest
// change never silently re-points an existing setup.
func (s *Service) fillAntlerReceivingConfig(ctx context.Context, merged, old map[string]any) error {
	email, _ := merged["contact_email"].(string)
	email = strings.TrimSpace(email)
	if email == "" && old != nil {
		email, _ = old["contact_email"].(string)
	}
	if !mxwire.ValidContactEmail(email) {
		return invalidConfig("a contact email is required for Antler MX")
	}
	setupID, _ := old["setup_id"].(string)
	if !mxwire.ValidSetupID(setupID) {
		setupID = idgen.New("setup")
	}
	resolver := s.AntlerEndpoints
	if resolver == nil {
		resolver = mxdial.DefaultAntlerResolver()
	}
	receivers, err := resolver.Receivers(ctx)
	if err != nil {
		return invalidConfig("Antler MX endpoints are unavailable right now; try again shortly")
	}
	snapshot, err := json.Marshal(receivers)
	if err != nil {
		return err
	}
	urls := make([]string, 0, len(receivers))
	for _, r := range receivers {
		urls = append(urls, r.SessionURL)
	}
	merged["service"] = mxdial.ServiceAntler
	merged["contact_email"] = email
	merged["setup_id"] = setupID
	merged["receiver_urls"] = strings.Join(urls, ",")
	merged["antler_receivers"] = string(snapshot)
	return nil
}

// UpdateAntlerContactEmail changes only the contact metadata, preserving the
// saved receiver snapshot, setup identity, enforcement and domain credential.
func (s *Service) UpdateAntlerContactEmail(ctx context.Context, accountID, domainID, email string) (store.DomainReceivingConfig, error) {
	cfg, err := s.Store.GetDomainReceivingConfig(ctx, accountID, domainID)
	if err != nil {
		return store.DomainReceivingConfig{}, err
	}
	values, err := s.DecryptDomainReceivingConfig(cfg)
	if err != nil {
		return store.DomainReceivingConfig{}, err
	}
	if cfg.Provider != "dialmx" || values["service"] != mxdial.ServiceAntler {
		return store.DomainReceivingConfig{}, invalidConfig("contact email is only available for hosted Antler MX")
	}
	email = strings.TrimSpace(email)
	if !mxwire.ValidContactEmail(email) {
		return store.DomainReceivingConfig{}, invalidConfig("a valid contact email is required")
	}
	values["contact_email"] = email
	encrypted, err := s.encryptConfig(configAAD(accountID, domainID), values)
	if err != nil {
		return store.DomainReceivingConfig{}, err
	}
	saved, err := s.Store.SaveDomainReceivingConfig(ctx, accountID, domainID, cfg.Provider, encrypted, store.ConfigVersion{ID: cfg.ID, Revision: cfg.Revision})
	if err == nil && s.DialMX != nil {
		s.DialMX.Wake()
	}
	return saved, err
}

// AntlerReceiversFromConfig decodes the receiver snapshot stored on an Antler
// MX domain configuration. A custom configuration returns nil.
func AntlerReceiversFromConfig(values map[string]any) []mxdial.AntlerReceiver {
	if v, _ := values["service"].(string); v != mxdial.ServiceAntler {
		return nil
	}
	raw, _ := values["antler_receivers"].(string)
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var receivers []mxdial.AntlerReceiver
	if err := json.Unmarshal([]byte(raw), &receivers); err != nil {
		return nil
	}
	if err := mxdial.ValidateAntlerReceivers(receivers); err != nil {
		return nil
	}
	return receivers
}

func validateDialMXReceiverURLs(raw string) ([]string, error) {
	parts := strings.Split(raw, ",")
	if len(parts) > 8 {
		return nil, fmt.Errorf("at most eight receiver URLs are supported")
	}
	urls := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, part := range parts {
		canonical, err := mxwire.ReceiverURL(strings.TrimSpace(part))
		if err != nil {
			return nil, fmt.Errorf("receiver URLs must be HTTPS base URLs without paths, userinfo, query or fragment")
		}
		if err := netutil.ValidateBaseURL(canonical); err != nil {
			return nil, err
		}
		if !seen[canonical] {
			urls = append(urls, canonical)
			seen[canonical] = true
		}
	}
	if len(urls) == 0 || strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("at least one receiver URL is required")
	}
	return urls, nil
}

func (s *Service) DecryptDomainSendingConfig(c store.DomainSendingConfig) (map[string]any, error) {
	return s.decryptConfig(configAAD(c.AccountID, c.DomainID), c.EncryptedConfig)
}

func (s *Service) DecryptDomainReceivingConfig(c store.DomainReceivingConfig) (map[string]any, error) {
	return s.decryptConfig(configAAD(c.AccountID, c.DomainID), c.EncryptedConfig)
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

// DecryptSecret decrypts a stored secret with any key that can still read it,
// so data written before a key-derivation upgrade remains readable.
func (s *Service) DecryptSecret(encrypted string) ([]byte, error) {
	return cryptox.DecryptFirst(s.encryptionKeys, encrypted)
}

// DecryptSecretAAD decrypts a secret bound to a row identity. A legacy
// (unbound) blob is still read, so migrating a call site does not strand
// existing data.
func (s *Service) DecryptSecretAAD(aad string, encrypted string) ([]byte, error) {
	return cryptox.DecryptFirstAAD(s.encryptionKeys, encrypted, []byte(aad))
}

// EnsureDialMXCredential creates an exact-domain key without copying inherited
// receiving settings. Concurrent creators converge on the first saved key.
func (s *Service) EnsureDialMXCredential(ctx context.Context, accountID, domainID string) (store.DialMXCredential, error) {
	if _, err := s.Store.ResolveDomainReceivingConfig(ctx, accountID, domainID, mxdial.Provider); err != nil {
		return store.DialMXCredential{}, err
	}
	c, err := s.Store.GetDialMXCredential(ctx, accountID, domainID)
	if err == nil {
		return c, nil
	}
	if !errors.Is(err, store.ErrNoProvider) {
		return store.DialMXCredential{}, err
	}
	c, err = s.newDialMXCredential(accountID, domainID)
	if err != nil {
		return store.DialMXCredential{}, err
	}
	saved, _, err := s.Store.EnsureDialMXCredential(ctx, accountID, domainID, c.KeyID, c.EncryptedPrivateSeed, c.PublicKey)
	return saved, err
}

func (s *Service) RotateDialMXCredential(ctx context.Context, accountID, domainID string) (store.DialMXCredential, error) {
	current, err := s.EnsureDialMXCredential(ctx, accountID, domainID)
	if err != nil {
		return store.DialMXCredential{}, err
	}
	next, err := s.newDialMXCredential(accountID, domainID)
	if err != nil {
		return store.DialMXCredential{}, err
	}
	credential, err := s.Store.RotateDialMXCredentialCAS(ctx, accountID, domainID, next.KeyID, next.EncryptedPrivateSeed, next.PublicKey, store.ConfigVersion{ID: current.KeyID, Revision: current.Revision})
	if err == nil && s.DialMX != nil {
		s.DialMX.Wake()
	}
	return credential, err
}

func (s *Service) newDialMXCredential(accountID, domainID string) (store.DialMXCredential, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return store.DialMXCredential{}, err
	}
	encrypted, err := s.EncryptSecretAAD(dialMXSeedAAD(accountID, domainID), private.Seed())
	if err != nil {
		return store.DialMXCredential{}, err
	}
	keyID, err := auth.RandomToken(16)
	if err != nil {
		return store.DialMXCredential{}, err
	}
	return store.DialMXCredential{KeyID: keyID, EncryptedPrivateSeed: encrypted, PublicKey: base64.RawURLEncoding.EncodeToString(public)}, nil
}

// EncryptSecret encrypts a secret with the current primary key.
func (s *Service) EncryptSecret(plaintext []byte) (string, error) {
	return cryptox.Encrypt(s.EncryptionKey, plaintext)
}

// EncryptSecretAAD encrypts a secret bound to a row identity, so a blob copied
// to another row fails to decrypt under its new owner.
func (s *Service) EncryptSecretAAD(aad string, plaintext []byte) (string, error) {
	return cryptox.EncryptWithAAD(s.EncryptionKey, plaintext, []byte(aad))
}

// configAAD is the additional-authenticated-data binding for an encrypted
// provider/connector configuration blob: its owning account and the scoped row
// (a domain id).
func configAAD(accountID, scopeID string) string {
	return "cfg:" + accountID + ":" + scopeID
}

// dialMXSeedAAD binds a Dial MX credential's encrypted private seed to its
// account and domain.
func dialMXSeedAAD(accountID, domainID string) string {
	return "dialmx_seed:" + accountID + ":" + domainID
}

// DecryptDialMXSeed decrypts a Dial MX credential's private seed, unbound by its
// account and domain.
func (s *Service) DecryptDialMXSeed(accountID, domainID, encrypted string) ([]byte, error) {
	return s.DecryptSecretAAD(dialMXSeedAAD(accountID, domainID), encrypted)
}

func (s *Service) decryptConfig(aad, encrypted string) (map[string]any, error) {
	b, err := s.DecryptSecretAAD(aad, encrypted)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err = json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) encryptConfig(aad string, values map[string]any) (string, error) {
	b, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	return s.EncryptSecretAAD(aad, b)
}

// CreateHermesRelay issues a Hermes relay connection's credentials directly
// and returns the plaintext secret and delivery key exactly once. The gateway
// id is generated here so the operator only has to paste the printed .env
// block.
func (s *Service) CreateHermesRelay(ctx context.Context, p model.Principal, inboxID, name string) (string, string, string, error) {
	return s.CreateRelay(ctx, p, inboxID, name, store.KindHermes)
}

// CreateRelay issues a relay connection's credentials directly for the given
// connector kind and returns the plaintext secret and delivery key exactly
// once. Both Hermes and OpenClaw share the relay transport; the kind selects
// the product connector stored on the client row.
func (s *Service) CreateRelay(ctx context.Context, p model.Principal, inboxID, name string, kind store.RelayKind) (string, string, string, error) {
	if !p.Admin {
		return "", "", "", store.ErrForbidden
	}
	if kind != store.KindHermes && kind != store.KindOpenClaw {
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
	secEnc, err := cryptox.Encrypt(s.EncryptionKey, []byte(secret))
	if err != nil {
		return "", "", "", err
	}
	rec := store.EnrollRecord{AccountID: p.AccountID, InboxID: inboxID, Name: strings.TrimSpace(name), Kind: kind}
	if rec.Name == "" {
		if kind == store.KindOpenClaw {
			rec.Name = "OpenClaw"
		} else {
			rec.Name = "Hermes"
		}
	}
	if _, err = s.Store.CreateHermesConnection(ctx, rec, gatewayID, secEnc, ""); err != nil {
		return "", "", "", err
	}
	return gatewayID, secret, "", nil
}

// CreateRelayEnrollCode mints a one-time setup code that a connector host
// redeems at POST /relay/enroll. The code carries the inbox, name and
// connector kind; it is the authority, not an address, so the claiming host
// supplies the MailMoose URL.
func (s *Service) CreateRelayEnrollCode(ctx context.Context, p model.Principal, inboxID, name string, kind store.RelayKind, ttl time.Duration) (string, error) {
	if !p.Admin {
		return "", store.ErrForbidden
	}
	if kind != store.KindOpenClaw {
		// Hermes keeps its direct env-block flow; the code path is for
		// connectors whose setup wizard redeems it.
		return "", store.ErrForbidden
	}
	if _, err := s.Store.GetInboxInternal(ctx, p.AccountID, inboxID); err != nil {
		return "", err
	}
	if strings.TrimSpace(name) == "" {
		name = "OpenClaw"
	}
	return s.Store.CreateRelayEnrollToken(ctx, p.AccountID, inboxID, name, kind, ttl)
}

type SendAttachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type,omitempty"`
	Content     []byte `json:"content"`
}

type SendInput struct {
	InboxID string `json:"inbox_id"`
	// FromAddress selects the sender: the inbox primary (empty or matching) or
	// one of its aliases. The provider is resolved from the chosen address's
	// domain, which may differ from the inbox's. FromName optionally overrides
	// the alias/inbox display name (used when resending a frozen draft). Neither
	// is accepted from API JSON; handlers map their own fields onto them.
	FromAddress        string           `json:"-"`
	FromName           string           `json:"-"`
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
	return s.sendWithLimit(ctx, p.AccountID, in, idem, true)
}

// SendSystemMail enqueues outbound installation mail (currently account
// invitations) from a specific inbox. There is no mailbox-role principal to
// check: the caller is the system-administrator surface and the inbox is an
// explicit installation setting, so authority has already been established.
// The message goes through the ordinary outbox queue exactly like user mail.
func (s *Service) SendSystemMail(ctx context.Context, accountID, inboxID string, in SendInput) (SendResult, error) {
	in.InboxID = inboxID
	in.ClientLabel = "System"
	in.ClientID = ""
	return s.send(ctx, accountID, in, "")
}

// send is the principal-free outbound core. The external email approval path
// calls it after validating the token and approver; mailbox ownership was
// already established by the send request itself.
func (s *Service) send(ctx context.Context, accountID string, in SendInput, idem string) (result SendResult, err error) {
	return s.sendWithLimit(ctx, accountID, in, idem, false)
}

func (s *Service) sendWithLimit(ctx context.Context, accountID string, in SendInput, idem string, limited bool) (result SendResult, err error) {
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
		// Release the reservation on any failure so a retry can re-send. Use a
		// cancellation-independent context: if the client disconnects, the
		// request context is cancelled and a release on it would silently fail,
		// leaving the key reserved for the stale-lease interval.
		defer func() {
			if err != nil {
				relCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				_ = s.Store.IdempotencyRelease(relCtx, accountID, idem)
			}
		}()
	}
	if limited && !s.sendLim.Allow(accountID) {
		return SendResult{}, ErrRateLimited
	}
	inbox, err := s.Store.GetInboxInternal(ctx, accountID, in.InboxID)
	if err != nil {
		return SendResult{}, err
	}
	// Resolve the sender before anything is built. The chosen address may be an
	// alias on another domain of the account, in which case that domain's
	// sending configuration and DKIM identity are used, and the alias's own
	// display name is used unless the caller supplied one (a frozen draft).
	// The requested address is resolved (falling back to the inbox primary for
	// an empty one).
	var from model.Address
	var sendingTarget store.SendingTarget
	from, sendingTarget, err = s.Store.ResolveSendingTarget(ctx, accountID, inbox.ID, in.FromAddress)
	if err != nil {
		return SendResult{}, err
	}
	if in.FromName != "" {
		from.Name = in.FromName
	}
	fromAddress := from.Address
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
		target, rerr := s.resolveSendSource(ctx, accountID, inbox.ID, in.ReplyToMessageID)
		if rerr != nil {
			return SendResult{}, rerr
		}
		if target.InboxID != inbox.ID || target.Internal {
			return SendResult{}, store.ErrForbidden
		}
		// A Spam message must be released before it can be a reply source, so a
		// quarantined message cannot be used to trigger outbound mail.
		if target.Spam {
			return SendResult{}, ErrReplyFromSpam
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
		target, carried, rerr := s.resolveSendSourceForward(ctx, accountID, inbox.ID, in.ForwardOfMessageID)
		if rerr != nil {
			return SendResult{}, rerr
		}
		if target.InboxID != inbox.ID || target.Internal {
			return SendResult{}, store.ErrForbidden
		}
		// Forwarding shares the reply rule: a Spam message is not a send source
		// until it is released.
		if target.Spam {
			return SendResult{}, ErrReplyFromSpam
		}
		if subject == "" {
			subject = ForwardSubject(target.Subject)
		}
		if strings.TrimSpace(in.Text) == "" {
			in.Text = forwardPrefix(target)
		} else {
			in.Text = strings.TrimRight(in.Text, "\n") + "\n\n" + forwardPrefix(target)
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
	// Providers reject a message with no subject (Brevo returns a permanent
	// 400), so reject it here rather than letting the send fail asynchronously in
	// the outbox. Replies and forwards derive a "Re:"/"Fwd:" subject above and are
	// unaffected.
	if subject == "" {
		return SendResult{}, fmt.Errorf("subject is required (email providers reject a message with an empty subject)")
	}
	// A missing provider is not fatal: the message is queued and the outbox
	// worker holds it until a provider is configured for the sending domain.
	sending, cfgErr := s.Store.SendingConfigForTarget(ctx, accountID, inbox.ID, sendingTarget)
	queuedReason := ""
	if cfgErr != nil {
		if !errors.Is(cfgErr, store.ErrNoProvider) {
			return SendResult{}, cfgErr
		}
		queuedReason = "no outbound provider configured for this domain"
	}
	if cfgErr == nil {
		if outboundProvider, ok := transport.LookupOutbound(sending.Provider); ok {
			if limit := transport.MaxEnvelopeRecipients(outboundProvider); limit > 0 && len(uniqueEnvelopeRecipients(to, cc, bcc)) > limit {
				return SendResult{}, fmt.Errorf("selected provider supports at most %d envelope recipient", limit)
			}
		}
	}
	msgID := fmt.Sprintf("<%s@%s>", strings.TrimPrefix(idgen.New("msg"), "msg_"), strings.SplitN(fromAddress, "@", 2)[1])
	now := time.Now().UTC()
	html := in.HTML
	attachments, err := outboundAttachments(in.Attachments)
	if err != nil {
		return SendResult{}, err
	}
	if size := attachmentsSize(attachments); size > s.Config.MaxMessageBytes {
		return SendResult{}, fmt.Errorf("attachments exceed maximum message size")
	}
	raw, err := mailparse.BuildMessage(mailparse.Address{Name: from.Name, Address: from.Address}, to, cc, bcc, subject, in.Text, html, msgID, inReply, refs, now, attachmentParts(attachments))
	if err != nil {
		return SendResult{}, err
	}
	// The earlier attachment check is a fast preflight; base64 encoding, MIME
	// boundaries and headers can expand the final message, so enforce the limit
	// on the bytes actually submitted to the provider.
	if int64(len(raw)) > s.Config.MaxMessageBytes {
		return SendResult{}, fmt.Errorf("message exceeds the maximum message size")
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
	m, draftEvent, err := s.Store.CommitOutbound(ctx, store.OutboundRecord{Inbox: inbox, Provider: sending.Provider, RFCMessageID: msgID, InReplyTo: inReply, References: refs, From: from, SendingDomainID: sendingTarget.DomainID, To: to, CC: cc, BCC: bcc, Subject: subject, Text: in.Text, HTML: html, RawPath: filepath.ToSlash(rel), SizeBytes: int64(len(raw)), ThreadID: threadID, IdemKey: idem, LastError: queuedReason, DraftID: in.DraftID, ClientLabel: in.ClientLabel, ClientID: in.ClientID, Attachments: metadata, SendRequestID: in.SendRequestID, DecisionActor: in.DecisionActor, DecisionActorID: in.DecisionActorID, DecisionMethod: in.DecisionMethod, DecisionFeedback: in.DecisionFeedback})
	if err != nil {
		_ = os.Remove(path)
		return SendResult{}, err
	}
	if draftEvent.Type != "" {
		s.Hub.Publish(draftEvent)
	}
	// The message is durably queued: request an immediate delivery pass so a
	// fresh send does not wait for the worker's next poll tick.
	s.wakeOutbox()
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
	// The draft remembers its chosen managed sender. A caller that requested a
	// genuinely different address re-resolves; an empty request keeps the draft's
	// sender.
	if in.FromAddress == "" {
		in.FromAddress = d.FromAddress
		in.FromName = d.FromName
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
		aPath, perr := s.dataPath(a.RawPath)
		if perr != nil {
			return SendResult{}, perr
		}
		data, rerr := os.ReadFile(aPath)
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
	res, err := s.sendWithLimit(ctx, accountID, in, idem, true)
	if err != nil {
		return SendResult{}, err
	}
	// The draft rows were consumed with the enqueue; remove the files now.
	for _, path := range paths {
		if p, perr := s.dataPath(path); perr == nil {
			_ = os.Remove(p)
		}
	}
	return res, nil
}

// outcomeTimeout bounds how long the store may take to persist a delivery
// outcome (sent/failed/held). It is a var so tests can shrink it without
// waiting the production timeout.
var outcomeTimeout = 15 * time.Second

// SetOutcomeTimeoutForTest overrides the delivery-outcome write budget.
func SetOutcomeTimeoutForTest(d time.Duration) { outcomeTimeout = d }

// outcomeContext returns a context for recording a delivery outcome. It is
// detached from ctx so a cancelled delivery still records its result, and it is
// bounded so a hung store cannot leak the worker. Critically, the caller must
// create it only when the outcome is actually written: creating it before the
// provider send would let a slow send consume the entire budget, leaving the
// outcome context already expired when fail/mark-sent runs, so no outcome is
// recorded, attempts never advance, and the message loops forever on the claim
// lease.
func outcomeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), outcomeTimeout)
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
	// The outcome context is created per-outcome, immediately before the write,
	// so the (possibly long) provider send cannot consume its budget.
	sending, err := s.Store.SendingConfigForMessage(ctx, m.AccountID, m.ID)
	if err != nil {
		if errors.Is(err, store.ErrNoProvider) {
			outcomeCtx, cancelOutcome := outcomeContext(ctx)
			defer cancelOutcome()
			return s.Store.HoldPending(outcomeCtx, m.AccountID, m.ID, "sending paused: selected sender has no connector", time.Now().UTC().Add(5*time.Minute))
		}
		return s.failDetached(ctx, m, err, "")
	}
	cfg, err := s.DecryptDomainSendingConfig(sending)
	if err != nil {
		return s.failDetached(ctx, m, err, sending.Provider)
	}
	rawPath, err := s.dataPath(m.RawPath)
	if err != nil {
		return s.failDetached(ctx, m, err, sending.Provider)
	}
	provider, ok := transport.LookupOutbound(sending.Provider)
	if !ok {
		return s.failDetached(ctx, m, fmt.Errorf("%w: %s", transport.ErrUnknownProvider, sending.Provider), sending.Provider)
	}
	if limit := transport.MaxEnvelopeRecipients(provider); limit > 0 && len(uniqueEnvelopeRecipients(m.To, m.CC, m.BCC)) > limit {
		return s.failDetached(ctx, m, &transport.PermanentError{Err: fmt.Errorf("provider supports at most %d envelope recipient", limit)}, sending.Provider)
	}
	outbound := transport.OutboundMessage{
		FromName:       m.From.Name,
		FromAddress:    m.From.Address,
		To:             m.To,
		CC:             m.CC,
		BCC:            m.BCC,
		Subject:        m.Subject,
		Text:           m.Text,
		HTML:           m.HTML,
		MessageID:      m.RFCMessageID,
		InReplyTo:      m.InReplyTo,
		References:     m.References,
		IdempotencyKey: m.ID,
	}
	// Transports that build their request from the raw MIME (SMTP, Direct MX)
	// need the stored message read into memory. HTTP adapters build from
	// structured fields, so reading the whole message here would be an unused
	// full-size allocation; they reconstruct only the attachment bytes instead.
	if prefersRawMIME(provider) {
		raw, rerr := os.ReadFile(rawPath)
		if rerr != nil {
			return s.failDetached(ctx, m, rerr, sending.Provider)
		}
		outbound.RawMIME = raw
	} else {
		atts, aerr := s.deliveryAttachments(m)
		if aerr != nil {
			return s.failDetached(ctx, m, aerr, sending.Provider)
		}
		outbound.Attachments = atts
	}
	// Record the attempt as in flight before the provider call, so an
	// interrupted send (restart, crash or dropped connection) leaves a durable
	// trace instead of looping as pending with an empty sending log. This is a
	// fast pre-send write, so it gets its own bounded context.
	startedCtx, cancelStarted := outcomeContext(ctx)
	err = s.Store.RecordDeliveryStarted(startedCtx, m.AccountID, m.ID, sending.Provider)
	cancelStarted()
	if err != nil {
		return err
	}
	// The provider send uses the delivery context only; recording the outcome
	// afterwards gets a fresh, bounded context so a slow send cannot exhaust it.
	providerResult, err := provider.Send(ctx, cfg, outbound)
	if err != nil {
		return s.failDetached(ctx, m, err, sending.Provider)
	}
	outcomeCtx, cancelOutcome := outcomeContext(ctx)
	defer cancelOutcome()
	_, events, err := s.Store.MarkSent(outcomeCtx, m.AccountID, m.ID, providerResult.ProviderMessageID, sending.Provider)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// The queued message was cancelled (deleted) while the provider
			// call was in flight. The provider may still have accepted it, and
			// no local action can recall it; the message must not be
			// resurrected. Log the ambiguity rather than treating it as an
			// error to retry.
			s.Log.Warn("outbound message cancelled during delivery; provider may still have accepted it", "message_id", m.ID, "to", m.To)
			return nil
		}
		return err
	}
	s.Log.Info("outbound sent", "message_id", m.ID, "from", m.From.Address, "to", m.To, "provider", outboundProviderLabel(sending.Provider))
	for _, ev := range events {
		s.Hub.Publish(ev)
	}
	// A standalone inbox copies the sent message into its own remote Sent folder.
	// The copy is a separate durable job: it never re-runs this SMTP submission, and
	// a copy failure is confined to the copy job (MarkSent has already committed).
	s.enqueueSentCopyIfStandalone(ctx, m)
	return nil
}

// enqueueSentCopyIfStandalone queues a durable copy of a just-sent message into a
// standalone inbox's remote Sent folder. It is a no-op for a domain inbox, whose
// Sent view is local. The copy job is idempotent per RFC Message-ID, so a retried
// send path never queues a duplicate copy.
func (s *Service) enqueueSentCopyIfStandalone(ctx context.Context, m model.Message) {
	inbox, err := s.Store.GetInboxInternal(ctx, m.AccountID, m.InboxID)
	if err != nil || inbox.Kind != model.InboxKindStandalone {
		return
	}
	// An operator can disable the remote Sent copy (for example Gmail already
	// files sent mail), so no duplicate is created.
	if !inbox.RemoteSentCopyEnabled {
		return
	}
	if strings.TrimSpace(m.RawPath) == "" {
		return
	}
	// Freeze an independent copy of the raw MIME for the copy job. The queue is
	// durable and independently retried, while the outbound message's own raw
	// file is unlinked when the message is purged; referencing that path would
	// make a pending/ambiguous copy unverifiable if the user trashes and purges
	// the sent message first. The copy owns its bytes for its whole lifetime.
	frozenRel, ferr := s.freezeSentCopyRaw(ctx, m.AccountID, m.RawPath)
	if ferr != nil {
		s.Log.Warn("freeze remote sent-copy raw", "message_id", m.ID, "inbox_id", m.InboxID, "error", ferr)
		return
	}
	if _, _, err := s.Store.EnqueueUniqueRemoteSentCopy(ctx, m.AccountID, m.InboxID, store.RemoteSentCopyInput{
		MessageID:    m.ID,
		FolderPath:   inbox.RemoteSentCopyFolder,
		RFCMessageID: m.RFCMessageID,
		MessageIDHdr: m.RFCMessageID,
		RawPath:      frozenRel,
		SizeBytes:    m.SizeBytes,
	}); err != nil {
		s.Log.Warn("enqueue remote sent-copy", "message_id", m.ID, "inbox_id", m.InboxID, "error", err)
	}
}

// freezeSentCopyRaw copies a message's raw MIME into a dedicated sent-copy tree
// so the copy job's bytes are independent of the outbound message's own raw file
// (which is unlinked when the message is purged). It returns the data-dir-relative
// path of the frozen copy.
func (s *Service) freezeSentCopyRaw(ctx context.Context, accountID, rawRel string) (string, error) {
	src, err := s.dataPath(rawRel)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	id := idgen.New("raw")
	path := filepath.Join(s.Config.DataDir, "sentshare", id[4:6], id[6:8], id+".eml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	rel, _ := filepath.Rel(s.Config.DataDir, path)
	return filepath.ToSlash(rel), nil
}

// failMessagePanic records a delivery panic as a failed attempt so a poison
// message backs off rather than being reclaimed and re-panicking. It fails
// closed: any error reading the message or writing the outcome is returned to
// the caller, never raised, so it cannot escape the worker's own recovery.
func (s *Service) failMessagePanic(ctx context.Context, accountID, msgID string, cause error) error {
	m, err := s.Store.GetMessageByID(ctx, accountID, msgID)
	if err != nil {
		return err
	}
	// Only a still-pending message is ours to fail; a concurrent delivery may
	// have already resolved it.
	if m.Direction != "outbound" || m.Status != "pending" {
		return nil
	}
	return s.fail(ctx, m, cause, m.Provider)
}

// fail records a failed delivery attempt with exponential backoff, returning
// the error so the worker can log it. provider attributes the attempt to the
// provider snapshot actually used; it is empty when no config was available.
// The attempt's domain is derived inside the store from the message and inbox,
// so the config may be deleted while a send is in flight without losing the
// outcome.
func (s *Service) fail(ctx context.Context, m model.Message, err error, provider string) error {
	// A permanent provider error will never succeed on retry, so fail the
	// message immediately instead of retrying with backoff. An ambiguous send
	// (a client timeout that may follow a provider-accepted request) is also
	// terminal: retrying a provider without an idempotency key could deliver
	// twice.
	if transport.IsPermanent(err) || transport.AsAmbiguous(err) {
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

// failDetached records a failed delivery attempt with a fresh, bounded outcome
// context so it is always persisted regardless of how long the delivery itself
// took. It is the form used by Deliver/DeliverWorkflow for every failure on the
// send path; fail itself keeps taking the caller's context so the panic
// recovery path (which already supplies a bounded context) is unchanged.
func (s *Service) failDetached(ctx context.Context, m model.Message, err error, provider string) error {
	outcomeCtx, cancelOutcome := outcomeContext(ctx)
	defer cancelOutcome()
	return s.fail(outcomeCtx, m, err, provider)
}

// AccountIDForWorkflow resolves the account id for a workflow job id. The
// worker claims jobs without a principal, so it needs a way to look up the
// account.
func (s *Service) AccountIDForWorkflow(ctx context.Context, workflowID string) (string, error) {
	return s.Store.WorkflowAccountID(ctx, workflowID)
}

// DeliverWorkflow performs the provider send for a pending workflow job. It is
// the workflow-queue analogue of Deliver: the job is not mailbox content, so
// success only marks the job sent (starting the approval expiry clock) and
// failure updates the request's notification state. Attachments are
// reconstructed from the retained raw MIME for HTTP adapters.
func (s *Service) DeliverWorkflow(ctx context.Context, accountID, workflowID, owner string) error {
	w, err := s.Store.GetWorkflowInternal(ctx, accountID, workflowID)
	if err != nil {
		return err
	}
	if w.Status != model.WorkflowPending {
		return nil
	}
	if owner != "" {
		current, err := s.Store.WorkflowClaimOwner(ctx, accountID, workflowID)
		if err != nil {
			return err
		}
		if current != owner {
			return fmt.Errorf("workflow %s is claimed by another worker", workflowID)
		}
	}
	// The outcome context is created per-outcome, immediately before the write,
	// so the (possibly long) provider send cannot consume its budget.
	sending, err := s.Store.DomainSendingConfigForWorkflow(ctx, accountID, workflowID)
	if err != nil {
		if errors.Is(err, store.ErrNoProvider) {
			outcomeCtx, cancelOutcome := outcomeContext(ctx)
			defer cancelOutcome()
			return s.Store.HoldWorkflow(outcomeCtx, accountID, workflowID, "no outbound provider configured for this domain", time.Now().UTC().Add(5*time.Minute))
		}
		return s.failWorkflowDetached(ctx, w, err, "")
	}
	cfg, err := s.DecryptDomainSendingConfig(sending)
	if err != nil {
		return s.failWorkflowDetached(ctx, w, err, sending.Provider)
	}
	rawPath, err := s.dataPath(w.RawPath)
	if err != nil {
		return s.failWorkflowDetached(ctx, w, err, sending.Provider)
	}
	provider, ok := transport.LookupOutbound(sending.Provider)
	if !ok {
		return s.failWorkflowDetached(ctx, w, fmt.Errorf("%w: %s", transport.ErrUnknownProvider, sending.Provider), sending.Provider)
	}
	if limit := transport.MaxEnvelopeRecipients(provider); limit > 0 && len(uniqueEnvelopeRecipients(w.To, w.CC, w.BCC)) > limit {
		return s.failWorkflowDetached(ctx, w, &transport.PermanentError{Err: fmt.Errorf("provider supports at most %d envelope recipient", limit)}, sending.Provider)
	}
	outbound := transport.OutboundMessage{
		FromName:    w.From.Name,
		FromAddress: w.From.Address,
		To:          w.To,
		CC:          w.CC,
		BCC:         w.BCC,
		Subject:     w.Subject,
		Text:        w.Text,
		HTML:        w.HTML,
	}
	// Only raw-MIME transports need the whole message in memory; HTTP adapters
	// reconstruct just the attachment bytes (see Deliver).
	if prefersRawMIME(provider) {
		raw, rerr := os.ReadFile(rawPath)
		if rerr != nil {
			return s.failWorkflowDetached(ctx, w, rerr, sending.Provider)
		}
		outbound.RawMIME = raw
	} else {
		atts, aerr := s.workflowAttachments(w)
		if aerr != nil {
			return s.failWorkflowDetached(ctx, w, aerr, sending.Provider)
		}
		outbound.Attachments = atts
	}
	// Record the handoff as in flight before the provider call, so an
	// interrupted send leaves a durable trace in the domain log. This is a fast
	// pre-send write, so it gets its own bounded context.
	startedCtx, cancelStarted := outcomeContext(ctx)
	err = s.Store.RecordWorkflowDeliveryStarted(startedCtx, accountID, w.InboxID, workflowID, sending.Provider)
	cancelStarted()
	if err != nil {
		return err
	}
	// The provider send uses the delivery context only; recording the outcome
	// afterwards gets a fresh, bounded context so a slow send cannot exhaust it.
	providerResult, err := provider.Send(ctx, cfg, outbound)
	if err != nil {
		return s.failWorkflowDetached(ctx, w, err, sending.Provider)
	}
	tokenExpiry := time.Duration(s.Config.ApprovalExpiryHours) * time.Hour
	outcomeCtx, cancelOutcome := outcomeContext(ctx)
	defer cancelOutcome()
	events, err := s.Store.MarkWorkflowSent(outcomeCtx, accountID, workflowID, providerResult.ProviderMessageID, sending.Provider, tokenExpiry)
	if err != nil {
		return err
	}
	s.Log.Info("workflow sent", "workflow_id", workflowID, "from", w.From.Address, "to", w.To, "provider", outboundProviderLabel(sending.Provider))
	for _, ev := range events {
		s.Hub.Publish(ev)
	}
	return nil
}

// failWorkflow records a failed workflow handoff with exponential backoff. A
// permanent provider error is terminal immediately, as is an ambiguous send
// (see fail).
func (s *Service) failWorkflow(ctx context.Context, w store.Workflow, err error, provider string) error {
	if transport.IsPermanent(err) || transport.AsAmbiguous(err) {
		events, ferr := s.Store.MarkWorkflowFailed(ctx, w.AccountID, w.ID, err.Error(), time.Time{}, 1, provider)
		if ferr != nil {
			return ferr
		}
		for _, ev := range events {
			s.Hub.Publish(ev)
		}
		return err
	}
	backoff := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 4 * time.Hour}
	attempt := w.Attempts
	if attempt < 0 || attempt >= len(backoff) {
		attempt = len(backoff) - 1
	}
	next := time.Now().UTC().Add(backoff[attempt])
	events, ferr := s.Store.MarkWorkflowFailed(ctx, w.AccountID, w.ID, err.Error(), next, len(backoff)+1, provider)
	if ferr != nil {
		return ferr
	}
	for _, ev := range events {
		s.Hub.Publish(ev)
	}
	return err
}

// failWorkflowDetached records a failed workflow handoff with a fresh, bounded
// outcome context so it is always persisted regardless of how long the delivery
// took. See failDetached.
func (s *Service) failWorkflowDetached(ctx context.Context, w store.Workflow, err error, provider string) error {
	outcomeCtx, cancelOutcome := outcomeContext(ctx)
	defer cancelOutcome()
	return s.failWorkflow(outcomeCtx, w, err, provider)
}

// failWorkflowPanic records a workflow delivery panic as a failed attempt so a
// poison job backs off instead of re-panicking. It fails closed: errors are
// returned, never raised, so nothing escapes the worker's own recovery.
func (s *Service) failWorkflowPanic(ctx context.Context, accountID, workflowID string, cause error) error {
	w, err := s.Store.GetWorkflowInternal(ctx, accountID, workflowID)
	if err != nil {
		return err
	}
	if w.Status != model.WorkflowPending {
		return nil
	}
	return s.failWorkflow(ctx, w, cause, w.Provider)
}

// redactWorkflow removes approval control tokens from a terminal workflow
// job's stored bodies and retained raw MIME. It is idempotent.
func (s *Service) redactWorkflow(w store.Workflow) error {
	path, perr := s.dataPath(w.RawPath)
	if perr != nil {
		return perr
	}
	if raw, err := os.ReadFile(path); err == nil {
		redacted := redactWorkflowTokens(string(raw))
		if redacted != string(raw) {
			if werr := os.WriteFile(path, []byte(redacted), 0o600); werr != nil {
				return werr
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return s.Store.UpdateWorkflowBodies(context.Background(), w.AccountID, w.ID, redactWorkflowTokens(w.Text), redactWorkflowTokens(w.HTML))
}

// workflowAttachments reconstructs attachment bytes from a workflow job's
// retained raw MIME so HTTP adapters send the same content.
func (s *Service) workflowAttachments(w store.Workflow) ([]transport.OutboundAttachment, error) {
	path, perr := s.dataPath(w.RawPath)
	if perr != nil {
		return nil, perr
	}
	var out []transport.OutboundAttachment
	err := mailparse.ExtractAllAttachments(path, func(a mailparse.Attachment, r io.Reader) error {
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, r); err != nil {
			return err
		}
		out = append(out, transport.OutboundAttachment{Filename: a.Filename, ContentType: a.ContentType, Content: buf.Bytes()})
		return nil
	}, s.mimeLimits())
	if err != nil {
		return nil, err
	}
	return out, nil
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

func uniqueEnvelopeRecipients(groups ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, group := range groups {
		for _, value := range group {
			value = strings.ToLower(strings.TrimSpace(value))
			if value != "" && !seen[value] {
				seen[value] = true
				out = append(out, value)
			}
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

// resolveSendSource resolves a reply source: the locally-persisted message when
// it exists, else — when a remote resolver is installed — the remote message with
// the same id. A remote message has no local row, so without the resolver a reply
// to it would be reported not found.
func (s *Service) resolveSendSource(ctx context.Context, accountID, inboxID, messageID string) (model.Message, error) {
	target, err := s.Store.GetMessageByID(ctx, accountID, messageID)
	if err == nil {
		return target, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return model.Message{}, err
	}
	if s.RemoteMessages != nil {
		m, ok, rerr := s.RemoteMessages.ResolveRemoteReply(ctx, accountID, messageID)
		if rerr != nil {
			return model.Message{}, rerr
		}
		if ok {
			return m, nil
		}
	}
	return model.Message{}, store.ErrNotFound
}

// resolveSendSourceForward resolves a forward source and any attachments to
// carry. A local message's attachments are extracted from its stored raw MIME; a
// remote message's are extracted from a transient live fetch.
func (s *Service) resolveSendSourceForward(ctx context.Context, accountID, inboxID, messageID string) (model.Message, []SendAttachment, error) {
	target, err := s.Store.GetMessageByID(ctx, accountID, messageID)
	if err == nil {
		carried, aerr := s.forwardAttachments(ctx, accountID, target)
		if aerr != nil {
			return model.Message{}, nil, aerr
		}
		return target, carried, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return model.Message{}, nil, err
	}
	if s.RemoteMessages != nil {
		m, carried, ok, rerr := s.RemoteMessages.ResolveRemoteForward(ctx, accountID, messageID)
		if rerr != nil {
			return model.Message{}, nil, rerr
		}
		if ok {
			return m, carried, nil
		}
	}
	return model.Message{}, nil, store.ErrNotFound
}

func (s *Service) forwardAttachments(ctx context.Context, accountID string, m model.Message) ([]SendAttachment, error) {
	meta, err := s.Store.ListAttachmentsInternal(ctx, accountID, m.ID)
	if err != nil {
		return nil, err
	}
	if len(meta) == 0 {
		return nil, nil
	}
	path, perr := s.dataPath(m.RawPath)
	if perr != nil {
		return nil, perr
	}
	var out []SendAttachment
	err = mailparse.ExtractAllAttachments(path, func(a mailparse.Attachment, r io.Reader) error {
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, r); err != nil {
			return err
		}
		out = append(out, SendAttachment{Filename: a.Filename, ContentType: a.ContentType, Content: buf.Bytes()})
		return nil
	}, s.mimeLimits())
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
	path, perr := s.dataPath(m.RawPath)
	if perr != nil {
		return nil, perr
	}
	var out []transport.OutboundAttachment
	err := mailparse.ExtractAllAttachments(path, func(a mailparse.Attachment, r io.Reader) error {
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, r); err != nil {
			return err
		}
		out = append(out, transport.OutboundAttachment{Filename: a.Filename, ContentType: a.ContentType, Content: buf.Bytes()})
		return nil
	}, s.mimeLimits())
	if err != nil {
		return nil, err
	}
	return out, nil
}
