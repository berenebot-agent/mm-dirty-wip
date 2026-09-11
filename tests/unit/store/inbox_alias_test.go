package store_test

import (
	"context"
	"errors"
	"testing"

	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
)

func TestInboxAliasResolvePrecedenceAndCrossDomain(t *testing.T) {
	ctx := context.Background()
	s, u, d, b := testStore(t)
	target := b[0]

	// A second domain owned by the same account, to prove an alias may deliver
	// across domains.
	d2, err := s.CreateDomain(ctx, u.AccountID, "other.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetInboxAliases(ctx, u.AccountID, target.ID, []store.AliasInput{
		{DomainID: d2.ID, LocalPart: "sales"},
		{DomainID: d.ID, LocalPart: "info"},
	}); err != nil {
		t.Fatal(err)
	}

	// Cross-domain alias resolves to the target inbox.
	got, route, err := s.ResolveRecipient(ctx, "sales@other.com")
	if err != nil || route != store.RouteAlias {
		t.Fatalf("cross-domain alias box=%+v route=%v err=%v", got, route, err)
	}
	if got.ID != target.ID || got.Address != target.Address {
		t.Fatalf("alias resolved to %s (%s), want %s", got.ID, got.Address, target.ID)
	}

	// Same-domain alias resolves to the target too.
	if got, route, err = s.ResolveRecipient(ctx, "info@example.com"); err != nil || route != store.RouteAlias || got.ID != target.ID {
		t.Fatalf("same-domain alias box=%s route=%v err=%v", got.ID, route, err)
	}

	// An exact inbox address wins over any alias and over catch-all.
	if err = s.SetDomainCatchAll(ctx, u.AccountID, d.ID, b[1].ID); err != nil {
		t.Fatal(err)
	}
	if got, route, err = s.ResolveRecipient(ctx, target.Address); err != nil || route != store.RouteInbox || got.ID != target.ID {
		t.Fatalf("exact precedence box=%s route=%v err=%v", got.ID, route, err)
	}

	// An alias wins over the catch-all it lives on.
	if got, route, err = s.ResolveRecipient(ctx, "info@example.com"); err != nil || route != store.RouteAlias || got.ID != target.ID {
		t.Fatalf("alias-over-catchall box=%s route=%v err=%v", got.ID, route, err)
	}

	// An unknown local part falls through to the catch-all.
	if got, route, err = s.ResolveRecipient(ctx, "nobody@example.com"); err != nil || route != store.RouteCatchAll || got.ID != b[1].ID {
		t.Fatalf("catch-all box=%s route=%v err=%v", got.ID, route, err)
	}
}

func TestInboxAliasListedOnInbox(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	target := b[0]
	if err := s.SetInboxAliases(ctx, u.AccountID, target.ID, []store.AliasInput{{DomainID: target.DomainID, LocalPart: "sales"}, {DomainID: target.DomainID, LocalPart: "billing"}}); err != nil {
		t.Fatal(err)
	}
	box, err := s.GetInboxInternal(ctx, u.AccountID, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(box.Aliases) != 2 || box.Aliases[0] != "billing@example.com" || box.Aliases[1] != "sales@example.com" {
		t.Fatalf("inbox aliases %#v", box.Aliases)
	}
	boxes, err := s.ListInboxes(ctx, model.Principal{AccountID: u.AccountID, Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, bx := range boxes {
		if bx.ID == target.ID && len(bx.Aliases) != 2 {
			t.Fatalf("list aliases %#v", bx.Aliases)
		}
	}
}

func TestInboxAliasRejectsCollisionsAndForeignScope(t *testing.T) {
	ctx := context.Background()
	s, u, d, b := testStore(t)

	// An alias may not shadow a real mailbox on the same domain.
	if err := s.SetInboxAliases(ctx, u.AccountID, b[0].ID, []store.AliasInput{{DomainID: d.ID, LocalPart: b[1].LocalPart}}); err == nil {
		t.Fatal("alias shadowing a mailbox was accepted")
	}

	// A real inbox may not shadow an existing alias.
	if err := s.SetInboxAliases(ctx, u.AccountID, b[0].ID, []store.AliasInput{{DomainID: d.ID, LocalPart: "sales"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInbox(ctx, u.AccountID, d.ID, "sales", ""); err == nil {
		t.Fatal("inbox shadowing an alias was accepted")
	}

	// Duplicate aliases in one submission are rejected.
	if err := s.SetInboxAliases(ctx, u.AccountID, b[0].ID, []store.AliasInput{
		{DomainID: d.ID, LocalPart: "dup"},
		{DomainID: d.ID, LocalPart: "dup"},
	}); err == nil {
		t.Fatal("duplicate aliases were accepted")
	}

	// A domain from another account is forbidden.
	other, err := s.CreateAccountAndAdmin(ctx, "B", "admin@b.test", "correct horse battery staple", 100<<20)
	if err != nil {
		t.Fatal(err)
	}
	od, err := s.CreateDomain(ctx, other.AccountID, "b.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetInboxAliases(ctx, u.AccountID, b[0].ID, []store.AliasInput{{DomainID: od.ID, LocalPart: "foreign"}}); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("foreign domain err=%v", err)
	}

	// A foreign inbox target is not found in the caller's account.
	if err := s.SetInboxAliases(ctx, other.AccountID, b[0].ID, nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign inbox err=%v", err)
	}
}

func TestInboxAliasDisabledTargetAndCascade(t *testing.T) {
	ctx := context.Background()
	s, u, d, b := testStore(t)
	target := b[0]
	if err := s.SetInboxAliases(ctx, u.AccountID, target.ID, []store.AliasInput{{DomainID: d.ID, LocalPart: "sales"}}); err != nil {
		t.Fatal(err)
	}

	// Disabling the target inbox makes the alias unresolved.
	disabled := false
	if err := s.UpdateInbox(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, target.ID, "", &disabled); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveRecipient(ctx, "sales@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("disabled target err=%v", err)
	}
	enabled := true
	if err := s.UpdateInbox(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, target.ID, "", &enabled); err != nil {
		t.Fatal(err)
	}

	// Purging the target inbox cascades to its aliases.
	if _, err := s.PurgeInbox(ctx, u.AccountID, target.ID); err != nil {
		t.Fatal(err)
	}
	aliases, err := s.ListInboxAliases(ctx, u.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if len(aliases) != 0 {
		t.Fatalf("aliases survived purge: %#v", aliases)
	}
	if _, _, err := s.ResolveRecipient(ctx, "sales@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("alias after purge err=%v", err)
	}
}

func TestInboxAliasValidation(t *testing.T) {
	ctx := context.Background()
	s, u, d, b := testStore(t)
	if err := s.SetInboxAliases(ctx, u.AccountID, b[0].ID, []store.AliasInput{{DomainID: d.ID, LocalPart: "bad local"}}); err == nil {
		t.Fatal("invalid local part accepted")
	}
	if err := s.SetInboxAliases(ctx, u.AccountID, b[0].ID, []store.AliasInput{{DomainID: d.ID, LocalPart: ""}}); err == nil {
		t.Fatal("empty local part accepted")
	}
}
