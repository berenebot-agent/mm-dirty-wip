package auth

import (
	"strings"
	"testing"
)

func TestLivePBKDF2RoundTrip(t *testing.T) {
	if passwordIterations != defaultPasswordIterations {
		t.Fatalf("prod default changed: iterations=%d want=%d", passwordIterations, defaultPasswordIterations)
	}
	enc, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "pbkdf2-sha256$310000$") {
		t.Fatalf("unexpected encoding prefix: %q", enc[:40])
	}
	if !CheckPassword(enc, "correct horse battery staple") {
		t.Fatal("live hash did not verify")
	}
	if CheckPassword(enc, "wrong password here") {
		t.Fatal("wrong password verified")
	}
}

func TestFastIterationsRoundTrip(t *testing.T) {
	old := passwordIterations
	SetIterationsForTest(1000)
	defer SetIterationsForTest(old)
	enc, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "pbkdf2-sha256$1000$") {
		t.Fatalf("unexpected encoding prefix: %q", enc[:40])
	}
	if !CheckPassword(enc, "correct horse battery staple") {
		t.Fatal("fast hash did not verify")
	}
}
