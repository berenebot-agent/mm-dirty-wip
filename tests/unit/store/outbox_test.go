package store_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
)

func TestOutboxEnqueueClaimMarkSent(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	rec := store.OutboundRecord{Inbox: box, Provider: "brevo", RFCMessageID: "<out@test>", From: model.Address{Address: box.Address}, To: []string{"x@y.test"}, Subject: "s", Text: "t", RawPath: "messages/o.eml", SizeBytes: 100}
	m, _, err := s.CommitOutbound(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != "pending" {
		t.Fatalf("status = %q, want pending", m.Status)
	}
	// Claim it.
	id, err := s.ClaimNextPending(ctx, time.Now().UTC(), "w1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if id != m.ID {
		t.Fatalf("claimed %q, want %q", id, m.ID)
	}
	// A second claim must find nothing (in-flight).
	id, err = s.ClaimNextPending(ctx, time.Now().UTC(), "w1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if id != "" {
		t.Fatalf("second claim got %q, want empty", id)
	}
	// Mark sent.
	sent, evs, err := s.MarkSent(ctx, u.AccountID, m.ID, "<provider-id>", "brevo")
	if err != nil {
		t.Fatal(err)
	}
	if sent.Status != "sent" || sent.ProviderMessageID != "<provider-id>" || len(evs) != 1 || evs[0].Type != "message.sent" {
		t.Fatalf("sent %+v ev %+v", sent, evs)
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
	rec := store.OutboundRecord{Inbox: box, Provider: "brevo", RFCMessageID: "<out@test>", From: model.Address{Address: box.Address}, To: []string{"x@y.test"}, Subject: "s", Text: "t", RawPath: "messages/o.eml", SizeBytes: 100}
	m, _, err := s.CommitOutbound(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	// Fail it (attempt 1 of 6 -> stays pending with a next_attempt_at).
	failed, _, err := s.MarkFailed(ctx, u.AccountID, m.ID, "provider down", time.Now().UTC().Add(time.Minute), 6, "brevo")
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != "pending" || failed.Attempts != 1 || failed.LastError != "provider down" {
		t.Fatalf("failed %+v", failed)
	}
	// Not due yet.
	id, err := s.ClaimNextPending(ctx, time.Now().UTC(), "w1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if id != "" {
		t.Fatalf("claimed not-due message %q", id)
	}
	// Due now.
	id, err = s.ClaimNextPending(ctx, time.Now().UTC().Add(2*time.Minute), "w1", time.Minute)
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
		if _, _, err = s.MarkFailed(ctx, u.AccountID, m.ID, "down", time.Now().UTC(), 6, "brevo"); err != nil {
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
	if err = s.RequeueFailed(ctx, ro, m.ID); !errors.Is(err, store.ErrForbidden) {
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

func TestClaimRecoveredAfterRestart(t *testing.T) {
	ctx := context.Background()
	s, _, _, b := testStore(t)
	box := b[0]
	m, _, err := s.CommitOutbound(ctx, store.OutboundRecord{Inbox: box, Provider: "brevo", RFCMessageID: "<r@test>",
		From: model.Address{Address: box.Address}, To: []string{"x@y.test"}, Subject: "s", Text: "t",
		RawPath: "messages/r.eml", SizeBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if id, err := s.ClaimNextPending(ctx, time.Now().UTC(), "w1", time.Hour); err != nil || id != m.ID {
		t.Fatalf("first claim id=%q err=%v", id, err)
	}
	// A long lease blocks a different worker.
	if id, err := s.ClaimNextPending(ctx, time.Now().UTC(), "w2", time.Hour); err != nil || id != "" {
		t.Fatalf("second claim id=%q err=%v, want empty", id, err)
	}
	// Startup recovery clears the abandoned claim so it can be delivered.
	if err := s.RecoverAbandonedClaims(ctx); err != nil {
		t.Fatal(err)
	}
	if id, err := s.ClaimNextPending(ctx, time.Now().UTC(), "w3", time.Hour); err != nil || id != m.ID {
		t.Fatalf("recovered claim id=%q err=%v", id, err)
	}
}

func TestConcurrentDeleteSubtractsOnce(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	m, _, _, err := s.CommitInbound(ctx, inbound(b[0], "del-race", "<race@test>", "", nil, "s", "body"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.GetAccount(ctx, u.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var successes, notFounds int32
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _, err := s.DeleteMessage(ctx, p, m.ID)
			switch {
			case err == nil:
				atomic.AddInt32(&successes, 1)
			case errors.Is(err, store.ErrNotFound):
				atomic.AddInt32(&notFounds, 1)
			default:
				t.Errorf("delete err %v", err)
			}
		}()
	}
	wg.Wait()
	if successes != 1 || notFounds != 1 {
		t.Fatalf("successes=%d notFounds=%d", successes, notFounds)
	}
	after, err := s.GetAccount(ctx, u.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if before.StorageUsedBytes-after.StorageUsedBytes != m.SizeBytes {
		t.Fatalf("storage delta %d, want %d", before.StorageUsedBytes-after.StorageUsedBytes, m.SizeBytes)
	}
}
