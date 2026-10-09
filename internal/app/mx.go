package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/mailparse"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport"
)

// mxwire.Provider is the registered receiving provider name for the MX edge.
const mxProvider = "mx"

// ErrMXDisabled is returned when MX ingest is attempted while the operator has
// not enabled the optional edge endpoints.
var ErrMXDisabled = errors.New("mx receiving is not enabled")

// MXResolveResult is the routing decision for one recipient. Temporary marks a
// transient internal failure (retryable), as opposed to a permanent unknown or
// unauthorized recipient.
type MXResolveResult struct {
	Recipient string
	Accept    bool
	Domain    string
	Code      mxwire.MachineCode
	Temporary bool
}

// ResolveMXRecipients maps each envelope recipient to a routing decision using
// the same account/domain/alias/catch-all resolution and provider binding as
// webhook ingest. It is read-only. A recipient is accepted only when the
// domain's receiving provider is "mx"; unknown, disabled or foreign-provider
// recipients are uniformly rejected as unknown so existence is not leaked.
func (s *Service) ResolveMXRecipients(ctx context.Context, recipients []string) []MXResolveResult {
	return s.resolveMXRecipients(ctx, mxProvider, recipients)
}

func (s *Service) ResolveDialMXRecipients(ctx context.Context, recipients []string) []MXResolveResult {
	return s.resolveMXRecipients(ctx, "dialmx", recipients)
}

// selfHostedReceivingProviders returns the provider spellings that name a
// self-hosted receiver, most specific first.
//
// The self-hosted family is served by two spellings: the embedded/private MX
// edge ("mx") and the per-account Remote MX receiver ("remotemx"). Both describe
// the same kind of deployment — an operator-run receiver the core dials
// outbound to — and an account picks one per domain in the receiving dialog.
//
// A session backend serves whichever receiver it was started for, but the
// domain's receiving config records the operator's own choice. Resolving
// against only the backend's spelling means a domain configured for the other
// member of the family rejects every recipient as unknown (550 at RCPT), which
// reads as "no such mailbox" rather than "wrong provider name". Accepting the
// family keeps the operator's choice authoritative without leaking existence:
// if neither spelling resolves, the recipient is still reported unknown.
//
// "dialmx" (the hosted Antler MX service) is deliberately NOT part of the
// family: a domain routed to the hosted service must not be accepted by a
// self-hosted receiver, or vice versa.
func selfHostedReceivingProviders(provider string) []string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case mxProvider:
		return []string{mxProvider, RemoteMXProvider}
	case RemoteMXProvider:
		return []string{RemoteMXProvider, mxProvider}
	default:
		return []string{provider}
	}
}

// resolveInboundBindingForProvider resolves the inbound binding for a routing
// provider, tolerating either self-hosted provider spelling. The first spelling
// that resolves wins; when none does, the initial error is returned so the
// caller maps it to the same unknown-recipient verdict as before.
func (s *Service) resolveInboundBindingForProvider(ctx context.Context, routingProvider, recipient string) (transport.InboundBinding, error) {
	var firstErr error
	for _, p := range selfHostedReceivingProviders(routingProvider) {
		b, err := s.ResolveInboundBinding(ctx, p, recipient)
		if err == nil {
			return b, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return transport.InboundBinding{}, firstErr
}

func (s *Service) resolveMXRecipients(ctx context.Context, routingProvider string, recipients []string) []MXResolveResult {
	out := make([]MXResolveResult, 0, len(recipients))
	seen := map[string]bool{}
	for _, raw := range recipients {
		r := strings.ToLower(strings.TrimSpace(raw))
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		res := MXResolveResult{Recipient: r}
		binding, err := s.resolveInboundBindingForProvider(ctx, routingProvider, r)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				res.Code = mxwire.CodeUnknownRecipient
			} else {
				res.Code = mxwire.CodeTempFail
				res.Temporary = true
			}
			out = append(out, res)
			continue
		}
		// Resolve the actual inbox too, so a domain with an "mx" config but no
		// matching inbox/catch-all rejects before DATA rather than after.
		inbox, route, rerr := s.Store.ResolveRecipient(ctx, r)
		if rerr != nil || !recipientAuthorizedForBinding(inbox, route, binding.AccountID, binding.DomainID) {
			res.Code = mxwire.CodeUnknownRecipient
			out = append(out, res)
			continue
		}
		res.Accept = true
		res.Domain = domainOf(r)
		res.Code = mxwire.CodeOK
		out = append(out, res)
	}
	return out
}

// recipientAuthorizedForBinding applies the same route-aware account/domain
// binding rule as the shared ingest core: exact and catch-all matches must stay
// on the authenticated domain and account; only an explicit alias may cross
// domains within the account.
func recipientAuthorizedForBinding(inbox model.Inbox, route store.RecipientRoute, accountID, domainID string) bool {
	if inbox.AccountID != accountID {
		return false
	}
	if route == store.RouteAlias {
		return true
	}
	return inbox.DomainID == domainID
}

// MXIngestInput is one authenticated ingest request: the staged original MIME
// plus metadata received on an authorized session. Recipients is the whole
// accepted envelope recipient set for this one message.
type MXIngestInput struct {
	Recipients []string
	// Recipient is a single-recipient convenience; when Recipients is empty it
	// is used as the recipient set.
	Recipient    string
	EnvelopeFrom string
	RawPath      string
	Size         int64
	// ContentDigest is the hex SHA-256 of the original MIME, used to derive each
	// recipient's delivery fingerprint.
	ContentDigest     string
	AuthResults       mxwire.AuthResults
	TrustedAuth       bool
	ProviderMessageID string
}

// MXIngestResult is the durable outcome returned to the edge: one result per
// accepted recipient.
type MXIngestResult struct {
	PerRecipient []mxwire.RecipientIngestResult
	MessageID    string
}

// IngestMX persists an MX message for the whole accepted recipient set through
// the shared staged mailbox pipeline, applying the per-domain auth policy per
// recipient. The MIME is parsed once and fanned out internally; the edge never
// streams a copy per recipient. A duplicate delivery fingerprint returns the
// recorded disposition without a second message, quota charge or event. Auth
// failure is a durable Spam delivery, not a rejection. It must not be reached
// through provider webhook dispatch.
func (s *Service) IngestMX(ctx context.Context, in MXIngestInput) (MXIngestResult, error) {
	return s.ingestMX(ctx, mxProvider, in)
}

func (s *Service) IngestDialMX(ctx context.Context, in MXIngestInput) (MXIngestResult, error) {
	select {
	case s.dialMXIngestSem <- struct{}{}:
		defer func() { <-s.dialMXIngestSem }()
	case <-ctx.Done():
		return MXIngestResult{}, ctx.Err()
	}
	return s.ingestMX(ctx, "dialmx", in)
}

func (s *Service) ingestMX(ctx context.Context, routingProvider string, in MXIngestInput) (MXIngestResult, error) {
	if routingProvider == mxProvider {
		// The persisted MX receiver configuration is authoritative, not the
		// legacy MX_ENABLE environment. A configured receiver of either mode
		// enables the mx-routed ingest path; an absent or cleared one rejects
		// it, so a deployment that never configured MX cannot ingest.
		enabled, err := s.mxReceiverConfigured(ctx)
		if err != nil {
			return MXIngestResult{}, err
		}
		if !enabled {
			return MXIngestResult{}, ErrMXDisabled
		}
	}
	// Remote MX routes per recipient to that recipient's own account receiver; a
	// per-recipient miss is a rejection, not a global disable, so there is no
	// installation-wide gate here.
	recipients := in.Recipients
	if len(recipients) == 0 && strings.TrimSpace(in.Recipient) != "" {
		recipients = []string{in.Recipient}
	}
	out := MXIngestResult{}
	if len(recipients) == 0 {
		return out, nil
	}
	// Resolve the set once, de-duplicating case-insensitively while preserving
	// order, then map each recipient to its delivery inbox. Aliases that target
	// the same inbox collapse to one stored message (like the webhook fan-out),
	// with one result returned per envelope recipient so the edge's transaction
	// accounting stays exact.
	seen := map[string]bool{}
	ordered := make([]string, 0, len(recipients))
	for _, raw := range recipients {
		r := strings.ToLower(strings.TrimSpace(raw))
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		ordered = append(ordered, r)
	}
	if len(ordered) == 0 {
		return out, nil
	}

	// Parse the MIME once; a malformed message is a permanent per-recipient
	// invalid outcome, not a retryable one.
	parsed, perr := mailparse.ParseFile(in.RawPath, s.mimeLimits())
	if perr != nil {
		for _, r := range ordered {
			out.PerRecipient = append(out.PerRecipient, mxwire.RecipientIngestResult{Recipient: r, MachineCode: mxwire.CodeInvalid})
		}
		return out, nil
	}

	type inboundTarget struct {
		inboxID   string
		recipient string
	}
	byInbox := map[string]string{}
	targets := make([]inboundTarget, 0, len(ordered))
	for _, recipient := range ordered {
		inbox, route, rerr := s.Store.ResolveRecipient(ctx, recipient)
		if rerr != nil || inbox.ID == "" {
			targets = append(targets, inboundTarget{recipient: recipient})
			continue
		}
		_ = route
		if _, ok := byInbox[inbox.ID]; !ok {
			byInbox[inbox.ID] = recipient
		}
		targets = append(targets, inboundTarget{inboxID: inbox.ID, recipient: recipient})
	}
	// One physical commit per distinct inbox; the staged file is moved only for
	// a single-inbox message and copied per inbox otherwise.
	single := len(byInbox) == 1
	committed := make([]mxwire.RecipientIngestResult, len(targets))
	seenInbox := map[string]bool{}
	for i, t := range targets {
		if t.inboxID == "" {
			res := s.ingestMXRecipient(ctx, routingProvider, in, t.recipient, parsed, single)
			committed[i] = res
			if out.MessageID == "" && res.MessageID != "" {
				out.MessageID = res.MessageID
			}
			continue
		}
		if seenInbox[t.inboxID] {
			continue
		}
		seenInbox[t.inboxID] = true
		res := s.ingestMXRecipient(ctx, routingProvider, in, t.recipient, parsed, single)
		committed[i] = res
		if out.MessageID == "" && res.MessageID != "" {
			out.MessageID = res.MessageID
		}
	}
	// Fan mirrored per-recipient results out of the inbox's canonical result so
	// each envelope recipient reports the same durable outcome.
	for i, t := range targets {
		if t.inboxID == "" || committed[i].Recipient != "" {
			out.PerRecipient = append(out.PerRecipient, committed[i])
			continue
		}
		for _, c := range committed {
			if c.Recipient != "" {
				anchor, _, _ := s.Store.ResolveRecipient(ctx, c.Recipient)
				if anchor.ID == t.inboxID {
					mirror := c
					mirror.Recipient = t.recipient
					mirror.Duplicate = true
					mirror.MachineCode = mxwire.CodeDuplicate
					out.PerRecipient = append(out.PerRecipient, mirror)
					break
				}
			}
		}
	}
	return out, nil
}

// ingestMXRecipient resolves, authorizes and persists one recipient. Each
// envelope recipient gets its own durable message and receipt, so a retry of the
// same recipient set deduplicates per recipient.
func (s *Service) ingestMXRecipient(ctx context.Context, routingProvider string, in MXIngestInput, recipient string, parsed mailparse.Parsed, single bool) mxwire.RecipientIngestResult {
	binding, err := s.Store.ResolveInboundBinding(ctx, routingProvider, recipient)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return mxwire.RecipientIngestResult{Recipient: recipient, MachineCode: mxwire.CodeUnknownRecipient}
		}
		return mxwire.RecipientIngestResult{Recipient: recipient, MachineCode: mxwire.CodeTempFail}
	}
	fingerprint := mxwire.DeliveryFingerprint(in.EnvelopeFrom, recipient, in.ContentDigest)
	// Duplicate check happens before touching MIME: a recorded receipt is the
	// durable truth and must survive the original message's deletion.
	if rec, rerr := s.Store.LookupMXReceipt(ctx, binding.AccountID, mxProvider, recipient, fingerprint); rerr == nil {
		return mxwire.RecipientIngestResult{
			Recipient:   recipient,
			Disposition: mxwire.Disposition(rec.Disposition),
			MachineCode: mxwire.CodeDuplicate,
			MessageID:   rec.MessageID,
			Reason:      rec.Reason,
			Duplicate:   true,
		}
	} else if !errors.Is(rerr, store.ErrNotFound) {
		return mxwire.RecipientIngestResult{Recipient: recipient, MachineCode: mxwire.CodeTempFail}
	}
	inbox, route, err := s.Store.ResolveRecipient(ctx, recipient)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return mxwire.RecipientIngestResult{Recipient: recipient, MachineCode: mxwire.CodeUnknownRecipient}
		}
		return mxwire.RecipientIngestResult{Recipient: recipient, MachineCode: mxwire.CodeTempFail}
	}
	if !recipientAuthorizedForBinding(inbox, route, binding.AccountID, binding.DomainID) {
		return mxwire.RecipientIngestResult{Recipient: recipient, MachineCode: mxwire.CodeUnknownRecipient}
	}

	enforcement := s.domainEnforcement(ctx, binding.AccountID, binding.DomainID, routingProvider)
	class := mxwire.Classify(mxAuthPtr(in.AuthResults, in.TrustedAuth), enforcement)
	authJSON := ""
	if in.TrustedAuth {
		if b, merr := json.Marshal(in.AuthResults); merr == nil {
			authJSON = string(b)
		}
	}
	authenticated := in.TrustedAuth && mxAuthenticated(in.AuthResults)

	msg := transport.InboundMessage{
		Provider:            mxProvider,
		Recipient:           recipient,
		EnvelopeFrom:        in.EnvelopeFrom,
		RawPath:             in.RawPath,
		Size:                in.Size,
		DeliveryID:          fingerprint,
		EnvelopeFingerprint: fingerprint,
		ProviderMessageID:   in.ProviderMessageID,
		AuthResults:         in.AuthResults,
		TrustedAuth:         in.TrustedAuth,
	}
	m, dup, err := s.deliverStaged(ctx, mxProvider, msg, inbox, parsed, single, &mxDeliverAuth{
		Spam:          class.Spam,
		Reason:        class.Reason,
		AuthJSON:      authJSON,
		Fingerprint:   fingerprint,
		ReceiptTTL:    s.Config.MXReceiptRetention,
		Authenticated: authenticated,
	})
	disp := mxwire.DispositionStored
	if class.Spam {
		disp = mxwire.DispositionSpam
	}
	if err != nil {
		switch {
		case errors.Is(err, transport.ErrInboundIgnored):
			// Consumed control mail (an approval decision) is a durable,
			// terminal outcome: acknowledge it so the edge returns 250 and the
			// sender does not retry. Record a receipt so a byte-identical
			// signed replay is deduplicated even though no message row exists.
			receipt := store.MXReceipt{
				AccountID:           binding.AccountID,
				Provider:            mxProvider,
				EnvelopeRecipient:   recipient,
				DeliveryFingerprint: fingerprint,
				Disposition:         store.DispositionControl,
				Reason:              "control",
			}
			if s.Config.MXReceiptRetention > 0 {
				receipt.ExpiresAt = time.Now().UTC().Add(s.Config.MXReceiptRetention)
			}
			if rerr := s.Store.RecordMXReceipt(ctx, receipt); rerr != nil {
				return mxwire.RecipientIngestResult{Recipient: recipient, MachineCode: mxwire.CodeTempFail}
			}
			return mxwire.RecipientIngestResult{Recipient: recipient, Disposition: mxwire.DispositionControl, MachineCode: mxwire.CodeOK}
		case errors.Is(err, store.ErrQuota):
			return mxwire.RecipientIngestResult{Recipient: recipient, MachineCode: mxwire.CodeQuota}
		case errors.Is(err, store.ErrNotFound), errors.Is(err, transport.ErrInboundUnauthorized):
			return mxwire.RecipientIngestResult{Recipient: recipient, MachineCode: mxwire.CodeUnknownRecipient}
		case isTerminalAppError(err):
			return mxwire.RecipientIngestResult{Recipient: recipient, MachineCode: mxwire.CodeInvalid}
		default:
			return mxwire.RecipientIngestResult{Recipient: recipient, MachineCode: mxwire.CodeTempFail}
		}
	}
	code := mxwire.CodeOK
	if dup {
		code = mxwire.CodeDuplicate
	}
	return mxwire.RecipientIngestResult{Recipient: recipient, Disposition: disp, MachineCode: code, MessageID: m.ID, Reason: class.Reason, Duplicate: dup}
}

// domainEnforcement reads the per-domain auth enforcement mode, defaulting to
// moderate. The stored receiving config carries it in the "enforcement" field
// once the domain editor exposes the control; absent values are moderate.
func (s *Service) domainEnforcement(ctx context.Context, accountID, domainID, routingProvider string) mxwire.Enforcement {
	b, err := s.Store.ResolveDomainReceivingConfig(ctx, accountID, domainID, routingProvider)
	if err != nil {
		return mxwire.EnforcementModerate
	}
	cfg, err := s.decryptConfig(configAAD(b.AccountID, b.DomainID), b.EncryptedConfig)
	if err != nil {
		return mxwire.EnforcementModerate
	}
	if v, _ := cfg["enforcement"].(string); strings.EqualFold(strings.TrimSpace(v), string(mxwire.EnforcementHard)) {
		return mxwire.EnforcementHard
	}
	return mxwire.EnforcementModerate
}

func mxAuthPtr(a mxwire.AuthResults, trusted bool) *mxwire.AuthResults {
	if !trusted {
		return nil
	}
	return &a
}

// mxAuthenticated reports whether the edge evidence establishes that the
// RFC5322.From domain is authenticated: a DMARC pass, or an SPF or DKIM pass
// that aligns with the From domain. It is deliberately conservative: absent,
// neutral, softfail, temperror and permerror never count, and an unaligned pass
// of a lookalike domain never counts. It is only consulted when the edge's
// evidence is trusted.
func mxAuthenticated(a mxwire.AuthResults) bool {
	// When a DMARC policy was discovered, DMARC is the authority: an aligned
	// SPF/DKIM pass that the policy itself rejects (for example a strict
	// adkim=s From domain with only a relaxed DKIM pass) must not authenticate.
	// Only when no policy was available do we fall back to the per-mechanism
	// alignment flags.
	if a.DMARC != nil && (strings.EqualFold(strings.TrimSpace(a.DMARC.Result), "pass") || strings.EqualFold(strings.TrimSpace(a.DMARC.Result), "fail")) {
		return strings.EqualFold(strings.TrimSpace(a.DMARC.Result), "pass")
	}
	if a.SPF != nil && strings.EqualFold(strings.TrimSpace(a.SPF.Result), "pass") &&
		a.SPF.Aligned {
		return true
	}
	for _, d := range a.DKIM {
		if strings.EqualFold(strings.TrimSpace(d.Result), "pass") && d.Aligned {
			return true
		}
	}
	return false
}

// isTerminalAppError reports whether an MX ingest error is permanent and the
// edge should not retry. It mirrors the HTTP layer's provider terminal set.
func isTerminalAppError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, prefix := range []string{
		"message too large",
		"parse MIME:",
		"multipart without boundary",
		"too many mime parts",
		"mime nesting too deep",
	} {
		if strings.HasPrefix(msg, prefix) {
			return true
		}
	}
	return false
}
