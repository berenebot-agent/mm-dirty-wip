package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/config"
	"github.com/dellarb/mailmoose/internal/events"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/tests/support/testdb"
)

// rateLimitedService builds a service with a small per-minute send limit so the
// central limiter can be exercised without sending a realistic volume.
func rateLimitedService(t *testing.T, limit int) (*app.Service, model.User, model.Inbox) {
	t.Helper()
	st, dir := testdb.OpenDir(t)
	cfg := config.Config{DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted", AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901", MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20, SessionTTL: time.Hour, LoginLimitPerMinute: 10, SendLimitPerMinute: limit}
	svc, err := app.New(cfg, st, events.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	u, err := st.CreateAccountAndAdmin(ctx, "A", "admin@example.com", "correct horse battery staple", cfg.DefaultQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.CreateDomain(ctx, u.AccountID, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	box, err := st.CreateInbox(ctx, u.AccountID, d.ID, "hermes", "Hermes")
	if err != nil {
		t.Fatal(err)
	}
	return svc, u, box
}

// TestSendRateLimitCoversDraftPath proves the outbound send limit is enforced
// centrally in the service, so an account cannot bypass it by using the
// draft-send path instead of the plain send path.
func TestSendRateLimitCoversDraftPath(t *testing.T) {
	svc, u, box := rateLimitedService(t, 2)
	ctx := context.Background()
	admin := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{}}

	// Two plain sends consume the limit.
	for i := 0; i < 2; i++ {
		if _, err := svc.Send(ctx, admin, app.SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "Hi", Text: "hello"}, ""); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	// A third plain send is refused.
	if _, err := svc.Send(ctx, admin, app.SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "Hi", Text: "hello"}, ""); !errors.Is(err, app.ErrRateLimited) {
		t.Fatalf("third send = %v, want ErrRateLimited", err)
	}
	// A draft send is also refused: the limit is not bypassable via that path.
	draft, err := svc.Store.CreateDraft(ctx, admin, model.Draft{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "s", Text: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SendDraft(ctx, admin, draft.ID, app.SendInput{InboxID: box.ID, To: draft.To, Subject: draft.Subject, Text: draft.Text}, ""); !errors.Is(err, app.ErrRateLimited) {
		t.Fatalf("draft send = %v, want ErrRateLimited", err)
	}
}

func TestIdempotentReplayDoesNotConsumeSendAllowance(t *testing.T) {
	svc, u, box := rateLimitedService(t, 1)
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	in := app.SendInput{InboxID: box.ID, To: []string{"a@b.test"}, Subject: "hi", Text: "body"}
	first, err := svc.Send(context.Background(), p, in, "same-request")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := svc.Send(context.Background(), p, in, "same-request")
	if err != nil || replay.Message.ID != first.Message.ID {
		t.Fatalf("replay was rate limited or duplicated: %v", err)
	}
	if _, err := svc.Send(context.Background(), p, in, "new-request"); !errors.Is(err, app.ErrRateLimited) {
		t.Fatalf("new request bypassed limit: %v", err)
	}
}

func TestExternalApprovalRateLimitKeepsRequestPending(t *testing.T) {
	svc, u, box := rateLimitedService(t, 1)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	if err := svc.Store.SetInboxApprover(ctx, u.AccountID, box.ID, "owner@example.net"); err != nil {
		t.Fatal(err)
	}
	d, err := svc.Store.CreateDraft(ctx, p, model.Draft{InboxID: box.ID, To: []string{"a@b.test"}, Subject: "hi", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	d, err = svc.RequestSend(ctx, p, d.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Send(ctx, p, app.SendInput{InboxID: box.ID, To: []string{"a@b.test"}, Subject: "other", Text: "body"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApproveExternal(ctx, u.AccountID, box.ID, d.SendRequest.ID, "owner@example.net", ""); !errors.Is(err, app.ErrRateLimited) {
		t.Fatalf("external approval bypassed limit: %v", err)
	}
	request, err := svc.Store.GetSendRequestInternal(ctx, u.AccountID, d.SendRequest.ID)
	if err != nil || request.Status != model.SendRequestPending {
		t.Fatalf("rate-limited approval consumed request: %v %+v", err, request)
	}
}
