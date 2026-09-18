package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/store"
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
	victim, err := s.CreateHermesConnection(ctx, store.EnrollRecord{AccountID: ub.AccountID, InboxID: bb.ID, Name: "v"}, "gw-victim", "victim-secret", "victim-delivery")
	if err != nil {
		t.Fatal(err)
	}

	tok, err := s.CreateHermesEnrollToken(ctx, u.AccountID, b[0].ID, "attacker", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnrollHermesConnection(ctx, tok, "gw-victim", "stolen", "stolen"); !errors.Is(err, store.ErrForbidden) {
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

// TestHermesOutboundRoleSetAndValidate proves the role defaults to owner,
// accepts assistant, and rejects anything else.
func TestHermesOutboundRoleSetAndValidate(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	conn, err := s.CreateHermesConnection(ctx, store.EnrollRecord{AccountID: u.AccountID, InboxID: b[0].ID, Name: "gw"}, "gw-role", "sec", "del")
	if err != nil {
		t.Fatal(err)
	}
	if conn.OutboundRole != "owner" {
		t.Fatalf("default role %q, want owner", conn.OutboundRole)
	}
	if err := s.SetHermesOutboundRole(ctx, u.AccountID, conn.ID, "ASSISTANT"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetHermesConnectionByGateway(ctx, "gw-role")
	if err != nil {
		t.Fatal(err)
	}
	if got.OutboundRole != "assistant" {
		t.Fatalf("role after set %q", got.OutboundRole)
	}
	if err := s.SetHermesOutboundRole(ctx, u.AccountID, conn.ID, "admin"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("invalid role err=%v, want forbidden", err)
	}
}
