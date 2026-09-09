package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gatehouse-mail/internal/model"
)

func TestSendEmptyBodyRejected(t *testing.T) {
	svc, u, _, box := testService(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{box.ID: "owner"}}
	_, err := svc.Send(ctx, p, SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "Empty", Text: "", HTML: ""}, "")
	if err == nil || !strings.Contains(err.Error(), "message body is required") {
		t.Fatalf("expected empty-body rejection, got %v", err)
	}
}

func TestDeliverPermanentErrorFailsImmediately(t *testing.T) {
	svc, u, dom, box := testService(t)
	ctx := context.Background()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		io.WriteString(w, `{"code":"missing_parameter","message":"Either of htmlContent or textContent is required"}`)
	}))
	defer api.Close()
	cred, err := svc.SaveOutboundCredential(ctx, u.AccountID, "", "Brevo", "brevo", map[string]any{"api_key": "k", "api_base": api.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Store.SetDomainOutboundCredential(ctx, u.AccountID, dom.ID, cred.ID); err != nil {
		t.Fatal(err)
	}
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{box.ID: "owner"}}
	res, err := svc.Send(ctx, p, SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "Bad", Text: "hi"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Deliver(ctx, u.AccountID, res.Message.ID); err == nil {
		t.Fatal("expected delivery error")
	}
	m, err := svc.Store.GetMessageByID(ctx, u.AccountID, res.Message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != "failed" {
		t.Fatalf("status %q, want failed", m.Status)
	}
	if m.Attempts != 1 {
		t.Fatalf("attempts %d, want 1", m.Attempts)
	}
	if m.NextRetry != "" {
		t.Fatalf("next_retry %q, want empty (no retry)", m.NextRetry)
	}
	if !strings.Contains(m.LastError, "400") {
		t.Fatalf("last_error %q", m.LastError)
	}
}

func TestDeliverTransientErrorRetries(t *testing.T) {
	svc, u, dom, box := testService(t)
	ctx := context.Background()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		io.WriteString(w, `{"message":"upstream error"}`)
	}))
	defer api.Close()
	cred, err := svc.SaveOutboundCredential(ctx, u.AccountID, "", "Brevo", "brevo", map[string]any{"api_key": "k", "api_base": api.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Store.SetDomainOutboundCredential(ctx, u.AccountID, dom.ID, cred.ID); err != nil {
		t.Fatal(err)
	}
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{box.ID: "owner"}}
	res, err := svc.Send(ctx, p, SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "Transient", Text: "hi"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Deliver(ctx, u.AccountID, res.Message.ID); err == nil {
		t.Fatal("expected delivery error")
	}
	m, err := svc.Store.GetMessageByID(ctx, u.AccountID, res.Message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != "pending" {
		t.Fatalf("status %q, want pending (retryable)", m.Status)
	}
	if m.Attempts != 1 {
		t.Fatalf("attempts %d, want 1", m.Attempts)
	}
	if m.NextRetry == "" {
		t.Fatalf("next_retry empty, want a future retry time")
	}
}
