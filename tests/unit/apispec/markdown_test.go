package apispec_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/dellarb/mailmoose/internal/apispec"
)

// TestMarkdownReferenceUpToDate guards against a stale checked-in
// docs/API-REFERENCE.md by re-rendering the canonical route table and
// comparing it with the file on disk.
func TestMarkdownReferenceUpToDate(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	root := filepath.Join(filepath.Dir(file), "..", "..", "..")
	path := filepath.Join(root, "docs", "API-REFERENCE.md")

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (run `make docs` to regenerate)", path, err)
	}
	want := apispec.RenderMarkdown(apispec.Routes())
	if string(got) != want {
		t.Fatalf("%s is stale; run `make docs` to regenerate", path)
	}
}
