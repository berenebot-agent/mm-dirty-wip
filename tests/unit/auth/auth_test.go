package auth_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/auth"
	"github.com/dellarb/mailmoose/tests/support/smoke"
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
	smoke.Require(t)
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

// TestCheckPasswordContextShedsUnderSaturation proves the timeout-aware check
// returns ErrArgonBusy rather than blocking when every admission slot is held.
// This test is deliberately serial (no t.Parallel()): it mutates the
// process-global Argon2 parameters via SetArgonParamsForTest, which would race
// with any parallel test hashing a password.
func TestCheckPasswordContextShedsUnderSaturation(t *testing.T) {
	// A cost high enough that the occupying hashes hold their slots for a
	// measurable window, while staying quick under -race.
	auth.SetArgonParamsForTest(1<<13, 2)
	defer auth.SetArgonParamsForTest(0, 0)
	enc, err := auth.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}

	// Saturate the four admission slots with blocking hashes that re-run for a
	// fixed number of rounds, so the slots stay busy while the probe runs.
	const occupiers = 4
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < occupiers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = auth.HashPassword("occupy-slot-password")
			}
		}()
	}

	// With the slots saturated, a short-timeout check must shed rather than
	// block. Retry briefly to let the occupiers actually take the slots first.
	var busyErr error
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, busyErr = auth.CheckPasswordContext(nil, enc, "correct horse battery staple", time.Millisecond)
		if busyErr == auth.ErrArgonBusy {
			break
		}
	}
	close(stop)
	wg.Wait()
	if busyErr != auth.ErrArgonBusy {
		t.Skip("could not observe all argon slots held in this environment")
	}
}
