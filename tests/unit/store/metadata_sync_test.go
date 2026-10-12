package store_test

import (
	"context"
	"testing"

	"github.com/dellarb/mailmoose/internal/store"
)

func TestMetadataSyncMigration069AndDurableScope(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.CreateAccountAndAdmin(ctx, "Sync", "sync@example.test", "fixture-only-password", 100<<20)
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	box, err := s.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: "sync-box@example.test", DisplayName: "Custom"})
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	path := s.Path()
	s.Close()
	db := rawDB(t, path)
	if _, err := db.ExecContext(ctx, `DROP TABLE inbox_metadata_sync`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version='069'`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	s, err = store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveMetadataSyncState(ctx, u.AccountID, box.ID, "paused", "initial", "Metadata storage limit reached"); err != nil {
		s.Close()
		t.Fatal(err)
	}
	if err := s.SaveMetadataSyncTotal(ctx, u.AccountID, box.ID, 100); err != nil {
		s.Close()
		t.Fatal(err)
	}
	if err := s.SaveMetadataSyncState(ctx, "other-account", box.ID, "done", "refresh", ""); err != nil {
		s.Close()
		t.Fatal(err)
	}
	s.Close()
	s, err = store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	snapshot, err := s.MetadataSyncSnapshot(ctx, u.AccountID, box.ID)
	if err != nil || snapshot.State != "paused" || snapshot.Total != 100 || snapshot.UpdatedAt.IsZero() {
		t.Fatalf("durable state: %+v %v", snapshot, err)
	}
	other, err := s.MetadataSyncSnapshot(ctx, "other-account", box.ID)
	if err != nil || other.State != "" || other.Total != 0 {
		t.Fatalf("scope leak: %+v %v", other, err)
	}
	if err := s.SaveMetadataSyncState(ctx, u.AccountID, box.ID, "running", "initial", ""); err == nil {
		t.Fatal("running persisted across restart")
	}
	got, err := s.GetInboxInternal(ctx, u.AccountID, box.ID)
	if err != nil || got.DisplayName != "Custom" {
		t.Fatalf("migration changed inbox: %+v %v", got, err)
	}
}
