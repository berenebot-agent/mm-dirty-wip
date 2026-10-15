package apispec_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/apispec"
)

// TestMarkdownReferenceUpToDate guards against a stale generated API section in
// docs/API.md by re-rendering the canonical route table and
// comparing it with the file on disk.
func TestMarkdownReferenceUpToDate(t *testing.T) {
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(workingDir, "..", "..", "..")
	path := filepath.Join(root, "docs", "API.md")

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (run `make docs` to regenerate)", path, err)
	}
	want := apispec.RenderMarkdown(apispec.Routes())
	text := string(got)
	startMarker := "<!-- BEGIN GENERATED API REFERENCE -->"
	endMarker := "<!-- END GENERATED API REFERENCE -->"
	start := strings.Index(text, startMarker)
	end := strings.Index(text, endMarker)
	if start < 0 || end < start || strings.Count(text, startMarker) != 1 || strings.Count(text, endMarker) != 1 {
		t.Fatalf("%s is missing ordered generated-section markers", path)
	}
	section := strings.TrimSpace(text[start+len(startMarker) : end])
	if section != strings.TrimSpace(want) {
		t.Fatalf("generated API section in %s is stale; run `make docs` to regenerate", path)
	}
}
