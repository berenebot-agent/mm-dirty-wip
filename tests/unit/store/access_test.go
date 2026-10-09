package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// seedAccessAccount creates an account admin plus two inboxes on one domain and
// returns them for the access-model tests.
func seedAccessAccount(t *testing.T) (*store.Store, model.User, model.Inbox, model.Inbox) {
	t.Helper()
	ctx := context.Background()
	s := openStore(t)
	u, err := s.CreateAccountAndAdmin(ctx, "A", "admin@example.com", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDomain(ctx, u.AccountID, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	b1, err := s.CreateInbox(ctx, u.AccountID, d.ID, "one", "One")
	if err != nil {
		t.Fatal(err)
	}
	b2, err := s.CreateInbox(ctx, u.AccountID, d.ID, "two", "Two")
	if err != nil {
		t.Fatal(err)
	}
	return s, u, b1, b2
}

// TestUpdateAPIKeyPerInboxRoleChange verifies the read-modify-write pattern the
// Clients & Access tab uses to change one inbox's role without disturbing the
// key's other bindings, and to remove a single binding.
func TestUpdateAPIKeyPerInboxRoleChange(t *testing.T) {
	ctx := context.Background()
	s, u, b1, b2 := seedAccessAccount(t)

	key, _, err := s.CreateAPIKey(ctx, u.AccountID, "Agent", false, map[string]string{b1.ID: "read", b2.ID: "owner"})
	if err != nil {
		t.Fatal(err)
	}

	// Change b1 to assistant, keeping b2 at owner.
	if err := s.UpdateAPIKey(ctx, u.AccountID, key.ID, key.Name, false, map[string]string{b1.ID: "assistant", b2.ID: "owner"}); err != nil {
		t.Fatal(err)
	}
	keys, err := s.ListAPIKeys(ctx, u.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, k := range keys {
		if k.ID == key.ID {
			got = k.Roles
		}
	}
	if got[b1.ID] != "assistant" || got[b2.ID] != "owner" {
		t.Fatalf("roles after change = %#v", got)
	}

	// Remove b1 only; the key keeps b2.
	if err := s.UpdateAPIKey(ctx, u.AccountID, key.ID, key.Name, false, map[string]string{b2.ID: "owner"}); err != nil {
		t.Fatal(err)
	}
	keys, _ = s.ListAPIKeys(ctx, u.AccountID)
	for _, k := range keys {
		if k.ID == key.ID {
			if _, ok := k.Roles[b1.ID]; ok {
				t.Fatalf("binding for %s should be gone: %#v", b1.ID, k.Roles)
			}
			if k.Roles[b2.ID] != "owner" {
				t.Fatalf("b2 binding lost: %#v", k.Roles)
			}
		}
	}
}

// TestSetUserRolesMerge verifies adding a mailbox user by merging a role into
// their existing map, then removing just that one inbox.
func TestSetUserRolesMerge(t *testing.T) {
	ctx := context.Background()
	s, u, b1, b2 := seedAccessAccount(t)

	inv, _, err := s.CreateInvite(ctx, store.InviteInput{
		AccountID: u.AccountID, Email: "member@example.com", Kind: model.InviteKindOperator,
		InboxIDs: []string{b1.ID}, CreatedBy: u.ID, TTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.RotateInviteToken(ctx, u.AccountID, inv.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	member, err := s.RedeemInvite(ctx, token, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}

	// Grant the second inbox (the tab's "add existing user" merge).
	if err := s.SetUserRoles(ctx, u.AccountID, member.ID, map[string]string{b1.ID: "owner", b2.ID: "owner"}); err != nil {
		t.Fatal(err)
	}
	users, err := s.ListAccountUsers(ctx, u.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	var roles map[string]string
	for _, m := range users {
		if m.ID == member.ID {
			roles = m.Roles
		}
	}
	if roles[b1.ID] != "owner" || roles[b2.ID] != "owner" {
		t.Fatalf("merged roles = %#v", roles)
	}

	// Remove b1 only.
	if err := s.SetUserRoles(ctx, u.AccountID, member.ID, map[string]string{b2.ID: "owner"}); err != nil {
		t.Fatal(err)
	}
	users, _ = s.ListAccountUsers(ctx, u.AccountID)
	for _, m := range users {
		if m.ID == member.ID {
			if _, ok := m.Roles[b1.ID]; ok {
				t.Fatalf("b1 role should be gone: %#v", m.Roles)
			}
			if m.Roles[b2.ID] != "owner" {
				t.Fatalf("b2 role lost: %#v", m.Roles)
			}
		}
	}
}

// TestSetUserRolesRejectsAdmin verifies an account Admin (implicit access) is
// not editable through the per-inbox access tab's store path.
func TestSetUserRolesRejectsAdmin(t *testing.T) {
	ctx := context.Background()
	s, u, b1, _ := seedAccessAccount(t)
	if err := s.SetUserRoles(ctx, u.AccountID, u.ID, map[string]string{b1.ID: "owner"}); err == nil {
		t.Fatal("expected SetUserRoles to refuse the account admin")
	}
}
