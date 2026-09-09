package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"gatehouse-mail/internal/mailparse"
	"gatehouse-mail/internal/model"
)

func TestSendBrevoWithAttachmentRoundTrip(t *testing.T) {
	svc, u, d, box := testService(t)
	ctx := context.Background()
	var calls atomic.Int32
	var body map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"messageId":"<brevo-out>"}`)
	}))
	defer api.Close()
	cred, err := svc.SaveOutboundCredential(ctx, u.AccountID, "", "Brevo", "brevo", map[string]any{"api_key": "xkeysib-test", "api_base": api.URL})
	if err != nil {
		t.Fatal(err)
	}
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{}}
	if err = svc.Store.SetDomainOutboundCredential(ctx, u.AccountID, d.ID, cred.ID); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Send(ctx, p, SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "Report", Text: "See attached", Attachments: []SendAttachment{{Filename: "report.txt", ContentType: "text/plain", Content: []byte("hello attachment")}}}, "brevo-key")
	if err != nil {
		t.Fatal(err)
	}
	if res.Message.Status != "pending" {
		t.Fatalf("expected pending, got %q", res.Message.Status)
	}
	// Deliver via the worker path.
	if err = svc.Deliver(ctx, u.AccountID, res.Message.ID); err != nil {
		t.Fatal(err)
	}
	sent, err := svc.Store.GetMessageByID(ctx, u.AccountID, res.Message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sent.Status != "sent" || sent.ProviderMessageID != "<brevo-out>" || calls.Load() != 1 {
		t.Fatalf("provider id %q status %q calls %d", sent.ProviderMessageID, sent.Status, calls.Load())
	}
	if body["textContent"] != "See attached" {
		t.Fatalf("brevo body %#v", body)
	}
	atts, err := svc.Store.ListAttachments(ctx, p, res.Message.ID)
	if err != nil || len(atts) != 1 || atts[0].Filename != "report.txt" {
		t.Fatalf("attachments %v %#v", err, atts)
	}
	path := filepath.Join(svc.Config.DataDir, filepath.FromSlash(res.Message.RawPath))
	var buf bytes.Buffer
	if err = mailparse.ExtractAttachment(path, atts[0].PartIndex, &buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "hello attachment" {
		t.Fatalf("attachment bytes %q", buf.String())
	}
}

func TestUnknownOutboundProviderRejected(t *testing.T) {
	svc, u, _, _ := testService(t)
	_, err := svc.SaveOutboundCredential(context.Background(), u.AccountID, "", "X", "not-a-provider", map[string]any{"api_key": "k"})
	if err == nil || !strings.Contains(err.Error(), "unknown outbound provider") {
		t.Fatalf("err %v", err)
	}
}

func TestDeliverRecordsDeliveryAttempts(t *testing.T) {
	svc, u, d, box := testService(t)
	ctx := context.Background()
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"messageId":"<log-out>"}`)
	}))
	defer api.Close()
	cred, err := svc.SaveOutboundCredential(ctx, u.AccountID, "", "Brevo", "brevo", map[string]any{"api_key": "k", "api_base": api.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Store.SetDomainOutboundCredential(ctx, u.AccountID, d.ID, cred.ID); err != nil {
		t.Fatal(err)
	}
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{box.ID: "owner"}}
	res, err := svc.Send(ctx, p, SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "Log", Text: "hi"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Deliver(ctx, u.AccountID, res.Message.ID); err != nil {
		t.Fatal(err)
	}
	attempts, err := svc.Store.ListDeliveryAttempts(ctx, u.AccountID, cred.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempts len %d", len(attempts))
	}
	a := attempts[0]
	if a.Status != "sent" || a.MessageID != res.Message.ID || a.CredentialID != cred.ID || a.Provider != "brevo" || a.ProviderMessageID != "<log-out>" || a.Attempt != 1 {
		t.Fatalf("attempt %+v", a)
	}
}
