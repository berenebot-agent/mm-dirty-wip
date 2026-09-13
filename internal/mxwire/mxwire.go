// Package mxwire is the shared, versioned contract between the optional MX
// edge (cmd/mx, internal/mxagent) and the core (internal/app). It holds the
// request envelope, the normalized authentication-evidence model, the bound
// limits, the machine codes and the HMAC sign/verify helpers. Both binaries are
// built from this module, but deployed version skew is still possible, so the
// protocol version is mandatory and unsupported versions are rejected.
package mxwire

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ProtocolVersion is the only wire version this build speaks.
const ProtocolVersion = "mx-v1"

// Endpoint paths. They live on the dedicated inbound connector listener.
const (
	PathResolve = "/internal/mx/resolve"
	PathIngest  = "/internal/mx/ingest"
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
	// MaxSignatureSkew bounds how old a signed request may be. It bounds the
	// replay duration, not replay itself; request IDs are recorded to reject
	// conflicting reuse.
	MaxSignatureSkew = 10 * time.Minute
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

var (
	// ErrSignature is returned when a request signature does not verify.
	ErrSignature = errors.New("mxwire: invalid signature")
	// ErrSkew is returned when a signed request timestamp is outside the
	// accepted window.
	ErrSkew = errors.New("mxwire: timestamp outside accepted window")
	// ErrVersion is returned for an unsupported protocol version.
	ErrVersion = errors.New("mxwire: unsupported protocol version")
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

// ResolveRequest is the metadata envelope of POST /internal/mx/resolve. Per
// the frozen wire contract, the bounded recipient JSON is the request BODY
// (hashed into the signature), and irrelevant ingest metadata is absent under
// this schema.
type ResolveRequest struct {
	Version   string `json:"version"`
	KeyID     string `json:"key_id"`
	Timestamp int64  `json:"timestamp"`
	RequestID string `json:"request_id"`
	Edge      string `json:"edge,omitempty"`
}

// ResolveBody is the bounded resolve body: the recipient list.
type ResolveBody struct {
	Recipients []string `json:"recipients"`
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

// IngestMetadata is the signed metadata for one POST /internal/mx/ingest. Its
// exact transmitted JSON bytes are hashed into the signature; core never reads
// routing or auth material from unsigned headers.
type IngestMetadata struct {
	Version string `json:"version"`
	KeyID   string `json:"key_id"`
	// Timestamp is unix seconds; skew-bounded.
	Timestamp int64  `json:"timestamp"`
	RequestID string `json:"request_id"`
	Edge      string `json:"edge,omitempty"`
	// Recipient is the canonical envelope recipient this ingest is for.
	Recipient string `json:"recipient"`
	// EnvelopeFrom is the SMTP MAIL FROM (may be empty for the null path).
	EnvelopeFrom string `json:"envelope_from,omitempty"`
	ClientIP     string `json:"client_ip,omitempty"`
	HELO         string `json:"helo,omitempty"`
	// ContentDigest is the hex SHA-256 of the original MIME bytes.
	ContentDigest string `json:"content_digest"`
	Size          int64  `json:"size"`
	// DeliveryFingerprint is the versioned retry identity over canonical
	// envelope sender, recipient and MIME digest.
	DeliveryFingerprint string      `json:"delivery_fingerprint"`
	AuthResults         AuthResults `json:"auth_results"`
	ProviderMessageID   string      `json:"provider_message_id,omitempty"`
}

// IngestResponse is the durable outcome for one recipient. A duplicate carries
// the originally recorded disposition and entity reference.
type IngestResponse struct {
	Version     string      `json:"version"`
	Disposition Disposition `json:"disposition"`
	MachineCode MachineCode `json:"machine_code"`
	MessageID   string      `json:"message_id,omitempty"`
	Reason      string      `json:"reason,omitempty"`
	Duplicate   bool        `json:"duplicate,omitempty"`
}

// CanonicalString builds the exact string that is signed. method and path are
// fixed by the endpoint; metaBytes and bodyBytes are the exact transmitted
// bytes (metadata JSON and, for ingest, the original MIME).
func CanonicalString(version, keyID string, ts int64, requestID, method, path string, metaBytes, bodyBytes []byte) string {
	return version + "\n" +
		fmt.Sprintf("%d", ts) + "\n" +
		requestID + "\n" +
		keyID + "\n" +
		method + "\n" +
		path + "\n" +
		hexHash(metaBytes) + "\n" +
		hexHash(bodyBytes)
}

func hexHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Sign returns the hex HMAC-SHA256 over the canonical string.
func Sign(key []byte, version, keyID string, ts int64, requestID, method, path string, metaBytes, bodyBytes []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(CanonicalString(version, keyID, ts, requestID, method, path, metaBytes, bodyBytes)))
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify checks version, timestamp skew, key id and constant-time signature.
// Callers supply the resolved key for keyID and the clock/skew.
func Verify(key []byte, version, expectedKeyID, keyID string, ts int64, requestID, method, path string, metaBytes, bodyBytes []byte, signature string, now time.Time, skew time.Duration) error {
	if version != ProtocolVersion {
		return fmt.Errorf("%w: %q", ErrVersion, version)
	}
	if expectedKeyID == "" || keyID != expectedKeyID {
		return fmt.Errorf("%w: unknown key id", ErrSignature)
	}
	if skew <= 0 {
		skew = MaxSignatureSkew
	}
	delta := now.Sub(time.Unix(ts, 0))
	if delta < -skew || delta > skew {
		return ErrSkew
	}
	want, err := hex.DecodeString(Sign(key, version, keyID, ts, requestID, method, path, metaBytes, bodyBytes))
	if err != nil {
		return ErrSignature
	}
	got, err := hex.DecodeString(strings.TrimSpace(signature))
	if err != nil {
		return ErrSignature
	}
	if subtle.ConstantTimeCompare(want, got) != 1 {
		return ErrSignature
	}
	return nil
}

// BodyDigest returns the hex SHA-256 of b, the content digest carried in the
// signed metadata and re-checked against the streamed body.
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
