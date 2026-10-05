package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// relayEventFor commits an inbound message, builds a relay connection on its
// inbox, and returns the first event the connector would deliver (its id and
// the message). The caller drives the real ack path.
func relayEventFor(t *testing.T, s *store.Store, box model.Inbox, u model.User, gatewayID string) (store.HermesConnection, model.Message, model.Event) {
	t.Helper()
	ctx := context.Background()
	conn, err := s.CreateHermesConnection(ctx, store.EnrollRecord{AccountID: u.AccountID, InboxID: box.ID, Name: "gw"}, gatewayID, "sec", "del")
	if err != nil {
		t.Fatal(err)
	}
	m, _, _, err := s.CommitInbound(ctx, inbound(box, "d-"+gatewayID, "<"+gatewayID+"@test>", "", nil, "Hello", "body"))
	if err != nil {
		t.Fatal(err)
	}
	ev, err := s.NextHermesEvent(ctx, conn.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	return conn, m, ev
}

// TestDeliveryAutoMarkReadOnAnyTrigger proves the any-trigger marks a delivered
// message read and records the per-connector delivery, with the default
// trigger leaving actions off until the inbox opts in.
func TestDeliveryAutoMarkReadOnAnyTrigger(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)

	// Default: no auto-actions, a delivery is still recorded.
	conn, m, ev := relayEventFor(t, s, b[0], u, "gw-default")
	if err := s.AckHermesEventLogged(ctx, conn.ID, ev.ID, 1); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetMessageByID(ctx, u.AccountID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Read {
		t.Fatal("message marked read with auto-actions off")
	}
	if len(got.Deliveries) != 1 || got.Deliveries[0].ClientID != conn.ID {
		t.Fatalf("deliveries not recorded: %#v", got.Deliveries)
	}

	// Enable mark-read on a fresh inbox.
	box := b[1]
	if err := s.SetInboxAutoActions(ctx, u.AccountID, box.ID, boolPtr(true), nil, nil); err != nil {
		t.Fatal(err)
	}
	conn2, m2, ev2 := relayEventFor(t, s, box, u, "gw-markread")
	if err := s.AckHermesEventLogged(ctx, conn2.ID, ev2.ID, 1); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetMessageByID(ctx, u.AccountID, m2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Read {
		t.Fatal("message not marked read on delivery")
	}
}

// TestDeliveryRecordIsIdempotent proves a repeated ack for the same
// (message, connector) pair inserts only one delivery row.
func TestDeliveryRecordIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	conn, m, ev := relayEventFor(t, s, b[0], u, "gw-idem")

	first, err := s.RecordDelivery(ctx, b[0].ID, conn.ID, m.ID, time.Now().UTC())
	if err != nil || !first {
		t.Fatalf("first delivery inserted=%v err=%v", first, err)
	}
	again, err := s.RecordDelivery(ctx, b[0].ID, conn.ID, m.ID, time.Now().UTC())
	if err != nil || again {
		t.Fatalf("repeat delivery inserted=%v err=%v", again, err)
	}
	got, err := s.GetMessageByID(ctx, u.AccountID, m.ID)
	if err != nil || len(got.Deliveries) != 1 {
		t.Fatalf("deliveries=%#v err=%v", got.Deliveries, err)
	}
	_ = ev
}

// TestDeliveryTrashAfterHours proves the due instant is stamped on delivery and
// the sweep trashes only once the window has passed.
func TestDeliveryTrashAfterHours(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]

	hours := 6
	if err := s.SetInboxAutoActions(ctx, u.AccountID, box.ID, nil, &hours, nil); err != nil {
		t.Fatal(err)
	}
	conn, m, ev := relayEventFor(t, s, box, u, "gw-trash")
	if err := s.AckHermesEventLogged(ctx, conn.ID, ev.ID, 1); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetMessageByID(ctx, u.AccountID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DeliveryActionDueAt == nil {
		t.Fatal("delivery_action_due_at not stamped")
	}

	// Before the window: nothing trashed.
	if events, err := s.TrashDeliveredDue(ctx, time.Now().UTC()); err != nil || len(events) != 0 {
		t.Fatalf("premature trash %v %#v", err, events)
	}
	// After the window: trashed with the durable event.
	future := time.Now().UTC().Add(time.Duration(hours)*time.Hour + time.Minute)
	events, err := s.TrashDeliveredDue(ctx, future)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != model.EventMessageTrashed {
		t.Fatalf("trash events %#v", events)
	}
	got, err = s.GetMessageByID(ctx, u.AccountID, m.ID)
	if err != nil || got.DeletedAt == nil {
		t.Fatalf("not trashed: %#v err=%v", got.DeletedAt, err)
	}
	// A second sweep is a no-op.
	if again, _ := s.TrashDeliveredDue(ctx, future); len(again) != 0 {
		t.Fatalf("second sweep retrashed %d", len(again))
	}
}

// TestDeliveryAllTriggerWaitsForReceiptTimeConnectors proves the all trigger
// waits for every connector present at receipt and is not blocked by a
// connector added afterwards.
func TestDeliveryAllTriggerWaitsForReceiptTimeConnectors(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]

	trigger := store.DeliveryTriggerAll
	if err := s.SetInboxAutoActions(ctx, u.AccountID, box.ID, boolPtr(true), nil, &trigger); err != nil {
		t.Fatal(err)
	}
	// First connector exists before the message arrives.
	first, err := s.CreateHermesConnection(ctx, store.EnrollRecord{AccountID: u.AccountID, InboxID: box.ID, Name: "a"}, "gw-all-a", "s", "d")
	if err != nil {
		t.Fatal(err)
	}
	m, _, _, err := s.CommitInbound(ctx, inbound(box, "d-all-1", "<all1@test>", "", nil, "Hi", "body"))
	if err != nil {
		t.Fatal(err)
	}
	// A connector is added *after* the message arrived; it must not be required.
	if _, err := s.CreateHermesConnection(ctx, store.EnrollRecord{AccountID: u.AccountID, InboxID: box.ID, Name: "b"}, "gw-all-b", "s", "d"); err != nil {
		t.Fatal(err)
	}
	evA, err := s.NextHermesEvent(ctx, first.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AckHermesEventLogged(ctx, first.ID, evA.ID, 1); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetMessageByID(ctx, u.AccountID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Read {
		t.Fatal("all trigger did not fire for the receipt-time connector set")
	}
}

// TestDeliveryAllTriggerWaits proves the all trigger does not fire until every
// receipt-time connector has delivered.
func TestDeliveryAllTriggerWaits(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]

	trigger := store.DeliveryTriggerAll
	if err := s.SetInboxAutoActions(ctx, u.AccountID, box.ID, boolPtr(true), nil, &trigger); err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateHermesConnection(ctx, store.EnrollRecord{AccountID: u.AccountID, InboxID: box.ID, Name: "a"}, "gw-wait-a", "s", "d")
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateHermesConnection(ctx, store.EnrollRecord{AccountID: u.AccountID, InboxID: box.ID, Name: "c"}, "gw-wait-c", "s", "d")
	if err != nil {
		t.Fatal(err)
	}
	m, _, _, err := s.CommitInbound(ctx, inbound(box, "d-wait", "<wait@test>", "", nil, "Hi", "body"))
	if err != nil {
		t.Fatal(err)
	}
	evA, _ := s.NextHermesEvent(ctx, a.ID, 0)
	if err := s.AckHermesEventLogged(ctx, a.ID, evA.ID, 1); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetMessageByID(ctx, u.AccountID, m.ID)
	if got.Read {
		t.Fatal("all trigger fired before every connector delivered")
	}
	evC, _ := s.NextHermesEvent(ctx, c.ID, 0)
	if err := s.AckHermesEventLogged(ctx, c.ID, evC.ID, 1); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetMessageByID(ctx, u.AccountID, m.ID)
	if !got.Read {
		t.Fatal("all trigger did not fire once every connector delivered")
	}
}

// TestDeliveryActionsSkipSpamAndInternal proves Spam mail is never acted on.
func TestDeliveryActionsSkipSpamAndInternal(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]

	trigger := store.DeliveryTriggerAny
	if err := s.SetInboxAutoActions(ctx, u.AccountID, box.ID, boolPtr(true), nil, &trigger); err != nil {
		t.Fatal(err)
	}
	conn, err := s.CreateHermesConnection(ctx, store.EnrollRecord{AccountID: u.AccountID, InboxID: box.ID, Name: "gw"}, "gw-spam", "s", "d")
	if err != nil {
		t.Fatal(err)
	}
	rec := inbound(box, "d-spam", "<spam@test>", "", nil, "Spam", "body")
	rec.Spam = true
	m, _, _, err := s.CommitInbound(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	// The relay skips Spam without an ack, so record the delivery directly to
	// exercise the action guard.
	if _, err := s.RecordDelivery(ctx, box.ID, conn.ID, m.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetMessageByID(ctx, u.AccountID, m.ID)
	if got.Read {
		t.Fatal("spam message marked read")
	}
}

func boolPtr(v bool) *bool { return &v }

func TestDeliveryAllTriggerIgnoresSenderDate(t *testing.T) {
	for _, offset := range []time.Duration{-365 * 24 * time.Hour, 365 * 24 * time.Hour} {
		t.Run(offset.String(), func(t *testing.T) {
			ctx := context.Background()
			s, u, _, boxes := testStore(t)
			box := boxes[0]
			trigger, hours := store.DeliveryTriggerAll, 6
			if err := s.SetInboxAutoActions(ctx, u.AccountID, box.ID, boolPtr(true), &hours, &trigger); err != nil {
				t.Fatal(err)
			}
			a, err := s.CreateHermesConnection(ctx, store.EnrollRecord{AccountID: u.AccountID, InboxID: box.ID, Name: "a"}, "gw-date-a", "s", "d")
			if err != nil {
				t.Fatal(err)
			}
			b, err := s.CreateWebhookClient(ctx, u.AccountID, box.ID, "b", "https://hooks.example.test/x", "notify", "signature", "enc")
			if err != nil {
				t.Fatal(err)
			}
			rec := inbound(box, "d-date", "<date@test>", "", nil, "Date", "body")
			rec.ReceivedAt = time.Now().UTC().Add(offset)
			m, ev, _, err := s.CommitInbound(ctx, rec)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.CreateHermesConnection(ctx, store.EnrollRecord{AccountID: u.AccountID, InboxID: box.ID, Name: "late"}, "gw-date-late", "s", "d"); err != nil {
				t.Fatal(err)
			}
			if err := s.AckHermesEventLogged(ctx, a.ID, ev.ID, 1); err != nil {
				t.Fatal(err)
			}
			got, err := s.GetMessageByID(ctx, u.AccountID, m.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Read || got.DeliveryActionDueAt != nil {
				t.Fatal("sender date allowed actions before every ingress-time connector delivered")
			}
			now := time.Now().UTC()
			if err := s.RecordWebhookDelivery(ctx, b.ID, ev.ID, true, "", now, now.Add(7*24*time.Hour)); err != nil {
				t.Fatal(err)
			}
			got, err = s.GetMessageByID(ctx, u.AccountID, m.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Read || got.DeliveryActionDueAt == nil || got.DeliveryActionDueAt.Before(now.Add(time.Duration(hours)*time.Hour)) || got.DeliveryActionDueAt.After(time.Now().UTC().Add(time.Duration(hours)*time.Hour)) {
				t.Fatalf("actions did not fire on last required delivery: read=%v due=%v", got.Read, got.DeliveryActionDueAt)
			}
		})
	}
}

// TestDeliveryWebhookSuccessRecords proves a successful webhook delivery records
// a per-connector delivery and fires the auto-actions, while a failed attempt
// does not.
func TestDeliveryWebhookSuccessRecords(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]

	trigger := store.DeliveryTriggerAny
	if err := s.SetInboxAutoActions(ctx, u.AccountID, box.ID, boolPtr(true), nil, &trigger); err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateWebhookClient(ctx, u.AccountID, box.ID, "ep", "https://hooks.example.test/x", "notify", "signature", "enc")
	if err != nil {
		t.Fatal(err)
	}
	m, ev, _, err := s.CommitInbound(ctx, inbound(box, "d-wh", "<wh@test>", "", nil, "Hook", "body"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	// A failed attempt records no delivery and fires no action.
	if err := s.RecordWebhookDelivery(ctx, c.ID, ev.ID, false, "boom", now, now.Add(7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetMessageByID(ctx, u.AccountID, m.ID)
	if got.Read || len(got.Deliveries) != 0 {
		t.Fatalf("failed delivery acted: read=%v deliveries=%#v", got.Read, got.Deliveries)
	}
	// A successful attempt records the delivery and marks read.
	if err := s.RecordWebhookDelivery(ctx, c.ID, ev.ID, true, "", now, now.Add(7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetMessageByID(ctx, u.AccountID, m.ID)
	if !got.Read {
		t.Fatal("webhook delivery did not mark read")
	}
	if len(got.Deliveries) != 1 || got.Deliveries[0].ClientID != c.ID {
		t.Fatalf("webhook delivery not recorded: %#v", got.Deliveries)
	}
}

// TestNewInboxDefaultsToAllDeliveryTrigger pins the trigger a new inbox carries:
// "all", so the delivery auto-actions wait for every connector on the inbox
// instead of firing on whichever one polls first. Both the value CreateInbox
// returns and the stored row are asserted, because CreateInbox builds its own
// model.Inbox and previously reported no trigger at all.
func TestNewInboxDefaultsToAllDeliveryTrigger(t *testing.T) {
	ctx := context.Background()
	s, u, d, boxes := testStore(t)

	if store.DeliveryTriggerDefault != store.DeliveryTriggerAll {
		t.Fatalf("DeliveryTriggerDefault = %q, want %q", store.DeliveryTriggerDefault, store.DeliveryTriggerAll)
	}
	if boxes[0].DeliveryTrigger != store.DeliveryTriggerAll {
		t.Fatalf("CreateInbox returned trigger %q, want %q", boxes[0].DeliveryTrigger, store.DeliveryTriggerAll)
	}
	got, err := s.GetInboxInternal(ctx, u.AccountID, boxes[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DeliveryTrigger != store.DeliveryTriggerAll {
		t.Fatalf("stored trigger = %q, want %q", got.DeliveryTrigger, store.DeliveryTriggerAll)
	}
	fresh, err := s.CreateInbox(ctx, u.AccountID, d.ID, "later", "Later")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := s.GetInboxInternal(ctx, u.AccountID, fresh.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.DeliveryTrigger != store.DeliveryTriggerAll {
		t.Fatalf("inbox created after the first = %q, want %q", stored.DeliveryTrigger, store.DeliveryTriggerAll)
	}
}
