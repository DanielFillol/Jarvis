package parse

import (
	"regexp"
	"strings"
)

var (
	// Example: https://workspace.slack.com/archives/C02ABCDEF/p1770123456789012
	reSlackArchivesPermalink = regexp.MustCompile(`https?://[^\s]+/archives/([A-Z0-9]+)/p(\d{16})`)
)

// ExtractSlackThreadPermalink extracts (channelID, messageTs) from a Slack
// archives permalink. It returns found=false when no permalink is present.
//
// It supports the classic archives format:
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
