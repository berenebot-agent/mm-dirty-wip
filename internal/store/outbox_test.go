package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"gatehouse-mail/internal/model"
)

func TestOutboxEnqueueClaimMarkSent(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	rec := OutboundRecord{Inbox: box, Provider: "brevo", RFCMessageID: "<out@test>", From: model.Address{Address: box.Address}, To: []string{"x@y.test"}, Subject: "s", Text: "t", RawPath: "messages/o.eml", SizeBytes: 100}
	m, _, err := s.CommitOutbound(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != "pending" {
		t.Fatalf("status = %q, want pending", m.Status)
	}
	// Claim it.
	id, err := s.ClaimNextPending(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if id != m.ID {
		t.Fatalf("claimed %q, want %q", id, m.ID)
	}
	// A second claim must find nothing (in-flight).
	id, err = s.ClaimNextPending(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if id != "" {
		t.Fatalf("second claim got %q, want empty", id)
	}
	// Mark sent.
	sent, ev, err := s.MarkSent(ctx, u.AccountID, m.ID, "<provider-id>", "", "brevo")
	if err != nil {
		t.Fatal(err)
	}
	if sent.Status != "sent" || sent.ProviderMessageID != "<provider-id>" || ev.Type != "message.sent" {
		t.Fatalf("sent %+v ev %+v", sent, ev)
	}
	// No longer in outbox.
	out, err := s.ListOutbox(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("outbox after sent = %#v", out)
	}
}

func TestOutboxMarkFailedAndRetry(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	rec := OutboundRecord{Inbox: box, Provider: "brevo", RFCMessageID: "<out@test>", From: model.Address{Address: box.Address}, To: []string{"x@y.test"}, Subject: "s", Text: "t", RawPath: "messages/o.eml", SizeBytes: 100}
	m, _, err := s.CommitOutbound(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	// Fail it (attempt 1 of 6 -> stays pending with a next_attempt_at).
	failed, err := s.MarkFailed(ctx, u.AccountID, m.ID, "provider down", time.Now().UTC().Add(time.Minute), 6, "", "brevo")
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != "pending" || failed.Attempts != 1 || failed.LastError != "provider down" {
		t.Fatalf("failed %+v", failed)
	}
	// Not due yet.
	id, err := s.ClaimNextPending(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if id != "" {
		t.Fatalf("claimed not-due message %q", id)
	}
	// Due now.
	id, err = s.ClaimNextPending(ctx, time.Now().UTC().Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if id != m.ID {
		t.Fatalf("claimed %q, want %q", id, m.ID)
	}
	// Exhaust attempts -> failed.
	for i := 0; i < 6; i++ {
		cur, err := s.GetMessageByID(ctx, u.AccountID, m.ID)
		if err != nil {
			t.Fatal(err)
		}
		if cur.Status == "failed" {
			break
		}
		if _, err = s.MarkFailed(ctx, u.AccountID, m.ID, "down", time.Now().UTC(), 6, "", "brevo"); err != nil {
			t.Fatal(err)
		}
	}
	cur, err := s.GetMessageByID(ctx, u.AccountID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Status != "failed" {
		t.Fatalf("status = %q, want failed", cur.Status)
	}
	// Non-owner cannot retry a failed message.
	ro := model.Principal{AccountID: u.AccountID, MailboxRoles: map[string]string{box.ID: "read"}}
	if err = s.RequeueFailed(ctx, ro, m.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("read retry err=%v", err)
	}
	// Owner retry resets to pending.
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{box.ID: "owner"}}
	if err = s.RequeueFailed(ctx, p, m.ID); err != nil {
		t.Fatal(err)
	}
	cur, err = s.GetMessageByID(ctx, u.AccountID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Status != "pending" || cur.Attempts != 0 {
		t.Fatalf("after retry %+v", cur)
	}
}

func TestDraftAttachmentsCRUD(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{box.ID: "owner"}}
	d, err := s.CreateDraft(ctx, p, model.Draft{InboxID: box.ID, Subject: "draft"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.AddDraftAttachment(ctx, p, d.ID, model.DraftAttachment{Filename: "a.txt", ContentType: "text/plain", Size: 5, RawPath: "drafts/a.bin"})
	if err != nil {
		t.Fatal(err)
	}
	atts, err := s.ListDraftAttachments(ctx, p, d.ID)
	if err != nil || len(atts) != 1 || atts[0].Filename != "a.txt" {
		t.Fatalf("list %v %#v", err, atts)
	}
	// Delete all.
	paths, err := s.DeleteDraftAttachments(ctx, p, d.ID)
	if err != nil || len(paths) != 1 || paths[0] != "drafts/a.bin" {
		t.Fatalf("delete all %v %#v", err, paths)
	}
	atts, err = s.ListDraftAttachments(ctx, p, d.ID)
	if err != nil || len(atts) != 0 {
		t.Fatalf("after delete %v %#v", err, atts)
	}
	_ = a
}
