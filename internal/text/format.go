// internal/text/format.go
package text

import (
	"regexp"
	"strings"
)

// Pre-compiled regexes for MarkdownToMarkdown.
var (
	reMDBold      = regexp.MustCompile(`\*\*(.+?)\*\*`)
	reMDUnderBold = regexp.MustCompile(`__(.+?)__`)
	reMDTilde     = regexp.MustCompile(`~~(.+?)~~`)
	reMDHeading   = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	// Matches blocks of 2+ consecutive lines starting with "|" (Markdown tables).
	// Slack does not render Markdown tables; we wrap them in a code block so the
	// columns stay readable in a monospace font.
	reMDTable = regexp.MustCompile(`(?m)(^\|[^\n]+\n?){2,}`)
	// Ordered list items: "1. " at the start of a line → "1. " (already valid in Slack, just ensure no conversion needed)
	// Code blocks (```) pass through as-is — Slack renders them natively.
	// Blockquotes (>) pass through as-is — Slack renders them natively.
)

// MarkdownToMarkdown converts GitHub-flavored Markdown into Slack mrkdwn.
// Code blocks and blockquotes are preserved unchanged since Slack renders
// them natively. Bold, italic and heading markers are converted.
func MarkdownToMarkdown(s string) string {
	// Protect code blocks from being modified: extract them, convert the rest,
	// then reinsert. This prevents bold/heading regexes from mangling code.
	var blocks []string
	s = reMDCodeBlock.ReplaceAllStringFunc(s, func(m string) string {
		idx := len(blocks)
		blocks = append(blocks, m)
		return "\x00CODEBLOCK" + string(rune('0'+idx)) + "\x00"
	})

	// **text** or __text__ → *text*
	s = reMDBold.ReplaceAllString(s, "*$1*")
	s = reMDUnderBold.ReplaceAllString(s, "*$1*")
	// ~~text~~ → ~text~
	s = reMDTilde.ReplaceAllString(s, "~$1~")
	// ### Title / ## Title / # Title → *Title*
	s = reMDHeading.ReplaceAllString(s, "*$1*")

	// Wrap Markdown tables in code blocks (Slack doesn't render | tables natively).
	s = reMDTable.ReplaceAllStringFunc(s, func(m string) string {
		return "```\n" + strings.TrimRight(m, "\n") + "\n```\n"
	})

	// Reinsert code blocks
	for i, block := range blocks {
		s = strings.ReplaceAll(s, "\x00CODEBLOCK"+string(rune('0'+i))+"\x00", block)
	}
	return s
}

// reMDCodeBlock matches fenced code blocks (``` ... ```) across multiple lines.
var reMDCodeBlock = regexp.MustCompile("(?s)```[^`]*```")

// MarkdownToMrkdwn is an alias for MarkdownToMarkdown.
func MarkdownToMrkdwn(s string) string {
	return MarkdownToMarkdown(s)
}
