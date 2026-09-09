package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

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
	svc, u, _, box := testService(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{}}

	res, err := svc.Send(ctx, p, SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "Hi", Text: "hello"}, "queue-key")
	if err != nil {
		t.Fatalf("send without provider should queue, got %v", err)
	}
	if res.Message.Status != "pending" || res.Message.LastError == "" {
		t.Fatalf("queued message: status=%q err=%q", res.Message.Status, res.Message.LastError)
	}
	if err = svc.Deliver(ctx, u.AccountID, res.Message.ID); err != nil {
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
	if err = svc.Store.SetActiveOutboundCredential(ctx, u.AccountID, cred.ID); err != nil {
		t.Fatal(err)
	}
	if err = svc.Deliver(ctx, u.AccountID, res.Message.ID); err != nil {
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

// TestDomainCredentialOverridesAccountActive verifies that a domain credential
// takes precedence over the account's active provider.
func TestDomainCredentialOverridesAccountActive(t *testing.T) {
	svc, u, d, box := testService(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{}}

	var brevoCalls, mgCalls atomic.Int32
	brevo := providerServer(t, "<brevo>", &brevoCalls)
	mg := providerServer(t, "<mailgun>", &mgCalls)

	brevoCred, err := svc.SaveOutboundCredential(ctx, u.AccountID, "", "Brevo", "brevo", map[string]any{"api_key": "k", "api_base": brevo.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Store.SetActiveOutboundCredential(ctx, u.AccountID, brevoCred.ID); err != nil {
		t.Fatal(err)
	}
	mgCred, err := svc.SaveOutboundCredential(ctx, u.AccountID, "", "MG", "mailgun", map[string]any{"api_key": "k", "domain": "mg.example.com", "api_base": mg.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Store.SetDomainOutboundCredential(ctx, u.AccountID, d.ID, mgCred.ID); err != nil {
		t.Fatal(err)
	}

	res, err := svc.Send(ctx, p, SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "Hi", Text: "hello"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Deliver(ctx, u.AccountID, res.Message.ID); err != nil {
		t.Fatal(err)
	}
	sent, err := svc.Store.GetMessageByID(ctx, u.AccountID, res.Message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sent.Status != "sent" || sent.Provider != "mailgun" || mgCalls.Load() != 1 || brevoCalls.Load() != 0 {
		t.Fatalf("domain provider: status=%q provider=%q mg=%d brevo=%d", sent.Status, sent.Provider, mgCalls.Load(), brevoCalls.Load())
	}
}
