// Package messagepreview builds bounded plain-text previews without retaining bodies.
package messagepreview

import (
	"strings"
	"unicode"

	"golang.org/x/net/html"
)

const MaxRunes = 200

// Text collapses whitespace and control characters and caps a preview by Unicode
// characters, not bytes. Empty remains empty internally; the API supplies "-".
func Text(text string) string {
	return boundedText(text, MaxRunes)
}

func boundedText(text string, maxRunes int) string {
	var out strings.Builder
	count, space := 0, false
	for _, r := range text {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			space = out.Len() > 0
			continue
		}
		if space {
			if count >= maxRunes {
				break
			}
			out.WriteByte(' ')
			count++
			space = false
		}
		if count >= maxRunes {
			break
		}
		out.WriteRune(r)
		count++
	}
	return strings.TrimSpace(out.String())
}

// FromBodies prefers plain text. The HTML fallback excludes script/style/head
// content, never executes HTML, and stops after enough visible text is collected.
func FromBodies(text, markup string) string {
	if preview := Text(text); preview != "" {
		return preview
	}
	return HTML(markup, MaxRunes)
}

// HTML extracts at most maxRunes of readable text without executing HTML.
func HTML(markup string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	z := html.NewTokenizer(strings.NewReader(markup))
	z.SetMaxBuf(max(64<<10, 4*maxRunes))
	var out strings.Builder
	skip := ""
	for out.Len() < 4*maxRunes {
		t := z.Next()
		if t == html.ErrorToken {
			// EOF, malformed HTML, or an oversized token ends the preview.
			break
		}
		if t == html.StartTagToken || t == html.EndTagToken || t == html.SelfClosingTagToken {
			name, _ := z.TagName()
			tag := string(name)
			if skip != "" {
				if t == html.EndTagToken && tag == skip {
					skip = ""
				}
				continue
			}
			if t == html.StartTagToken && (tag == "script" || tag == "style" || tag == "head" || tag == "template") {
				skip = tag
				continue
			}
			switch tag {
			case "p", "div", "br", "li", "tr", "td", "h1", "h2", "h3", "hr":
				out.WriteByte(' ')
			}
		} else if t == html.TextToken && skip == "" {
			chunk := z.Text()
			for _, r := range string(chunk) {
				if out.Len() >= 4*maxRunes {
					break
				}
				out.WriteRune(r)
			}
		}
	}
	return boundedText(out.String(), maxRunes)
}
