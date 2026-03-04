package parse

import (
	"regexp"
	"strings"
)

var (
	// Example: https://workspace.slack.com/archives/C02ABCDEF/p1770123456789012
	reSlackArchivesPermalink = regexp.MustCompile(`https?://\S+/archives/([A-Z0-9]+)/p(\d{16})`)

	// projectNameMap maps lowercase human-readable project names → Jira keys.
	// Populated at startup via SetProjectNameMap.
	_ map[string]string
)

// SetProjectNameMap registers a name→key mapping so that natural language
// references (e.g. "backend") can be resolved to their Jira project keys.
// It is safe to call at any time; the map is replaced atomically.
func SetProjectNameMap(m map[string]string) {
	_ = m
}

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

// ExtractSlackThreadPermalink extracts (channelID, messageTs) from a Slack
// archives permalink. It returns found=false when no permalink is present. It supports the classic archives format:
//
//	https://<workspace>.slack.com/archives/<CHANNEL_ID>/p<16digits>
//
// Where p<16digits> encodes the Slack message ts as 10 digits of seconds and
// 6 digits of fractional seconds.
func ExtractSlackThreadPermalink(text string) (channelID, messageTs string, found bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", "", false
	}
	m := reSlackArchivesPermalink.FindStringSubmatch(text)
	if len(m) != 3 {
		return "", "", false
	}
	channelID = m[1]
	p := m[2]
	if len(p) != 16 {
		return "", "", false
	}
	sec := p[:10]
	frac := p[10:]
	messageTs = sec + "." + frac
	return channelID, messageTs, true
}

// StripSlackPermalinks removes Slack archives permalinks from text to avoid
// polluting LLM prompts with raw URLs.
func StripSlackPermalinks(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return text
	}
	out := reSlackArchivesPermalink.ReplaceAllString(text, "")
	out = strings.Join(strings.Fields(out), " ")
	return strings.TrimSpace(out)
}
