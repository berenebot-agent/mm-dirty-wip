package httpapp_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestChangelogServed asserts /changelog serves the project changelog.
func TestChangelogServed(t *testing.T) {
	_, h, _, _, _ := httpFixture(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/changelog", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /changelog = %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Fatalf("content-type = %q, want text/markdown", ct)
	}
	if !strings.Contains(rr.Body.String(), "# Changelog") {
		t.Fatalf("changelog body missing heading: %s", rr.Body.String())
	}
}

// TestChangelogAssetUpToDate guards the embedded copy against drifting from the
// repository-root CHANGELOG.md. Run `make changelog` to sync.
func TestChangelogAssetUpToDate(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	root := filepath.Join(filepath.Dir(file), "..", "..", "..")
	want, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err != nil {
		t.Fatalf("read root CHANGELOG.md: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "internal", "httpapp", "assets", "CHANGELOG.md"))
	if err != nil {
		t.Fatalf("read embedded CHANGELOG.md: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("internal/httpapp/assets/CHANGELOG.md is stale; run `make changelog`")
	}
}
