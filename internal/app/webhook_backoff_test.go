// These tests live in package app rather than under tests/unit because
// webhookBackoff is unexported and is a pure scheduling function; the black-box
// tests/unit convention cannot reach it. This mirrors the documented in-package
// exception for cmd/server/mxruntime_test.go.
package app

import (
	"testing"
	"time"
)

func TestWebhookBackoffBoundsAndJitter(t *testing.T) {
	// The base grows exponentially and is capped at an hour; jitter stays within
	// +/-25% of the base.
	for _, attempt := range []int{0, 1, 2, 5, 10, 20} {
		base := time.Second * time.Duration(1<<min(attempt, 10))
		if base > time.Hour {
			base = time.Hour
		}
		lo := base - base/4
		if lo < time.Second {
			lo = time.Second
		}
		hi := base + base/4
		got := webhookBackoff(attempt, "client-1", 42)
		if got < lo || got > hi {
			t.Fatalf("attempt %d: backoff %v outside [%v,%v]", attempt, got, lo, hi)
		}
	}
	// The cap holds even at a large attempt count.
	if got := webhookBackoff(30, "client-1", 1); got > time.Hour+time.Hour/4 {
		t.Fatalf("backoff %v exceeds the capped base", got)
	}
	// Jitter varies across clients/events, so a set of endpoints that failed
	// together does not share one retry instant.
	seen := map[time.Duration]bool{}
	for i := int64(1); i <= 32; i++ {
		seen[webhookBackoff(3, "client-"+string(rune('a'+i%26)), i)] = true
	}
	if len(seen) < 2 {
		t.Fatal("jitter did not vary across clients/events")
	}
	// The value is deterministic for the same inputs.
	if webhookBackoff(3, "client-x", 7) != webhookBackoff(3, "client-x", 7) {
		t.Fatal("backoff is not deterministic for identical inputs")
	}
}
