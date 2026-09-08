package store

import (
	"context"
	"errors"
	"testing"
)

func TestActiveOutboundCredential(t *testing.T) {
	s, u, _, _ := testStore(t)
	ctx := context.Background()
	if _, err := s.ActiveOutboundCredential(ctx, u.AccountID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
	a, err := s.SaveOutboundCredential(ctx, u.AccountID, "", "A", "brevo", "enc")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.SaveOutboundCredential(ctx, u.AccountID, "", "B", "smtp", "enc")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetActiveOutboundCredential(ctx, u.AccountID, a.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.ActiveOutboundCredential(ctx, u.AccountID)
	if err != nil || got.ID != a.ID {
		t.Fatalf("active %v %q", err, got.ID)
	}
	if err = s.SetActiveOutboundCredential(ctx, u.AccountID, b.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.ActiveOutboundCredential(ctx, u.AccountID); got.ID != b.ID {
		t.Fatalf("switch active %q", got.ID)
	}
	if err = s.DeleteOutboundCredential(ctx, u.AccountID, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ActiveOutboundCredential(ctx, u.AccountID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete should clear active, got %v", err)
	}
	if err = s.SetActiveOutboundCredential(ctx, u.AccountID, "out_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown credential should be rejected, got %v", err)
	}
}

func TestMigrationReopenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	_ = s2.Close()
}
