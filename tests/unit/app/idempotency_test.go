package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dellarb/mammothmail/internal/app"
	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
)

// TestIdempotencyReplayScopedToMailbox guards against cross-mailbox disclosure:
// an account-wide idempotency key must only replay the message from the mailbox
// the caller asked for and owns.
func TestIdempotencyReplayScopedToMailbox(t *testing.T) {
	t.Parallel()
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

func TestIdempotencyBindsSendInputs(t *testing.T) {
	t.Parallel()
	svc, u, _, box := testService(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	in := app.SendInput{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "s", Text: "b"}
	first, err := svc.Send(ctx, p, in, "bound-key")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*app.SendInput)
	}{
		{"sender", func(in *app.SendInput) { in.FromAddress = box.Address }},
		{"sender name", func(in *app.SendInput) { in.FromName = "Other" }},
		{"to", func(in *app.SendInput) { in.To = []string{"other@y.test"} }},
		{"cc", func(in *app.SendInput) { in.CC = []string{"cc@y.test"} }},
		{"bcc", func(in *app.SendInput) { in.BCC = []string{"bcc@y.test"} }},
		{"subject", func(in *app.SendInput) { in.Subject = "different" }},
		{"text", func(in *app.SendInput) { in.Text = "different" }},
		{"html", func(in *app.SendInput) { in.HTML = "<p>different</p>" }},
		{"reply", func(in *app.SendInput) { in.ReplyToMessageID = first.Message.ID }},
		{"forward", func(in *app.SendInput) { in.ForwardOfMessageID = first.Message.ID }},
		{"draft", func(in *app.SendInput) { in.DraftID = "different-draft" }},
		{"approval", func(in *app.SendInput) { in.SendRequestID = "different-request" }},
		{"attachment", func(in *app.SendInput) {
			in.Attachments = []app.SendAttachment{{Filename: "note.txt", ContentType: "text/plain", Content: []byte("different")}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := in
			tc.change(&changed)
			if _, err := svc.Send(ctx, p, changed, "bound-key"); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("changed request err=%v, want conflict", err)
			}
		})
	}
	// A rejected mismatch must not release or overwrite the original reservation.
	again, err := svc.Send(ctx, p, in, "bound-key")
	if err != nil || again.Message.ID != first.Message.ID {
		t.Fatalf("original replay = %+v, %v", again, err)
	}
}

func TestIdempotencyFailedEnqueueReleasesBinding(t *testing.T) {
	t.Parallel()
	svc, u, _, box := testService(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	in := app.SendInput{InboxID: box.ID, Subject: "s", Text: "b"}
	if _, err := svc.Send(ctx, p, in, "retry-key"); err == nil {
		t.Fatal("send without recipients succeeded")
	}
	in.To = []string{"friend@example.net"}
	if _, err := svc.Send(ctx, p, in, "retry-key"); err != nil {
		t.Fatalf("failed enqueue retained binding: %v", err)
	}
}
