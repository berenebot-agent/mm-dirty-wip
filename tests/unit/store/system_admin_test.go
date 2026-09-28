package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSyncSystemAdminCreatesThenRotates(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	const first = "correct horse battery staple"
	u, changed, err := s.SyncSystemAdmin(ctx, "MailMoose", "admin@example.com", first, 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || !u.SystemAdmin || !u.IsAdmin {
		t.Fatalf("create: changed=%v user=%#v", changed, u)
	}
	if has, err := s.HasSystemAdmin(ctx); err != nil || !has {
		t.Fatalf("HasSystemAdmin = %v, %v", has, err)
	}
	session, _, err := s.CreateSession(ctx, u.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Re-applying identical credentials is a no-op and keeps the session.
	if _, changed, err = s.SyncSystemAdmin(ctx, "MailMoose", "admin@example.com", first, 50<<20); err != nil || changed {
		t.Fatalf("no-op sync changed=%v err=%v", changed, err)
	}
	if _, _, err := s.SessionPrincipal(ctx, session); err != nil {
		t.Fatalf("session dropped by no-op sync: %v", err)
	}
	// Changing the password rotates the login and revokes existing sessions.
	const second = "another correct horse battery staple"
	if _, changed, err = s.SyncSystemAdmin(ctx, "MailMoose", "admin@example.com", second, 50<<20); err != nil || !changed {
		t.Fatalf("password sync changed=%v err=%v", changed, err)
	}
	if _, _, err := s.SessionPrincipal(ctx, session); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("session survived password rotation: %v", err)
	}
	if _, err := s.AuthenticateUser(ctx, "admin@example.com", first); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old password still works: %v", err)
	}
	if _, err := s.AuthenticateUser(ctx, "admin@example.com", second); err != nil {
		t.Fatalf("new password rejected: %v", err)
	}
	// Changing the email rotates the login identity too.
	if _, changed, err = s.SyncSystemAdmin(ctx, "MailMoose", "root@example.com", second, 50<<20); err != nil || !changed {
		t.Fatalf("email sync changed=%v err=%v", changed, err)
	}
	if _, err := s.AuthenticateUser(ctx, "admin@example.com", second); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old email still works: %v", err)
	}
	if _, err := s.AuthenticateUser(ctx, "root@example.com", second); err != nil {
		t.Fatalf("new email rejected: %v", err)
	}
}

func TestSyncSystemAdminRefusesTakenEmail(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	if _, _, err := s.SyncSystemAdmin(ctx, "MailMoose", "admin@example.com", "correct horse battery staple", 50<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAccountAndAdmin(ctx, "Other", "other@example.com", "correct horse battery staple", 50<<20); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SyncSystemAdmin(ctx, "MailMoose", "other@example.com", "correct horse battery staple", 50<<20); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("sync onto an existing email error = %v, want ErrConflict", err)
	}
}

func TestAdminResetRefusesSystemAdmin(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	u, _, err := s.SyncSystemAdmin(ctx, "MailMoose", "admin@example.com", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AdminResetPassword(ctx, u.ID, "another correct horse battery staple"); !errors.Is(err, store.ErrSystemAdmin) {
		t.Fatalf("reset system admin error = %v, want ErrSystemAdmin", err)
	}
}

func TestSystemMailerOwnership(t *testing.T) {
	ctx := context.Background()
	s, u, _, boxes := testStore(t)
	if err := s.SetSystemMailerInbox(ctx, u.AccountID, boxes[0].ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.SystemMailerInboxID(ctx)
	if err != nil || got != boxes[0].ID {
		t.Fatalf("SystemMailerInboxID = %q, %v", got, err)
	}
	if err := s.SetSystemMailerInbox(ctx, u.AccountID, "inb_missing"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("foreign system mailer error = %v, want ErrForbidden", err)
	}
	if err := s.SetSystemMailerInbox(ctx, u.AccountID, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.SystemMailerInboxID(ctx); got != "" {
		t.Fatalf("cleared system mailer = %q, want empty", got)
	}
}

func TestAccountAdminInviteRedeemsSeparateAccount(t *testing.T) {
	ctx := context.Background()
	s, u, _, _ := testStore(t)
	inv, token, err := s.CreateInvite(ctx, store.InviteInput{
		Email: "new@example.com", Kind: model.InviteKindAccountAdmin, Quota: 50 << 20, TTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if inv.AccountID == "" || inv.AccountID == u.AccountID {
		t.Fatalf("account_admin invite must own a fresh account, got %q (existing %q)", inv.AccountID, u.AccountID)
	}
	if got, err := s.GetInviteByToken(ctx, token); err != nil || got.ID != inv.ID {
		t.Fatalf("GetInviteByToken = %#v, %v", got, err)
	}
	created, err := s.RedeemInvite(ctx, token, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !created.IsAdmin || created.SystemAdmin || created.AccountID != inv.AccountID {
		t.Fatalf("redeemed account Admin = %#v", created)
	}
	if _, err := s.AuthenticateUser(ctx, "new@example.com", "correct horse battery staple"); err != nil {
		t.Fatalf("redeemed login rejected: %v", err)
	}
	if _, err := s.RedeemInvite(ctx, token, "correct horse battery staple"); !errors.Is(err, store.ErrInviteExpired) {
		t.Fatalf("second redeem error = %v, want ErrInviteExpired", err)
	}
}

func TestOperatorInviteGrantsOwnerOnSelectedInboxes(t *testing.T) {
	ctx := context.Background()
	s, u, _, boxes := testStore(t)
	inv, token, err := s.CreateInvite(ctx, store.InviteInput{
		AccountID: u.AccountID, Email: "op@example.com", Kind: model.InviteKindOperator,
		InboxIDs: []string{boxes[0].ID, boxes[1].ID}, TTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if inv.AccountID != u.AccountID {
		t.Fatalf("operator invite account = %q, want %q", inv.AccountID, u.AccountID)
	}
	op, err := s.RedeemInvite(ctx, token, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if op.IsAdmin || op.SystemAdmin {
		t.Fatalf("operator must not be an Admin: %#v", op)
	}
	session, _, err := s.CreateSession(ctx, op.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := s.SessionPrincipal(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	if !p.CanOwn(boxes[0].ID) || !p.CanOwn(boxes[1].ID) {
		t.Fatalf("operator roles = %#v, want owner on both", p.MailboxRoles)
	}
	if p.CanRead(boxes[2].ID) {
		t.Fatalf("operator can read an unassigned inbox: %#v", p.MailboxRoles)
	}
	// An operator invite cannot target an inbox outside the account.
	if _, _, err := s.CreateInvite(ctx, store.InviteInput{AccountID: u.AccountID, Email: "x@example.com", Kind: model.InviteKindOperator, InboxIDs: []string{"inb_missing"}}); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("foreign inbox invite error = %v, want ErrForbidden", err)
	}
}

func TestRevokeInviteInvalidatesToken(t *testing.T) {
	ctx := context.Background()
	s, u, _, _ := testStore(t)
	inv, token, err := s.CreateInvite(ctx, store.InviteInput{AccountID: u.AccountID, Email: "op@example.com", Kind: model.InviteKindOperator, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeInvite(ctx, u.AccountID, inv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetInviteByToken(ctx, token); !errors.Is(err, store.ErrInviteExpired) {
		t.Fatalf("revoked token error = %v, want ErrInviteExpired", err)
	}
	if _, err := s.RedeemInvite(ctx, token, "correct horse battery staple"); !errors.Is(err, store.ErrInviteExpired) {
		t.Fatalf("revoked redeem error = %v, want ErrInviteExpired", err)
	}
}

func TestSetUserRolesReplacesOperatorGrants(t *testing.T) {
	ctx := context.Background()
	s, u, _, boxes := testStore(t)
	_, token, err := s.CreateInvite(ctx, store.InviteInput{AccountID: u.AccountID, Email: "op@example.com", Kind: model.InviteKindOperator, InboxIDs: []string{boxes[0].ID}, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.RedeemInvite(ctx, token, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserRoles(ctx, u.AccountID, op.ID, map[string]string{boxes[2].ID: "owner"}); err != nil {
		t.Fatal(err)
	}
	session, _, err := s.CreateSession(ctx, op.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	opPrincipal, _, err := s.SessionPrincipal(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	if !opPrincipal.CanOwn(boxes[2].ID) || opPrincipal.CanRead(boxes[0].ID) {
		t.Fatalf("roles after replace = %#v", opPrincipal.MailboxRoles)
	}
	members, err := s.ListAccountUsers(ctx, u.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, m := range members {
		if m.ID == op.ID {
			found = true
			if m.Roles[boxes[2].ID] != "owner" {
				t.Fatalf("ListAccountUsers roles = %#v", m.Roles)
			}
		}
	}
	if !found {
		t.Fatal("operator missing from ListAccountUsers")
	}
	if err := s.DeleteAccountMember(ctx, u.AccountID, op.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAccountMember(ctx, u.AccountID, u.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleting an account Admin error = %v, want ErrNotFound", err)
	}
}
