package cryptox_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dellarb/mammothmail/internal/cryptox"
)

// TestKeyFileGeneratedWhenMissing proves a fresh deployment needs no
// operator-supplied key: one is generated, persisted 0600, and reused.
func TestKeyFileGeneratedWhenMissing(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), cryptox.KeyFileName)

	key, generated, err := cryptox.LoadOrCreateKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !generated {
		t.Fatal("first load did not report generating a key")
	}
	if len(key) != 64 || strings.Trim(key, "0123456789abcdef") != "" {
		t.Fatalf("generated key is not 64 hex characters: %q", key)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v, want 0600", info.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != key+"\n" {
		t.Fatalf("key file content %q, want %q", raw, key+"\n")
	}

	// A later start reuses the persisted key rather than minting another, which
	// would orphan every stored secret.
	again, generated, err := cryptox.LoadOrCreateKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if generated || again != key {
		t.Fatalf("second load = (%q, %v), want (%q, false)", again, generated, key)
	}
}

// TestKeyFileInvalidIsFatal proves an unusable key file is never replaced.
func TestKeyFileInvalidIsFatal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, content string }{
		{name: "empty", content: ""},
		{name: "too short", content: "short-key\n"},
		{name: "whitespace only", content: "   \n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), cryptox.KeyFileName)
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := cryptox.LoadOrCreateKeyFile(path); err == nil {
				t.Fatal("expected an error for an unusable encryption key file")
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != tc.content {
				t.Fatalf("key file was rewritten to %q", raw)
			}
		})
	}
}

// TestKeyFileAcceptsSuppliedValue proves a hand-managed key (an operator's own
// value copied into place) is read verbatim.
func TestKeyFileAcceptsSuppliedValue(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), cryptox.KeyFileName)
	const supplied = "an-operator-chosen-passphrase-over-24-chars"
	if err := os.WriteFile(path, []byte(supplied+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	key, generated, err := cryptox.LoadOrCreateKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if generated || key != supplied {
		t.Fatalf("got (%q, %v), want (%q, false)", key, generated, supplied)
	}
}

// TestKeyFileConcurrentCreatesConverge proves two starts racing on a fresh data
// directory end up with one key, not two.
func TestKeyFileConcurrentCreatesConverge(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), cryptox.KeyFileName)

	const starters = 8
	keys := make([]string, starters)
	created := make([]bool, starters)
	errs := make([]error, starters)
	var wg sync.WaitGroup
	for i := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			keys[i], created[i], errs[i] = cryptox.LoadOrCreateKeyFile(path)
		}()
	}
	wg.Wait()

	winners := 0
	for i := range keys {
		if errs[i] != nil {
			t.Fatalf("start %d: %v", i, errs[i])
		}
		if keys[i] != keys[0] {
			t.Fatalf("start %d got key %q, want %q", i, keys[i], keys[0])
		}
		if created[i] {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("%d starts reported generating the key, want exactly 1", winners)
	}
}
