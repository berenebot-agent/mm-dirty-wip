// Package testdb hands out isolated store.Store instances for tests without
// paying the full migration bootstrap on every fixture.
//
// Background: store.Open runs the whole migration chain (baseline + the
// versioned migrations), each step its own transaction, marker check and
// detector query. A test package with a per-test fixture (httpFixture,
// testService, testStore) therefore pays that cost hundreds of times, and it
// dominates the package's wall time.
//
// BuildTemplate migrates one database once per test binary, into a scratch dir,
// and consolidates it to a single file with VACUUM INTO (so no -wal/-shm side
// files need copying). Open then copies that template into the test's own temp
// dir and calls store.Open: every migration sees an already-populated
// schema_migrations row, so each step is a marker SELECT that skips, with no
// DDL and no commits.
//
// Do not use this helper in tests that assert migration behaviour or need a
// genuinely fresh database (fresh-migrate checks, pre-upgrade fixtures); those
// must call store.Open on a real empty dir.
package testdb

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/dellarb/mailmoose/internal/store"
)

// templatePath is the consolidated, fully-migrated database built once per test
// binary. templateErr records a build failure so every Open reports it rather
// than retrying.
var (
	templateOnce sync.Once
	templatePath string
	templateErr  error
)

// buildTemplate migrates a database once, then VACUUMs it into a single file
// with no WAL side files.
func buildTemplate() {
	scratch, err := os.MkdirTemp("", "mailmoose-testdb-template-*")
	if err != nil {
		templateErr = fmt.Errorf("testdb: create scratch dir: %w", err)
		return
	}
	defer os.RemoveAll(scratch)

	st, err := store.Open(filepath.Join(scratch, "src"))
	if err != nil {
		templateErr = fmt.Errorf("testdb: migrate template: %w", err)
		return
	}
	if err := st.Checkpoint(); err != nil {
		_ = st.Close()
		templateErr = fmt.Errorf("testdb: checkpoint template: %w", err)
		return
	}
	out := filepath.Join(scratch, "template.db")
	if err := st.VacuumInto(out); err != nil {
		_ = st.Close()
		templateErr = fmt.Errorf("testdb: consolidate template: %w", err)
		return
	}
	if err := st.Close(); err != nil {
		templateErr = fmt.Errorf("testdb: close template: %w", err)
		return
	}
	// Move the consolidated file to a path that outlives this function. Keep it
	// beside the (removed) scratch dir by writing into a stable temp location.
	final := filepath.Join(os.TempDir(), fmt.Sprintf("mailmoose-testdb-%d.db", os.Getpid()))
	if err := os.Rename(out, final); err != nil {
		templateErr = fmt.Errorf("testdb: publish template: %w", err)
		return
	}
	templatePath = final
}

// Open returns a store backed by a private copy of the migrated template, and
// registers its Close on t.Cleanup.
func Open(t *testing.T) *store.Store {
	t.Helper()
	st, _ := OpenDir(t)
	return st
}

// OpenDir is Open plus the data directory, for tests that need to reopen or
// otherwise reach the on-disk database directly.
func OpenDir(t *testing.T) (*store.Store, string) {
	t.Helper()
	templateOnce.Do(buildTemplate)
	if templateErr != nil {
		t.Fatal(templateErr)
	}
	dir := t.TempDir()
	if err := copyFile(templatePath, filepath.Join(dir, "inbox.db")); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, dir
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
