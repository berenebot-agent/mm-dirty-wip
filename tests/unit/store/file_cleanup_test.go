package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPendingFileCleanupRetriesFailedRemoval(t *testing.T) {
	t.Parallel()
	s, _, _, _ := testStore(t)
	dir := filepath.Dir(s.Path())
	db := rawDB(t, s.Path())
	defer db.Close()
	blocked := filepath.Join(dir, "blocked")
	if err := os.MkdirAll(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(blocked, "child")
	for _, path := range []string{child, filepath.Join(dir, "retired.eml")} {
		if err := os.WriteFile(path, []byte("retired"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"blocked", "retired.eml", "already-gone.eml"} {
		if _, err := db.Exec(`INSERT INTO pending_file_cleanup(rel_path,created_at) VALUES(?,?)`, path, "2026-01-01T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SweepPendingFileCleanup(context.Background()); err == nil {
		t.Fatal("non-empty directory removal should fail")
	}
	var remaining string
	if err := db.QueryRow(`SELECT rel_path FROM pending_file_cleanup`).Scan(&remaining); err != nil || remaining != "blocked" {
		t.Fatalf("failed cleanup was not retained: %q %v", remaining, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "retired.eml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed entry blocked successful cleanup: %v", err)
	}
	if err := os.Remove(child); err != nil {
		t.Fatal(err)
	}
	if err := s.SweepPendingFileCleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pending_file_cleanup`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("cleanup queue=%d err=%v", n, err)
	}
}
