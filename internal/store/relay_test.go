package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestHermesEnrollRejectsCrossAccountGateway(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)

	ub, err := s.CreateAccountAndAdmin(ctx, "B", "b@example.com", "correct horse battery staple", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	db, err := s.CreateDomain(ctx, ub.AccountID, "b.example")
	if err != nil {
		t.Fatal(err)
	}
	bb, err := s.CreateInbox(ctx, ub.AccountID, db.ID, "victim", "Victim")
	if err != nil {
		t.Fatal(err)
	}
	victim, err := s.CreateHermesConnection(ctx, EnrollRecord{AccountID: ub.AccountID, InboxID: bb.ID, Name: "v"}, "gw-victim", "victim-secret", "victim-delivery")
	if err != nil {
		t.Fatal(err)
	}

	tok, err := s.CreateHermesEnrollToken(ctx, u.AccountID, b[0].ID, "attacker", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnrollHermesConnection(ctx, tok, "gw-victim", "stolen", "stolen"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("cross-account enroll err=%v, want forbidden", err)
	}

	got, err := s.GetHermesConnectionByGateway(ctx, "gw-victim")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != victim.ID || got.AccountID != ub.AccountID || got.SecretEncrypted != "victim-secret" {
		t.Fatalf("victim connection was modified: %#v", got)
	}

	// The rejected attempt must not have burned the attacker's token.
	conn, err := s.EnrollHermesConnection(ctx, tok, "gw-attacker", "s", "d")
	if err != nil {
		t.Fatalf("token was burned by rejected enrollment: %v", err)
	}
	if conn.AccountID != u.AccountID || conn.InboxID != b[0].ID {
		t.Fatalf("attacker connection %#v", conn)
	}
}

func TestHermesEnrollSameAccountRotates(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	tok1, err := s.CreateHermesEnrollToken(ctx, u.AccountID, b[0].ID, "first", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.EnrollHermesConnection(ctx, tok1, "gw-same", "secret-1", "delivery-1")
	if err != nil {
		t.Fatal(err)
	}
	tok2, err := s.CreateHermesEnrollToken(ctx, u.AccountID, b[0].ID, "second", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.EnrollHermesConnection(ctx, tok2, "gw-same", "secret-2", "delivery-2")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("re-enroll should rotate in place, got %s then %s", first.ID, second.ID)
	}
	if second.SecretEncrypted != "secret-2" || second.DeliveryKeyEncrypted != "delivery-2" {
		t.Fatalf("secrets not rotated: %#v", second)
	}
}
