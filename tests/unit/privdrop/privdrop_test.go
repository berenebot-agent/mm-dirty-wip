package privdrop_test

import (
	"os"
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
