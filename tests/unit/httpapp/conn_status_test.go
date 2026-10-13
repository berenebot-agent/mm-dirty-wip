package httpapp_test

import (
	"strings"
	"testing"
)

// TestConnStatusFlagRendering proves the header Live/Disconnected status flag is
// rendered with its marker, initial connecting state and assistive semantics on
// the pages that hold a live event stream (dashboard, inbox list and message
// reader), so the client script can attach to it, and is absent from pages
// without a stream.
func TestConnStatusFlagRendering(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)
	m := seedInbound(t, svc, box, "conn-1", "<conn-1@test>", "Hello", "body")

	for name, tc := range map[string]struct {
		path string
		want bool
	}{
		"dashboard": {"/", true},
		"inbox":     {"/ui/inboxes/" + box.ID, true},
		"message":   {"/ui/messages/" + m.ID, true},
		"account":   {"/account", false},
		"compose":   {"/ui/inboxes/" + box.ID + "/compose", false},
	} {
		body := uiGet(t, h, cookie, tc.path).Body.String()
		if got := strings.Contains(body, "data-conn-status"); got != tc.want {
			t.Fatalf("%s: data-conn-status present=%v, want %v", name, got, tc.want)
		}
		if !tc.want {
			continue
		}
		frag := connStatusHTML(t, body)
		for _, marker := range []string{
			`class="conn-status"`, `data-state="connecting"`,
			`role="status"`, `aria-live="polite"`,
			`class="conn-dot"`, `class="conn-label"`, "Connecting…",
		} {
			if !strings.Contains(frag, marker) {
				t.Fatalf("%s: status flag missing %q in %q", name, marker, frag)
			}
		}
	}
}

// connStatusHTML extracts the status flag markup from a rendered page.
func connStatusHTML(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `<span class="conn-status"`)
	if start < 0 {
		t.Fatalf("page missing the conn-status flag")
	}
	end := strings.Index(body[start:], "</span></span>")
	if end < 0 {
		t.Fatalf("page missing the conn-status flag end")
	}
	return body[start : start+end+len("</span></span>")]
}
