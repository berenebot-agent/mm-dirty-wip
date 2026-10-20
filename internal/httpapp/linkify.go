package httpapp

import (
	"html"
	"html/template"
	"regexp"
	"strings"
)

// linkifyRe matches absolute http(s) and mailto URLs plus bare "www." hosts in
// plain-text bodies. The trailing character class deliberately excludes
// whitespace and the angle/quote characters so matches stop at markup-like
// boundaries.
var linkifyRe = regexp.MustCompile(`(?i)\b(?:https?://|mailto:)[^\s<>"']+|www\.[^\s<>"']+`)

// linkifyText HTML-escapes a plain-text body and turns recognized URLs into
// anchors. Only http, https, mailto and bare www. hosts are linked; the result
// is safe to inject into a template because every non-link span and every
// attribute value is escaped first.
func linkifyText(s string) template.HTML {
	if s == "" {
		return ""
	}
	var b strings.Builder
	last := 0
	for _, loc := range linkifyRe.FindAllStringIndex(s, -1) {
		start, end := loc[0], loc[1]
		match := trimTrailingPunct(s[start:end])
		if match == "" {
			continue
		}
		end = start + len(match)

		b.WriteString(html.EscapeString(s[last:start]))
		href := match
		if !strings.Contains(href, "://") && !strings.HasPrefix(strings.ToLower(href), "mailto:") {
			href = "https://" + href
		}
		b.WriteString(`<a href="`)
		b.WriteString(html.EscapeString(href))
		b.WriteString(`" target="_blank" rel="noopener noreferrer nofollow">`)
		b.WriteString(html.EscapeString(match))
		b.WriteString(`</a>`)
		last = end
	}
	b.WriteString(html.EscapeString(s[last:]))
	return template.HTML(b.String())
}

// trimTrailingPunct strips sentence punctuation and unbalanced closing
// brackets that a URL regex would otherwise swallow.
func trimTrailingPunct(s string) string {
	for len(s) > 0 {
		switch c := s[len(s)-1]; c {
		case '.', ',', ';', ':', '!', '?', '\'', '"':
			s = s[:len(s)-1]
		case ')':
			if strings.Count(s, "(") < strings.Count(s, ")") {
				s = s[:len(s)-1]
				continue
			}
			return s
		case ']':
			if strings.Count(s, "[") < strings.Count(s, "]") {
				s = s[:len(s)-1]
				continue
			}
			return s
		case '}':
			if strings.Count(s, "{") < strings.Count(s, "}") {
				s = s[:len(s)-1]
				continue
			}
			return s
		default:
			return s
		}
	}
	return s
}
