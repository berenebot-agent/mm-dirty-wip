package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
)

// TestTimezoneSetting covers the account default and per-user override
// round-trips, validation, and the effective zone resolved on a session.
func TestTimezoneSetting(t *testing.T) {
	ctx := context.Background()
	s, u, _, _ := testStore(t)
	p := model.Principal{AccountID: u.AccountID, Admin: true, UserID: u.ID}

	// Defaults are empty (UTC).
	if tz, err := s.GetAccountTimezone(ctx, p); err != nil || tz != "" {
		t.Fatalf("default account tz = %q err=%v", tz, err)
	}
	acc, err := s.GetAccount(ctx, u.AccountID)
	if err != nil || acc.Timezone != "" {
		t.Fatalf("GetAccount tz = %q err=%v", acc.Timezone, err)
	}

	// Account default round-trips and is visible via GetAccount.
	if err := s.SetAccountTimezone(ctx, p, "Europe/London"); err != nil {
		t.Fatal(err)
	}
	if tz, _ := s.GetAccountTimezone(ctx, p); tz != "Europe/London" {
		t.Fatalf("account tz = %q", tz)
	}
	if acc, _ = s.GetAccount(ctx, u.AccountID); acc.Timezone != "Europe/London" {
		t.Fatalf("GetAccount tz after set = %q", acc.Timezone)
	}

	// An unknown zone is rejected and leaves the stored value intact.
	if err := s.SetAccountTimezone(ctx, p, "Not/AZone"); err == nil {
		t.Fatal("invalid account timezone accepted")
	}
	if tz, _ := s.GetAccountTimezone(ctx, p); tz != "Europe/London" {
		t.Fatalf("account tz changed on invalid set: %q", tz)
	}

	// Setting an empty string clears the preference back to UTC.
	if err := s.SetAccountTimezone(ctx, p, ""); err != nil {
		t.Fatal(err)
	}
	if tz, _ := s.GetAccountTimezone(ctx, p); tz != "" {
		t.Fatalf("account tz not cleared: %q", tz)
	}

	// Per-user override round-trips via GetUser.
	if err := s.SetUserTimezone(ctx, p, "Asia/Tokyo"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetUser(ctx, u.ID)
	if err != nil || got.Timezone != "Asia/Tokyo" {
		t.Fatalf("GetUser tz = %q err=%v", got.Timezone, err)
	}
	if err := s.SetUserTimezone(ctx, p, "Nope/Nope"); err == nil {
		t.Fatal("invalid user timezone accepted")
	}
	if err := s.SetUserTimezone(ctx, p, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetUser(ctx, u.ID); got.Timezone != "" {
		t.Fatalf("user tz not cleared: %q", got.Timezone)
	}
}

// TestSessionPrincipalResolvesTimezone verifies the effective zone carried on a
// web session: user override beats the account default, which beats UTC.
func TestSessionPrincipalResolvesTimezone(t *testing.T) {
	ctx := context.Background()
	s, u, _, _ := testStore(t)
	admin := model.Principal{AccountID: u.AccountID, Admin: true, UserID: u.ID}

	// Account default only.
	if err := s.SetAccountTimezone(ctx, admin, "Europe/London"); err != nil {
		t.Fatal(err)
	}
	tok, _, err := s.CreateSession(ctx, u.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := s.SessionPrincipal(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	if p.Timezone != "Europe/London" {
		t.Fatalf("session tz = %q, want account default", p.Timezone)
	}

	// User override wins.
	if err := s.SetUserTimezone(ctx, admin, "Asia/Tokyo"); err != nil {
		t.Fatal(err)
	}
	p, _, err = s.SessionPrincipal(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	if p.Timezone != "Asia/Tokyo" {
		t.Fatalf("session tz = %q, want user override", p.Timezone)
	}

	// Clearing both resolves to UTC.
	if err := s.SetUserTimezone(ctx, admin, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAccountTimezone(ctx, admin, ""); err != nil {
		t.Fatal(err)
	}
	p, _, err = s.SessionPrincipal(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	if p.Timezone != "UTC" {
		t.Fatalf("session tz = %q, want UTC", p.Timezone)
	}
}

// TestMigration040AddsTimezoneColumns verifies both columns exist with an empty
// default after the migration chain runs on a fresh store.
func TestMigration040AddsTimezoneColumns(t *testing.T) {
	ctx := context.Background()
	s, u, _, _ := testStore(t)
	if !columnPresent(t, s, "accounts", "timezone") {
		t.Fatal("accounts.timezone missing")
	}
	if !columnPresent(t, s, "users", "timezone") {
		t.Fatal("users.timezone missing")
	}
	if n := queryInt(t, s, `SELECT count(*) FROM accounts WHERE id=? AND timezone=''`, u.AccountID); n != 1 {
		t.Fatalf("account timezone default not empty (n=%d)", n)
	}
	if n := queryInt(t, s, `SELECT count(*) FROM users WHERE id=? AND timezone=''`, u.ID); n != 1 {
		t.Fatalf("user timezone default not empty (n=%d)", n)
	}
	_ = ctx
}
