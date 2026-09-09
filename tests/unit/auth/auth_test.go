package auth_test

import (
	"strings"
	"testing"

	"gatehouse-mail/internal/auth"
)

// testDefaultIterations mirrors internal/auth's unexported defaultPasswordIterations.
const testDefaultIterations = 310000

func TestLivePBKDF2RoundTrip(t *testing.T) {
	enc, err := auth.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "pbkdf2-sha256$310000$") {
		t.Fatalf("unexpected encoding prefix: %q", enc[:40])
	}
	if !auth.CheckPassword(enc, "correct horse battery staple") {
		t.Fatal("live hash did not verify")
	}
	if auth.CheckPassword(enc, "wrong password here") {
		t.Fatal("wrong password verified")
	}
}

func TestFastIterationsRoundTrip(t *testing.T) {
	auth.SetIterationsForTest(1000)
	defer auth.SetIterationsForTest(testDefaultIterations)
	enc, err := auth.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "pbkdf2-sha256$1000$") {
		t.Fatalf("unexpected encoding prefix: %q", enc[:40])
	}
	if !auth.CheckPassword(enc, "correct horse battery staple") {
		t.Fatal("fast hash did not verify")
	}
}
