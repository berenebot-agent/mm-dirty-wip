package app_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
)

func assistantPrincipal(accountID, inboxID string) model.Principal {
	return model.Principal{AccountID: accountID, MailboxRoles: map[string]string{inboxID: "assistant"}}
}

func ownerPrincipal(accountID, inboxID string) model.Principal {
	return model.Principal{AccountID: accountID, MailboxRoles: map[string]string{inboxID: "owner"}}
}

func TestRequestSendValidationAndFreeze(t *testing.T) {
	svc, u, _, box := testService(t)
	ctx := context.Background()
	asst := assistantPrincipal(u.AccountID, box.ID)

	// A draft with no recipient cannot be requested.
	empty, err := svc.Store.CreateDraft(ctx, asst, model.Draft{InboxID: box.ID, Subject: "s", Text: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.RequestSend(ctx, asst, empty.ID); err == nil {
		t.Fatal("request-send accepted a draft with no recipient")
	}

	// A valid draft is frozen while pending and unfrozen on cancel.
	draft, err := svc.Store.CreateDraft(ctx, asst, model.Draft{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "s", Text: "b"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.RequestSend(ctx, asst, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.DraftStatusPendingApproval || got.SendRequest == nil {
		t.Fatalf("after request %+v", got)
	}
	if _, err = svc.Store.UpdateDraft(ctx, asst, model.Draft{ID: draft.ID, InboxID: box.ID, To: []string{"x@y.test"}, Subject: "changed", Text: "b"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("edit while pending err=%v", err)
	}
	if _, err = svc.CancelSendRequest(ctx, asst, draft.ID); err != nil {
		t.Fatal(err)
	}
	got, err = svc.Store.GetDraft(ctx, asst, draft.ID)
	if err != nil || got.Status != model.DraftStatusDraft {
		t.Fatalf("after cancel %+v err=%v", got, err)
	}
}

func TestApproveDraftSendsStoredContent(t *testing.T) {
	svc, u, _, box := testService(t)
	ctx := context.Background()
	admin := model.Principal{AccountID: u.AccountID, Admin: true}
	asst := assistantPrincipal(u.AccountID, box.ID)

	draft, err := svc.Store.CreateDraft(ctx, asst, model.Draft{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "stored subject", Text: "stored body"})
	if err != nil {
		t.Fatal(err)
	}
	rawRel := "drafts/approve-att.bin"
	rawPath := filepath.Join(svc.Config.DataDir, filepath.FromSlash(rawRel))
	if err = os.MkdirAll(filepath.Dir(rawPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(rawPath, []byte("attdata"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Store.AddDraftAttachment(ctx, asst, draft.ID, model.DraftAttachment{Filename: "a.txt", ContentType: "text/plain", Size: int64(len("attdata")), RawPath: rawRel}); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.RequestSend(ctx, asst, draft.ID); err != nil {
		t.Fatal(err)
	}

	res, err := svc.ApproveDraft(ctx, admin, draft.ID, "", model.DecisionMethodUI, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Message.Subject != "stored subject" || res.Message.Text != "stored body" {
		t.Fatalf("approved message used wrong content: %+v", res.Message)
	}
	atts, err := svc.Store.ListAttachments(ctx, admin, res.Message.ID)
	if err != nil || len(atts) != 1 || atts[0].Filename != "a.txt" {
		t.Fatalf("attachments %v %#v", err, atts)
	}
	if _, err = svc.Store.GetDraft(ctx, asst, draft.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("draft not consumed: %v", err)
	}
	sr, err := svc.Store.GetSendRequestByDraft(ctx, asst, draft.ID)
	if err != nil || sr.Status != model.SendRequestApproved || sr.MessageID != res.Message.ID {
		t.Fatalf("send request %+v err=%v", sr, err)
	}
	if sr.DecisionMethod != model.DecisionMethodUI || sr.DecisionActor == "" {
		t.Fatalf("decision metadata %+v", sr)
	}
	// A second approval cannot succeed.
	if _, err = svc.ApproveDraft(ctx, admin, draft.ID, "", model.DecisionMethodUI, ""); err == nil {
		t.Fatal("second approval succeeded")
	}
}

func TestApproveDraftRejectsTamperedFingerprint(t *testing.T) {
	svc, u, _, box := testService(t)
	ctx := context.Background()
	admin := model.Principal{AccountID: u.AccountID, Admin: true}
	asst := assistantPrincipal(u.AccountID, box.ID)

	draft, err := svc.Store.CreateDraft(ctx, asst, model.Draft{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "s", Text: "b"})
	if err != nil {
		t.Fatal(err)
	}
	// Force a request with a fingerprint that does not match the draft.
	if _, _, err = svc.Store.CreateSendRequest(ctx, asst, draft.ID, "not-the-real-hash"); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.ApproveDraft(ctx, admin, draft.ID, "", model.DecisionMethodUI, ""); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("tampered approve err=%v", err)
	}
}

func TestRejectDraftRevisionFlow(t *testing.T) {
	svc, u, _, box := testService(t)
	ctx := context.Background()
	admin := model.Principal{AccountID: u.AccountID, Admin: true}
	asst := assistantPrincipal(u.AccountID, box.ID)

	draft, err := svc.Store.CreateDraft(ctx, asst, model.Draft{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "s", Text: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.RequestSend(ctx, asst, draft.ID); err != nil {
		t.Fatal(err)
	}
	rejected, err := svc.RejectDraft(ctx, admin, draft.ID, "fix pricing", model.DecisionMethodUI)
	if err != nil {
		t.Fatal(err)
	}
	if rejected.Status != model.DraftStatusRejected || rejected.SendRequest == nil || rejected.SendRequest.Feedback != "fix pricing" {
		t.Fatalf("rejected %+v", rejected)
	}
	// Revise and resubmit.
	edited, err := svc.Store.UpdateDraft(ctx, asst, model.Draft{ID: draft.ID, InboxID: box.ID, To: []string{"x@y.test"}, Subject: "s2", Text: "b2"})
	if err != nil || edited.Status != model.DraftStatusDraft {
		t.Fatalf("revise %+v err=%v", edited, err)
	}
	resubmitted, err := svc.RequestSend(ctx, asst, draft.ID)
	if err != nil || resubmitted.Status != model.DraftStatusPendingApproval {
		t.Fatalf("resubmit %+v err=%v", resubmitted, err)
	}
}
