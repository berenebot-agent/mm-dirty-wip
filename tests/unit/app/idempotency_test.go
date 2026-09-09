package app_test

import (
	"context"
	"errors"
	"testing"

	"gatehouse-mail/internal/app"
	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
)

// TestIdempotencyReplayScopedToMailbox guards against cross-mailbox disclosure:
// an account-wide idempotency key must only replay the message from the mailbox
// the caller asked for and owns.
func TestIdempotencyReplayScopedToMailbox(t *testing.T) {
	svc, u, d, box := testService(t)
	ctx := context.Background()
	other, err := svc.Store.CreateInbox(ctx, u.AccountID, d.ID, "other", "Other")
	if err != nil {
		t.Fatal(err)
	}

	admin := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{}}
	res, err := svc.Send(ctx, admin, app.SendInput{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "s", Text: "b"}, "shared-key")
	if err != nil {
		t.Fatal(err)
	}

	ownerOther := model.Principal{AccountID: u.AccountID, MailboxRoles: map[string]string{other.ID: "owner"}}
	if _, err := svc.Send(ctx, ownerOther, app.SendInput{InboxID: other.ID, To: []string{"x@y.test"}, Subject: "s", Text: "b"}, "shared-key"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("cross-mailbox replay err=%v, want conflict", err)
	}

	again, err := svc.Send(ctx, admin, app.SendInput{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "s", Text: "b"}, "shared-key")
	if err != nil {
		t.Fatal(err)
	}
	if again.Message.ID != res.Message.ID {
		t.Fatalf("replay id=%q want %q", again.Message.ID, res.Message.ID)
	}
}
