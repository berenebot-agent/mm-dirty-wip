package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"gatehouse-mail/internal/mailparse"
	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/mxwire"
	"gatehouse-mail/internal/store"
	"gatehouse-mail/internal/transport"
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
	out := make([]MXResolveResult, 0, len(recipients))
	seen := map[string]bool{}
	for _, raw := range recipients {
		r := strings.ToLower(strings.TrimSpace(raw))
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		res := MXResolveResult{Recipient: r}
		binding, err := s.Store.ResolveInboundBinding(ctx, mxProvider, r)
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
// plus the signed metadata the core already verified. Recipients is the whole
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
	if !s.Config.MXReceiveEnabled {
		return MXIngestResult{}, ErrMXDisabled
	}
	recipients := in.Recipients
	if len(recipients) == 0 && strings.TrimSpace(in.Recipient) != "" {
		recipients = []string{in.Recipient}
	}
	out := MXIngestResult{}
	if len(recipients) == 0 {
		return out, nil
	}
	// Resolve the set once, de-duplicating case-insensitively while preserving
	// order.
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

	single := len(ordered) == 1
	for _, recipient := range ordered {
		res := s.ingestMXRecipient(ctx, in, recipient, parsed, single)
		if out.MessageID == "" && res.MessageID != "" {
			out.MessageID = res.MessageID
		}
		out.PerRecipient = append(out.PerRecipient, res)
	}
	return out, nil
}

// ingestMXRecipient resolves, authorizes and persists one recipient. Each
// envelope recipient gets its own durable message and receipt, so a retry of the
// same recipient set deduplicates per recipient.
func (s *Service) ingestMXRecipient(ctx context.Context, in MXIngestInput, recipient string, parsed mailparse.Parsed, single bool) mxwire.RecipientIngestResult {
	binding, err := s.Store.ResolveInboundBinding(ctx, mxProvider, recipient)
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

	enforcement := s.domainEnforcement(ctx, binding.AccountID, binding.DomainID)
	class := mxwire.Classify(mxAuthPtr(in.AuthResults, in.TrustedAuth), enforcement)
	authJSON := ""
	if in.TrustedAuth {
		if b, merr := json.Marshal(in.AuthResults); merr == nil {
			authJSON = string(b)
		}
	}

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
		Spam:        class.Spam,
		Reason:      class.Reason,
		AuthJSON:    authJSON,
		Fingerprint: fingerprint,
		ReceiptTTL:  s.Config.MXReceiptRetention,
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
			// sender does not retry.
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
func (s *Service) domainEnforcement(ctx context.Context, accountID, domainID string) mxwire.Enforcement {
	b, err := s.Store.GetDomainReceivingConfig(ctx, accountID, domainID)
	if err != nil {
		return mxwire.EnforcementModerate
	}
	cfg, err := s.decryptConfig(b.EncryptedConfig)
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
