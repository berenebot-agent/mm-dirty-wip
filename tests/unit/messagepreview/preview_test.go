package messagepreview_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dellarb/mammothmail/internal/messagepreview"
)

func TestPreview(t *testing.T) {
	t.Parallel()
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

func TestHTML(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, markup, want string
		limit              int
	}{
		{"entities and blocks", `<HEAD><title>hidden</title></HEAD><script>hidden</script><style>hidden</style><template>hidden</template><P>Hello &amp; welcome</P><BR/><div>next&nbsp;line</div>`, "Hello & welcome next line", 100},
		{"empty", `<style>hidden</style><p> &#160; </p>`, "", 100},
		{"unicode cap", `<p>` + strings.Repeat("界", 300) + `</p>`, strings.Repeat("界", 250), 250},
		{"long body", `<p>` + strings.Repeat("x", 60002) + `</p>`, strings.Repeat("x", 60001), 60001},
		{"zero cap", `<p>hello</p>`, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := messagepreview.HTML(tc.markup, tc.limit); got != tc.want {
				t.Fatalf("extracted text differs: got %d runes, want %d", utf8.RuneCountInString(got), utf8.RuneCountInString(tc.want))
			}
		})
	}
}
