package app_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dellarb/mammothmail/internal/app"
	"github.com/dellarb/mammothmail/internal/model"
)

// TestDeliverRejectsStaleClaimToken proves the app layer's claim verification
// and the store-side outcome guard together: a delivery carrying a claim token
// that no longer owns the message (the lease lapsed and another attempt
// re-claimed it) is refused, records nothing, and the live attempt then
// delivers normally with its own token.
func TestDeliverRejectsStaleClaimToken(t *testing.T) {
	t.Parallel()
	svc, u, dom, box := testService(t)
	ctx := context.Background()
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"messageId":"<brevo-out>"}`)
	}))
	defer api.Close()
	seedSending(t, svc, u.AccountID, dom.ID, "brevo", map[string]any{"api_key": "xkeysib-test", "api_base": api.URL})
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{}}
	res, err := svc.Send(ctx, p, app.SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "Stale", Text: "hi"}, "")
	if err != nil {
		t.Fatal(err)
	}
	// Attempt 1 claims with a long lease, then loses it: attempt 2 re-claims
	// past the lease (the provider call outlived the first claim).
	id, claim1, err := svc.Store.ClaimNextPending(ctx, time.Now().UTC(), "w1", time.Hour)
	if err != nil || id != res.Message.ID {
		t.Fatalf("claim = %q err=%v", id, err)
	}
	// Attempt 2 takes over after the first lease expires. Keep the first
	// attempt's actual token to prove its stale delivery is refused.
	_, claim2, err := svc.Store.ClaimNextPending(ctx, time.Now().UTC().Add(2*time.Hour), "w2", time.Hour)
	if err != nil || claim2 == "" {
		t.Fatalf("re-claim = %q err=%v", claim2, err)
	}
	// Attempt 1, mid-send, tries to deliver with its lost claim: refused before
	// any provider call, no outcome recorded.
	if err = svc.Deliver(ctx, u.AccountID, res.Message.ID, claim1); err == nil {
		t.Fatal("expected the stale attempt to be refused")
	}
	if calls.Load() != 0 {
		t.Fatalf("stale attempt reached the provider %d times", calls.Load())
	}
	m, err := svc.Store.GetMessageByID(ctx, u.AccountID, res.Message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != "pending" || m.Attempts != 0 {
		t.Fatalf("stale attempt recorded an outcome: %+v", m)
	}
	// The live attempt delivers normally with its own token.
	if err = svc.Deliver(ctx, u.AccountID, res.Message.ID, claim2); err != nil {
		t.Fatalf("live attempt Deliver = %v", err)
	}
	m, err = svc.Store.GetMessageByID(ctx, u.AccountID, res.Message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != "sent" || m.Attempts != 1 {
		t.Fatalf("after live attempt: %+v, want sent once", m)
	}
	if calls.Load() != 1 {
		t.Fatalf("provider called %d times, want exactly 1", calls.Load())
	}
}
