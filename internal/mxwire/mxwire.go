// Package mxwire defines MX session framing, normalized authentication evidence,
// bounds, durable outcome codes and retry fingerprints shared by receiver/core.
package mxwire

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Bounds. These are the package defaults; the core may apply a tighter
// configured body cap. Metadata must stay well under typical proxy header
// limits because it is carried as a bounded JSON field, not a header.
const (
	// MaxMetadataBytes bounds the encoded metadata JSON.
	MaxMetadataBytes = 16 << 10
	// MaxResolveRecipients bounds one resolve request.
	MaxResolveRecipients = 100
	// DefaultMaxBodyBytes mirrors the core default 30 MiB message cap.
	DefaultMaxBodyBytes = 30 << 20
	// MaxDKIMSignatures bounds how many signatures the edge evaluates.
	MaxDKIMSignatures = 10
)

// Disposition is the durable outcome the core returns for an ingest.
type Disposition string

const (
	DispositionStored  Disposition = "stored"
	DispositionSpam    Disposition = "spam"
	DispositionBlocked Disposition = "blocked"
	DispositionControl Disposition = "control"
)

// MachineCode is a stable, versioned outcome code. It is an internal contract,
// not an SMTP response; the edge maps it to the SMTP transaction result.
type MachineCode string

const (
	CodeOK               MachineCode = "ok"
	CodeDuplicate        MachineCode = "duplicate"
	CodeUnknownRecipient MachineCode = "unknown_recipient"
	CodeUnauthorized     MachineCode = "unauthorized"
	CodeQuota            MachineCode = "quota_exceeded"
	CodeTooLarge         MachineCode = "too_large"
	CodeInvalid          MachineCode = "invalid_message"
	CodeTempFail         MachineCode = "temporary_failure"
	CodeReplay           MachineCode = "replay_rejected"
)

// AuthResults is the bounded, normalized authentication evidence the
// authenticated edge supplies. The policy engine consumes only this; incoming
// Authentication-Results/Received-SPF headers are never trusted as a
// substitute for it.
type AuthResults struct {
	SPF       *SPFEvidence   `json:"spf,omitempty"`
	DKIM      []DKIMEvidence `json:"dkim,omitempty"`
	DMARC     *DMARCEvidence `json:"dmarc,omitempty"`
	Evaluator string         `json:"evaluator,omitempty"`
	Source    string         `json:"source,omitempty"`
}

// SPFEvidence is one SPF evaluation. Result and alignment are kept separate:
// a valid but unaligned mechanism is still a pass.
type SPFEvidence struct {
	// Result is one of pass, fail, softfail, neutral, none, temperror, permerror.
	Result string `json:"result"`
	// Domain is the identity actually checked.
	Domain string `json:"domain,omitempty"`
	// Scope is "mailfrom" or "helo".
	Scope string `json:"scope,omitempty"`
	// Aligned reports whether Domain aligns with the From domain under the
	// discovered DMARC mode.
	Aligned bool `json:"aligned,omitempty"`
	// Reason is a bounded diagnostic classification, never a raw resolver error.
	Reason string `json:"reason,omitempty"`
}

// DKIMEvidence is one signature's verification result. Cap the count with
// MaxDKIMSignatures before encoding.
type DKIMEvidence struct {
	// Result is one of pass, fail, none, temperror, permerror.
	Result   string `json:"result"`
	Domain   string `json:"domain,omitempty"`
	Selector string `json:"selector,omitempty"`
	Algo     string `json:"algo,omitempty"`
	Aligned  bool   `json:"aligned,omitempty"`
	// Error is a bounded diagnostic classification, never raw key material.
	Error string `json:"error,omitempty"`
}

// DMARCEvidence is the locally evaluated DMARC result for the From domain.
type DMARCEvidence struct {
	// Result is one of pass, fail, none, temperror, permerror. A published
	// p=none is NOT result none.
	Result string `json:"result"`
	// FromDomain is the RFC5322.From domain the policy was evaluated for.
	FromDomain string `json:"from_domain,omitempty"`
	// PolicyDomain is the organizational domain whose record was discovered.
	PolicyDomain string `json:"policy_domain,omitempty"`
	// Policy is the published p= policy (none|quarantine|reject), preserved and
	// displayed even when the local disposition differs.
	Policy      string `json:"policy,omitempty"`
	SPFAligned  bool   `json:"spf_aligned,omitempty"`
	DKIMAligned bool   `json:"dkim_aligned,omitempty"`
	// Reason is a bounded classification, never a raw resolver error.
	Reason string `json:"reason,omitempty"`
}

// Enforcement is the per-domain local Spam policy.
type Enforcement string

const (
	EnforcementModerate Enforcement = "moderate"
	EnforcementHard     Enforcement = "hard"
)

// Classification is the pure policy outcome.
type Classification struct {
	Spam   bool
	Reason string
}

// dkimAggregate classifies the DKIM signature set per the frozen V1 semantics.
func (a *AuthResults) dkimAggregate() string {
	if a == nil || len(a.DKIM) == 0 {
		return "none"
	}
	var passed, failed, unresolved int
	for _, s := range a.DKIM {
		switch strings.ToLower(strings.TrimSpace(s.Result)) {
		case "pass":
			passed++
		case "fail":
			failed++
		default:
			// temperror, permerror, none, empty: unresolved evidence, never a
			// definitive cryptographic failure on its own.
			unresolved++
		}
	}
	switch {
	case passed > 0:
		return "pass"
	case failed > 0 && unresolved == 0:
		return "fail"
	case failed > 0:
		return "indeterminate"
	default:
		return "none"
	}
}

func spfFailed(a *AuthResults) bool {
	return a != nil && a.SPF != nil && strings.EqualFold(strings.TrimSpace(a.SPF.Result), "fail")
}

func dkimFailed(a *AuthResults) bool { return a.dkimAggregate() == "fail" }

func dmarcDefinitiveFail(a *AuthResults) bool {
	return a != nil && a.DMARC != nil && strings.EqualFold(strings.TrimSpace(a.DMARC.Result), "fail")
}

// Classify applies the frozen V1 policy. moderate: Spam for a definitive DMARC
// failure, or SPF and DKIM both definitively failed. hard: Spam for any one of
// SPF fail, DKIM fail or a definitive DMARC fail. Absent/neutral/softfail/
// temperror/permerror evidence never counts as a failure on its own.
func Classify(a *AuthResults, mode Enforcement) Classification {
	if mode != EnforcementHard {
		mode = EnforcementModerate
	}
	spf := spfFailed(a)
	dkim := dkimFailed(a)
	dmarc := dmarcDefinitiveFail(a)
	switch mode {
	case EnforcementHard:
		switch {
		case spf && dkim:
			return Classification{Spam: true, Reason: "auth:hard spf+dkim fail"}
		case spf:
			return Classification{Spam: true, Reason: "auth:hard spf fail"}
		case dkim:
			return Classification{Spam: true, Reason: "auth:hard dkim fail"}
		case dmarc:
			return Classification{Spam: true, Reason: "auth:hard dmarc fail"}
		}
	default:
		switch {
		case dmarc:
			return Classification{Spam: true, Reason: "auth:dmarc fail"}
		case spf && dkim:
			return Classification{Spam: true, Reason: "auth:moderate spf+dkim fail"}
		}
	}
	return Classification{}
}

// ResolveRecipient is one routing decision.
type ResolveRecipient struct {
	Recipient string `json:"recipient"`
	Accept    bool   `json:"accept"`
	Domain    string `json:"domain,omitempty"`
	Code      string `json:"code,omitempty"`
	// Temporary distinguishes a transient internal failure from a permanent
	// unknown/unauthorized recipient.
	Temporary bool `json:"temporary,omitempty"`
}

// ResolveResponse is the bounded resolve result.
type ResolveResponse struct {
	Version     string             `json:"version"`
	Results     []ResolveRecipient `json:"results"`
	MachineCode MachineCode        `json:"machine_code"`
}

// IngestMetadata carries SMTP evidence and original MIME integrity information
// over an authorized session. The core fans out the accepted recipient set.
type IngestMetadata struct {
	// Recipients is the accepted envelope recipient set this ingest covers.
	Recipients []string `json:"recipients"`
	// EnvelopeFrom is the SMTP MAIL FROM (may be empty for the null path).
	EnvelopeFrom string `json:"envelope_from,omitempty"`
	ClientIP     string `json:"client_ip,omitempty"`
	HELO         string `json:"helo,omitempty"`
	// ContentDigest is the hex SHA-256 of the original MIME bytes.
	ContentDigest     string      `json:"content_digest"`
	Size              int64       `json:"size"`
	AuthResults       AuthResults `json:"auth_results"`
	ProviderMessageID string      `json:"provider_message_id,omitempty"`
}

// IngestResponse is the durable outcome for one ingest request. PerRecipient
// carries the outcome for each accepted recipient; the edge aggregates these
// into the single SMTP transaction response.
type IngestResponse struct {
	Version      string                  `json:"version"`
	MachineCode  MachineCode             `json:"machine_code"`
	PerRecipient []RecipientIngestResult `json:"per_recipient,omitempty"`
	// MessageID is the first stored message, for logs and simple callers.
	MessageID string `json:"message_id,omitempty"`
}

// RecipientIngestResult is the durable outcome for one recipient. A duplicate
// carries the originally recorded disposition and entity reference.
type RecipientIngestResult struct {
	Recipient   string      `json:"recipient"`
	Disposition Disposition `json:"disposition"`
	MachineCode MachineCode `json:"machine_code"`
	MessageID   string      `json:"message_id,omitempty"`
	Reason      string      `json:"reason,omitempty"`
	Duplicate   bool        `json:"duplicate,omitempty"`
}

func hexHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// BodyDigest returns the hex SHA-256 of b, the content digest carried in the
// session metadata and re-checked against the streamed body.
func BodyDigest(b []byte) string { return hexHash(b) }

// FingerprintVersion is prefixed to a delivery fingerprint so its scheme is
// explicit and can evolve.
const FingerprintVersion = "mxfp-v1"

// DeliveryFingerprint derives the versioned retry identity over canonical
// envelope sender, canonical recipient and the SHA-256 of the original MIME,
// computed before any local trace/header change. It is not RFC Message-ID: that
// is sender-controlled and may be reused for different content.
func DeliveryFingerprint(envelopeFrom, recipient, mimeDigest string) string {
	h := sha256.New()
	h.Write([]byte(FingerprintVersion))
	h.Write([]byte{0})
	h.Write([]byte(strings.ToLower(strings.TrimSpace(envelopeFrom))))
	h.Write([]byte{0})
	h.Write([]byte(strings.ToLower(strings.TrimSpace(recipient))))
	h.Write([]byte{0})
	h.Write([]byte(mimeDigest))
	return FingerprintVersion + ":" + hex.EncodeToString(h.Sum(nil))
}
