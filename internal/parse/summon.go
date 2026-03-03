package parse

import "strings"

// LooksLikeDirectMention determines whether a message contains a direct Slack
// mention of the bot user ID (e.g. "<@U123>" or "<@U123|jarvis>").
func LooksLikeDirectMention(text, botUserID string) bool {
	text = strings.TrimSpace(text)
	botUserID = strings.TrimSpace(botUserID)
	if text == "" || botUserID == "" {
		return false
	}
	return strings.Contains(text, "<@"+botUserID+">") || strings.Contains(text, "<@"+botUserID+"|")
}

// StripSummon removes bot mentions and prefixes from a message, leaving
// the remainder to be interpreted as the user's question.  It trims
// surrounding whitespace.
func StripSummon(text, botUserID string) string {
	t := strings.TrimSpace(text)
	if botUserID != "" {
		t = strings.ReplaceAll(t, "<@"+botUserID+">", "")
	}
	prefixes := []string{"Jarvis:", "jarvis:", "!jarvis", "@Jarvis", "@jarvis"}
	for _, p := range prefixes {
		if strings.HasPrefix(t, p) {
			t = strings.TrimPrefix(t, p)
		}
	}
	return strings.TrimSpace(t)
}
