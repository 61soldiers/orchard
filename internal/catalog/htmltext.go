package catalog

import (
	"html"
	"regexp"
	"strings"
)

// Apple's editorial notes and artist bios come back as HTML fragments — `<b>`,
// `<i>`, `<a href>`, `<br>`, and &-entities. Orchard's clients render text, not
// markup, so every notes/bio/description field is flattened to plain text
// before it leaves.

var (
	htmlBrRe   = regexp.MustCompile(`(?i)<\s*br\s*/?\s*>`)
	htmlPEndRe = regexp.MustCompile(`(?i)</\s*p\s*>`)
	htmlTagRe  = regexp.MustCompile(`<[^>]+>`)
	htmlWsRe   = regexp.MustCompile(`[ \t]+`)
	htmlNlRe   = regexp.MustCompile(`\n{3,}`)
)

// htmlToText strips tags from an Apple editorial HTML string and unescapes its
// entities. It is a no-op for a string that has neither `<` nor `&`, so it is
// safe to call on fields that are usually already plain (copyright lines).
func htmlToText(s string) string {
	if s == "" || !strings.ContainsAny(s, "<&") {
		return s
	}
	s = htmlBrRe.ReplaceAllString(s, "\n")
	s = htmlPEndRe.ReplaceAllString(s, "\n\n")
	s = htmlTagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = htmlWsRe.ReplaceAllString(s, " ")
	s = htmlNlRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
