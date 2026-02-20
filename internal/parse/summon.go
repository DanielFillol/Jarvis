// internal/parse/summon.go
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

// LooksLikeSummon determines whether a message appears to be summoning
// the bot.  A mention of the bot's user ID (e.g. <@U123>) or
// certain textual prefixes ("jarvis:", "!jarvis", "@jarvis") causes
// this function to return true.  The check is case-insensitive.
func LooksLikeSummon(text, botUserID string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	if LooksLikeDirectMention(text, botUserID) {
		return true
	}
	if strings.Contains(text, "|@Jarvis>") || strings.Contains(text, "|@jarvis>") {
		return true
	}
	if strings.HasPrefix(t, "jarvis:") || strings.HasPrefix(t, "!jarvis") || strings.HasPrefix(t, "@jarvis") || strings.HasPrefix(t, "@jarvis") {
		return true
	}
	if strings.Contains(t, "@jarvis") {
		return true
	}
	return false
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
