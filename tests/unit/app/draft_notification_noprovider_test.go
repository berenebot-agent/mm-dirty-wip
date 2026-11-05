package app_test

import (
	"context"
	"testing"

	"github.com/dellarb/mammothmail/internal/model"
)

// TestApprovalNotificationFailsWithoutProvider covers a standalone inbox in
// local_approval mode with an approver but no SMTP binding. The approval email
// can never be sent, so the request must be reported failed (with a
// draft.notification_failed event) rather than sit at 'queued' indefinitely.
func TestApprovalNotificationFailsWithoutProvider(t *testing.T) {
	t.Parallel()
	svc, u, box, _ := remoteTestEnv(t)
	ctx := context.Background()
	owner := model.Principal{AccountID: u.AccountID, UserID: u.ID, Admin: true, MailboxRoles: map[string]string{box.ID: "owner"}}
	if err := svc.Store.SetInboxAuthoringMode(ctx, owner, box.ID, model.AuthoringLocalApproval); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SetInboxApprover(ctx, u.AccountID, box.ID, approverAddress); err != nil {
		t.Fatal(err)
	}
	asst := assistantPrincipal(u.AccountID, box.ID)
	d, err := svc.Store.CreateDraft(ctx, asst, model.Draft{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "s", Text: "b"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.RequestSend(ctx, asst, d.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.SendRequest == nil || got.SendRequest.NotificationStatus != model.NotificationQueued {
		t.Fatalf("request after commit = %+v, want queued", got.SendRequest)
	}
	w := queuedApprovalWorkflow(t, svc, ctx, u.AccountID, box.ID)
	if err := svc.DeliverWorkflow(ctx, u.AccountID, w.ID, ""); err != nil {
		t.Fatalf("deliver workflow: %v", err)
	}
	after, err := svc.Store.GetDraft(ctx, asst, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.SendRequest == nil || after.SendRequest.NotificationStatus != model.NotificationFailed {
		t.Fatalf("notification status = %+v, want failed", after.SendRequest)
	}
	evs, err := svc.Store.ListEvents(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, 0, box.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range evs {
		if ev.Type == model.EventDraftNotificationFailed {
			found = true
		}
	}
	if !found {
		t.Fatalf("no %s event among %d events", model.EventDraftNotificationFailed, len(evs))
	}
}

// TestEnsureRemoteFolderAcceptsExisting proves the role-mapping create path
// treats a folder the server already has as success (some servers answer CREATE
// of an existing folder with a bare NO), while the plain folder create keeps its
// conflict.
func TestEnsureRemoteFolderAcceptsExisting(t *testing.T) {
	t.Parallel()
	_, u, box, rm := remoteTestEnv(t)
	ctx := context.Background()
	configureSecrets(t, rm, u, box, "imap-pw", "")
	fake := newFakeRemoteServer()
	installFake(t, rm, fake)
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, MailboxRoles: map[string]string{box.ID: "owner"}, Admin: true}
	if _, err := rm.CreateRemoteFolder(ctx, p, box.ID, "Drafts", ""); err == nil {
		t.Fatal("plain create of an existing folder should still fail")
	}
	f, err := rm.EnsureRemoteFolder(ctx, p, box.ID, "Drafts", "")
	if err != nil {
		t.Fatalf("ensure existing folder: %v", err)
	}
	if f.Path != "Drafts" {
		t.Fatalf("path = %q, want Drafts", f.Path)
	}
}
