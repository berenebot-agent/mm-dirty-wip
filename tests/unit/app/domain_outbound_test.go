package app_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"gatehouse-mail/internal/app"
	"gatehouse-mail/internal/model"
)

func providerServer(t *testing.T, id string, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"messageId":"`+id+`"}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSendQueuesWithoutProviderThenDelivers verifies that a domain with no
// provider still accepts mail into the outbox, holds it without consuming a
// retry, and delivers it once a provider is assigned.
func TestSendQueuesWithoutProviderThenDelivers(t *testing.T) {
	svc, u, d, box := testService(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{}}

	res, err := svc.Send(ctx, p, app.SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "Hi", Text: "hello"}, "queue-key")
	if err != nil {
		t.Fatalf("send without provider should queue, got %v", err)
	}
	if res.Message.Status != "pending" || res.Message.LastError == "" {
		t.Fatalf("queued message: status=%q err=%q", res.Message.Status, res.Message.LastError)
	}
	if err = svc.Deliver(ctx, u.AccountID, res.Message.ID, ""); err != nil {
		t.Fatalf("deliver while held: %v", err)
	}
	held, err := svc.Store.GetMessageByID(ctx, u.AccountID, res.Message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if held.Status != "pending" || held.Attempts != 0 {
		t.Fatalf("held message: status=%q attempts=%d", held.Status, held.Attempts)
	}

	var calls atomic.Int32
	api := providerServer(t, "<brevo-queued>", &calls)
	cred, err := svc.SaveOutboundCredential(ctx, u.AccountID, "", "Brevo", "brevo", map[string]any{"api_key": "k", "api_base": api.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Store.SetDomainOutboundCredential(ctx, u.AccountID, d.ID, cred.ID); err != nil {
		t.Fatal(err)
	}
	if err = svc.Deliver(ctx, u.AccountID, res.Message.ID, ""); err != nil {
		t.Fatalf("deliver after provider assigned: %v", err)
	}
	sent, err := svc.Store.GetMessageByID(ctx, u.AccountID, res.Message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sent.Status != "sent" || sent.Provider != "brevo" || calls.Load() != 1 {
		t.Fatalf("sent: status=%q provider=%q calls=%d", sent.Status, sent.Provider, calls.Load())
	}
}

// TestSendUsesDomainCredential verifies that a domain sends through the
// credential assigned to it, not another credential on the account.
func TestSendUsesDomainCredential(t *testing.T) {
	svc, u, d, box := testService(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{}}

	var chosenCalls, otherCalls atomic.Int32
	chosen := providerServer(t, "<mailgun>", &chosenCalls)
	other := providerServer(t, "<brevo>", &otherCalls)

	if _, err := svc.SaveOutboundCredential(ctx, u.AccountID, "", "Brevo", "brevo", map[string]any{"api_key": "k", "api_base": other.URL}); err != nil {
		t.Fatal(err)
	}
	mgCred, err := svc.SaveOutboundCredential(ctx, u.AccountID, "", "MG", "mailgun", map[string]any{"api_key": "k", "domain": "mg.example.com", "api_base": chosen.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Store.SetDomainOutboundCredential(ctx, u.AccountID, d.ID, mgCred.ID); err != nil {
		t.Fatal(err)
	}

	res, err := svc.Send(ctx, p, app.SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "Hi", Text: "hello"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Deliver(ctx, u.AccountID, res.Message.ID, ""); err != nil {
		t.Fatal(err)
	}
	sent, err := svc.Store.GetMessageByID(ctx, u.AccountID, res.Message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sent.Status != "sent" || sent.Provider != "mailgun" || chosenCalls.Load() != 1 || otherCalls.Load() != 0 {
		t.Fatalf("domain provider: status=%q provider=%q chosen=%d other=%d", sent.Status, sent.Provider, chosenCalls.Load(), otherCalls.Load())
	}
}

// TestSendDoesNotUseOtherDomainCredential verifies the core isolation rule: a
// domain with no credential must never send through another domain's provider.
func TestSendDoesNotUseOtherDomainCredential(t *testing.T) {
	svc, u, _, box := testService(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{}}

	otherDomain, err := svc.Store.CreateDomain(ctx, u.AccountID, "other.example")
	if err != nil {
		t.Fatal(err)
	}
	otherBox, err := svc.Store.CreateInbox(ctx, u.AccountID, otherDomain.ID, "agent", "Agent")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	api := providerServer(t, "<brevo-other>", &calls)
	cred, err := svc.SaveOutboundCredential(ctx, u.AccountID, "", "Brevo", "brevo", map[string]any{"api_key": "k", "api_base": api.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Store.SetDomainOutboundCredential(ctx, u.AccountID, otherDomain.ID, cred.ID); err != nil {
		t.Fatal(err)
	}

	// box is on a different domain with no provider: it must queue, not send.
	res, err := svc.Send(ctx, p, app.SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "Hi", Text: "hello"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Deliver(ctx, u.AccountID, res.Message.ID, ""); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatalf("unassigned domain must not send through another domain's provider, calls=%d", calls.Load())
	}

	// The other domain's inbox does send through its assigned credential.
	res2, err := svc.Send(ctx, p, app.SendInput{InboxID: otherBox.ID, To: []string{"friend@example.net"}, Subject: "Hi", Text: "hello"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Deliver(ctx, u.AccountID, res2.Message.ID, ""); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("assigned domain should send, calls=%d", calls.Load())
	}
}
