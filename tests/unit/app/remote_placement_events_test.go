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

// TestRemoteLabelActionRecordsDurableEvent proves a remote label change is
// described by a durable event, not only by the transient queue notification:
// the event names the message and the labels it now carries.
func TestRemoteLabelActionRecordsDurableEvent(t *testing.T) {
	t.Parallel()
	svc, u, box, rm := remoteTestEnv(t)
	configureSecrets(t, rm, u, box, "pw", "")
	fake := newFakeRemoteServer()
	fake.addMessage("INBOX", "body", "<label@test>", "Label")
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
	action, err := svc.Store.EnqueueMessageAction(ctx, p, box.ID, messageID, store.MessageActionInput{Kind: "label", Label: "Work", LabelOp: "add"})
	if err != nil {
		t.Fatal(err)
	}
	w := app.NewActionWorker(svc, rm)
	w.Start()
	defer w.Stop()
	waitForAction(t, svc, p, action.ID)

	events, err := svc.Store.ListEvents(ctx, p, 0, box.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.Type != model.EventMessageLabelsChanged || ev.EntityID != messageID {
			continue
		}
		found = true
		labels := eventLabels(ev.Payload["labels"])
		if len(labels) != 1 || labels[0] != "Work" {
			t.Fatalf("durable label event = %+v", ev.Payload)
		}
	}
	if !found {
		t.Fatalf("no durable label event for %s", messageID)
	}
}

// TestLegacyExclusiveLabelActionIsRefused proves a queued exclusive label
// action — placement smuggled in as a label — is refused and cleared, never
// executed as a move.
func TestLegacyExclusiveLabelActionIsRefused(t *testing.T) {
	t.Parallel()
	svc, u, box, rm := remoteTestEnv(t)
	configureSecrets(t, rm, u, box, "pw", "")
	fake := newFakeRemoteServer()
	fake.addMessage("INBOX", "body", "<legacy@test>", "Legacy")
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
	// A new exclusive label action is refused at submission.
	if _, err := svc.EnqueueInboxAction(ctx, p, box.ID, messageID, store.MessageActionInput{Kind: "label", Label: "Work", LabelOp: "add", Destination: "Trash"}); err == nil {
		t.Fatal("an exclusive label action was accepted")
	}
	// A queued one is refused when it runs, and never retried.
	queued, err := svc.Store.EnqueueMessageAction(ctx, p, box.ID, messageID, store.MessageActionInput{Kind: "label", Label: "Work", LabelOp: "add", Destination: "Trash"})
	if err != nil {
		t.Fatal(err)
	}
	w := app.NewActionWorker(svc, rm)
	w.Start()
	defer w.Stop()
	waitForAction(t, svc, p, queued.ID)
	got, err := svc.Store.GetMessageAction(ctx, p, queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "failed" {
		t.Fatalf("legacy label action state = %q, want failed", got.State)
	}
	// The message was not moved.
	view, err := rm.GetRemoteMessage(ctx, p, box.ID, messageID)
	if err != nil {
		t.Fatal(err)
	}
	if view.FolderPath != "INBOX" {
		t.Fatalf("legacy label action moved the message to %q", view.FolderPath)
	}
}

// eventLabels reads a label list from an event payload, which is JSON.
func eventLabels(value any) []string {
	switch v := value.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if name, ok := item.(string); ok {
				out = append(out, name)
			}
		}
		return out
	}
	return nil
}

// waitForAction waits until a queued action settles.
func waitForAction(t *testing.T, svc *app.Service, p model.Principal, id string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		a, err := svc.Store.GetMessageAction(context.Background(), p, id)
		if err == nil && a.State != "pending" && a.State != "running" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("action %s did not settle", id)
}
