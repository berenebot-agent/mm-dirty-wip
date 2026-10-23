package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// TestDomainWideSendingGrant covers the per-key "domain-wide sending" grant: a
// key holding it may send as any local part on the catch-all's domain, the grant
// is only live while the inbox is the domain's enabled catch-all, and any other
// key on the same inbox is unaffected.
func TestDomainWideSendingGrant(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, d, boxes := testStore(t)
	catch := boxes[0]

	if err := s.SetDomainCatchAll(ctx, u.AccountID, d.ID, catch.ID); err != nil {
		t.Fatal(err)
	}
	// A key with the grant on the catch-all, and a second key without it.
	withGrant, _, err := s.CreateAPIKey(ctx, u.AccountID, "granted", false, map[string]string{catch.ID: "owner"}, map[string]bool{catch.ID: true})
	if err != nil {
		t.Fatal(err)
	}
	plainKey, _, err := s.CreateAPIKey(ctx, u.AccountID, "plain", false, map[string]string{catch.ID: "owner"})
	if err != nil {
		t.Fatal(err)
	}

	// The granted key may send as any address on the domain.
	from, target, err := s.ResolveSendingTarget(ctx, u.AccountID, catch.ID, withGrant.ID, "anything@example.com")
	if err != nil || from.Address != "anything@example.com" || target.DomainID != d.ID {
		t.Fatalf("granted wildcard -> %+v target=%+v err=%v", from, target, err)
	}
	// Including an address that is already a real inbox on the domain.
	if _, _, err := s.ResolveSendingTarget(ctx, u.AccountID, catch.ID, withGrant.ID, boxes[1].Address); err != nil {
		t.Fatalf("granted wildcard over an existing inbox err=%v", err)
	}
	// The ungranted key is still refused.
	if _, _, err := s.ResolveSendingTarget(ctx, u.AccountID, catch.ID, plainKey.ID, "anything@example.com"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("ungranted wildcard err=%v, want forbidden", err)
	}
	// A session (no key) is refused.
	if _, _, err := s.ResolveSendingTarget(ctx, u.AccountID, catch.ID, "", "anything@example.com"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("session wildcard err=%v, want forbidden", err)
	}
	// A different domain is never covered by the grant, and a lookalike suffix
	// ("@notexample.com") must not match "example.com".
	if _, _, err := s.ResolveSendingTarget(ctx, u.AccountID, catch.ID, withGrant.ID, "anything@elsewhere.test"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("other-domain wildcard err=%v, want forbidden", err)
	}
	if _, _, err := s.ResolveSendingTarget(ctx, u.AccountID, catch.ID, withGrant.ID, "anything@notexample.com"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("lookalike-domain wildcard err=%v, want forbidden", err)
	}

	// Clearing the catch-all makes the grant dormant without deleting it.
	if err := s.SetDomainCatchAll(ctx, u.AccountID, d.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveSendingTarget(ctx, u.AccountID, catch.ID, withGrant.ID, "anything@example.com"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("wildcard after catch-all cleared err=%v, want forbidden", err)
	}
	// Pointing the catch-all back restores it.
	if err := s.SetDomainCatchAll(ctx, u.AccountID, d.ID, catch.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveSendingTarget(ctx, u.AccountID, catch.ID, withGrant.ID, "anything@example.com"); err != nil {
		t.Fatalf("wildcard after catch-all restored err=%v", err)
	}

	// Disabling the catch-all also makes it dormant.
	admin := model.Principal{AccountID: u.AccountID, UserID: u.ID, Admin: true}
	disabled := false
	if err := s.UpdateInbox(ctx, admin, catch.ID, catch.DisplayName, &disabled); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveSendingTarget(ctx, u.AccountID, catch.ID, withGrant.ID, "anything@example.com"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("wildcard while catch-all disabled err=%v, want forbidden", err)
	}
}

// TestDomainWideSendingGrantForcesOwner proves that setting the grant forces the
// binding's role to owner, that clearing it leaves the role alone, and that a
// role-only save preserves an existing grant.
func TestDomainWideSendingGrantForcesOwner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, d, boxes := testStore(t)
	catch := boxes[0]
	if err := s.SetDomainCatchAll(ctx, u.AccountID, d.ID, catch.ID); err != nil {
		t.Fatal(err)
	}
	// A key with only read on the catch-all.
	key, _, err := s.CreateAPIKey(ctx, u.AccountID, "reader", false, map[string]string{catch.ID: "read"})
	if err != nil {
		t.Fatal(err)
	}

	// Setting the grant forces owner.
	if err := s.SetBindingDomainSend(ctx, u.AccountID, key.ID, catch.ID, true); err != nil {
		t.Fatal(err)
	}
	keys, err := s.ListAPIKeys(ctx, u.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	got := findKey(t, keys, key.ID)
	if got.Roles[catch.ID] != "owner" {
		t.Fatalf("role after grant = %q, want owner", got.Roles[catch.ID])
	}
	if !got.DomainSend[catch.ID] {
		t.Fatal("grant not reported live after SetBindingDomainSend")
	}

	// Clearing the grant leaves the role as owner.
	if err := s.SetBindingDomainSend(ctx, u.AccountID, key.ID, catch.ID, false); err != nil {
		t.Fatal(err)
	}
	keys, err = s.ListAPIKeys(ctx, u.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	got = findKey(t, keys, key.ID)
	if got.Roles[catch.ID] != "owner" {
		t.Fatalf("role after clearing grant = %q, want owner (never auto-demoted)", got.Roles[catch.ID])
	}
	if got.DomainSend[catch.ID] {
		t.Fatal("grant still reported after clearing")
	}

	// A grant set through CreateAPIKey survives a role-only UpdateAPIKey (the
	// wholesale binding rewrite must carry it through).
	granted, _, err := s.CreateAPIKey(ctx, u.AccountID, "granted", false, map[string]string{catch.ID: "owner"}, map[string]bool{catch.ID: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateAPIKey(ctx, u.AccountID, granted.ID, granted.Name, false, map[string]string{catch.ID: "owner"}, nil); err != nil {
		t.Fatal(err)
	}
	keys, err = s.ListAPIKeys(ctx, u.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if !findKey(t, keys, granted.ID).DomainSend[catch.ID] {
		t.Fatal("role-only save dropped the domain-send grant")
	}

	// Removing the binding removes the grant with it.
	if err := s.UpdateAPIKey(ctx, u.AccountID, granted.ID, granted.Name, false, map[string]string{}, nil); err != nil {
		t.Fatal(err)
	}
	keys, err = s.ListAPIKeys(ctx, u.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if findKey(t, keys, granted.ID).DomainSend[catch.ID] {
		t.Fatal("grant survived removal of its binding")
	}
}

// TestSetBindingDomainSendRequiresCatchAll proves the grant can only be placed
// on a domain's catch-all, mirroring the UI restriction at the store.
func TestSetBindingDomainSendRequiresCatchAll(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, d, boxes := testStore(t)
	notCatch := boxes[1]
	key, _, err := s.CreateAPIKey(ctx, u.AccountID, "k", false, map[string]string{notCatch.ID: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetBindingDomainSend(ctx, u.AccountID, key.ID, notCatch.ID, true); !errors.Is(err, store.ErrNoCatchAll) {
		t.Fatalf("grant on a non-catch-all inbox err=%v, want ErrNoCatchAll", err)
	}
	// A missing binding is not found.
	if err := s.SetBindingDomainSend(ctx, u.AccountID, key.ID, "in_missing", true); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("grant on missing inbox err=%v, want not found", err)
	}
	_ = d
}

// TestAdminKeyHoldsDomainWideSending proves an account Admin key holds the
// domain-wide sending grant implicitly: it may send as any address on a domain
// from that domain's enabled catch-all, and nowhere else. A session holds no
// key, so it keeps the primary/alias rule.
func TestAdminKeyHoldsDomainWideSending(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, d, boxes := testStore(t)
	catch, notCatch := boxes[0], boxes[1]
	if err := s.SetDomainCatchAll(ctx, u.AccountID, d.ID, catch.ID); err != nil {
		t.Fatal(err)
	}
	adminKey, _, err := s.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}

	// The Admin key may send as any address on the domain, from the catch-all.
	from, target, err := s.ResolveSendingTarget(ctx, u.AccountID, catch.ID, adminKey.ID, "anything@example.com")
	if err != nil || from.Address != "anything@example.com" || target.DomainID != d.ID {
		t.Fatalf("admin wildcard -> %+v target=%+v err=%v", from, target, err)
	}
	// Another inbox on the same domain is not the catch-all, so it is refused.
	if _, _, err := s.ResolveSendingTarget(ctx, u.AccountID, notCatch.ID, adminKey.ID, "anything@example.com"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("admin wildcard from a non-catch-all err=%v, want forbidden", err)
	}
	// A session holds no key and keeps the primary/alias rule.
	if _, _, err := s.ResolveSendingTarget(ctx, u.AccountID, catch.ID, "", "anything@example.com"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("session wildcard err=%v, want forbidden", err)
	}
	// Clearing the catch-all makes the implicit grant dormant.
	if err := s.SetDomainCatchAll(ctx, u.AccountID, d.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveSendingTarget(ctx, u.AccountID, catch.ID, adminKey.ID, "anything@example.com"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("admin wildcard after catch-all cleared err=%v, want forbidden", err)
	}
	// Pointing the catch-all back restores it.
	if err := s.SetDomainCatchAll(ctx, u.AccountID, d.ID, catch.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveSendingTarget(ctx, u.AccountID, catch.ID, adminKey.ID, "anything@example.com"); err != nil {
		t.Fatalf("admin wildcard after catch-all restored err=%v", err)
	}
}

// TestAdminKeyDemotedLosesDomainWideSending proves the implicit grant follows
// the key's admin flag: demoting the key to an ordinary key takes the grant
// away, because the key then holds no binding row.
func TestAdminKeyDemotedLosesDomainWideSending(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, d, boxes := testStore(t)
	catch := boxes[0]
	if err := s.SetDomainCatchAll(ctx, u.AccountID, d.ID, catch.ID); err != nil {
		t.Fatal(err)
	}
	adminKey, _, err := s.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateAPIKey(ctx, u.AccountID, adminKey.ID, "demoted", false, map[string]string{catch.ID: "owner"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveSendingTarget(ctx, u.AccountID, catch.ID, adminKey.ID, "anything@example.com"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("demoted key wildcard err=%v, want forbidden", err)
	}
}

func findKey(t *testing.T, keys []model.APIKey, id string) model.APIKey {
	t.Helper()
	for _, k := range keys {
		if k.ID == id {
			return k
		}
	}
	t.Fatalf("key %s not found", id)
	return model.APIKey{}
}
