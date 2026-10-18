package store_test

import (
	"context"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

func TestRemotePreviewStorageAccountingAndHeaderRefresh(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, _, _ := testStore(t)
	box, err := s.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: "preview@remote.test"})
	if err != nil {
		t.Fatal(err)
	}
	in := store.RemoteMessageInput{FolderPath: "INBOX", UIDValidity: 100, UID: 1}
	m, err := s.UpsertRemoteMessage(ctx, u.AccountID, box.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.GetAccount(ctx, u.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.QueueRemotePreview(ctx, u.AccountID, box.ID, m.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteRemotePreview(ctx, u.AccountID, box.ID, m.ID, "hello"); err != nil {
		t.Fatal(err)
	}
	after, err := s.GetAccount(ctx, u.AccountID)
	if err != nil || after.StorageUsedBytes-before.StorageUsedBytes != 5 {
		t.Fatalf("preview not quota charged: before=%+v after=%+v err=%v", before, after, err)
	}
	m, err = s.UpsertRemoteMessage(ctx, u.AccountID, box.ID, in)
	if err != nil || m.Snippet != "hello" {
		t.Fatalf("header refresh erased preview: %+v %v", m, err)
	}
}

func TestEventReplayOmitsLegacyIMAPIdentifiers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, _, _ := testStore(t)
	box, err := s.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: "events@remote.test"})
	if err != nil {
		t.Fatal(err)
	}
	arrival, _, err := s.RecordRemoteArrival(ctx, u.AccountID, box.ID, store.RemoteArrivalInput{FolderPath: "INBOX", Source: "imap", UIDValidity: 100, UID: 1, RFCMessageID: "<event@example.test>"})
	if err != nil {
		t.Fatal(err)
	}
	ev, inserted, err := s.RecordRemoteArrivalEvent(ctx, u.AccountID, box.ID, arrival)
	if err != nil || !inserted {
		t.Fatalf("event: %+v %v", ev, err)
	}
	for _, key := range []string{"uid", "remote_uid", "uid_validity"} {
		if _, exists := ev.Payload[key]; exists {
			t.Errorf("new event exposes %s", key)
		}
	}
	db := rawDB(t, s.Path())
	defer db.Close()
	if _, err := db.ExecContext(ctx, `UPDATE events SET payload_json=json_set(payload_json,'$.remote_uid',1,'$.uid_validity',100) WHERE id=?`, ev.ID); err != nil {
		t.Fatal(err)
	}
	events, err := s.ListEvents(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, ev.ID-1, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	var replayed model.Event
	for _, e := range events {
		if e.ID == ev.ID {
			replayed = e
		}
	}
	if replayed.ID != ev.ID {
		t.Fatalf("event %d missing from replay: %+v", ev.ID, events)
	}
	for _, key := range []string{"uid", "remote_uid", "uid_validity"} {
		if _, exists := replayed.Payload[key]; exists {
			t.Errorf("legacy replay exposes %s", key)
		}
	}
	if replayed.EntityID != arrival.ID || replayed.Payload["remote"] != true {
		t.Fatal("removing locators changed canonical event identity")
	}
}
