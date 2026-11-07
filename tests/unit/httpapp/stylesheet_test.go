package httpapp_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// appStylesheet fetches the embedded UI stylesheet. UI rules live in
// assets/app.css rather than inline in each page, so style assertions read it.
func appStylesheet(t *testing.T, h http.Handler) string {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/assets/app.css", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /assets/app.css: %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Fatalf("app.css content type %q", ct)
	}
	return rr.Body.String()
}

// TestSettingsNavTabsCentreTheirLabel pins that the settings tab rail centres
// its label inside the row. The rail holds both <a> tabs (the Settings dialog,
// which links to real URLs) and <button> tabs (the inbox dialogs); only buttons
// inherit align-items:center from the global button rule, so the rail rule has
// to set it itself or the anchor labels sit at the top of the highlight.
func TestSettingsNavTabsCentreTheirLabel(t *testing.T) {
	t.Parallel()
	_, h, _, _, _ := httpFixture(t)
	css := appStylesheet(t, h)
	const sel = ".inbox-settings-nav .dialog-tab{"
	i := strings.Index(css, sel)
	if i < 0 {
		t.Fatal("app.css has no .inbox-settings-nav .dialog-tab rule")
	}
	rest := css[i+len(sel):]
	j := strings.Index(rest, "}")
	if j < 0 {
		t.Fatal("app.css .inbox-settings-nav .dialog-tab rule is unterminated")
	}
	if block := rest[:j]; !strings.Contains(block, "align-items:center") {
		t.Fatalf("settings nav tab rule does not centre its label: %s", block)
	}
}

// TestPagesLinkVersionedStylesheet pins that rendered pages load the shared
// stylesheet through a content-hashed URL and no longer inline it, and that
// app.js no longer injects style blocks at runtime.
func TestPagesLinkVersionedStylesheet(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)
	for _, path := range []string{"/", "/ui/inboxes/" + box.ID} {
		body := uiGet(t, h, cookie, path).Body.String()
		if !strings.Contains(body, `<link rel="stylesheet" href="/assets/app.css?v=`) {
			t.Fatalf("%s does not link the versioned stylesheet", path)
		}
		if strings.Contains(body, "<style>") {
			t.Fatalf("%s still inlines a <style> block", path)
		}
	}
	css := appStylesheet(t, h)
	if !strings.Contains(css, ":root{") || !strings.Contains(css, "--mm-ink:") {
		t.Fatal("app.css is missing the design token layer")
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/assets/app.js", nil))
	if strings.Contains(rr.Body.String(), "createElement('style')") {
		t.Fatal("app.js still injects a style block; rules belong in app.css")
	}
}
