package htmlsanitize_test

import (
	"strings"
	"testing"

	"github.com/dellarb/mammothmail/internal/htmlsanitize"
)

// TestStripRemoteImagesRemovesAbsoluteAndProtocolRelative proves the default
// (non-opt-in) render cannot phone a sender through an <img> whose src is an
// absolute or protocol-relative remote URL, while same-origin, cid: and data:
// images are untouched. t.Parallel() is safe: pure string transformation.
func TestStripRemoteImagesRemovesAbsoluteAndProtocolRelative(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"https quoted", `<img src="https://tracker.example/p.png">`, ``},
		{"http single quoted", `<img src='http://tracker.example/p.png'>`, ``},
		{"https unquoted", `<img src=https://tracker.example/p.png>`, ``},
		{"protocol-relative quoted", `<img src="//tracker.example/p.png">`, ``},
		{"protocol-relative single quoted", `<img src='//tracker.example/p.png'>`, ``},
		{"protocol-relative unquoted", `<img src=//tracker.example/p.png>`, ``},
		{"same-origin kept", `<img src="/ui/attachments/abc/inline">`, `<img src="/ui/attachments/abc/inline">`},
		{"cid kept", `<img src="cid:logo@test">`, `<img src="cid:logo@test">`},
		{"data kept", `<img src="data:image/png;base64,AAAA">`, `<img src="data:image/png;base64,AAAA">`},
		{"absolute kept among text", `<p>hi <img src="https://t.example/p.png"> there</p>`, `<p>hi  there</p>`},
	}
	for _, tc := range cases {
		if got := htmlsanitize.StripRemoteImages(tc.in); got != tc.want {
			t.Errorf("%s: StripRemoteImages(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// TestHasRemoteImagesDetectsProtocolRelative guards the UI opt-in control: a
// protocol-relative image must be reported so the "show images" banner appears,
// and a same-origin image must not be.
func TestHasRemoteImagesDetectsProtocolRelative(t *testing.T) {
	t.Parallel()
	if !htmlsanitize.HasRemoteImages(`<img src="//tracker.example/p.png">`) {
		t.Fatal("protocol-relative image was not detected as remote")
	}
	if !htmlsanitize.HasRemoteImages(`<img src="https://tracker.example/p.png">`) {
		t.Fatal("absolute image was not detected as remote")
	}
	if htmlsanitize.HasRemoteImages(`<img src="/ui/attachments/abc/inline">`) {
		t.Fatal("same-origin image was wrongly detected as remote")
	}
	if htmlsanitize.HasRemoteImages("") {
		t.Fatal("empty body reported remote images")
	}
}

// TestSanitizeStripsScripts is a smoke check that the sanitizer allowlist still
// removes active content while keeping ordinary email markup.
func TestSanitizeStripsScripts(t *testing.T) {
	t.Parallel()
	got := htmlsanitize.Sanitize(`<p onclick="x()">hi</p><script>alert(1)</script><a href="https://ok.test">link</a>`)
	if strings.Contains(got, "script") || strings.Contains(got, "onclick") {
		t.Fatalf("active content survived sanitization: %q", got)
	}
	if !strings.Contains(got, "hi") || !strings.Contains(got, "https://ok.test") {
		t.Fatalf("benign markup was dropped: %q", got)
	}
}
