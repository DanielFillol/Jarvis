package slack

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	reSlackUserMention    = regexp.MustCompile(`<@([UW][A-Z0-9]+)(?:\|([^>]+))?>`)
	reSlackChannelMention = regexp.MustCompile(`<#([CG][A-Z0-9]+)(?:\|([^>]+))?>`)
	reSlackUserGroup      = regexp.MustCompile(`<!subteam\^([A-Z0-9]+)(?:\|([^>]+))?>`)
	reSlackSpecial        = regexp.MustCompile(`<!(here|channel|everyone)>`)
	reSlackLink           = regexp.MustCompile(`<((?:https?://|mailto:)[^>|]+)(?:\|([^>]+))?>`)
)

// ChannelInfo holds a minimal channel summary returned by ListChannels.
type ChannelInfo struct {
	ID   string
	Name string
}

func shouldIncludeThreadMessage(subtype, text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	switch strings.TrimSpace(subtype) {
	case "message_changed", "message_deleted",
		"channel_join", "channel_leave",
		"channel_topic", "channel_purpose", "channel_name",
		"channel_archive", "channel_unarchive":
		return false
	default:
		return true
	}
}

func parseSlackTs(ts string) (sec int64, micro int64) {
	ts = strings.TrimSpace(ts)
	if ts == "" {
		return 0, 0
	}
	parts := strings.SplitN(ts, ".", 2)
	sec, _ = strconv.ParseInt(parts[0], 10, 64)
	if len(parts) == 2 {
		frac := parts[1]
		if len(frac) > 6 {
			frac = frac[:6]
		}
		for len(frac) < 6 {
			frac += "0"
		}
		micro, _ = strconv.ParseInt(frac, 10, 64)
	}
	return sec, micro
}

func preview(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func cleanSlackTextMax(s string, max int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = normalizeSlackMarkup(s)
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	if max > 0 && len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

func normalizeSlackMarkup(s string) string {
	// User mentions: <@U123|name> -> @name, <@U123> -> @U123
	s = reSlackUserMention.ReplaceAllStringFunc(s, func(m string) string {
		sub := reSlackUserMention.FindStringSubmatch(m)
		if len(sub) < 3 {
			return ""
		}
		id := sub[1]
		name := strings.TrimSpace(sub[2])
		if name != "" {
			return "@" + name
		}
		return "@" + id
	})

	// Channel mentions: <#C123|general> -> #general, <#C123> -> #C123
	s = reSlackChannelMention.ReplaceAllStringFunc(s, func(m string) string {
		sub := reSlackChannelMention.FindStringSubmatch(m)
		if len(sub) < 3 {
			return ""
		}
		id := sub[1]
		name := strings.TrimSpace(sub[2])
		if name != "" {
			return "#" + name
		}
		return "#" + id
	})

	// User groups: <!subteam^S123|team> -> @team
	s = reSlackUserGroup.ReplaceAllStringFunc(s, func(m string) string {
		sub := reSlackUserGroup.FindStringSubmatch(m)
		if len(sub) < 3 {
			return ""
		}
		id := sub[1]
		name := strings.TrimSpace(sub[2])
		if name != "" {
			return "@" + name
		}
		return "@subteam^" + id
	})

	// Special mentions: <!here> -> @here
	s = reSlackSpecial.ReplaceAllString(s, "@$1")

	// Links: <url|text> -> text (url), <url> -> url
	s = reSlackLink.ReplaceAllStringFunc(s, func(m string) string {
		sub := reSlackLink.FindStringSubmatch(m)
		if len(sub) < 3 {
			return ""
		}
		u := strings.TrimSpace(sub[1])
		txt := strings.TrimSpace(sub[2])
		if txt != "" {
			return txt + " (" + u + ")"
		}
		return u
	})

	// Minimal entity decoding commonly seen in Slack payloads.
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	return s
}

func cleanSlackText(s string) string {
	return cleanSlackTextMax(s, 420)
}

// splitTopLevelOR splits a Slack search query on "OR" tokens that
// appear outside double-quoted strings.  It returns a slice with
// the individual clauses.  If no top-level OR is found, a single-element slice containing the original query is returned.
func splitTopLevelOR(query string) []string {
	var clauses []string
	var cur strings.Builder
	inQuote := false
	i := 0
	for i < len(query) {
		ch := query[i]
		if ch == '"' {
			inQuote = !inQuote
			cur.WriteByte(ch)
			i++
			continue
		}
		if !inQuote && i+4 <= len(query) && query[i:i+4] == " OR " {
			if s := strings.TrimSpace(cur.String()); s != "" {
				clauses = append(clauses, s)
			}
			cur.Reset()
			i += 4
			continue
		}
		cur.WriteByte(ch)
		i++
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		clauses = append(clauses, s)
	}
	if len(clauses) == 0 {
		return []string{query}
	}
	return clauses
}
