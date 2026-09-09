package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gatehouse-mail/internal/model"
)

func TestOutboxWorkerDeliversPending(t *testing.T) {
	svc, u, dom, box := testService(t)
	ctx := context.Background()
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"messageId":"<worker-out>"}`)
	}))
	defer api.Close()
	cred, err := svc.SaveOutboundCredential(ctx, u.AccountID, "", "Brevo", "brevo", map[string]any{"api_key": "k", "api_base": api.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Store.SetDomainOutboundCredential(ctx, u.AccountID, dom.ID, cred.ID); err != nil {
		t.Fatal(err)
	}
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{}}
	res, err := svc.Send(ctx, p, SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "Worker", Text: "hi"}, "")
	if err != nil {
		t.Fatal(err)
	}
	// Start the worker and wait for delivery.
	w := NewOutboxWorker(svc, nil)
	w.SetPeriod(10 * time.Millisecond)
	w.Start()
	defer w.Stop()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m, err := svc.Store.GetMessageByID(ctx, u.AccountID, res.Message.ID)
		if err != nil {
			t.Fatal(err)
		}
		if m.Status == "sent" {
			if m.ProviderMessageID != "<worker-out>" {
				t.Fatalf("provider id %q", m.ProviderMessageID)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("worker did not deliver; calls=%d", calls.Load())
}

func TestDraftSendFlow(t *testing.T) {
	svc, u, dom, box := testService(t)
	ctx := context.Background()
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"messageId":"<draft-out>"}`)
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
	// Create a draft with an attachment.
	d, err := svc.Store.CreateDraft(ctx, p, model.Draft{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "Draft", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	rawPath := filepath.Join(svc.Config.DataDir, "drafts", "test.bin")
	if err = os.MkdirAll(filepath.Dir(rawPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(rawPath, []byte("draft attachment"), 0o600); err != nil {
		t.Fatal(err)
	}
	rel, _ := filepath.Rel(svc.Config.DataDir, rawPath)
	if _, err = svc.Store.AddDraftAttachment(ctx, p, d.ID, model.DraftAttachment{Filename: "d.txt", ContentType: "text/plain", Size: 16, RawPath: filepath.ToSlash(rel)}); err != nil {
		t.Fatal(err)
	}
	// Send the draft via the send-draft path (copy fields + attachments, enqueue, delete draft).
	atts, err := svc.Store.ListDraftAttachments(ctx, p, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	sendAtts := make([]SendAttachment, 0, len(atts))
	for _, a := range atts {
		data, rerr := os.ReadFile(filepath.Join(svc.Config.DataDir, filepath.FromSlash(a.RawPath)))
		if rerr != nil {
			t.Fatal(rerr)
		}
		sendAtts = append(sendAtts, SendAttachment{Filename: a.Filename, ContentType: a.ContentType, Content: data})
	}
	res, err := svc.Send(ctx, p, SendInput{InboxID: d.InboxID, To: d.To, Subject: d.Subject, Text: d.Text, Attachments: sendAtts}, "")
	if err != nil {
		t.Fatal(err)
	}
	// Delete the draft and its attachments.
	paths, _ := svc.Store.DeleteDraftAttachments(ctx, p, d.ID)
	for _, path := range paths {
		_ = os.Remove(filepath.Join(svc.Config.DataDir, filepath.FromSlash(path)))
	}
	if err = svc.Store.DeleteDraft(ctx, p, d.ID); err != nil {
		t.Fatal(err)
	}
	// Draft is gone.
	if _, err = svc.Store.GetDraft(ctx, p, d.ID); !strings.Contains(err.Error(), "not found") {
		t.Fatalf("draft still present: %v", err)
	}
	// Deliver and confirm the attachment carried over.
	if err = svc.Deliver(ctx, u.AccountID, res.Message.ID); err != nil {
		t.Fatal(err)
	}
	msgAtts, err := svc.Store.ListAttachments(ctx, p, res.Message.ID)
	if err != nil || len(msgAtts) != 1 || msgAtts[0].Filename != "d.txt" {
		t.Fatalf("message attachments %v %#v", err, msgAtts)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
}
