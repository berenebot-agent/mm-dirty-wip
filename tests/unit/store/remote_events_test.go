package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// TestMigration057RemoteEvents proves migration 057 adds the durable remote
// event/detection surface on a genuinely fresh database.
func TestMigration057RemoteEvents(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if n := markerCount(t, st, "057"); n != 1 {
		t.Fatalf("migration 057 marker count %d", n)
	}
	db := rawDB(t, st.Path())
	defer db.Close()
	ctx := context.Background()
	var n int
	for _, table := range []string{"inbox_remote_cursors", "inbox_remote_arrivals", "inbox_remote_notifications", "inbox_remote_actions"} {
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("table %s missing after 057", table)
		}
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('inboxes') WHERE name='remote_notify_baseline_set'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("inboxes.remote_notify_baseline_set missing after 057")
	}
}

// remoteEventEnv seeds a standalone inbox with a configured remote binding and a
// credential so it is detectable.
func remoteEventEnv(t *testing.T) (*store.Store, string, model.Inbox) {
	t.Helper()
	st, acct := seedStandaloneAccount(t)
	ctx := context.Background()
	in, err := st.CreateStandaloneInbox(ctx, acct, store.StandaloneCreate{
		Address: "agent@remote.example",
		Remote:  &model.RemoteConnection{Host: "imap.remote.example", Username: "agent@remote.example"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveRemoteCredentials(ctx, acct, in.ID, "enc-imap", "", store.ConfigVersion{}); err != nil {
		t.Fatal(err)
	}
	return st, acct, in
}

// TestRemoteDetectionBaselineAndArrivals proves the baseline is established once
// without emitting arrivals, then only strictly-new UIDs are recorded, and a
// repeated detection is idempotent.
func TestRemoteDetectionBaselineAndArrivals(t *testing.T) {
	st, acct, in := remoteEventEnv(t)
	ctx := context.Background()

	// No cursor yet.
	if _, ok, err := st.GetRemoteCursor(ctx, acct, in.ID, "INBOX"); err != nil || ok {
		t.Fatalf("unexpected cursor before baseline: ok=%v err=%v", ok, err)
	}

	// Establish the baseline at UID 10, then record two new arrivals at 11 and 12.
	if err := st.EstablishRemoteBaseline(ctx, acct, in.ID, "INBOX", 100, 10); err != nil {
		t.Fatal(err)
	}
	c, ok, err := st.GetRemoteCursor(ctx, acct, in.ID, "INBOX")
	if err != nil || !ok {
		t.Fatalf("cursor missing after baseline: ok=%v err=%v", ok, err)
	}
	if c.LastUID != 10 || !c.BaselineDone || c.UIDValidity != 100 {
		t.Fatalf("cursor = %+v", c)
	}

	a11, inserted, err := st.RecordRemoteArrival(ctx, acct, in.ID, store.RemoteArrivalInput{FolderPath: "INBOX", UIDValidity: 100, UID: 11, RFCMessageID: "<a11@remote>", FromAddress: "x@y.test", Subject: "New"})
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("arrival 11 not inserted")
	}
	if a11.DeliveryState != store.RemoteArrivalPending {
		t.Fatalf("arrival state = %q", a11.DeliveryState)
	}

	// Idempotent re-detection.
	_, inserted, err = st.RecordRemoteArrival(ctx, acct, in.ID, store.RemoteArrivalInput{FolderPath: "INBOX", UIDValidity: 100, UID: 11, RFCMessageID: "<a11@remote>"})
	if err != nil {
		t.Fatal(err)
	}
	if inserted {
		t.Fatal("arrival 11 re-inserted (not idempotent)")
	}

	if err := st.AdvanceRemoteCursor(ctx, acct, in.ID, "INBOX", 100, 12); err != nil {
		t.Fatal(err)
	}
	c, _, _ = st.GetRemoteCursor(ctx, acct, in.ID, "INBOX")
	if c.LastUID != 12 {
		t.Fatalf("cursor last_uid = %d want 12", c.LastUID)
	}
}

// TestRemoteArrivalEventAndNotifyBaseline proves the arrival event is persisted
// remote-aware and the notification baseline is independent.
func TestRemoteArrivalEventAndNotifyBaseline(t *testing.T) {
	st, acct, in := remoteEventEnv(t)
	ctx := context.Background()
	if err := st.EstablishRemoteBaseline(ctx, acct, in.ID, "INBOX", 5, 0); err != nil {
		t.Fatal(err)
	}
	a, _, err := st.RecordRemoteArrival(ctx, acct, in.ID, store.RemoteArrivalInput{FolderPath: "INBOX", UIDValidity: 5, UID: 1, RFCMessageID: "<e1@remote>", FromAddress: "alice@example.test", Subject: "Hi"})
	if err != nil {
		t.Fatal(err)
	}
	ev, emitted, err := st.RecordRemoteArrivalEvent(ctx, acct, in.ID, a)
	if err != nil {
		t.Fatal(err)
	}
	if !emitted {
		t.Fatal("arrival event not emitted")
	}
	if ev.Type != store.EventRemoteMessageReceived {
		t.Fatalf("event type = %q", ev.Type)
	}
	if !store.IsRemoteArrivalEvent(ev.Payload) {
		t.Fatal("payload not marked remote")
	}
	if ev.EntityID != a.ID {
		t.Fatalf("event entity = %q want arrival id %q", ev.EntityID, a.ID)
	}
	// A second call is idempotent: no second event is emitted.
	if _, emitted2, err := st.RecordRemoteArrivalEvent(ctx, acct, in.ID, a); err != nil || emitted2 {
		t.Fatalf("duplicate event emitted: emitted=%v err=%v", emitted2, err)
	}
	// Notification baseline is independent and idempotent.
	if notified, err := st.RemoteNotified(ctx, acct, in.ID, "INBOX", 5, 1); err != nil || notified {
		t.Fatalf("unexpected notified=%v err=%v", notified, err)
	}
	if err := st.MarkRemoteNotified(ctx, acct, in.ID, "INBOX", 5, 1); err != nil {
		t.Fatal(err)
	}
	if notified, _ := st.RemoteNotified(ctx, acct, in.ID, "INBOX", 5, 1); !notified {
		t.Fatal("notify baseline not recorded")
	}
}

// TestRemoteArrivalClaimAndSettle proves the proactive fan-out claim/retry/settle
// state machine.
func TestRemoteArrivalClaimAndSettle(t *testing.T) {
	st, acct, in := remoteEventEnv(t)
	ctx := context.Background()
	if err := st.EstablishRemoteBaseline(ctx, acct, in.ID, "INBOX", 5, 0); err != nil {
		t.Fatal(err)
	}
	a, _, err := st.RecordRemoteArrival(ctx, acct, in.ID, store.RemoteArrivalInput{FolderPath: "INBOX", UIDValidity: 5, UID: 1, RFCMessageID: "<c1@remote>", FromAddress: "x@y.test", Subject: "s"})
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := st.ClaimNextRemoteArrival(ctx, time.Now().UTC(), "test", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if claimed.ID != a.ID {
		t.Fatalf("claimed %q want %q", claimed.ID, a.ID)
	}
	// A claimed arrival is not claimed twice concurrently.
	if _, ok, _ := st.ClaimNextRemoteArrival(ctx, time.Now().UTC(), "test2", time.Minute); ok {
		t.Fatal("claimed an already-claimed arrival")
	}
	if err := st.SettleRemoteArrival(ctx, acct, a.ID, store.RemoteArrivalDelivered, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.SettleRemoteArrival(ctx, acct, a.ID, store.RemoteArrivalDelivered, ""); err == nil {
		t.Fatal("re-settled a settled arrival")
	}
}

// TestRemoteNotifyBaselineFlag proves the baseline flag toggles once.
func TestRemoteNotifyBaselineFlag(t *testing.T) {
	st, acct, in := remoteEventEnv(t)
	ctx := context.Background()
	if set, err := st.RemoteNotifyBaselineSet(ctx, acct, in.ID); err != nil || set {
		t.Fatalf("baseline flag = %v err=%v", set, err)
	}
	if err := st.MarkRemoteNotifyBaselineSet(ctx, acct, in.ID); err != nil {
		t.Fatal(err)
	}
	if set, _ := st.RemoteNotifyBaselineSet(ctx, acct, in.ID); !set {
		t.Fatal("baseline flag not set")
	}
}

// TestRemoteActionsDelayedTrash proves the durable auto-action stamps a trash due
// instant and is applied once.
func TestRemoteActionsDelayedTrash(t *testing.T) {
	st, acct, in := remoteEventEnv(t)
	ctx := context.Background()
	if err := st.EstablishRemoteBaseline(ctx, acct, in.ID, "INBOX", 5, 0); err != nil {
		t.Fatal(err)
	}
	a, _, err := st.RecordRemoteArrival(ctx, acct, in.ID, store.RemoteArrivalInput{FolderPath: "INBOX", UIDValidity: 5, UID: 1, RFCMessageID: "<t1@remote>"})
	if err != nil {
		t.Fatal(err)
	}
	act, err := st.EnsureRemoteAction(ctx, acct, a, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if act.TrashDueAt == nil {
		t.Fatal("trash due not stamped")
	}
	// Not yet due.
	due, err := st.ListRemoteActionsDueForTrash(ctx, time.Now().UTC(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("unexpected due actions: %d", len(due))
	}
	// Simulate the window having elapsed.
	due, err = st.ListRemoteActionsDueForTrash(ctx, time.Now().UTC().Add(2*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("due actions = %d want 1", len(due))
	}
	if err := st.MarkRemoteActionTrashDone(ctx, acct, a.ID); err != nil {
		t.Fatal(err)
	}
	due, _ = st.ListRemoteActionsDueForTrash(ctx, time.Now().UTC().Add(2*time.Hour), 10)
	if len(due) != 0 {
		t.Fatalf("action trashed twice")
	}
}

// TestListDetectableRemoteInboxes proves only configured standalone inboxes are
// returned.
func TestListDetectableRemoteInboxes(t *testing.T) {
	st, acct, in := remoteEventEnv(t)
	ctx := context.Background()
	// An unconfigured standalone inbox is not detectable.
	if _, err := st.CreateStandaloneInbox(ctx, acct, store.StandaloneCreate{Address: "unconfigured@remote.example"}); err != nil {
		t.Fatal(err)
	}
	list, err := st.ListDetectableRemoteInboxes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != in.ID {
		t.Fatalf("detectable inboxes = %+v", list)
	}
}
