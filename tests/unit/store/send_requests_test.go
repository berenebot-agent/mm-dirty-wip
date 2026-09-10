package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
)

func assistant(b model.Inbox, accountID string) model.Principal {
	return model.Principal{AccountID: accountID, MailboxRoles: map[string]string{b.ID: "assistant"}}
}

func owner(b model.Inbox, accountID string) model.Principal {
	return model.Principal{AccountID: accountID, MailboxRoles: map[string]string{b.ID: "owner"}}
}

func TestSendRequestFreezeCancelReject(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	asst := assistant(box, u.AccountID)

	d, err := s.CreateDraft(ctx, asst, model.Draft{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "s", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != model.DraftStatusDraft {
		t.Fatalf("new draft status=%q", d.Status)
	}

	r, ev, err := s.CreateSendRequest(ctx, asst, d.ID, "hash-1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != model.SendRequestPending || ev.Type != model.EventDraftSendRequested {
		t.Fatalf("request=%+v ev=%+v", r, ev)
	}
	got, err := s.GetDraft(ctx, asst, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.DraftStatusPendingApproval {
		t.Fatalf("frozen status=%q", got.Status)
	}
	if got.SendRequest == nil || got.SendRequest.ID != r.ID {
		t.Fatalf("send request not attached: %+v", got.SendRequest)
	}

	// Frozen: edits and attachment changes are rejected.
	if _, err = s.UpdateDraft(ctx, asst, model.Draft{ID: d.ID, InboxID: box.ID, Subject: "changed"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("edit while pending err=%v", err)
	}
	if _, err = s.AddDraftAttachment(ctx, asst, d.ID, model.DraftAttachment{Filename: "a.txt", Size: 1, RawPath: "drafts/a"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("attach while pending err=%v", err)
	}
	// A second request for the same draft is rejected.
	if _, _, err = s.CreateSendRequest(ctx, asst, d.ID, "hash-2"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("double request err=%v", err)
	}

	// Cancel unfreezes.
	cr, cev, err := s.CancelSendRequest(ctx, asst, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cr.Status != model.SendRequestCancelled || cev.Type != model.EventDraftSendRequestCancelled {
		t.Fatalf("cancel=%+v ev=%+v", cr, cev)
	}
	got, _ = s.GetDraft(ctx, asst, d.ID)
	if got.Status != model.DraftStatusDraft {
		t.Fatalf("status after cancel=%q", got.Status)
	}
	if _, err = s.UpdateDraft(ctx, asst, model.Draft{ID: d.ID, InboxID: box.ID, To: []string{"x@y.test"}, Subject: "s2", Text: "body2"}); err != nil {
		t.Fatalf("edit after cancel: %v", err)
	}

	// Re-request then reject as owner.
	if _, _, err = s.CreateSendRequest(ctx, asst, d.ID, "hash-3"); err != nil {
		t.Fatal(err)
	}
	own := owner(box, u.AccountID)
	rr, rev, err := s.RejectSendRequest(ctx, own, d.ID, "pricing wrong", model.DecisionMethodAPI)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Status != model.SendRequestRejected || rr.Feedback != "pricing wrong" || rev.Type != model.EventDraftRejected {
		t.Fatalf("reject=%+v ev=%+v", rr, rev)
	}
	if rr.DecidedAt == nil || rr.DecisionActor == "" {
		t.Fatalf("decision metadata missing: %+v", rr)
	}
	got, _ = s.GetDraft(ctx, asst, d.ID)
	if got.Status != model.DraftStatusRejected {
		t.Fatalf("status after reject=%q", got.Status)
	}
	// Editing a rejected draft returns it to draft.
	edited, err := s.UpdateDraft(ctx, asst, model.Draft{ID: d.ID, InboxID: box.ID, To: []string{"x@y.test"}, Subject: "s3", Text: "body3"})
	if err != nil {
		t.Fatal(err)
	}
	if edited.Status != model.DraftStatusDraft {
		t.Fatalf("status after edit=%q", edited.Status)
	}
	// Assistant cannot reject.
	if _, _, err = s.CreateSendRequest(ctx, asst, d.ID, "hash-4"); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.RejectSendRequest(ctx, asst, d.ID, "no", model.DecisionMethodAPI); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("assistant reject err=%v", err)
	}
	// Read cannot request.
	read := model.Principal{AccountID: u.AccountID, MailboxRoles: map[string]string{box.ID: "read"}}
	if _, err = s.CreateDraft(ctx, read, model.Draft{InboxID: box.ID}); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("read create err=%v", err)
	}
}

func TestSendRequestApproveClaimsAndSurvivesConsumption(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	asst := assistant(box, u.AccountID)

	d, err := s.CreateDraft(ctx, asst, model.Draft{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "s", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := s.CreateSendRequest(ctx, asst, d.ID, "hash")
	if err != nil {
		t.Fatal(err)
	}
	rec := store.OutboundRecord{Inbox: box, Provider: "brevo", RFCMessageID: "<out@test>", From: model.Address{Address: box.Address}, To: []string{"x@y.test"}, Subject: "s", Text: "body", RawPath: "messages/o.eml", SizeBytes: 10, DraftID: d.ID, SendRequestID: r.ID, DecisionActor: "Ben", DecisionActorID: "usr_1", DecisionMethod: model.DecisionMethodAPI}
	m, ev, err := s.CommitOutbound(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != model.EventDraftApproved {
		t.Fatalf("approval event=%+v", ev)
	}
	// Draft consumed.
	if _, err = s.GetDraft(ctx, asst, d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("draft not consumed: %v", err)
	}
	// Request survives and links to the message.
	got, err := s.GetSendRequestByDraft(ctx, asst, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.SendRequestApproved || got.DeliveryStatus != model.SendDeliveryPending || got.MessageID != m.ID {
		t.Fatalf("request after approve=%+v", got)
	}
	if got.DecisionActor != "Ben" || got.DecisionMethod != model.DecisionMethodAPI {
		t.Fatalf("decision metadata=%+v", got)
	}

	// Delivery success updates the request and emits draft.sent.
	_, evs, err := s.MarkSent(ctx, u.AccountID, m.ID, "<provider>", "brevo")
	if err != nil {
		t.Fatal(err)
	}
	var sentEvent *model.Event
	for i := range evs {
		if evs[i].Type == model.EventDraftSent {
			sentEvent = &evs[i]
		}
	}
	if sentEvent == nil {
		t.Fatalf("no draft.sent event in %+v", evs)
	}
	got, _ = s.GetSendRequestByDraft(ctx, asst, d.ID)
	if got.DeliveryStatus != model.SendDeliverySent {
		t.Fatalf("delivery status=%q", got.DeliveryStatus)
	}
}

func TestSendRequestDeliveryFailureAndSingleWinner(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	asst := assistant(box, u.AccountID)

	d, err := s.CreateDraft(ctx, asst, model.Draft{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "s", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := s.CreateSendRequest(ctx, asst, d.ID, "hash")
	if err != nil {
		t.Fatal(err)
	}
	rec := store.OutboundRecord{Inbox: box, Provider: "brevo", RFCMessageID: "<out@test>", From: model.Address{Address: box.Address}, To: []string{"x@y.test"}, Subject: "s", Text: "body", RawPath: "messages/o.eml", SizeBytes: 10, DraftID: d.ID, SendRequestID: r.ID, DecisionActor: "Ben", DecisionMethod: model.DecisionMethodAPI}
	m, _, err := s.CommitOutbound(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	// Terminal failure emits draft.send_failed and records delivery_status.
	_, evs, err := s.MarkFailed(ctx, u.AccountID, m.ID, "permanent", time.Time{}, 1, "brevo")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range evs {
		if ev.Type == model.EventDraftSendFailed {
			found = true
		}
	}
	if !found {
		t.Fatalf("no draft.send_failed event in %+v", evs)
	}
	got, _ := s.GetSendRequestByDraft(ctx, asst, d.ID)
	if got.DeliveryStatus != model.SendDeliveryFailed {
		t.Fatalf("delivery status=%q", got.DeliveryStatus)
	}

	// A send cannot claim a request that was already decided: the transaction
	// rolls back and the draft must survive.
	d2, err := s.CreateDraft(ctx, asst, model.Draft{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "s", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	r2, _, err := s.CreateSendRequest(ctx, asst, d2.ID, "hash2")
	if err != nil {
		t.Fatal(err)
	}
	own := owner(box, u.AccountID)
	if _, _, err = s.RejectSendRequest(ctx, own, d2.ID, "no", model.DecisionMethodAPI); err != nil {
		t.Fatal(err)
	}
	rec2 := store.OutboundRecord{Inbox: box, Provider: "brevo", RFCMessageID: "<out2@test>", From: model.Address{Address: box.Address}, To: []string{"x@y.test"}, Subject: "s", Text: "body", RawPath: "messages/o.eml", SizeBytes: 10, DraftID: d2.ID, SendRequestID: r2.ID, DecisionActor: "Ben", DecisionMethod: model.DecisionMethodAPI}
	if _, _, err = s.CommitOutbound(ctx, rec2); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("claim of decided request err=%v", err)
	}
	// The failed claim rolled back: the draft is intact.
	if _, err = s.GetDraft(ctx, asst, d2.ID); err != nil {
		t.Fatalf("draft lost on failed claim: %v", err)
	}
}
