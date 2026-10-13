package httpapp

// These tests need unexported access to verify memory accounting, clock expiry,
// and quota admission directly; public HTTP ownership tests live in tests/unit.
import (
	"fmt"
	"testing"
	"time"
)

func TestFlashOwnershipAndConsumption(t *testing.T) {
	t.Parallel()
	f := newFlashStore(64, flashStoreMaxBytes)
	tok := f.put("alice", "draft", 32)
	if tok == "" {
		t.Fatal("put failed")
	}
	for _, owner := range []string{"", "bob"} {
		if _, ok := f.peek(owner, tok); ok {
			t.Fatal("foreign peek succeeded")
		}
		if _, ok := f.take(owner, tok); ok {
			t.Fatal("foreign take succeeded")
		}
	}
	if v, ok := f.peek("alice", tok); !ok || v != "draft" {
		t.Fatal("foreign read consumed owner's entry")
	}
	if _, ok := f.take("alice", tok); !ok {
		t.Fatal("owner take failed")
	}
	if _, ok := f.peek("alice", tok); ok || f.bytes != 0 {
		t.Fatal("take retained data")
	}
}

func TestFlashQuotasDoNotEvictOtherOwners(t *testing.T) {
	t.Parallel()
	f := newFlashStore(64, flashStoreMaxBytes)
	protected := f.put("protected", "secret", 1)
	for i := 0; i < flashOwnerMaxEntries; i++ {
		if f.put("attacker", "draft", 1) == "" {
			t.Fatal("premature quota rejection")
		}
	}
	if f.put("attacker", "overflow", 1) != "" {
		t.Fatal("owner entry quota bypassed")
	}
	if f.put("large", "draft", flashOwnerMaxBytes+1) != "" {
		t.Fatal("owner byte quota bypassed")
	}
	if f.put("bytes", "draft", flashOwnerMaxBytes) == "" || f.put("bytes", "overflow", 1) != "" {
		t.Fatal("aggregate byte quota broken")
	}
	for i := 0; i < 100; i++ {
		f.put(fmt.Sprint(i), "pressure", 1)
	}
	if _, ok := f.peek("protected", protected); !ok {
		t.Fatal("global pressure evicted another owner")
	}
	if len(f.entries) != 64 || f.put("last", "overflow", 1) != "" {
		t.Fatal("global entry quota broken")
	}
	bytes := newFlashStore(64, 64)
	kept := bytes.put("first", "secret", 64)
	if bytes.put("second", "overflow", 1) != "" {
		t.Fatal("global byte quota bypassed")
	}
	if _, ok := bytes.peek("first", kept); !ok {
		t.Fatal("byte pressure evicted first owner")
	}
}

func TestFlashExpiryReleasesQuota(t *testing.T) {
	t.Parallel()
	f := newFlashStore(1, 32)
	tok := f.put("alice", "draft", 32)
	e := f.entries[tok]
	e.expires = time.Now().Add(-time.Second)
	f.entries[tok] = e
	if _, ok := f.peek("alice", tok); ok || f.bytes != 0 {
		t.Fatal("expired entry retained")
	}
	if f.put("alice", "replacement", 32) == "" {
		t.Fatal("expiry did not release quota")
	}
}
