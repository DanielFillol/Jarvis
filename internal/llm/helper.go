package llm

import (
	"math"
	"math/rand"
	"regexp"
	"strings"
	"time"
)

// OpenAIChatRequest and OpenAIChatResponse are exported aliases for the
// internal request and response types, allowing external packages to
// reference them without importing internal symbols directly.
type OpenAIChatRequest = openAIChatRequest
type OpenAIChatResponse = openAIChatResponse

func preview(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func clip(s string, n int) string {
	if n <= 0 {
		return ""
	}
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// normalizeSlackQuery fixes common formatting mistakes for Slack search.
// It converts LLM-generated "menção USERID" patterns to <@USERID> and
// normalizes from:/to: user ID filters.
func normalizeSlackQuery(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return q
	}

	// Convert "menção/mencao/mencionado USERID" (LLM hallucination) to <@USERID>
	reMentionWord := regexp.MustCompile(`(?i)\b(?:menção|mencao|mencionado|mencionada|mentioned)\s+(([UW])[A-Z0-9]+)\b`)
	q = reMentionWord.ReplaceAllString(q, "<@$1>")

	// Strip leftover mention words when a <@USERID> is already present.
	// e.g. "<@U09FJSKP407> menção" → "<@U09FJSKP407>"
	if strings.Contains(q, "<@") {
		reMentionLeftover := regexp.MustCompile(`(?i)\s*\b(?:menção|mencao|mencionado|mencionada|mentioned)\b\s*`)
		q = strings.TrimSpace(reMentionLeftover.ReplaceAllString(q, " "))
	}

	q = strings.ReplaceAll(q, "to:@", "to:")

	reFrom := regexp.MustCompile(`\bfrom:\s*(?:<@)?@?(U[A-Z0-9]+)(?:\|[^>]+)?>?`)
	q = reFrom.ReplaceAllString(q, "from:$1")

	reTo := regexp.MustCompile(`\bto:\s*(?:<@)?@?(U[A-Z0-9]+)(?:\|[^>]+)?>?`)
	q = reTo.ReplaceAllString(q, "to:$1")

	// Strip unresolved channel ID filters: in:#C09H8S8A0VD
	// Raw Slack channel IDs are not supported in the in: search filter;
	// keeping them returns 0 results. Better to search without a channel filter.
	reRawChannelFilter := regexp.MustCompile(`\bin:#[CG][A-Z0-9]{8,}\b`)
	q = strings.TrimSpace(reRawChannelFilter.ReplaceAllString(q, ""))
	// Also, strip leftover <#CHANID> tokens the LLM might have included verbatim.
	reRawChannelMention := regexp.MustCompile(`<#[CG][A-Z0-9]{8,}(?:\|[^>]*)?>`)
	q = strings.TrimSpace(reRawChannelMention.ReplaceAllString(q, ""))
	// Collapse multiple spaces left by removals.
	q = strings.Join(strings.Fields(q), " ")

	return strings.TrimSpace(q)
}

func backoffWithJitter(base time.Duration, attempt int) time.Duration {
	// exponential backoff: base * 2^(attempt-1), capped
	multi := math.Pow(2, float64(attempt-1))
	d := time.Duration(float64(base) * multi)
	const capDelay = 6 * time.Second
	if d > capDelay {
		d = capDelay
	}
	// full jitter in [0.7..1.3]
	j := 0.7 + rand.Float64()*0.6
	return time.Duration(float64(d) * j)
}
