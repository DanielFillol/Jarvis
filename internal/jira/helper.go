package jira

import (
	"regexp"
	"strings"
)

var (
	reHeading    = regexp.MustCompile(`^(#{1,6})\s+(.+)$`)
	reTask       = regexp.MustCompile(`^-\s+\[([xX ])]\s*(.*)$`)
	reBold       = regexp.MustCompile(`\*\*(.+?)\*\*`)
	reHR         = regexp.MustCompile(`^-{3,}$|^\*{3,}$|^_{3,}$`)
	jiraReHTML   = regexp.MustCompile(`<[^>]+>`)
	jiraReSpaces = regexp.MustCompile(`\s+`)
)

// jiraStripHTML removes HTML tags and normalizes spaces/line breaks.
func jiraStripHTML(s string) string {
	s = strings.ReplaceAll(s, "<br>", "\n")
	s = strings.ReplaceAll(s, "<br/>", "\n")
	s = strings.ReplaceAll(s, "<br />", "\n")
	s = strings.ReplaceAll(s, "</p>", "\n\n")
	s = jiraReHTML.ReplaceAllString(s, " ")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = jiraReSpaces.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// jiraClip truncates s to at most n bytes, appending an ellipsis when truncated.
func jiraClip(s string, n int) string {
	if n <= 0 {
		return ""
	}
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// preview truncates long strings for use in error messages.
func preview(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
