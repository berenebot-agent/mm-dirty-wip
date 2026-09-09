// Package transport defines the provider-neutral inbound webhook boundary.
//
// Each inbound provider (Mailgun, Cloudflare Email Routing via Worker, ...)
// implements InboundTransport: parse its own webhook format into an
// InboundMessage, then verify authenticity with its own scheme. The mailbox
// core downstream of this boundary (recipient resolution, MIME parsing,
// CommitInbound, event publishing) is transport-agnostic.
package transport

import (
	"errors"
	"net/http"
)

var (
	ErrUnknownProvider     = errors.New("unknown inbound provider")
	ErrInboundUnauthorized = errors.New("inbound webhook unauthorized")
)

// InboundMessage is a parsed provider webhook, normalized for the shared
// ingest pipeline. Raw MIME is staged at RawPath (0600 temp file, bounded by
// maxBytes) so large messages stream to disk instead of RAM.
//
// Timestamp, Token and Signature carry provider auth material and are
// interpreted per provider (Mailgun HMAC fields). Providers using other
// schemes (e.g. Bearer header) leave them empty and read what they need
// from the request in Verify.
type InboundMessage struct {
	Provider          string
	Recipient         string
	EnvelopeFrom      string
	RawPath           string
	Size              int64
	DeliveryID        string
	ProviderMessageID string
	Timestamp         string
	Token             string
	Signature         string
}

type InboundTransport interface {
	Name() string
	Parse(r *http.Request, tmpPath string, maxBytes int64) (InboundMessage, error)
	Verify(r *http.Request, msg InboundMessage, secret string) error
}

// PreVerifyTransport is implemented by inbound transports whose authenticity
// can be established from the request headers alone, before the (potentially
// large) MIME body is read. Cloudflare bearer auth is one such case.
type PreVerifyTransport interface {
	VerifyBeforeParse() bool
}
