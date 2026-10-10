package app_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/mxwire"
)

// TestBlockedInboundIsLogged pins that rejected inbound mail — which leaves no
// message row, event or relay delivery — still emits a terminal "inbound
// blocked" INFO line on the container stream, and that a retry is logged too
// (the sender retrying must not look silent).
func TestBlockedInboundIsLogged(t *testing.T) {
	svc, u, _, box := mxService(t)
	ctx := context.Background()
	if err := svc.Store.SetInboxAllowedSenders(ctx, u.AccountID, box.ID, []string{"friend@outside.test"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SetInboxSenderRestricted(ctx, u.AccountID, box.ID, true); err != nil {
		t.Fatal(err)
	}
	sink := newLogSink()
	svc.Log = slog.New(sink)

	// mxInput hardcodes EnvelopeFrom "sender@outside.test", and the allow-list
	// matches the MIME From, so an unlisted MIME From is what triggers the block.
	raw := "From: Stranger <stranger@outside.test>\r\nTo: " + box.Address +
		"\r\nSubject: blocked\r\nMessage-ID: <blk@test>\r\nDate: Mon, 07 Sep 2026 10:00:00 +0000\r\n\r\nbody"
	res, err := svc.IngestMX(ctx, mxInput(t, svc, box.Address, raw, mxwire.AuthResults{}))
	if err != nil {
		t.Fatal(err)
	}
	if r := mxResult(t, res); !strings.HasPrefix(r.MessageID, "blk_") {
		t.Fatalf("message id = %q, want a blocked record", r.MessageID)
	}

	// A second, distinct delivery (different Message-ID/digest) is a retry of
	// the same block and must also be logged.
	raw2 := "From: Stranger <stranger@outside.test>\r\nTo: " + box.Address +
		"\r\nSubject: blocked\r\nMessage-ID: <blk2@test>\r\nDate: Mon, 07 Sep 2026 10:00:00 +0000\r\n\r\nbody two"
	if _, err := svc.IngestMX(ctx, mxInput(t, svc, box.Address, raw2, mxwire.AuthResults{})); err != nil {
		t.Fatal(err)
	}

	blocked := 0
	for _, m := range sink.messages() {
		if m == "inbound blocked" {
			blocked++
		}
	}
	if blocked != 2 {
		t.Fatalf("logged %d blocked lines, want 2 (every retry)", blocked)
	}
	if got := sink.values("reason"); len(got) != 2 || got[0] != "sender not allowed" {
		t.Fatalf("reason log attrs = %q, want [sender not allowed sender not allowed]", got)
	}
}

// TestControlMailIsLogged pins that consumed approval control mail — also
// invisible in the inbox — emits a terminal "inbound control mail" INFO line
// whose outcome is the recorded control outcome.
func TestControlMailIsLogged(t *testing.T) {
	svc, _, _, box := mxService(t)
	sink := newLogSink()
	svc.Log = slog.New(sink)

	raw := mxControlRaw("stranger@outside.test", box.Address, "[GH-APPROVE:abcdefghijklmnop]", "approve")
	if _, err := svc.IngestMX(context.Background(), mxControlInput(t, svc, box.Address, "stranger@outside.test", raw, mxwire.AuthResults{})); err != nil {
		t.Fatal(err)
	}

	logged := false
	for _, m := range sink.messages() {
		if m == "inbound control mail" {
			logged = true
		}
	}
	if !logged {
		t.Fatalf("no inbound control mail line; messages = %q", sink.messages())
	}
	if got := sink.values("outcome"); len(got) != 1 || got[0] != "invalid" {
		t.Fatalf("outcome log attrs = %q, want [invalid]", got)
	}
}
