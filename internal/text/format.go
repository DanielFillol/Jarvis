// internal/text/format.go
package text

import (
	"regexp"
)

// MarkdownToMarkdown converts a subset of GitHub-flavored Markdown into
// Slack's Markdown syntax.  It currently handles bold, italic,
// strikethrough and headings.  Additional conversions can be added
// as needed.
func MarkdownToMarkdown(s string) string {
	// **text** or __text__ → *text*
	reBold := regexp.MustCompile(`\*\*(.+?)\*\*`)
	s = reBold.ReplaceAllString(s, "*$1*")
	reUnderBold := regexp.MustCompile(`__(.+?)__`)
	s = reUnderBold.ReplaceAllString(s, "*$1*")
	// ~~texto~~ → ~texto~
	reTilde := regexp.MustCompile(`~~(.+?)~~`)
	s = reTilde.ReplaceAllString(s, "~$1~")
	// ### Title / ## Title / # Title → *Title*
	reH := regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	s = reH.ReplaceAllString(s, "*$1*")
	return s
}

// MarkdownToMrkdwn is a compatibility alias for the monolith naming.
func MarkdownToMrkdwn(s string) string {
	return MarkdownToMarkdown(s)
}
