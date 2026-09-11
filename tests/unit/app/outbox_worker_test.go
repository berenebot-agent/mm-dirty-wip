package app_test

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

	"gatehouse-mail/internal/app"
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
	seedSending(t, svc, u.AccountID, dom.ID, "brevo", map[string]any{"api_key": "k", "api_base": api.URL})
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{}}
	res, err := svc.Send(ctx, p, app.SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "Worker", Text: "hi"}, "")
	if err != nil {
		t.Fatal(err)
	}
	// app.Start the worker and wait for delivery.
	w := app.NewOutboxWorker(svc, nil)
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

// TestSaveSendingConfigRequeuesPending verifies that saving a domain's sending
// configuration resets pending messages (held for lack of a provider, or in
// retry backoff) so the worker delivers them immediately instead of waiting out
// the hold or backoff timer.
func TestSaveSendingConfigRequeuesPending(t *testing.T) {
	svc, u, dom, box := testService(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{}}

	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"messageId":"<requeued>"}`)
	}))
	defer api.Close()

	// A message that has already consumed a retry and is backing off for an hour.
	backingOff, err := svc.Send(ctx, p, app.SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "backoff", Text: "hi"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = svc.Store.MarkFailed(ctx, u.AccountID, backingOff.Message.ID, "down", time.Now().UTC().Add(time.Hour), 6, "brevo"); err != nil {
		t.Fatal(err)
	}
	// A message held because the domain has no sending provider.
	held, err := svc.Send(ctx, p, app.SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "held", Text: "hi"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Store.HoldPending(ctx, u.AccountID, held.Message.ID, "no provider", time.Now().UTC().Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}

	w := app.NewOutboxWorker(svc, nil)
	w.SetPeriod(10 * time.Millisecond)
	w.Start()
	defer w.Stop()

	// Neither message is due for at least five minutes, so the worker must not
	// deliver either one while the old hold/backoff stands.
	time.Sleep(300 * time.Millisecond)
	if n := calls.Load(); n != 0 {
		t.Fatalf("delivered before config save: calls=%d", n)
	}

	// Saving the sender resets both pending messages to retry immediately.
	seedSending(t, svc, u.AccountID, dom.ID, "brevo", map[string]any{"api_key": "k", "api_base": api.URL})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		a, err := svc.Store.GetMessageByID(ctx, u.AccountID, backingOff.Message.ID)
		if err != nil {
			t.Fatal(err)
		}
		b, err := svc.Store.GetMessageByID(ctx, u.AccountID, held.Message.ID)
		if err != nil {
			t.Fatal(err)
		}
		if a.Status == "sent" && b.Status == "sent" {
			if n := calls.Load(); n != 2 {
				t.Fatalf("calls=%d, want 2", n)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pending messages not delivered after config save; calls=%d", calls.Load())
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
	seedSending(t, svc, u.AccountID, dom.ID, "brevo", map[string]any{"api_key": "k", "api_base": api.URL})
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
	// app.Send the draft via the send-draft path (copy fields + attachments, enqueue, delete draft).
	atts, err := svc.Store.ListDraftAttachments(ctx, p, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	sendAtts := make([]app.SendAttachment, 0, len(atts))
	for _, a := range atts {
		data, rerr := os.ReadFile(filepath.Join(svc.Config.DataDir, filepath.FromSlash(a.RawPath)))
		if rerr != nil {
			t.Fatal(rerr)
		}
		sendAtts = append(sendAtts, app.SendAttachment{Filename: a.Filename, ContentType: a.ContentType, Content: data})
	}
	res, err := svc.Send(ctx, p, app.SendInput{InboxID: d.InboxID, To: d.To, Subject: d.Subject, Text: d.Text, Attachments: sendAtts}, "")
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
	// app.Deliver and confirm the attachment carried over.
	if err = svc.Deliver(ctx, u.AccountID, res.Message.ID, ""); err != nil {
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
