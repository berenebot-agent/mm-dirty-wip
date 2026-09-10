package httpapp

import (
	"fmt"
	"testing"
	"time"
)

func TestLimiterPrunesExpiredEntries(t *testing.T) {
	l := newLimiter(10, 20*time.Millisecond)
	for i := 0; i < 50; i++ {
		if !l.Allow(fmt.Sprintf("ip-%d", i)) {
			t.Fatalf("Allow(ip-%d) = false", i)
		}
	}
	if got := len(l.m); got != 50 {
		t.Fatalf("entries = %d, want 50", got)
	}
	time.Sleep(30 * time.Millisecond)
	if !l.Allow("fresh") {
		t.Fatal("Allow(fresh) = false")
	}
	if got := len(l.m); got != 1 {
		t.Fatalf("entries after sweep = %d, want 1", got)
	}
}

func TestLimiterCapsTrackedKeys(t *testing.T) {
	l := newLimiter(1, time.Hour)
	for i := 0; i < maxTrackedIPs; i++ {
		l.Allow(fmt.Sprintf("k-%d", i))
	}
	if got := len(l.m); got != maxTrackedIPs {
		t.Fatalf("entries = %d, want %d", got, maxTrackedIPs)
	}
	if l.Allow("overflow") {
		t.Fatal("new key admitted while at cap")
	}
	if got := len(l.m); got > maxTrackedIPs {
		t.Fatalf("entries grew past cap: %d", got)
	}
}
