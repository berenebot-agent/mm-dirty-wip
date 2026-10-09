package mxdial_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

// TestRunSweepsStaleStagingFiles proves a manager removes mxdial staging files
// older than the grace window at startup, leaves recent files and other
// namespaces alone, and does not touch files outside its own namespace.
func TestRunSweepsStaleStagingFiles(t *testing.T) {
	dataDir := t.TempDir()
	tmp := filepath.Join(dataDir, "messages", ".tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(tmp, "mxdial-stale")
	recent := filepath.Join(tmp, "mxdial-recent")
	other := filepath.Join(tmp, "in000000.eml")
	for _, p := range []string{stale, recent, other} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	m := mxdial.New(&scriptedBackend{}, mxdial.Config{DataDir: dataDir, ReconcileInterval: time.Hour, AllowPrivateDestinations: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(stale); os.IsNotExist(err) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale mxdial temp file was not swept: %v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("recent mxdial temp file was removed: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("non-mxdial temp file was removed: %v", err)
	}
}
