package httpapp_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/tests/support/onefixture"
)

func TestOneMultiImportIndependentKeysAndSingleDeletion(t *testing.T) {
	t.Parallel()
	svc, _, u, _, _, _ := standaloneFixture(t)
	ctx := context.Background()
	f := onefixture.New(t)
	const secondID = "fixture-second-uuid"
	const secondEmail = "second-gmail@example.test"
	const replacement = "fixture-replacement-key-never-live"
	f.Change(func(f *onefixture.Fixture) {
		f.Keys = []string{replacement}
		f.SenderNames[secondEmail] = "Second Gmail Owner"
		f.Connections = append(f.Connections, map[string]any{"id": secondID, "key": "fixture-second-selector", "platform": "gmail", "active": true, "state": "operational", "environment": "live", "email": secondEmail})
	})
	rm := app.NewRemoteMailboxService(svc)
	defer rm.Stop()
	rm.OneHTTP, rm.OneInterval = f.Client(t), time.Nanosecond
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, Admin: true}
	setup, err := rm.PrepareOneImport(ctx, p, onefixture.Key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rm.DiscoverOne(ctx, model.Principal{AccountID: u.AccountID, UserID: "different-user", Admin: true}, setup); err == nil {
		t.Fatal("import setup leaked across users")
	}
	results, err := rm.ImportOne(ctx, p, setup, []string{onefixture.ConnectionID, secondID}, nil)
	if err != nil || len(results) != 2 || results[0].Error != "" || results[1].Error != "" {
		t.Fatalf("multi import: %+v %v", results, err)
	}
	if _, err := svc.Store.OneImportSetup(ctx, u.AccountID, u.ID, setup); err == nil {
		t.Fatal("completed setup retained credentials")
	}
	first, second := results[0].InboxID, results[1].InboxID
	for _, want := range []struct{ id, name string }{{first, onefixture.Name}, {second, "Second Gmail Owner"}} {
		box, err := svc.Store.GetInboxInternal(ctx, u.AccountID, want.id)
		if err != nil || box.DisplayName != want.name {
			t.Fatalf("import must use the proper Gmail display name: %+v %v", box, err)
		}
	}
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, first); err != nil {
		t.Fatal(err)
	}
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, second); err != nil {
		t.Fatal(err)
	}
	a, err := svc.Store.OneBinding(ctx, u.AccountID, first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.Store.OneBinding(ctx, u.AccountID, second)
	if err != nil {
		t.Fatal(err)
	}
	if a.EncryptedKey == b.EncryptedKey || a.KeyAAD == b.KeyAAD || a.RateGroup != b.RateGroup || strings.Contains(a.EncryptedKey, onefixture.Key) {
		t.Fatal("inboxes do not independently encrypt credentials or share key pacing")
	}
	secondClient, token, err := rm.GoogleMailboxClient(ctx, u.AccountID, second)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SetInboxDisplayName(ctx, u.AccountID, first, "My custom inbox"); err != nil {
		t.Fatal(err)
	}
	if err := rm.SaveOneInboxKey(ctx, p, first, replacement); err != nil {
		t.Fatal(err)
	}
	custom, err := svc.Store.GetInboxInternal(ctx, u.AccountID, first)
	if err != nil || custom.DisplayName != "My custom inbox" {
		t.Fatal("reconnect overwrote custom display name", err)
	}
	if err := svc.Store.SetInboxDisplayName(ctx, u.AccountID, first, onefixture.Email+" · Gmail via One"); err != nil {
		t.Fatal(err)
	}
	if err := rm.SaveOneInboxKey(ctx, p, first, replacement); err != nil {
		t.Fatal(err)
	}
	corrected, err := svc.Store.GetInboxInternal(ctx, u.AccountID, first)
	if err != nil || corrected.DisplayName != onefixture.Name {
		t.Fatal("existing auto-generated name not repaired with real name", err)
	}
	if err := svc.Store.SetOneHealth(ctx, u.AccountID, first, "attention", "stale queued request", a.Revision); err != nil {
		t.Fatal(err)
	}
	current, err := svc.Store.OneBinding(ctx, u.AccountID, first)
	if err != nil || current.Status != "active" {
		t.Fatal("stale request error overwrote replacement connection", err)
	}
	unchanged, err := svc.Store.OneBinding(ctx, u.AccountID, second)
	if err != nil || unchanged.EncryptedKey != b.EncryptedKey || unchanged.Revision != b.Revision {
		t.Fatal("replacing first changed second", err)
	}
	if _, err := secondClient.Profile(ctx, token); err != nil {
		t.Fatal("sibling rotation invalidated queued client", err)
	}
	if err := rm.DeleteOneConnection(ctx, model.Principal{AccountID: "other-account", UserID: u.ID, Admin: true}, first); err == nil {
		t.Fatal("cross-account deletion allowed")
	}
	draft, err := svc.Store.CreateDraft(ctx, p, model.Draft{InboxID: first, Subject: "Retained draft", Text: "Synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	if err := rm.DeleteOneConnection(ctx, p, first); err != nil {
		t.Fatal(err)
	}
	deleted, err := svc.Store.OneBinding(ctx, u.AccountID, first)
	if err != nil || deleted.EncryptedKey != "" || deleted.EncryptedConnectionKey != "" || deleted.Status != "disconnected" {
		t.Fatal("first credentials not cleared", err)
	}
	unchanged, err = svc.Store.OneBinding(ctx, u.AccountID, second)
	if err != nil || unchanged.EncryptedKey != b.EncryptedKey || unchanged.Status != "active" || unchanged.Revision != b.Revision {
		t.Fatal("single deletion affected sibling", err)
	}
	if _, err := secondClient.Profile(ctx, token); err != nil {
		t.Fatal("sibling deletion invalidated client", err)
	}
	if _, _, err := rm.GoogleMailboxClient(ctx, u.AccountID, first); err == nil {
		t.Fatal("deleted connection still works")
	}
	if _, err := svc.Store.GoogleLocalID(ctx, u.AccountID, first, "message-one"); err != nil {
		t.Fatal("connection deletion purged metadata", err)
	}
	if _, err := svc.Store.GetDraft(ctx, p, draft.ID); err != nil {
		t.Fatal("connection deletion purged draft", err)
	}
	if err := rm.SaveOneInboxKey(ctx, p, first, onefixture.Key); err != nil {
		t.Fatal("inbox cannot reconnect independently", err)
	}
}
