package safepath_test

import (
	"path/filepath"
	"testing"

	"github.com/dellarb/mailmoose/internal/safepath"
)

func TestJoinAllowsContainedPaths(t *testing.T) {
	root := t.TempDir()
	got, err := safepath.Join(root, "messages/ab/cd/x.eml")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	want := filepath.Join(root, "messages", "ab", "cd", "x.eml")
	if got != want {
		t.Fatalf("Join = %q, want %q", got, want)
	}
}

func TestJoinRejectsEscapesAndBadInput(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{
		"",
		".",
		"../etc/passwd",
		"a/../../b",
		"/etc/passwd",
		"a\\b",
		"a\x00b",
	} {
		if _, err := safepath.Join(root, rel); err == nil {
			t.Errorf("Join(%q) = nil error, want rejection", rel)
		}
	}
}
