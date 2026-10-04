package auth_test

import (
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/auth"
)

func TestLiveArgon2idRoundTrip(t *testing.T) {
	enc, err := auth.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Fatalf("unexpected encoding prefix: %q", enc[:40])
	}
	if !auth.CheckPassword(enc, "correct horse battery staple") {
		t.Fatal("live hash did not verify")
	}
	if auth.CheckPassword(enc, "wrong password here") {
		t.Fatal("wrong password verified")
	}
	if auth.NeedsRehash(enc) {
		t.Fatal("production Argon2id hash reported as needing rehash")
	}
}

func TestFastArgon2RoundTrip(t *testing.T) {
	auth.SetArgonParamsForTest(8, 1)
	defer auth.SetArgonParamsForTest(0, 0)
	enc, err := auth.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "$argon2id$v=19$m=8,t=1,p=4$") {
		t.Fatalf("unexpected encoding prefix: %q", enc[:40])
	}
	if !auth.CheckPassword(enc, "correct horse battery staple") {
		t.Fatal("fast hash did not verify")
	}
	// A lowered-cost hash must be flagged for upgrade.
	if !auth.NeedsRehash(enc) {
		t.Fatal("low-cost hash was not flagged for rehash")
	}
}

// legacyPBKDF2Hash is a pre-Argon2 pbkdf2-sha256 encoding of "correct horse
// battery staple" produced by the previous release, at the default 310000
// iterations. It guards the prefix-dispatch compatibility path: an account that
// has not logged in since the Argon2id migration must still verify.
const legacyPBKDF2Hash = "pbkdf2-sha256$310000$MDEyMzQ1Njc4OWFiY2RlZg$G33eWH4HzmUuCbDixK08x1/nRbrOAEEKU7d108gnqpg"

func TestLegacyPBKDF2StillVerifies(t *testing.T) {
	if !auth.CheckPassword(legacyPBKDF2Hash, "correct horse battery staple") {
		t.Fatal("legacy pbkdf2 hash did not verify")
	}
	if auth.CheckPassword(legacyPBKDF2Hash, "wrong password here") {
		t.Fatal("legacy pbkdf2 hash verified a wrong password")
	}
	if !auth.NeedsRehash(legacyPBKDF2Hash) {
		t.Fatal("legacy PBKDF2 hash was not flagged for rehash")
	}
}

func TestCheckPasswordRejectsUnknownFormat(t *testing.T) {
	if auth.CheckPassword("not-a-valid-hash", "whatever") {
		t.Fatal("unknown format verified")
	}
}
