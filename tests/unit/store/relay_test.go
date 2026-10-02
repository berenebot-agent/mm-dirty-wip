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

// TestOpenClawConnectorKind proves the OpenClaw connector shares the relay
// persistence while remaining a distinct client type: it is created with
// kind=openclaw, listed only by the OpenClaw list, and both kinds are
// addressed by the shared relay lookup.
func TestOpenClawConnectorKind(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)

	hermes, err := s.CreateHermesConnection(ctx, store.EnrollRecord{AccountID: u.AccountID, InboxID: b[0].ID, Name: "h", Kind: store.KindHermes}, "gw-kind-hermes", "hs", "hd")
	if err != nil {
		t.Fatal(err)
	}
	if hermes.Kind != "hermes" {
		t.Fatalf("hermes kind %q", hermes.Kind)
	}
	openclaw, err := s.CreateHermesConnection(ctx, store.EnrollRecord{AccountID: u.AccountID, InboxID: b[0].ID, Name: "o", Kind: store.KindOpenClaw}, "gw-kind-openclaw", "os", "od")
	if err != nil {
		t.Fatal(err)
	}
	if openclaw.Kind != "openclaw" {
		t.Fatalf("openclaw kind %q", openclaw.Kind)
	}

	// The gateway lookup is kind-agnostic: both connectors authenticate.
	got, err := s.GetHermesConnectionByGateway(ctx, "gw-kind-openclaw")
	if err != nil || got.ID != openclaw.ID || got.Kind != "openclaw" {
		t.Fatalf("openclaw gateway lookup %v %#v", err, got)
	}

	all, err := s.ListHermesConnections(ctx, u.AccountID)
	if err != nil || len(all) != 2 {
		t.Fatalf("all relay list %v %#v", err, all)
	}
	oc, err := s.ListRelayConnections(ctx, u.AccountID, store.KindOpenClaw)
	if err != nil || len(oc) != 1 || oc[0].ID != openclaw.ID {
		t.Fatalf("openclaw list %v %#v", err, oc)
	}
	hm, err := s.ListRelayConnections(ctx, u.AccountID, store.KindHermes)
	if err != nil || len(hm) != 1 || hm[0].ID != hermes.ID {
		t.Fatalf("hermes list %v %#v", err, hm)
	}

	// Kind-aware mutation and deletion reach the OpenClaw row through the
	// shared relay functions.
	if err := s.UpdateHermesConnectionName(ctx, u.AccountID, openclaw.ID, "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetHermesOutboundRole(ctx, u.AccountID, openclaw.ID, "assistant"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetHermesConnectionByGateway(ctx, "gw-kind-openclaw")
	if got.Name != "renamed" || got.OutboundRole != "assistant" {
		t.Fatalf("openclaw after update %#v", got)
	}

	// Cross-account access stays denied for the OpenClaw kind too.
	ub, _ := s.CreateAccountAndAdmin(ctx, "B", "b@example.com", "correct horse battery staple", 1<<20)
	if err := s.DeleteHermesConnection(ctx, ub.AccountID, openclaw.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-account openclaw delete err=%v, want not found", err)
	}
	if err := s.DeleteHermesConnection(ctx, u.AccountID, openclaw.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetHermesConnectionByGateway(ctx, "gw-kind-openclaw"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted openclaw still resolves: %v", err)
	}
}

// TestRelayEnrollTokenCarriesKind proves a one-time setup code remembers the
// connector kind, so claiming it creates an OpenClaw connector, and that an
// invalid kind is refused at mint time.
func TestRelayEnrollTokenCarriesKind(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)

	tok, err := s.CreateRelayEnrollToken(ctx, u.AccountID, b[0].ID, "oc", store.KindOpenClaw, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := s.EnrollHermesConnection(ctx, tok, "gw-code-oc", "s", "d")
	if err != nil {
		t.Fatal(err)
	}
	if conn.Kind != "openclaw" {
		t.Fatalf("enrolled kind %q, want openclaw", conn.Kind)
	}

	if _, err := s.CreateRelayEnrollToken(ctx, u.AccountID, b[0].ID, "bad", store.RelayKind("telegram"), time.Hour); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("invalid kind err=%v, want forbidden", err)
	}

	if err := s.DeleteHermesConnection(ctx, u.AccountID, conn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListRelayConnections(ctx, u.AccountID, store.KindOpenClaw); err != nil {
		t.Fatal(err)
	}
}
