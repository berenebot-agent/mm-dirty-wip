package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

func TestDeliveryLogRecordListAndPrune(t *testing.T) {
	ctx := context.Background()
	s, u, d, b := testStore(t)
	box := b[0]
	rec := store.OutboundRecord{Inbox: box, Provider: "brevo", RFCMessageID: "<out@test>", From: model.Address{Address: box.Address}, To: []string{"x@y.test"}, Subject: "s", Text: "t", RawPath: "messages/o.eml", SizeBytes: 100}
	m, _, err := s.CommitOutbound(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	// Record a failed then a sent attempt.
	if _, _, err = s.MarkFailed(ctx, u.AccountID, m.ID, "provider down", time.Now().UTC().Add(time.Minute), 6, "brevo"); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.MarkSent(ctx, u.AccountID, m.ID, "<provider-id>", "brevo"); err != nil {
		t.Fatal(err)
	}
	// List scoped to the domain, newest first; the domain is derived from the
	// message's inbox, so it matches the test's domain.
	attempts, err := s.ListDomainDeliveryAttempts(ctx, u.AccountID, d.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts len %d", len(attempts))
	}
	if attempts[0].Status != "sent" || attempts[0].MessageID != m.ID || attempts[0].ProviderMessageID != "<provider-id>" || attempts[0].Attempt != 2 || attempts[0].DomainID != d.ID {
		t.Fatalf("newest %+v", attempts[0])
	}
	if attempts[0].FromAddress != box.Address || len(attempts[0].To) != 1 || attempts[0].To[0] != "x@y.test" {
		t.Fatalf("newest addresses %+v", attempts[0])
	}
	if attempts[1].Status != "failed" || attempts[1].ErrorText != "provider down" || attempts[1].Attempt != 1 {
		t.Fatalf("oldest %+v", attempts[1])
	}
	// Keyset pagination: before the newest id returns only the older row.
	older, err := s.ListDomainDeliveryAttempts(ctx, u.AccountID, d.ID, 10, attempts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(older) != 1 || older[0].ID != attempts[1].ID {
		t.Fatalf("older %+v", older)
	}
	// Unknown domain for this account -> not found.
	if _, err = s.ListDomainDeliveryAttempts(ctx, u.AccountID, "dom_missing", 10, 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign domain err=%v", err)
	}
	// LastSentByDomain reflects the successful attempt.
	lastSent, err := s.LastSentByDomain(ctx, u.AccountID)
	if err != nil || lastSent[d.ID].IsZero() {
		t.Fatalf("last sent by domain %v err=%v", lastSent, err)
	}
}

func TestDeliveryLogPruneKeepsNewestAndRecent(t *testing.T) {
	// Lower the cap so the prune path is exercised in O(cap) inserts rather than
	// O(5000); every insert triggers a full-scan prune, so the default cap makes
	// this test quadratic and needlessly slow.
	const cap = 50
	store.SetDeliveryLogCapForTest(cap)
	defer store.SetDeliveryLogCapForTest(0)

	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	rec := store.OutboundRecord{Inbox: box, Provider: "brevo", RFCMessageID: "<out@test>", From: model.Address{Address: box.Address}, To: []string{"x@y.test"}, Subject: "s", Text: "t", RawPath: "messages/o.eml", SizeBytes: 100}
	m, _, err := s.CommitOutbound(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	// Insert more than the cap so the oldest rows are pruned.
	for i := 0; i < cap+10; i++ {
		if _, _, err = s.MarkSent(ctx, u.AccountID, m.ID, "<id>", "brevo"); err != nil {
			t.Fatal(err)
		}
	}
	if n := deliveryLogCount(t, s, u.AccountID); n != cap {
		t.Fatalf("after prune count %d, want %d", n, cap)
	}
	// A further insert still keeps the count at the cap (the oldest row is
	// pruned even though it is recent, because the count bound applies).
	if _, _, err = s.MarkSent(ctx, u.AccountID, m.ID, "<id>", "brevo"); err != nil {
		t.Fatal(err)
	}
	if n := deliveryLogCount(t, s, u.AccountID); n != cap {
		t.Fatalf("after recent insert count %d, want %d", n, cap)
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

// TestDeliveryStartedRecordsInFlightAndInterruptsStale covers the outcome-safety
// fix: a delivery attempt is visible while it is in flight, and a re-claim after
// an interrupted attempt marks the previous in-flight row "interrupted" rather
// than leaving a dangling "sending" row forever.
func TestDeliveryStartedRecordsInFlightAndInterruptsStale(t *testing.T) {
	ctx := context.Background()
	s, u, d, b := testStore(t)
	box := b[0]
	rec := store.OutboundRecord{Inbox: box, Provider: "mx", RFCMessageID: "<out@test>", From: model.Address{Address: box.Address}, To: []string{"x@y.test"}, Subject: "big", Text: "t", RawPath: "messages/o.eml", SizeBytes: 100}
	m, _, err := s.CommitOutbound(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	// First attempt starts but is never resolved (interrupted send).
	if err = s.RecordDeliveryStarted(ctx, u.AccountID, m.ID, "mx"); err != nil {
		t.Fatal(err)
	}
	attempts, err := s.ListDomainDeliveryAttempts(ctx, u.AccountID, d.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].Status != "sending" || attempts[0].MessageID != m.ID {
		t.Fatalf("in-flight attempt %+v", attempts)
	}
	// A second attempt re-claims the message: the stale sending row must become
	// interrupted, and the new attempt is visible as sending.
	if err = s.RecordDeliveryStarted(ctx, u.AccountID, m.ID, "mx"); err != nil {
		t.Fatal(err)
	}
	attempts, err = s.ListDomainDeliveryAttempts(ctx, u.AccountID, d.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts len %d", len(attempts))
	}
	if attempts[0].Status != "sending" || attempts[1].Status != "interrupted" {
		t.Fatalf("newest %q oldest %q", attempts[0].Status, attempts[1].Status)
	}
	// A resolved outcome still records normally after the in-flight row.
	if _, _, err = s.MarkSent(ctx, u.AccountID, m.ID, "<id>", "mx"); err != nil {
		t.Fatal(err)
	}
	attempts, err = s.ListDomainDeliveryAttempts(ctx, u.AccountID, d.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Newest first: the terminal sent row, the second (still open) sending row,
	// then the interrupted first attempt.
	if attempts[0].Status != "sent" || attempts[1].Status != "sending" || attempts[2].Status != "interrupted" {
		t.Fatalf("after sent %+v", attempts)
	}
}

// TestOutboxMarksSendingInFlight proves the outbox view distinguishes a message
// whose delivery is in flight from one merely queued.
func TestOutboxMarksSendingInFlight(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	rec := store.OutboundRecord{Inbox: box, Provider: "mx", RFCMessageID: "<out@test>", From: model.Address{Address: box.Address}, To: []string{"x@y.test"}, Subject: "s", Text: "t", RawPath: "messages/o.eml", SizeBytes: 100}
	m, _, err := s.CommitOutbound(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{box.ID: "owner"}}
	list, err := s.ListOutbox(ctx, p, box.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Sending {
		t.Fatalf("queued outbox %+v", list)
	}
	if err = s.RecordDeliveryStarted(ctx, u.AccountID, m.ID, "mx"); err != nil {
		t.Fatal(err)
	}
	list, err = s.ListOutbox(ctx, p, box.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || !list[0].Sending {
		t.Fatalf("in-flight outbox %+v", list)
	}
}

func TestDeliveryLogMessageDeleteNullsLink(t *testing.T) {
	ctx := context.Background()
	s, u, d, b := testStore(t)
	box := b[0]
	rec := store.OutboundRecord{Inbox: box, Provider: "brevo", RFCMessageID: "<out@test>", From: model.Address{Address: box.Address}, To: []string{"x@y.test"}, Subject: "s", Text: "t", RawPath: "messages/o.eml", SizeBytes: 100}
	m, _, err := s.CommitOutbound(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.MarkSent(ctx, u.AccountID, m.ID, "<id>", "brevo"); err != nil {
		t.Fatal(err)
	}
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{box.ID: "owner"}}
	if _, _, _, err = s.DeleteMessage(ctx, p, m.ID); err != nil {
		t.Fatal(err)
	}
	attempts, err := s.ListDomainDeliveryAttempts(ctx, u.AccountID, d.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].MessageID != "" {
		t.Fatalf("after message delete %+v", attempts)
	}
	if attempts[0].FromAddress != "owner@example.com" || len(attempts[0].To) != 1 || attempts[0].To[0] != "x@y.test" || attempts[0].Subject != "s" {
		t.Fatalf("message snapshot should survive deletion %+v", attempts[0])
	}
}
