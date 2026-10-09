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
