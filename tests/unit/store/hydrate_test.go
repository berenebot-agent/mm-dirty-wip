package store

import (
	"context"
	"reflect"
	"testing"

	"gatehouse-mail/internal/model"
)

// TestCommitReturnsCommittedMessage locks the invariant that the message
// returned by a write path is identical to a fresh read, so callers never need
// a fallible read after commit.
func TestCommitReturnsCommittedMessage(t *testing.T) {
	ctx := context.Background()
	s, _, _, b := testStore(t)

	in, _, _, err := s.CommitInbound(ctx, inbound(b[0], "d-hydrate", "<in@test>", "", nil, "subject", "body"))
	if err != nil {
		t.Fatal(err)
	}
	fetchedIn, err := s.GetMessageByID(ctx, in.AccountID, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, fetchedIn) {
		t.Fatalf("inbound mismatch:\n%#v\n%#v", in, fetchedIn)
	}

	out, _, err := s.CommitOutbound(ctx, OutboundRecord{Inbox: b[0], Provider: "smtp", RFCMessageID: "<out@test>",
		From: model.Address{Address: b[0].Address}, To: []string{"friend@example.net"}, Subject: "out", Text: "body",
		RawPath: "messages/out.eml", SizeBytes: 50, IdemKey: "hydrate-key"})
	if err != nil {
		t.Fatal(err)
	}
	fetchedOut, err := s.GetMessageByID(ctx, out.AccountID, out.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, fetchedOut) {
		t.Fatalf("outbound mismatch:\n%#v\n%#v", out, fetchedOut)
	}

	sent, _, err := s.MarkSent(ctx, out.AccountID, out.ID, "<provider@id>", "", "smtp")
	if err != nil {
		t.Fatal(err)
	}
	fetchedSent, err := s.GetMessageByID(ctx, sent.AccountID, sent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sent, fetchedSent) {
		t.Fatalf("sent mismatch:\n%#v\n%#v", sent, fetchedSent)
	}
}
