package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dellarb/mammothmail/internal/store"
)

func TestPendingRelaySetupLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, _, boxes := testStore(t)
	create := func(name string, ttl time.Duration) string {
		t.Helper()
		code, err := s.CreateRelayEnrollToken(ctx, u.AccountID, boxes[0].ID, name, store.KindOpenClaw, ttl)
		if err != nil {
			t.Fatal(err)
		}
		return code
	}
	code := create("pending", time.Hour)
	create("expired", -time.Hour)
	used := create("redeemed", time.Hour)
	before, err := s.ListPendingRelaySetups(ctx, u.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	var usedID string
	for _, setup := range before {
		if setup.Name == "redeemed" {
			usedID = setup.ID
		}
	}
	if usedID == "" {
		t.Fatal("unredeemed setup missing")
	}
	conn, err := s.EnrollHermesConnection(ctx, used, "gw-redeemed", "secret", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CancelRelaySetup(ctx, u.AccountID, usedID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("redeemed setup cancellation: %v", err)
	}
	list, err := s.ListPendingRelaySetups(ctx, u.AccountID)
	if err != nil || len(list) != 1 || list[0].Name != "pending" || list[0].InboxID != boxes[0].ID || !list[0].ExpiresAt.After(time.Now()) {
		t.Fatalf("pending list: %#v, %v", list, err)
	}
	id := list[0].ID
	if other, err := s.ListPendingRelaySetups(ctx, "other-account"); err != nil || len(other) != 0 {
		t.Fatalf("cross-account list: %#v, %v", other, err)
	}
	if err := s.CancelRelaySetup(ctx, "other-account", id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-account cancellation: %v", err)
	}
	if err := s.CancelRelaySetup(ctx, u.AccountID, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnrollHermesConnection(ctx, code, "gw-cancelled", "secret", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cancelled code redeemed: %v", err)
	}
	if err := s.CancelRelaySetup(ctx, u.AccountID, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("repeat cancellation: %v", err)
	}
	if list, err := s.ListPendingRelaySetups(ctx, u.AccountID); err != nil || len(list) != 0 {
		t.Fatalf("pending list after cancel: %#v, %v", list, err)
	}
	// Cancelling with a connector id must not remove an enrolled connection.
	if err := s.CancelRelaySetup(ctx, u.AccountID, conn.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cancel enrolled connection: %v", err)
	}
	if _, err := s.GetHermesConnectionByGateway(ctx, conn.GatewayID); err != nil {
		t.Fatal(err)
	}
}
