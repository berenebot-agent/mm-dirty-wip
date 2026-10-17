package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dellarb/mailmoose/internal/store"
)

// TestMicrosoftDeltaGenerationGuard proves the delta cursor's save is
// generation-conditional: a save read against one generation is rejected once a
// resync has reset the folder (a generation bump), so an in-flight delta page
// cannot advance the cursor of a superseded sweep. A save whose generation
// still matches — including a first save onto a fresh row — succeeds.
func TestMicrosoftDeltaGenerationGuard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, _, _ := testStore(t)
	box := standaloneBox(t, s, u.AccountID, "delta@outlook.test")
	inbox := box.ID

	// First save onto a fresh row: no generation recorded yet.
	if err := s.SaveMicrosoftDelta(ctx, inbox, "inbox", "cursor-1", true, ""); err != nil {
		t.Fatal(err)
	}
	cursor, initialized, gen, err := s.MicrosoftDelta(ctx, inbox, "inbox")
	if err != nil || cursor != "cursor-1" || !initialized || gen != "" {
		t.Fatalf("after first save: cursor=%q initialized=%v gen=%q err=%v", cursor, initialized, gen, err)
	}
	// The next save read that generation and may advance the cursor.
	if err := s.SaveMicrosoftDelta(ctx, inbox, "inbox", "cursor-2", true, gen); err != nil {
		t.Fatal(err)
	}
	// A resync resets the folder and bumps the generation.
	if err := s.StartMicrosoftResync(ctx, inbox, "inbox"); err != nil {
		t.Fatal(err)
	}
	cursor, initialized, gen, err = s.MicrosoftDelta(ctx, inbox, "inbox")
	if err != nil || cursor != "" || initialized {
		t.Fatalf("after resync: cursor=%q initialized=%v err=%v", cursor, initialized, err)
	}
	if gen == "" {
		t.Fatal("resync did not record a generation")
	}
	// A page that read the pre-resync generation must be rejected.
	if err := s.SaveMicrosoftDelta(ctx, inbox, "inbox", "cursor-1", true, ""); !errors.Is(err, store.ErrStaleCursor) {
		t.Fatalf("stale save = %v, want ErrStaleCursor", err)
	}
	cursor, _, _, _ = s.MicrosoftDelta(ctx, inbox, "inbox")
	if cursor != "" {
		t.Fatalf("stale page advanced the cursor to %q", cursor)
	}
	// The resync sweep itself (read against the new generation) saves cleanly.
	if err := s.SaveMicrosoftDelta(ctx, inbox, "inbox", "cursor-new", true, gen); err != nil {
		t.Fatal(err)
	}
	if c, _, _, _ := s.MicrosoftDelta(ctx, inbox, "inbox"); c != "cursor-new" {
		t.Fatalf("sweep saved cursor %q", c)
	}
}
