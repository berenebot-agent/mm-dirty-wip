package messagepreview_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dellarb/mailmoose/internal/messagepreview"
)

func TestPreview(t *testing.T) {
	for _, tc := range []struct{ text, markup, want string }{
		{" hello\n\tworld\x00 ", "<p>other</p>", "hello world"},
		{"", "<head><title>not body</title></head><script>secret()</script><style>hidden</style><p>Hello &amp; welcome</p><p>again</p>", "Hello & welcome again"},
		{"", "", ""},
		{"", "<p>hello <b>world</b>!</p>", "hello world!"},
	} {
		if got := messagepreview.FromBodies(tc.text, tc.markup); got != tc.want {
			t.Errorf("preview=%q want=%q", got, tc.want)
		}
	}
	for _, input := range []string{strings.Repeat("界", 1000), strings.Repeat("x", 1000)} {
		got := messagepreview.FromBodies("", "<p>"+input+"</p>")
		if !utf8.ValidString(got) || utf8.RuneCountInString(got) != messagepreview.MaxRunes {
			t.Fatalf("unbounded or invalid preview: %q", got)
		}
	}
}
