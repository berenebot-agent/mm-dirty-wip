// Package htmlsanitize sanitizes untrusted email HTML before it is served to a
// browser. It is the single rendering boundary for stored message bodies.
package htmlsanitize

import (
	"regexp"
	"sync"

	"github.com/microcosm-cc/bluemonday"
)

// remoteImgRe matches <img> tags whose src is an absolute remote URL. The
// sanitize policy allows remote images through; StripRemoteImages removes them
// from the default (non-opt-in) rendering so opening mail cannot phone the
// sender. CID rewrites are same-origin and untouched, as are data: images.
var remoteImgRe = regexp.MustCompile(`(?i)<img\b[^>]*\bsrc\s*=\s*("https?://[^"]*"|'https?://[^']*'|https?://[^\s>]+)[^>]*>`)

// StripRemoteImages removes remote-src <img> tags from already-sanitized HTML.
func StripRemoteImages(s string) string {
	if s == "" {
		return ""
	}
	return remoteImgRe.ReplaceAllString(s, "")
}

// HasRemoteImages reports whether the HTML references any remote image, so the
// UI can offer the "show images" opt-in only when it would do something.
func HasRemoteImages(s string) bool {
	return s != "" && remoteImgRe.MatchString(s)
}

var (
	policyOnce sync.Once
	policyVal  *bluemonday.Policy
)

// Sanitize returns HTML that is safe to render in a browser. Scripts, event
// handlers, embedded objects, forms and any markup outside the allowlist are
// removed; common email formatting is preserved.
func Sanitize(s string) string {
	if s == "" {
		return ""
	}
	return policy().Sanitize(s)
}

func policy() *bluemonday.Policy {
	policyOnce.Do(func() {
		p := bluemonday.NewPolicy()

		// Relative URLs (our /ui/attachments/... inlines), plus http, https and
		// mailto. Also adds rel="nofollow" to links.
		p.AllowStandardURLs()
		p.RequireNoReferrerOnLinks(true)
		// Open fully-qualified links in a new tab. The mail frame CSP sets
		// base-uri 'none', so a <base target> tag is ignored; the target must
		// be on each anchor.
		p.AddTargetBlankToFullyQualifiedLinks(true)

		p.AllowStandardAttributes()
		p.AllowAttrs("class").Matching(bluemonday.SpaceSeparatedTokens).Globally()

		p.AllowImages()
		// CID URLs identify attachments and do not execute code. Keeping them
		// preserves the API's image-to-attachment relationship; the UI rewrites
		// them to authenticated inline attachment URLs before sanitization.
		p.AllowURLSchemes("cid")
		p.AllowLists()
		p.AllowTables()

		p.AllowElements(
			"a", "abbr", "acronym", "address", "article", "aside", "b", "big",
			"blockquote", "br", "center", "cite", "code", "dd", "dfn", "div",
			"dl", "dt", "em", "figcaption", "figure", "font", "footer", "h1",
			"h2", "h3", "h4", "h5", "h6", "header", "hr", "i", "main", "mark",
			"nav", "p", "pre", "q", "s", "samp", "section", "small", "span",
			"strike", "strong", "sub", "sup", "tt", "u", "var", "wbr",
		)
		p.AllowAttrs("href").OnElements("a")
		p.AllowAttrs("cite").OnElements("blockquote", "q")
		p.AllowAttrs("color", "face", "size").OnElements("font")

		// Inline styles are required for faithful email rendering. bluemonday
		// validates each declared property with its built-in CSS handler, so the
		// raw style attribute is never passed through. Positioning/overlay
		// properties are deliberately omitted.
		p.AllowStyles(styleProperties...).Globally()

		policyVal = p
	})
	return policyVal
}

var styleProperties = []string{
	"background", "background-color", "background-image", "background-position",
	"background-repeat", "background-size", "background-attachment",
	"border", "border-top", "border-right", "border-bottom", "border-left",
	"border-color", "border-style", "border-width", "border-radius",
	"border-top-color", "border-right-color", "border-bottom-color", "border-left-color",
	"border-top-style", "border-right-style", "border-bottom-style", "border-left-style",
	"border-top-width", "border-right-width", "border-bottom-width", "border-left-width",
	"border-top-left-radius", "border-top-right-radius",
	"border-bottom-left-radius", "border-bottom-right-radius",
	"border-collapse", "border-spacing", "box-shadow", "box-sizing",
	"caption-side", "clear", "color", "direction", "display", "empty-cells", "float",
	"font", "font-family", "font-size", "font-style", "font-variant", "font-weight",
	"height", "letter-spacing", "line-height",
	"list-style", "list-style-image", "list-style-position", "list-style-type",
	"margin", "margin-top", "margin-right", "margin-bottom", "margin-left",
	"max-height", "max-width", "min-height", "min-width",
	"opacity", "overflow", "overflow-x", "overflow-y",
	"padding", "padding-top", "padding-right", "padding-bottom", "padding-left",
	"table-layout", "text-align", "text-decoration", "text-indent", "text-shadow",
	"text-transform", "vertical-align", "visibility", "white-space",
	"word-break", "word-spacing", "word-wrap", "overflow-wrap", "unicode-bidi",
}
