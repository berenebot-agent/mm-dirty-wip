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
