package app_test

import (
	"context"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// TestMicrosoftMoveRetriesRetiredProviderID proves a move whose cached provider id
// the provider no longer accepts is completed against the live id, and that the
// stale row it addressed is retired instead of being left behind: Microsoft hands
// one message out under two id forms, so the row the list showed could no longer be
// deleted and appeared to come straight back.
func TestMicrosoftMoveRetriesRetiredProviderID(t *testing.T) {
	t.Parallel()
	svc, box, rm, fixture := microsoftLogEnv(t)
	ctx := context.Background()
	p := model.Principal{AccountID: box.AccountID, Admin: true}

	// A baseline pass seeds the Inbox folder and its delta cursor.
	if _, err := rm.ReconcileRemote(ctx, box.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	trash, err := svc.Store.GetSystemFolder(ctx, box.AccountID, box.ID, model.FolderRoleTrash)
	if err != nil {
		t.Fatal(err)
	}
	// The same message cached twice: the retired id the list shows, and the live id
	// a provider write left behind. The differing received stamp keeps the identity
	// match from folding them here, so the fallback path is the one exercised.
	fixture.add("live-id", "inbox-id", "MMTEST phantom")
	received := "2026-10-11T10:00:00Z"
	for provider, stamp := range map[string]*string{"stale-id": nil, "live-id": &received} {
		in := store.RemoteMessageInput{
			FolderPath:   "microsoft:" + provider,
			RFCMessageID: "<live-id@graph>",
			FromAddress:  "sender@outside.test",
			Subject:      "MMTEST phantom",
			ReceivedAt:   stamp,
		}
		if _, e := svc.Store.UpsertMicrosoftMessage(ctx, box.AccountID, box.ID, provider, "thread", "inbox-id", in); e != nil {
			t.Fatal(e)
		}
	}
	fixture.retire("stale-id")
	staleLocal, err := svc.Store.MicrosoftLocalID(ctx, box.AccountID, box.ID, "stale-id")
	if err != nil {
		t.Fatal(err)
	}
	view, err := rm.MoveRemoteMessage(ctx, p, box.ID, staleLocal, trash.Path)
	if err != nil {
		t.Fatalf("move with a retired id: %v", err)
	}
	if view.ID == staleLocal {
		t.Fatal("move reported the stale row as the moved message")
	}
	// The retired id was tried first and refused, then the live id carried the move.
	if fixture.moves != 2 {
		t.Fatalf("move calls = %d, want the retired id and then the live one", fixture.moves)
	}
	if _, err = svc.Store.MicrosoftLocalID(ctx, box.AccountID, box.ID, "stale-id"); err == nil {
		t.Fatal("stale row survived the move")
	}
	rows, err := svc.Store.ListRemoteMessagesFiltered(ctx, box.AccountID, box.ID, store.RemoteMessageFilter{FolderPath: trash.Path})
	if err != nil || len(rows) != 1 || rows[0].ID != view.ID {
		t.Fatalf("trash rows = %+v (%v), want the one moved message", rows, err)
	}
	all, err := svc.Store.ListRemoteMessagesFiltered(ctx, box.AccountID, box.ID, store.RemoteMessageFilter{})
	if err != nil || len(all) != 1 {
		t.Fatalf("inbox rows = %+v (%v), want one row per message", all, err)
	}
}

// TestMicrosoftReconcileFoldsDuplicateRows proves a completed Microsoft reconcile
// folds an inbox's duplicated cache rows once, so an inbox that accumulated them
// before the fix heals without operator action.
func TestMicrosoftReconcileFoldsDuplicateRows(t *testing.T) {
	t.Parallel()
	svc, box, rm, _ := microsoftLogEnv(t)
	ctx := context.Background()

	// The duplicate pair exists before the first completed pass, the state an inbox
	// reached while the provider handed out two ids for one message.
	stamped := "2026-10-11T10:00:00Z"
	for provider, stamp := range map[string]*string{"stale-id": nil, "live-id": &stamped} {
		in := store.RemoteMessageInput{FolderPath: "microsoft:" + provider, RFCMessageID: "<dup@graph>", FromAddress: "sender@outside.test", Subject: "MMTEST phantom", ReceivedAt: stamp}
		if _, err := svc.Store.UpsertMicrosoftMessage(ctx, box.AccountID, box.ID, provider, "thread", "inbox-id", in); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rm.ReconcileRemote(ctx, box.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := svc.Store.ListRemoteMessagesFiltered(ctx, box.AccountID, box.ID, store.RemoteMessageFilter{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %+v (%v), want the duplicate folded", rows, err)
	}
}
