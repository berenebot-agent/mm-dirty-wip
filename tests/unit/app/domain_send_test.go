package app_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// TestSendDomainWideKeyCarriesGrantToSendPath proves the API-key identity
// reaches the send path: a key holding the domain-wide sending grant may send
// from an arbitrary address on the catch-all's domain, and a key without it
// cannot, even though both are Owners of the catch-all inbox.
func TestSendDomainWideKeyCarriesGrantToSendPath(t *testing.T) {
	t.Parallel()
	svc, u, d, box := testService(t)
	ctx := context.Background()

	if err := svc.Store.SetDomainCatchAll(ctx, u.AccountID, d.ID, box.ID); err != nil {
		t.Fatal(err)
	}
	granted, grantedToken, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "granted", false, map[string]string{box.ID: "owner"}, map[string]bool{box.ID: true})
	if err != nil {
		t.Fatal(err)
	}
	plain, plainToken, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "plain", false, map[string]string{box.ID: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	_ = granted

	gp, err := svc.Store.APIKeyPrincipal(ctx, grantedToken)
	if err != nil {
		t.Fatal(err)
	}
	pp, err := svc.Store.APIKeyPrincipal(ctx, plainToken)
	if err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	api := providerServer(t, "<wild-out>", &calls)
	seedSending(t, svc, u.AccountID, d.ID, "brevo", map[string]any{"api_key": "k", "api_base": api.URL})

	// The granted key may send as any address on the domain.
	res, err := svc.Send(ctx, gp, app.SendInput{InboxID: box.ID, FromAddress: "whatever@example.com", To: []string{"friend@example.net"}, Subject: "Hi", Text: "hello"}, "wild-key")
	if err != nil {
		t.Fatalf("granted key wildcard send: %v", err)
	}
	if res.Message.From.Address != "whatever@example.com" {
		t.Fatalf("from %+v", res.Message.From)
	}
	if err := svc.Deliver(ctx, u.AccountID, res.Message.ID, ""); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("provider calls %d, want 1", calls.Load())
	}

	// The ungranted key (also Owner of the same inbox) is refused.
	if _, err := svc.Send(ctx, pp, app.SendInput{InboxID: box.ID, FromAddress: "whatever@example.com", To: []string{"friend@example.net"}, Subject: "Hi", Text: "hello"}, "plain-key"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("ungranted key wildcard send err=%v, want forbidden", err)
	}
	_ = plain
}

// TestSendDomainWideDraftCarriesGrant proves a key with the grant can create a
// draft from an arbitrary address and send it, so the flow works through the
// draft path (which re-resolves the frozen address at send).
func TestSendDomainWideDraftCarriesGrant(t *testing.T) {
	t.Parallel()
	svc, u, d, box := testService(t)
	ctx := context.Background()
	if err := svc.Store.SetDomainCatchAll(ctx, u.AccountID, d.ID, box.ID); err != nil {
		t.Fatal(err)
	}
	_, token, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "granted", false, map[string]string{box.ID: "owner"}, map[string]bool{box.ID: true})
	if err != nil {
		t.Fatal(err)
	}
	p, err := svc.Store.APIKeyPrincipal(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	draft, err := svc.Store.CreateDraft(ctx, p, model.Draft{InboxID: box.ID, FromAddress: "arbitrary@example.com", To: []string{"friend@example.net"}, Subject: "Hi", Text: "hello"})
	if err != nil {
		t.Fatalf("draft with wildcard from: %v", err)
	}
	if draft.FromAddress != "arbitrary@example.com" {
		t.Fatalf("draft from %q", draft.FromAddress)
	}

	var calls atomic.Int32
	api := providerServer(t, "<wild-draft-out>", &calls)
	seedSending(t, svc, u.AccountID, d.ID, "brevo", map[string]any{"api_key": "k", "api_base": api.URL})
	res, err := svc.SendDraft(ctx, p, draft.ID, app.SendInput{InboxID: box.ID, FromAddress: draft.FromAddress, FromName: draft.FromName, To: []string{"friend@example.net"}, Subject: "Hi", Text: "hello"}, "wild-draft")
	if err != nil {
		t.Fatalf("send wildcard draft: %v", err)
	}
	if res.Message.From.Address != "arbitrary@example.com" {
		t.Fatalf("sent from %+v", res.Message.From)
	}
}
