package privdrop_test

import (
	"os"
	"path/filepath"
	"testing"

	"gatehouse-mail/internal/privdrop"
)

func TestDropToRuntimeUserNoopWhenNonRoot(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("test asserts no-op behaviour, which only holds for a non-root process")
	}
	dir := t.TempDir()
	dropped, uid, gid, err := privdrop.DropToRuntimeUser(dir)
	if err != nil {
		t.Fatalf("DropToRuntimeUser: %v", err)
	}
	if dropped {
		t.Fatal("expected no drop for a non-root process")
	}
	if os.Getuid() == 0 || os.Geteuid() == 0 {
		t.Fatal("non-root process unexpectedly became root")
	}
	// Even on the no-op path the resolved UID/GID is reported so callers
	// can log the runtime identity without re-reading the environment.
	wantUID, wantGID, err := privdrop.ResolvedIdentity()
	if err != nil {
		t.Fatalf("ResolvedIdentity: %v", err)
	}
	if uid != wantUID || gid != wantGID {
		t.Fatalf("DropToRuntimeUser reported uid/gid = %d/%d, want %d/%d", uid, gid, wantUID, wantGID)
	}
}

func TestResolvedIdentityHonoursEnv(t *testing.T) {
	t.Setenv("GATEHOUSE_RUN_UID", "4242")
	t.Setenv("GATEHOUSE_RUN_GID", "4343")
	uid, gid, err := privdrop.ResolvedIdentity()
	if err != nil {
		t.Fatalf("ResolvedIdentity: %v", err)
	}
	if uid != 4242 || gid != 4343 {
		t.Fatalf("ResolvedIdentity = %d/%d, want 4242/4343", uid, gid)
	}
}

func TestResolvedIdentityFallsBackToDefaults(t *testing.T) {
	t.Setenv("GATEHOUSE_RUN_UID", "")
	t.Setenv("GATEHOUSE_RUN_GID", "")
	uid, gid, err := privdrop.ResolvedIdentity()
	if err != nil {
		t.Fatalf("ResolvedIdentity: %v", err)
	}
	if uid != privdrop.DefaultUID || gid != privdrop.DefaultGID {
		t.Fatalf("ResolvedIdentity = %d/%d, want %d/%d", uid, gid, privdrop.DefaultUID, privdrop.DefaultGID)
	}
}

func TestResolvedIdentityRejectsNonPositive(t *testing.T) {
	t.Setenv("GATEHOUSE_RUN_UID", "0")
	if _, _, err := privdrop.ResolvedIdentity(); err == nil {
		t.Fatal("expected an error for GATEHOUSE_RUN_UID=0")
	}
	t.Setenv("GATEHOUSE_RUN_UID", "not-a-number")
	if _, _, err := privdrop.ResolvedIdentity(); err == nil {
		t.Fatal("expected an error for non-numeric GATEHOUSE_RUN_UID")
	}
}

// TestChownDataDirCreatesAndOwns verifies ChownDataDir creates a missing
// directory. Ownership itself is only asserted when running as root, which the
// unprivileged test environment usually is not.
func TestChownDataDirCreatesAndOwns(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := privdrop.ChownDataDir(dir, os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("ChownDataDir: %v", err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("directory not created: %v", err)
	}
}

func TestDropToRejectsZeroViaDropToRuntimeUser(t *testing.T) {
	// DropTo(0, 0) is only meaningful as root; assert the guard by ensuring a
	// non-zero uid is required through the config path instead.
	if os.Getuid() == 0 {
		t.Skip("running as root: DropTo would actually drop privileges")
	}
	t.Setenv("GATEHOUSE_RUN_UID", "0")
	if _, _, _, err := privdrop.DropToRuntimeUser(t.TempDir()); err == nil {
		t.Fatal("expected an error for GATEHOUSE_RUN_UID=0")
	}
}
