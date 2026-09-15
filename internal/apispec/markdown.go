package apispec

import "strings"

// RenderMarkdown renders docs/API-REFERENCE.md: a generated reference document
// with one table per group.
func RenderMarkdown(routes []Route) string {
	var b strings.Builder
	b.WriteString("# Gatehouse Mail API Reference\n\n")
	b.WriteString("Generated from `internal/apispec`; do not edit by hand.\n\n")
	for _, group := range distinctGroups(routes) {
		b.WriteString("## ")
		b.WriteString(group)
		b.WriteString("\n\n")
		b.WriteString("| Method | Path | Summary | Role |\n")
		b.WriteString("| --- | --- | --- | --- |\n")
		for _, r := range routes {
			if r.Group != group {
				continue
			}
			b.WriteString("| ")
			b.WriteString(markdownCell(r.Method))
			b.WriteString(" | ")
			b.WriteString(markdownCell(r.Path))
			b.WriteString(" | ")
			b.WriteString(markdownCell(r.Summary))
			b.WriteString(" | ")
			b.WriteString(markdownCell(r.Role))
			b.WriteString(" |\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// markdownCell escapes a value for use inside a markdown table row.
func markdownCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}
