package app

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/DanielFillol/Jarvis/internal/slack"
)

var reFromUserIDQuery = regexp.MustCompile(`\bfrom:([UW][A-Z0-9]+)\b`)
var reChannelIDInText = regexp.MustCompile(`<#([CG][A-Z0-9]{8,})(?:\|[^>]*)?>`)

// isLongReplyCancellation returns true when the user's message is a clear
// negative token in response to the "can I post the long reply?" prompt.
// Kept as exact-token matching to avoid false positives on new questions.
func isLongReplyCancellation(text string) bool {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "não", "nao", "n", "no", "cancelar", "cancel", "não quero", "nao quero":
		return true
	}
	return false
}

// isLongReplyConfirmation returns true when the user's message is a clear
// positive token in response to the "can I post the long reply?" prompt.
func isLongReplyConfirmation(text string) bool {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "sim", "s", "ok", "pode", "yes", "y", "postar", "claro", "confirmar":
		return true
	}
	return false
}

// extractFromUserIDs returns unique Slack user IDs referenced by from:USERID
// filters in the query (e.g. "from:U067UM4LRGB" → ["U067UM4LRGB"]).
func extractFromUserIDs(q string) []string {
	ms := reFromUserIDQuery.FindAllStringSubmatch(q, -1)
	seen := map[string]bool{}
	var out []string
	for _, m := range ms {
		if len(m) < 2 {
			continue
		}
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// buildSlackContext builds a textual summary of Slack search results.
// It limits the number of matches included to 'limit'.
func buildSlackContext(matches []slack.SearchMessage, limit int) string {
	if limit <= 0 {
		limit = 8
	}
	var b strings.Builder
	for i, m := range matches {
		if i >= limit {
			break
		}
		if m.Permalink != "" {
			b.WriteString(fmt.Sprintf("[#%s] %s: %s\nlink: %s\n\n", m.Channel, m.Username, m.Text, m.Permalink))
		} else {
			b.WriteString(fmt.Sprintf("[#%s] %s: %s\n\n", m.Channel, m.Username, m.Text))
		}
	}
	return b.String()
}

// extractChannelIDsFromText returns the unique channel IDs embedded in
// Slack <#CHANID> mentions within text.
func extractChannelIDsFromText(text string) []string {
	matches := reChannelIDInText.FindAllStringSubmatch(text, -1)
	seen := map[string]bool{}
	var out []string
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		id := m[1]
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}
