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
// plus the signed metadata the core already verified.
type MXIngestInput struct {
	Recipient           string
	EnvelopeFrom        string
	RawPath             string
	Size                int64
	DeliveryFingerprint string
	AuthResults         mxwire.AuthResults
	TrustedAuth         bool
	ProviderMessageID   string
}

// MXIngestResult is the durable outcome returned to the edge.
type MXIngestResult struct {
	Disposition mxwire.Disposition
	Code        mxwire.MachineCode
	MessageID   string
	Reason      string
	Duplicate   bool
}

// IngestMX persists one accepted recipient's MX message through the shared
// staged mailbox pipeline, applying the per-domain auth policy. A duplicate
// delivery fingerprint returns the recorded disposition without a second
// message, quota charge or event. Auth failure is a durable Spam delivery, not
// a rejection. It must not be reached through provider webhook dispatch.
func (s *Service) IngestMX(ctx context.Context, in MXIngestInput) (MXIngestResult, error) {
	if !s.Config.MXReceiveEnabled {
		return MXIngestResult{}, ErrMXDisabled
	}
	recipient := strings.ToLower(strings.TrimSpace(in.Recipient))
	if recipient == "" {
		return MXIngestResult{Code: mxwire.CodeInvalid}, nil
	}
	binding, err := s.Store.ResolveInboundBinding(ctx, mxProvider, recipient)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return MXIngestResult{Code: mxwire.CodeUnknownRecipient}, nil
		}
		return MXIngestResult{Code: mxwire.CodeTempFail}, err
	}
	// Duplicate check happens before touching MIME: a recorded receipt is the
	// durable truth and must survive the original message's deletion.
	if fp := strings.TrimSpace(in.DeliveryFingerprint); fp != "" {
		rec, rerr := s.Store.LookupMXReceipt(ctx, binding.AccountID, mxProvider, recipient, fp)
		switch {
		case rerr == nil:
			return MXIngestResult{
				Disposition: mxwire.Disposition(rec.Disposition),
				Code:        mxwire.CodeDuplicate,
				MessageID:   rec.MessageID,
				Reason:      rec.Reason,
				Duplicate:   true,
			}, nil
		case !errors.Is(rerr, store.ErrNotFound):
			return MXIngestResult{Code: mxwire.CodeTempFail}, rerr
		}
	}
	inbox, route, err := s.Store.ResolveRecipient(ctx, recipient)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return MXIngestResult{Code: mxwire.CodeUnknownRecipient}, nil
		}
		return MXIngestResult{Code: mxwire.CodeTempFail}, err
	}
	if !recipientAuthorizedForBinding(inbox, route, binding.AccountID, binding.DomainID) {
		return MXIngestResult{Code: mxwire.CodeUnknownRecipient}, nil
	}

	enforcement := s.domainEnforcement(ctx, binding.AccountID, binding.DomainID)
	class := mxwire.Classify(mxAuthPtr(in.AuthResults, in.TrustedAuth), enforcement)
	authJSON := ""
	if in.TrustedAuth {
		if b, merr := json.Marshal(in.AuthResults); merr == nil {
			authJSON = string(b)
		}
	}

	parsed, err := mailparse.ParseFile(in.RawPath, s.mimeLimits())
	if err != nil {
		return MXIngestResult{Code: mxwire.CodeInvalid}, nil
	}
	msg := transport.InboundMessage{
		Provider:            mxProvider,
		Recipient:           recipient,
		EnvelopeFrom:        in.EnvelopeFrom,
		RawPath:             in.RawPath,
		Size:                in.Size,
		DeliveryID:          in.DeliveryFingerprint,
		EnvelopeFingerprint: in.DeliveryFingerprint,
		ProviderMessageID:   in.ProviderMessageID,
		AuthResults:         in.AuthResults,
		TrustedAuth:         in.TrustedAuth,
	}
	m, dup, err := s.deliverStaged(ctx, mxProvider, msg, inbox, parsed, true, &mxDeliverAuth{
		Spam:        class.Spam,
		Reason:      class.Reason,
		AuthJSON:    authJSON,
		Fingerprint: in.DeliveryFingerprint,
	})
	if err != nil {
		switch {
		case errors.Is(err, transport.ErrInboundIgnored):
			// Consumed control mail (an approval decision) is a durable, terminal
			// outcome: acknowledge it so the edge returns 250 and the sender does
			// not retry. The control handler records its own dedup key.
			return MXIngestResult{Disposition: mxwire.DispositionControl, Code: mxwire.CodeOK}, nil
		case errors.Is(err, store.ErrQuota):
			return MXIngestResult{Code: mxwire.CodeQuota}, nil
		case errors.Is(err, store.ErrNotFound), errors.Is(err, transport.ErrInboundUnauthorized):
			return MXIngestResult{Code: mxwire.CodeUnknownRecipient}, nil
		case isTerminalAppError(err):
			return MXIngestResult{Code: mxwire.CodeInvalid}, nil
		default:
			return MXIngestResult{Code: mxwire.CodeTempFail}, err
		}
	}
	disp := mxwire.DispositionStored
	if class.Spam {
		disp = mxwire.DispositionSpam
	}
	code := mxwire.CodeOK
	if dup {
		code = mxwire.CodeDuplicate
	}
	return MXIngestResult{Disposition: disp, Code: code, MessageID: m.ID, Reason: class.Reason, Duplicate: dup}, nil
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
