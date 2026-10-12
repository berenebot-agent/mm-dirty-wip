package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/imap"
)

func TestActionQueueOrderingRecoveryAndScope(t *testing.T) {
	svc, u, box, _ := remoteTestEnv(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	read := true
	a, err := svc.Store.EnqueueMessageAction(ctx, p, box.ID, "message", store.MessageActionInput{Kind: "patch", Read: &read})
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.Store.EnqueueMessageAction(ctx, p, box.ID, "message", store.MessageActionInput{Kind: "delete"})
	if err != nil {
		t.Fatal(err)
	}
	first, ok, err := svc.Store.ClaimMessageAction(ctx, "test")
	if err != nil || !ok || first.ID != a.ID {
		t.Fatalf("first claim: %+v %v %v", first, ok, err)
	}
	if _, ok, err := svc.Store.ClaimMessageAction(ctx, "test"); err != nil || ok {
		t.Fatalf("successor passed running action: %v %v", ok, err)
	}
	if err := svc.Store.RecoverMessageActions(ctx); err != nil {
		t.Fatal(err)
	}
	first, ok, err = svc.Store.ClaimMessageAction(ctx, "test")
	if err != nil || !ok || first.ID != a.ID {
		t.Fatal("safe patch was not recovered")
	}
	if err := svc.Store.SettleMessageAction(ctx, a.ID, "done", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	second, ok, err := svc.Store.ClaimMessageAction(ctx, "test")
	if err != nil || !ok || second.ID != b.ID {
		t.Fatalf("second claim: %+v %v", second, err)
	}
	if err := svc.Store.RecoverMessageActions(ctx); err != nil {
		t.Fatal(err)
	}
	second, err = svc.Store.GetMessageAction(ctx, p, b.ID)
	if err != nil || second.State != "ambiguous" {
		t.Fatalf("move crash replayed: %+v %v", second, err)
	}
	if err := svc.Store.UpdateMessageAction(ctx, p, b.ID, true); err != store.ErrConflict {
		t.Fatalf("ambiguous retry: %v", err)
	}
	if _, err := svc.Store.GetMessageAction(ctx, model.Principal{AccountID: "another", Admin: true}, a.ID); err != store.ErrNotFound {
		t.Fatalf("cross account action: %v", err)
	}
}

func TestActionWorkerRemoteReadThenUnread(t *testing.T) {
	svc, u, box, rm := remoteTestEnv(t)
	configureSecrets(t, rm, u, box, "pw", "")
	fake := newFakeRemoteServer()
	fake.addMessage("INBOX", "body", "<actions@test>", "Actions")
	rm.SetRemoteDialer(func(context.Context, imap.Config) (app.RemoteSession, error) { return fake, nil })
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	res, err := rm.ListRemoteMessagesCached(ctx, p, box.ID, "INBOX", 10, "")
	if err != nil || len(res.Items) != 1 {
		t.Fatalf("messages: %+v %v", res, err)
	}
	read, unread := true, false
	a, err := svc.EnqueueInboxAction(ctx, p, box.ID, res.Items[0].ID, store.MessageActionInput{Kind: "patch", Read: &read})
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.EnqueueInboxAction(ctx, p, box.ID, res.Items[0].ID, store.MessageActionInput{Kind: "patch", Read: &unread})
	if err != nil {
		t.Fatal(err)
	}
	w := app.NewActionWorker(svc, rm)
	svc.ActionWaker = w
	w.Start()
	defer w.Stop()
	for _, id := range []string{a.ID, b.ID} {
		settled, err := svc.WaitForInboxAction(ctx, p, id, 3*time.Second)
		if err != nil || settled.State != "done" {
			t.Fatalf("action: %+v %v", settled, err)
		}
	}
	m, err := svc.Store.GetRemoteMessage(ctx, p.AccountID, box.ID, res.Items[0].ID)
	if err != nil || m.Read {
		t.Fatalf("out of order read state: %+v %v", m, err)
	}
}

// TestRecheckFailedRetriesPatchAndSupersedesStaleAction covers the two rules that
// make an unbounded recheck safe for absolute-state patches: a failed patch is
// retried, but not when a newer unsettled action for the same message exists.
func TestRecheckFailedRetriesPatchAndSupersedesStaleAction(t *testing.T) {
	svc, u, box, rm := remoteTestEnv(t)
	configureSecrets(t, rm, u, box, "pw", "")
	fake := newFakeRemoteServer()
	fake.addMessage("INBOX", "body", "<recheck@test>", "Recheck")
	rm.SetRemoteDialer(func(context.Context, imap.Config) (app.RemoteSession, error) { return fake, nil })
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	res, err := rm.ListRemoteMessagesCached(ctx, p, box.ID, "INBOX", 10, "")
	if err != nil || len(res.Items) != 1 {
		t.Fatalf("messages: %+v %v", res, err)
	}
	messageID := res.Items[0].ID
	read := true
	failed, err := svc.Store.EnqueueMessageAction(ctx, p, box.ID, messageID, store.MessageActionInput{Kind: "patch", Read: &read})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := svc.Store.ClaimMessageAction(ctx, "test"); err != nil || !ok {
		t.Fatalf("claim failed patch: %v %v", ok, err)
	}
	if err := svc.Store.SettleMessageAction(ctx, failed.ID, "failed", "provider blip", time.Now()); err != nil {
		t.Fatal(err)
	}
	makeRecheckDue(t, svc, failed.ID)
	w := app.NewActionWorker(svc, rm)
	w.RecheckFailed()
	got, err := svc.Store.GetMessageAction(ctx, p, failed.ID)
	if err != nil || got.State != "pending" {
		t.Fatalf("failed patch not requeued: %+v %v", got, err)
	}

	// With a newer unsettled action the stale failure must stay put so it cannot
	// be replayed out of order.
	svc2, u2, box2, rm2 := remoteTestEnv(t)
	configureSecrets(t, rm2, u2, box2, "pw", "")
	fake2 := newFakeRemoteServer()
	fake2.addMessage("INBOX", "body", "<stale@test>", "Stale")
	rm2.SetRemoteDialer(func(context.Context, imap.Config) (app.RemoteSession, error) { return fake2, nil })
	p2 := model.Principal{AccountID: u2.AccountID, Admin: true}
	if _, err := rm2.ReconcileRemote(ctx, u2.AccountID, box2.ID); err != nil {
		t.Fatal(err)
	}
	res2, err := rm2.ListRemoteMessagesCached(ctx, p2, box2.ID, "INBOX", 10, "")
	if err != nil || len(res2.Items) != 1 {
		t.Fatalf("messages: %+v %v", res2, err)
	}
	mid2 := res2.Items[0].ID
	stale, err := svc2.Store.EnqueueMessageAction(ctx, p2, box2.ID, mid2, store.MessageActionInput{Kind: "patch", Read: &read})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := svc2.Store.ClaimMessageAction(ctx, "test"); err != nil || !ok {
		t.Fatalf("claim stale: %v %v", ok, err)
	}
	if err := svc2.Store.SettleMessageAction(ctx, stale.ID, "failed", "provider blip", time.Now()); err != nil {
		t.Fatal(err)
	}
	makeRecheckDue(t, svc2, stale.ID)
	unread := false
	if _, err := svc2.Store.EnqueueMessageAction(ctx, p2, box2.ID, mid2, store.MessageActionInput{Kind: "patch", Read: &unread}); err != nil {
		t.Fatal(err)
	}
	w2 := app.NewActionWorker(svc2, rm2)
	w2.RecheckFailed()
	kept, err := svc2.Store.GetMessageAction(ctx, p2, stale.ID)
	if err != nil || kept.State != "failed" {
		t.Fatalf("stale action requeued past newer action: %+v %v", kept, err)
	}
}

// TestRecheckFailedRespectsRemoteLocation covers a location-changing action: an
// effect already visible at the provider settles as done, while one still in the
// source folder is safe to retry. An ambiguous row is never reconsidered.
func TestRecheckFailedRespectsRemoteLocation(t *testing.T) {
	svc, u, box, rm := remoteTestEnv(t)
	configureSecrets(t, rm, u, box, "pw", "")
	fake := newFakeRemoteServer()
	fake.addMessage("INBOX", "body", "<move@test>", "Move")
	rm.SetRemoteDialer(func(context.Context, imap.Config) (app.RemoteSession, error) { return fake, nil })
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	res, err := rm.ListRemoteMessagesCached(ctx, p, box.ID, "INBOX", 10, "")
	if err != nil || len(res.Items) != 1 {
		t.Fatalf("messages: %+v %v", res, err)
	}
	messageID := res.Items[0].ID

	// Already applied: the cached location is the destination folder.
	applied, err := svc.Store.EnqueueMessageAction(ctx, p, box.ID, messageID, store.MessageActionInput{Kind: "move", Destination: "Drafts"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := svc.Store.ClaimMessageAction(ctx, "test"); err != nil || !ok {
		t.Fatalf("claim applied: %v %v", ok, err)
	}
	if err := svc.Store.SettleMessageAction(ctx, applied.ID, "failed", "timed out", time.Now()); err != nil {
		t.Fatal(err)
	}
	makeRecheckDue(t, svc, applied.ID)
	if _, err := rm.MoveRemoteMessage(ctx, p, box.ID, messageID, "Drafts"); err != nil {
		t.Fatalf("pre-apply move: %v", err)
	}
	w := app.NewActionWorker(svc, rm)
	w.RecheckFailed()
	settled, err := svc.Store.GetMessageAction(ctx, p, applied.ID)
	if err != nil || settled.State != "done" {
		t.Fatalf("already-applied move not settled: %+v %v", settled, err)
	}
}

// makeRecheckDue ages a failed action so the next recheck pass treats it as due
// without the test waiting out a real ten-minute interval.
func makeRecheckDue(t *testing.T, svc *app.Service, id string) {
	t.Helper()
	if err := svc.Store.DeferMessageActionRecheck(context.Background(), id, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
}

// TestRecheckEndToEndRetriesFailedPatchThroughWorker proves the rechecker only
// requeues, and the main worker still performs the mutation, so a transient
// failure eventually takes effect without a human. It also proves an ambiguous
// row is left alone.
func TestRecheckEndToEndRetriesFailedPatchThroughWorker(t *testing.T) {
	svc, u, box, rm := remoteTestEnv(t)
	configureSecrets(t, rm, u, box, "pw", "")
	fake := newFakeRemoteServer()
	fake.addMessage("INBOX", "body", "<e2e@test>", "E2E")
	rm.SetRemoteDialer(func(context.Context, imap.Config) (app.RemoteSession, error) { return fake, nil })
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	res, err := rm.ListRemoteMessagesCached(ctx, p, box.ID, "INBOX", 10, "")
	if err != nil || len(res.Items) != 1 {
		t.Fatalf("messages: %+v %v", res, err)
	}
	messageID := res.Items[0].ID
	read := true
	failed, err := svc.Store.EnqueueMessageAction(ctx, p, box.ID, messageID, store.MessageActionInput{Kind: "patch", Read: &read})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := svc.Store.ClaimMessageAction(ctx, "test"); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if err := svc.Store.SettleMessageAction(ctx, failed.ID, "failed", "transient", time.Now()); err != nil {
		t.Fatal(err)
	}
	// An ambiguous action on another message must never be reconsidered.
	fake.addMessage("INBOX", "second", "<e2e2@test>", "E2E2")
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	res2, err := rm.ListRemoteMessagesCached(ctx, p, box.ID, "INBOX", 10, "")
	if err != nil || len(res2.Items) != 2 {
		t.Fatalf("messages: %+v %v", res2, err)
	}
	amb, err := svc.Store.EnqueueMessageAction(ctx, p, box.ID, res2.Items[0].ID, store.MessageActionInput{Kind: "patch", Read: &read})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SettleMessageAction(ctx, amb.ID, "ambiguous", "uncertain", time.Now()); err != nil {
		t.Fatal(err)
	}
	makeRecheckDue(t, svc, failed.ID)
	w := app.NewActionWorker(svc, rm)
	svc.ActionWaker = w
	w.Start()
	defer w.Stop()
	w.RecheckFailed()
	settled, err := svc.WaitForInboxAction(ctx, p, failed.ID, 3*time.Second)
	if err != nil || settled.State != "done" {
		t.Fatalf("requeued patch not executed: %+v %v", settled, err)
	}
	m, err := svc.Store.GetRemoteMessage(ctx, u.AccountID, box.ID, messageID)
	if err != nil || !m.Read {
		t.Fatalf("patch not applied remotely: %+v %v", m, err)
	}
	still, err := svc.Store.GetMessageAction(ctx, p, amb.ID)
	if err != nil || still.State != "ambiguous" {
		t.Fatalf("ambiguous row reconsidered: %+v %v", still, err)
	}
}

// TestRecheckFailedSettlesAppliedPurgeAndRetriesPendingPurge covers a purge's
// absence-based outcome: a purge whose message is already gone settles as done,
// while one whose message still exists is retried.
func TestRecheckFailedSettlesAppliedPurgeAndRetriesPendingPurge(t *testing.T) {
	svc, u, box, rm := remoteTestEnv(t)
	configureSecrets(t, rm, u, box, "pw", "")
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}

	// Already-applied purge: the remote row is gone.
	gone, err := svc.Store.EnqueueMessageAction(ctx, p, box.ID, "rm_missing_purge", store.MessageActionInput{Kind: "purge"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := svc.Store.ClaimMessageAction(ctx, "test"); err != nil || !ok {
		t.Fatalf("claim gone: %v %v", ok, err)
	}
	if err := svc.Store.SettleMessageAction(ctx, gone.ID, "failed", "transient", time.Now()); err != nil {
		t.Fatal(err)
	}
	makeRecheckDue(t, svc, gone.ID)
	w := app.NewActionWorker(svc, rm)
	w.RecheckFailed()
	settled, err := svc.Store.GetMessageAction(ctx, p, gone.ID)
	if err != nil || settled.State != "done" {
		t.Fatalf("applied purge not settled: %+v %v", settled, err)
	}

	// Pending purge: the remote row still exists in Trash, so it is retried.
	fake := newFakeRemoteServer()
	fake.addFolder("Trash", 100)
	fake.addMessage("Trash", "body", "<purge@test>", "Purge")
	rm.SetRemoteDialer(func(context.Context, imap.Config) (app.RemoteSession, error) { return fake, nil })
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	res, err := rm.ListRemoteMessagesCached(ctx, p, box.ID, "Trash", 10, "")
	if err != nil || len(res.Items) != 1 {
		t.Fatalf("trash messages: %+v %v", res, err)
	}
	pending, err := svc.Store.EnqueueMessageAction(ctx, p, box.ID, res.Items[0].ID, store.MessageActionInput{Kind: "purge"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := svc.Store.ClaimMessageAction(ctx, "test"); err != nil || !ok {
		t.Fatalf("claim pending: %v %v", ok, err)
	}
	if err := svc.Store.SettleMessageAction(ctx, pending.ID, "failed", "transient", time.Now()); err != nil {
		t.Fatal(err)
	}
	makeRecheckDue(t, svc, pending.ID)
	w.RecheckFailed()
	retried, err := svc.Store.GetMessageAction(ctx, p, pending.ID)
	if err != nil || retried.State != "pending" {
		t.Fatalf("pending purge not retried: %+v %v", retried, err)
	}
}

// TestRecheckFailedDoesNotOverrideCompletedNewerAction covers the case the old
// supersede check missed: a newer action that has already completed must stop a
// stale failure from being replayed, or the replay would silently undo the
// user's later instruction.
func TestRecheckFailedDoesNotOverrideCompletedNewerAction(t *testing.T) {
	svc, u, box, rm := remoteTestEnv(t)
	configureSecrets(t, rm, u, box, "pw", "")
	fake := newFakeRemoteServer()
	fake.addMessage("INBOX", "body", "<completed@test>", "Completed")
	rm.SetRemoteDialer(func(context.Context, imap.Config) (app.RemoteSession, error) { return fake, nil })
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	res, err := rm.ListRemoteMessagesCached(ctx, p, box.ID, "INBOX", 10, "")
	if err != nil || len(res.Items) != 1 {
		t.Fatalf("messages: %+v %v", res, err)
	}
	messageID := res.Items[0].ID

	// 1. An agent marks the message read, but the action fails.
	read := true
	stale, err := svc.Store.EnqueueMessageAction(ctx, p, box.ID, messageID, store.MessageActionInput{Kind: "patch", Read: &read})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := svc.Store.ClaimMessageAction(ctx, "test"); err != nil || !ok {
		t.Fatalf("claim stale: %v %v", ok, err)
	}
	if err := svc.Store.SettleMessageAction(ctx, stale.ID, "failed", "provider blip", time.Now()); err != nil {
		t.Fatal(err)
	}
	makeRecheckDue(t, svc, stale.ID)

	// 2. The user marks it unread, and that action completes.
	unread := false
	newer, err := svc.Store.EnqueueMessageAction(ctx, p, box.ID, messageID, store.MessageActionInput{Kind: "patch", Read: &unread})
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := svc.Store.ClaimMessageAction(ctx, "test")
	if err != nil || !ok || claimed.ID != newer.ID {
		t.Fatalf("claim newer: %+v %v %v", claimed, ok, err)
	}
	if err := svc.Store.SettleMessageAction(ctx, newer.ID, "done", "", time.Now()); err != nil {
		t.Fatal(err)
	}

	// 3. The stale failure must not be replayed over the completed newer action.
	w := app.NewActionWorker(svc, rm)
	w.RecheckFailed()
	kept, err := svc.Store.GetMessageAction(ctx, p, stale.ID)
	if err != nil || kept.State != "failed" {
		t.Fatalf("stale action replayed past a completed newer action: %+v %v", kept, err)
	}
}

// TestRecheckFailedOlderActionDoesNotSupersede proves ordering is directional: a
// failed action is not blocked by an action enqueued before it.
func TestRecheckFailedOlderActionDoesNotSupersede(t *testing.T) {
	svc, u, box, rm := remoteTestEnv(t)
	configureSecrets(t, rm, u, box, "pw", "")
	fake := newFakeRemoteServer()
	fake.addMessage("INBOX", "body", "<ordering@test>", "Ordering")
	rm.SetRemoteDialer(func(context.Context, imap.Config) (app.RemoteSession, error) { return fake, nil })
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	res, err := rm.ListRemoteMessagesCached(ctx, p, box.ID, "INBOX", 10, "")
	if err != nil || len(res.Items) != 1 {
		t.Fatalf("messages: %+v %v", res, err)
	}
	messageID := res.Items[0].ID
	read := true

	// An older, already-completed action.
	older, err := svc.Store.EnqueueMessageAction(ctx, p, box.ID, messageID, store.MessageActionInput{Kind: "patch", Read: &read})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := svc.Store.ClaimMessageAction(ctx, "test"); err != nil || !ok {
		t.Fatalf("claim older: %v %v", ok, err)
	}
	if err := svc.Store.SettleMessageAction(ctx, older.ID, "done", "", time.Now()); err != nil {
		t.Fatal(err)
	}

	// A later action fails, with nothing newer after it.
	unread := false
	failed, err := svc.Store.EnqueueMessageAction(ctx, p, box.ID, messageID, store.MessageActionInput{Kind: "patch", Read: &unread})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := svc.Store.ClaimMessageAction(ctx, "test"); err != nil || !ok {
		t.Fatalf("claim failed: %v %v", ok, err)
	}
	if err := svc.Store.SettleMessageAction(ctx, failed.ID, "failed", "provider blip", time.Now()); err != nil {
		t.Fatal(err)
	}
	makeRecheckDue(t, svc, failed.ID)

	w := app.NewActionWorker(svc, rm)
	w.RecheckFailed()
	got, err := svc.Store.GetMessageAction(ctx, p, failed.ID)
	if err != nil || got.State != "pending" {
		t.Fatalf("failed action blocked by an older action: %+v %v", got, err)
	}
}

// TestRecheckSpamPatchIsDecidedByProviderState covers a spam patch against a
// standalone mailbox: it is a physical folder move at the provider, so it must be
// resolved from the provider's current state, never replayed blindly. A spam
// patch whose effect is already visible settles as done instead of moving again.
func TestRecheckSpamPatchIsDecidedByProviderState(t *testing.T) {
	svc, u, box, rm := remoteTestEnv(t)
	configureSecrets(t, rm, u, box, "pw", "")
	fake := newFakeRemoteServer()
	fake.addFolder("Spam", 100)
	fake.addMessage("INBOX", "body", "<spam@test>", "Spam")
	rm.SetRemoteDialer(func(context.Context, imap.Config) (app.RemoteSession, error) { return fake, nil })
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	// Map the Spam role to the indexed Spam folder so destinationRole resolves.
	folders, err := svc.Store.ListFolders(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	spamFolderID := ""
	for _, f := range folders {
		if f.Path == "Spam" {
			spamFolderID = f.ID
		}
	}
	if spamFolderID == "" {
		t.Fatal("Spam folder not indexed")
	}
	if _, err := rm.SetRemoteFolderRole(ctx, p, box.ID, spamFolderID, model.FolderRoleSpam); err != nil {
		t.Fatalf("map spam role: %v", err)
	}
	res, err := rm.ListRemoteMessagesCached(ctx, p, box.ID, "INBOX", 10, "")
	if err != nil || len(res.Items) != 1 {
		t.Fatalf("messages: %+v %v", res, err)
	}
	messageID := res.Items[0].ID

	// A failed spam patch. Before the recheck, the move has already taken effect
	// at the provider: the message now sits in Spam.
	spam := true
	failed, err := svc.Store.EnqueueMessageAction(ctx, p, box.ID, messageID, store.MessageActionInput{Kind: "patch", Spam: &spam})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := svc.Store.ClaimMessageAction(ctx, "test"); err != nil || !ok {
		t.Fatalf("claim spam patch: %v %v", ok, err)
	}
	if err := svc.Store.SettleMessageAction(ctx, failed.ID, "failed", "timed out", time.Now()); err != nil {
		t.Fatal(err)
	}
	makeRecheckDue(t, svc, failed.ID)
	if _, err := rm.MoveRemoteMessage(ctx, p, box.ID, messageID, "Spam"); err != nil {
		t.Fatalf("pre-apply spam move: %v", err)
	}

	w := app.NewActionWorker(svc, rm)
	w.RecheckFailed()
	settled, err := svc.Store.GetMessageAction(ctx, p, failed.ID)
	if err != nil || settled.State != "done" {
		t.Fatalf("applied spam patch not settled by provider state: %+v %v", settled, err)
	}
}
