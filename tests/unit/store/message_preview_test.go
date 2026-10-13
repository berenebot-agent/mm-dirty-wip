package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// Migration tests reconstruct a real pre-072 database rather than using the
// migrated template, proving the upgrade does not generate historical previews.
func TestMigration072DoesNotBackGeneratePreviews(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateAccountAndAdmin(ctx, "Preview", "preview@example.test", "fixture password", 100<<20)
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.CreateDomain(ctx, u.AccountID, "example.test")
	if err != nil {
		t.Fatal(err)
	}
	box, err := st.CreateInbox(ctx, u.AccountID, d.ID, "preview", "Preview")
	if err != nil {
		t.Fatal(err)
	}
	record := store.InboundRecord{Inbox: box, Provider: "smtp", From: model.Address{Address: "sender@example.test"}, To: []string{box.Address}, Text: "existing body", RawPath: "messages/preview.eml", SizeBytes: 100, ReceivedAt: time.Now().UTC(), ProviderDeliveryID: "old"}
	old, _, _, err := st.CommitInbound(ctx, record)
	if err != nil {
		t.Fatal(err)
	}
	path := st.Path()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	db := rawDB(t, path)
	for _, statement := range []string{
		`DROP INDEX remote_snippet_pending`,
		`ALTER TABLE messages DROP COLUMN snippet`,
		`ALTER TABLE inbox_remote_messages DROP COLUMN snippet_pending`,
		`DELETE FROM schema_migrations WHERE version='072'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	db.Close()
	st, err = store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	old, err = st.GetMessageByID(ctx, u.AccountID, old.ID)
	if err != nil || old.Snippet != "" || old.Text != "existing body" {
		t.Fatalf("migration back-generated or changed mail: %+v %v", old, err)
	}
	record.ProviderDeliveryID = "new"
	record.Text = "new\n body"
	fresh, _, _, err := st.CommitInbound(ctx, record)
	if err != nil || fresh.Snippet != "new body" {
		t.Fatalf("new preview not cached: %+v %v", fresh, err)
	}
	if pending, err := st.PendingRemotePreviews(ctx, 10); err != nil || len(pending) != 0 {
		t.Fatalf("migration queued old mail: %+v %v", pending, err)
	}
}

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
	a, _, err := s.RecordRemoteArrival(ctx, u.AccountID, box.ID, store.RemoteArrivalInput{FolderPath: "INBOX", Source: "imap", UIDValidity: 100, UID: 1, RFCMessageID: "<event@example.test>"})
	if err != nil {
		t.Fatal(err)
	}
	ev, inserted, err := s.RecordRemoteArrivalEvent(ctx, u.AccountID, box.ID, a)
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
	replayed, err := s.GetEvent(ctx, u.AccountID, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"uid", "remote_uid", "uid_validity"} {
		if _, exists := replayed.Payload[key]; exists {
			t.Errorf("legacy replay exposes %s", key)
		}
	}
	if replayed.EntityID != a.ID || replayed.Payload["remote"] != true {
		t.Fatal("removing locators changed canonical event identity")
	}
}
