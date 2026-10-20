package mxagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
)

// This file defines the cross-package observability contract between the SMTP
// edge (this package) and whatever implements the Delivery interface (the
// in-process receiver). The edge assigns a stable smtp_connection_id to every
// accepted TCP connection and a fresh message_transaction_id to every MAIL
// transaction, then carries both on the context handed to Resolve and Ingest.
// A receiver that wants correlated logs reads them with
// TransactionAttrsFromContext, without importing edge internals or changing the
// Delivery interface.
//
// The edge never puts message content, MIME, subject, authentication material
// or credentials on this context: only opaque correlation ids and the
// connection-scoped envelope facts already observable at the SMTP edge.

const (
	// AttrConnectionID is the slog key for the per-TCP-connection correlation
	// id. It is stable across STARTTLS on one connection.
	AttrConnectionID = "smtp_connection_id"
	// AttrTransactionID is the slog key for the per-MAIL-transaction
	// correlation id. It changes for every MAIL transaction on a connection.
	AttrTransactionID = "message_transaction_id"
	// AttrPeerIP and AttrHELO are the connection-scoped envelope facts shared
	// for correlation; they are not authentication.
	AttrPeerIP = "peer_ip"
	AttrHELO   = "helo"
)

// TransactionAttrs is the correlation identity of one message transaction on
// one SMTP connection. It is safe to log: it deliberately contains no message
// content or secrets.
type TransactionAttrs struct {
	// ConnectionID identifies the accepted TCP connection. It is retained
	// across STARTTLS.
	ConnectionID string
	// TransactionID identifies one MAIL transaction on that connection and is
	// unique per transaction.
	TransactionID string
	// PeerIP is the remote IP observed at the edge (not provider-attested).
	PeerIP string
	// HELO is the advertised client hostname (not authentication).
	HELO string
}

// SlogArgs renders the correlation identity as alternating key/value pairs for
// logger.Info/logger.Warn, e.g.
//
//	attrs, _ := mxagent.TransactionAttrsFromContext(ctx)
//	log.Info("receiver resolve", attrs.SlogArgs()...)
//
// Only non-empty fields are included.
func (a TransactionAttrs) SlogArgs() []any {
	args := make([]any, 0, 8)
	if a.ConnectionID != "" {
		args = append(args, AttrConnectionID, a.ConnectionID)
	}
	if a.TransactionID != "" {
		args = append(args, AttrTransactionID, a.TransactionID)
	}
	if a.PeerIP != "" {
		args = append(args, AttrPeerIP, a.PeerIP)
	}
	if a.HELO != "" {
		args = append(args, AttrHELO, a.HELO)
	}
	return args
}

// LogAttrs is SlogArgs as []slog.Attr, for handlers that build a record
// directly.
func (a TransactionAttrs) LogAttrs() []slog.Attr {
	attrs := make([]slog.Attr, 0, 4)
	if a.ConnectionID != "" {
		attrs = append(attrs, slog.String(AttrConnectionID, a.ConnectionID))
	}
	if a.TransactionID != "" {
		attrs = append(attrs, slog.String(AttrTransactionID, a.TransactionID))
	}
	if a.PeerIP != "" {
		attrs = append(attrs, slog.String(AttrPeerIP, a.PeerIP))
	}
	if a.HELO != "" {
		attrs = append(attrs, slog.String(AttrHELO, a.HELO))
	}
	return attrs
}

type transactionAttrsKey struct{}

// WithTransactionContext returns a child context carrying the correlation
// identity. The receiver does not need to call this; the edge attaches it
// before invoking Delivery.Resolve and Delivery.Ingest.
func WithTransactionContext(ctx context.Context, attrs TransactionAttrs) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, transactionAttrsKey{}, attrs)
}

// TransactionAttrsFromContext extracts the correlation identity the edge
// attached to a Resolve or Ingest context. ok is false when the context did not
// come from the SMTP edge.
func TransactionAttrsFromContext(ctx context.Context) (TransactionAttrs, bool) {
	if ctx == nil {
		return TransactionAttrs{}, false
	}
	attrs, ok := ctx.Value(transactionAttrsKey{}).(TransactionAttrs)
	return attrs, ok
}

// newCorrelationID returns a 128-bit random, hex-encoded opaque id used for
// both connection and transaction correlation. It is not a credential and is
// never accepted as one.
func newCorrelationID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read does not fail on supported platforms; a zero id
		// would conflate unrelated events, so fall back to a distinct sentinel
		// rather than silently returning all zeros.
		return "unavailable"
	}
	return hex.EncodeToString(b[:])
}
