package store

// These tests need the private writer pool to hold its only connection and
// coordinate a password reset after verification, without timing the hash.
// testdb imports store, so this in-package test opens its own isolated database.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/auth"
)

func TestUpdateUserEmailVerifiesOutsideWriter(t *testing.T) {
	// Serial: changes the process-global Argon2 test cost.
	auth.SetArgonParamsForTest(64, 1)
	defer auth.SetArgonParamsForTest(0, 0)
	ctx := context.Background()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	u, err := s.CreateAccountAndAdmin(ctx, "Account", "old@example.com", "correct horse battery staple", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := s.write.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	checkCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := s.UpdateUserEmail(checkCtx, u.ID, u.AccountID, "new@example.com", "wrong password"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("verification waited for the writer: %v", err)
	}

	waits := s.write.Stats().WaitCount
	result := make(chan error, 1)
	go func() {
		result <- s.UpdateUserEmail(checkCtx, u.ID, u.AccountID, "new@example.com", "correct horse battery staple")
	}()
	for s.write.Stats().WaitCount == waits {
		select {
		case err := <-result:
			t.Fatalf("email update did not wait for writer: %v", err)
		case <-checkCtx.Done():
			t.Fatal("email update never reached writer")
		case <-time.After(time.Millisecond):
		}
	}
	newHash, err := auth.HashPassword("replacement password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE users SET password_hash=? WHERE id=?`, newHash, u.ID); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, ErrForbidden) {
		t.Fatalf("concurrent password reset did not reject old verification: %v", err)
	}
	got, err := s.GetUser(ctx, u.ID)
	if err != nil || got.Email != u.Email {
		t.Fatalf("email changed after password reset: %+v, %v", got, err)
	}
	if err := s.UpdateUserEmail(ctx, u.ID, u.AccountID, " NEW@example.com ", "replacement password"); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetUser(ctx, u.ID)
	if err != nil || got.Email != "new@example.com" {
		t.Fatalf("email update: %+v, %v", got, err)
	}
}
