package mxagent_test

import (
	"context"
	"testing"

	"github.com/dellarb/mailmoose/internal/mxagent"
)

// TestTransactionContextRoundTrip verifies the public helper contract the
// receiver consumes: the edge attaches a TransactionAttrs to a context and the
// receiver reads it back unchanged.
func TestTransactionContextRoundTrip(t *testing.T) {
	want := mxagent.TransactionAttrs{
		ConnectionID:  "conn-1",
		TransactionID: "txn-1",
		PeerIP:        "203.0.113.7",
		HELO:          "sender.test",
	}
	ctx := mxagent.WithTransactionContext(context.Background(), want)
	got, ok := mxagent.TransactionAttrsFromContext(ctx)
	if !ok {
		t.Fatal("expected attrs on context")
	}
	if got != want {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, want)
	}
}

// TestTransactionContextAbsent verifies a plain context reports no attrs, so a
// receiver can distinguish an edge-originated call from any other.
func TestTransactionContextAbsent(t *testing.T) {
	if _, ok := mxagent.TransactionAttrsFromContext(context.Background()); ok {
		t.Fatal("background context must not carry attrs")
	}
	// A nil context is tolerated and reports absent rather than panicking.
	if _, ok := mxagent.TransactionAttrsFromContext(nil); ok {
		t.Fatal("nil context must not carry attrs")
	}
}

// TestTransactionAttrsSlogArgs verifies the key/value rendering the receiver
// uses to correlate its own log lines, including omission of empty fields.
func TestTransactionAttrsSlogArgs(t *testing.T) {
	attrs := mxagent.TransactionAttrs{ConnectionID: "c", TransactionID: "t"}
	args := attrs.SlogArgs()
	if len(args) != 4 {
		t.Fatalf("SlogArgs len=%d want 4 (empty peer/helo omitted): %v", len(args), args)
	}
	if args[0] != mxagent.AttrConnectionID || args[1] != "c" {
		t.Fatalf("connection id arg: %v", args[:2])
	}
	if args[2] != mxagent.AttrTransactionID || args[3] != "t" {
		t.Fatalf("transaction id arg: %v", args[2:])
	}

	full := mxagent.TransactionAttrs{ConnectionID: "c", TransactionID: "t", PeerIP: "1.2.3.4", HELO: "h"}
	if got := len(full.SlogArgs()); got != 8 {
		t.Fatalf("full SlogArgs len=%d want 8", got)
	}
	if got := len(mxagent.TransactionAttrs{}.SlogArgs()); got != 0 {
		t.Fatalf("empty SlogArgs len=%d want 0", got)
	}

	logAttrs := full.LogAttrs()
	if len(logAttrs) != 4 {
		t.Fatalf("LogAttrs len=%d want 4", len(logAttrs))
	}
	if logAttrs[0].Key != mxagent.AttrConnectionID || logAttrs[0].Value.String() != "c" {
		t.Fatalf("LogAttrs[0]=%v", logAttrs[0])
	}
}
