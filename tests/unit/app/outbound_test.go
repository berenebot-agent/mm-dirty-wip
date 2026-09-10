package app_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"gatehouse-mail/internal/app"
	"gatehouse-mail/internal/mailparse"
	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/transport"
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
	seedSending(t, svc, u.AccountID, d.ID, "brevo", map[string]any{"api_key": "xkeysib-test", "api_base": api.URL})
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{}}
	res, err := svc.Send(ctx, p, app.SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "Report", Text: "See attached", Attachments: []app.SendAttachment{{Filename: "report.txt", ContentType: "text/plain", Content: []byte("hello attachment")}}}, "brevo-key")
	if err != nil {
		t.Fatal(err)
	}
	if res.Message.Status != "pending" {
		t.Fatalf("expected pending, got %q", res.Message.Status)
	}
	// app.Deliver via the worker path.
	if err = svc.Deliver(ctx, u.AccountID, res.Message.ID, ""); err != nil {
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
	attsPayload, _ := body["attachment"].([]any)
	if len(attsPayload) != 1 {
		t.Fatalf("brevo attachment payload %#v", body)
	}
	att, _ := attsPayload[0].(map[string]any)
	if att["name"] != "report.txt" {
		t.Fatalf("brevo attachment name %#v", att)
	}
	decoded, err := base64.StdEncoding.DecodeString(att["content"].(string))
	if err != nil || string(decoded) != "hello attachment" {
		t.Fatalf("brevo attachment content %v err %v", att["content"], err)
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

func TestSendMailgunWithAttachmentRoundTrip(t *testing.T) {
	svc, u, d, box := testService(t)
	ctx := context.Background()
	var gotName, gotContent string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
		}
		f, hdr, err := r.FormFile("attachment")
		if err != nil {
			t.Errorf("attachment missing: %v", err)
		} else {
			defer f.Close()
			data, _ := io.ReadAll(f)
			gotName = hdr.Filename
			gotContent = string(data)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"<mg-out>"}`)
	}))
	defer api.Close()
	seedSending(t, svc, u.AccountID, d.ID, "mailgun", map[string]any{"api_key": "key-test", "domain": "mg.example.com", "api_base": api.URL})
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{}}
	res, err := svc.Send(ctx, p, app.SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "Report", Text: "See attached", Attachments: []app.SendAttachment{{Filename: "report.txt", ContentType: "text/plain", Content: []byte("hello attachment")}}}, "mg-key")
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Deliver(ctx, u.AccountID, res.Message.ID, ""); err != nil {
		t.Fatal(err)
	}
	if gotName != "report.txt" || gotContent != "hello attachment" {
		t.Fatalf("mailgun attachment name=%q content=%q", gotName, gotContent)
	}
}

func TestUnknownSendingProviderRejected(t *testing.T) {
	svc, u, d, _ := testService(t)
	_, err := svc.SaveDomainSendingConfig(context.Background(), u.AccountID, d.ID, "not-a-provider", map[string]any{"api_key": "k"})
	if !errors.Is(err, app.ErrInvalidConfig) {
		t.Fatalf("err %v, want ErrInvalidConfig", err)
	}
	if !errors.Is(err, transport.ErrUnknownProvider) {
		t.Fatalf("err %v, want ErrUnknownProvider", err)
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
	seedSending(t, svc, u.AccountID, d.ID, "brevo", map[string]any{"api_key": "k", "api_base": api.URL})
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{box.ID: "owner"}}
	res, err := svc.Send(ctx, p, app.SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "Log", Text: "hi"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Deliver(ctx, u.AccountID, res.Message.ID, ""); err != nil {
		t.Fatal(err)
	}
	attempts, err := svc.Store.ListDomainDeliveryAttempts(ctx, u.AccountID, d.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempts len %d", len(attempts))
	}
	a := attempts[0]
	if a.Status != "sent" || a.MessageID != res.Message.ID || a.DomainID != d.ID || a.Provider != "brevo" || a.ProviderMessageID != "<log-out>" || a.Attempt != 1 {
		t.Fatalf("attempt %+v", a)
	}
}
