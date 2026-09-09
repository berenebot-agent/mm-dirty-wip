package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
)

// testDeliveryLogCap mirrors internal/store's unexported testDeliveryLogCap.
const testDeliveryLogCap = 5000

func TestDeliveryLogRecordListAndPrune(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	cred, err := s.SaveOutboundCredential(ctx, u.AccountID, "", "Brevo", "brevo", "enc")
	if err != nil {
		t.Fatal(err)
	}
	rec := store.OutboundRecord{Inbox: box, Provider: "brevo", RFCMessageID: "<out@test>", From: model.Address{Address: box.Address}, To: []string{"x@y.test"}, Subject: "s", Text: "t", RawPath: "messages/o.eml", SizeBytes: 100}
	m, _, err := s.CommitOutbound(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	// Record a failed then a sent attempt.
	if _, err = s.MarkFailed(ctx, u.AccountID, m.ID, "provider down", time.Now().UTC().Add(time.Minute), 6, cred.ID, "brevo"); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.MarkSent(ctx, u.AccountID, m.ID, "<provider-id>", cred.ID, "brevo"); err != nil {
		t.Fatal(err)
	}
	// List scoped to the credential, newest first.
	attempts, err := s.ListDeliveryAttempts(ctx, u.AccountID, cred.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts len %d", len(attempts))
	}
	if attempts[0].Status != "sent" || attempts[0].MessageID != m.ID || attempts[0].ProviderMessageID != "<provider-id>" || attempts[0].Attempt != 2 {
		t.Fatalf("newest %+v", attempts[0])
	}
	if attempts[0].FromAddress != box.Address || len(attempts[0].To) != 1 || attempts[0].To[0] != "x@y.test" {
		t.Fatalf("newest addresses %+v", attempts[0])
	}
	if attempts[1].Status != "failed" || attempts[1].ErrorText != "provider down" || attempts[1].Attempt != 1 {
		t.Fatalf("oldest %+v", attempts[1])
	}
	// Keyset pagination: before the newest id returns only the older row.
	older, err := s.ListDeliveryAttempts(ctx, u.AccountID, cred.ID, 10, attempts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(older) != 1 || older[0].ID != attempts[1].ID {
		t.Fatalf("older %+v", older)
	}
	// Unknown credential for this account -> not found.
	if _, err = s.ListDeliveryAttempts(ctx, u.AccountID, "out_missing", 10, 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign cred err=%v", err)
	}
}

func TestDeliveryLogPruneKeepsNewestAndRecent(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	cred, err := s.SaveOutboundCredential(ctx, u.AccountID, "", "Brevo", "brevo", "enc")
	if err != nil {
		t.Fatal(err)
	}
	rec := store.OutboundRecord{Inbox: box, Provider: "brevo", RFCMessageID: "<out@test>", From: model.Address{Address: box.Address}, To: []string{"x@y.test"}, Subject: "s", Text: "t", RawPath: "messages/o.eml", SizeBytes: 100}
	m, _, err := s.CommitOutbound(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	// Insert more than the cap so the oldest rows are pruned.
	for i := 0; i < testDeliveryLogCap+10; i++ {
		if _, _, err = s.MarkSent(ctx, u.AccountID, m.ID, "<id>", cred.ID, "brevo"); err != nil {
			t.Fatal(err)
		}
	}
	if n := deliveryLogCount(t, s, u.AccountID); n != testDeliveryLogCap {
		t.Fatalf("after prune count %d, want %d", n, testDeliveryLogCap)
	}
	// A further insert still keeps the count at the cap (the oldest row is
	// pruned even though it is recent, because the count bound applies).
	if _, _, err = s.MarkSent(ctx, u.AccountID, m.ID, "<id>", cred.ID, "brevo"); err != nil {
		t.Fatal(err)
	}
	if n := deliveryLogCount(t, s, u.AccountID); n != testDeliveryLogCap {
		t.Fatalf("after recent insert count %d, want %d", n, testDeliveryLogCap)
	}
}

func deliveryLogCount(t *testing.T, s *store.Store, accountID string) int {
	t.Helper()
	db := rawDB(t, s.Path())
	defer db.Close()
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM outbound_delivery_log WHERE account_id=?`, accountID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDeliveryLogMessageDeleteNullsLink(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	cred, err := s.SaveOutboundCredential(ctx, u.AccountID, "", "Brevo", "brevo", "enc")
	if err != nil {
		t.Fatal(err)
	}
	rec := store.OutboundRecord{Inbox: box, Provider: "brevo", RFCMessageID: "<out@test>", From: model.Address{Address: box.Address}, To: []string{"x@y.test"}, Subject: "s", Text: "t", RawPath: "messages/o.eml", SizeBytes: 100}
	m, _, err := s.CommitOutbound(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.MarkSent(ctx, u.AccountID, m.ID, "<id>", cred.ID, "brevo"); err != nil {
		t.Fatal(err)
	}
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{box.ID: "owner"}}
	if _, _, _, err = s.DeleteMessage(ctx, p, m.ID); err != nil {
		t.Fatal(err)
	}
	attempts, err := s.ListDeliveryAttempts(ctx, u.AccountID, cred.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].MessageID != "" {
		t.Fatalf("after message delete %+v", attempts)
	}
	if attempts[0].FromAddress != "" || len(attempts[0].To) != 0 {
		t.Fatalf("addresses should be cleared after message delete %+v", attempts[0])
	}
}
