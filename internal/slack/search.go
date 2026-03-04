package slack

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// SearchMessage is an internal representation of a search message
// result used by higher layers in the application.  It flattens
// certain fields and normalizes names.
type SearchMessage struct {
	Text      string
	Permalink string
	Channel   string
	UserID    string // Slack user ID, e.g. "U067UM4RGB"
	Username  string
	Ts        string
	Score     float64
}

// SearchMessagesResp models the Slack search.messages response used
// by this application.  Only the fields accessed by the code are
// represented here.
type SearchMessagesResp struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	Messages struct {
		Total  int `json:"total"`
		Paging struct {
			Count int `json:"count"`
			Total int `json:"total"`
			Page  int `json:"page"`
			Pages int `json:"pages"`
		} `json:"paging"`
		Matches []struct {
			Text      string `json:"text"`
			Permalink string `json:"permalink"`
			Channel   struct {
				Name string `json:"name"`
			} `json:"channel"`
			User     string  `json:"user"`
			Username string  `json:"username"`
			Ts       string  `json:"ts"`
			Score    float64 `json:"score"`
		} `json:"matches"`
	} `json:"messages"`
}

// SearchMessagesAll performs a Slack search across messages.  It
// iterates through pages up to the configured maximum and returns a
// slice of flattened results.  If no user token is configured, an
// error is returned.
//
// Queries containing "OR" (top-level, outside quoted strings) are
// split into independent sub-searches, and their results merged.  This
// works around the Slack search.messages API not reliably supporting
// complex boolean expressions that combine quoted phrases with OR —
// a combination that works in the Slack UI but returns 0 results via
// the API.
func (c *Client) SearchMessagesAll(query string) ([]SearchMessage, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("empty query")
	}
	if c.UserToken == "" {
		return nil, errors.New("missing Slack user token (xoxp)")
	}

	query = c.rewriteFromToUserIDs(query)

	clauses := splitTopLevelOR(query)
	if len(clauses) == 1 {
		return c.searchMessagesPages(clauses[0])
	}

	// Execute each OR clause independently and merge results.
	log.Printf("[SLACK] OR query split into %d clauses", len(clauses))
	seen := map[string]bool{}
	var merged []SearchMessage
	for _, clause := range clauses {
		log.Printf("[SLACK] OR clause search: %q", clause)
		results, err := c.searchMessagesPages(clause)
		if err != nil {
			log.Printf("[SLACK] OR clause %q failed: %v (skipping)", clause, err)
			continue
		}
		for _, m := range results {
			key := m.Permalink
			if key == "" {
				key = m.Channel + "/" + m.Ts
			}
			if !seen[key] {
				seen[key] = true
				merged = append(merged, m)
			}
		}
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Score > merged[j].Score })
	if len(merged) > 80 {
		merged = merged[:80]
	}
	return merged, nil
}

func (c *Client) rewriteFromToUserIDs(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return q
	}

	// capture from/to with:
	// from:U123, from:@U123, from:<@U123>, from:<@U123|name>
	re := regexp.MustCompile(`\b(from|to):\s*(?:<@)?@?(U[A-Z0-9]+)(?:\|[^>]+)?>?`)
	matches := re.FindAllStringSubmatch(q, -1)
	if len(matches) == 0 {
		return q
	}

	// resolve unique IDs
	type repl struct {
		full   string
		which  string
		userID string
	}
	var reps []repl
	seen := map[string]bool{}
	var ids []string

	for _, m := range matches {
		if len(m) < 3 {
			continue
		}
		which := m[1]  // from|to
		userID := m[2] // U...
		full := m[0]   // full-matched segment
		reps = append(reps, repl{full: full, which: which, userID: userID})
		if !seen[userID] {
			seen[userID] = true
			ids = append(ids, userID)
		}
	}

	idToName := map[string]string{}
	for _, id := range ids {
		name, err := c.GetUsernameByID(id)
		if err != nil {
			// fallback: leave empty; the filter will be removed below
			continue
		}
		idToName[id] = name
	}

	out := q
	for _, r := range reps {
		if name := idToName[r.userID]; name != "" {
			out = strings.ReplaceAll(out, r.full, fmt.Sprintf("%s:@%s", r.which, name))
		} else {
			// fallback: use <@USERID> a format which Slack search accepts even without users:read scope
			out = strings.ReplaceAll(out, r.full, fmt.Sprintf("%s:<@%s>", r.which, r.userID))
		}
	}

	// Also resolve bare <@USERID> and <@USERID|name> mentions to @username
	reBare := regexp.MustCompile(`<@(([UW])[A-Z0-9]+)(?:\|[^>]+)?>`)
	bareMentions := reBare.FindAllStringSubmatch(out, -1)
	for _, m := range bareMentions {
		if len(m) < 2 {
			continue
		}
		userID := m[1]
		full := m[0]
		if _, already := idToName[userID]; !already {
			name, err := c.GetUsernameByID(userID)
			if err == nil && name != "" {
				idToName[userID] = name
			}
		}
		if name := idToName[userID]; name != "" {
			out = strings.ReplaceAll(out, full, "@"+name)
		}
	}

	// clean up double spaces and trim
	out = strings.Join(strings.Fields(out), " ")
	return strings.TrimSpace(out)
}

// searchMessagesPages executes a single Slack search query with
// cursor-based page iteration and returns up to 200 raw matches.
func (c *Client) searchMessagesPages(query string) ([]SearchMessage, error) {
	maxPages := c.SearchMaxPages
	if maxPages < 1 {
		maxPages = 10
	}
	if maxPages > 50 {
		maxPages = 50
	}
	var out []SearchMessage
	for page := 1; page <= maxPages; page++ {
		u := fmt.Sprintf("%s/search.messages?query=%s&count=20&page=%d", c.APIBaseURL, url.QueryEscape(query), page)
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("Authorization", "Bearer "+c.UserToken)
		resp, err := c.Do(req, 20*time.Second)
		if err != nil {
			return nil, err
		}
		rb, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 429 {
			return nil, fmt.Errorf("rate_limited retry_after=%s", resp.Header.Get("Retry-After"))
		}
		if resp.StatusCode >= 300 {
			return nil, fmt.Errorf("slack status=%d body=%s", resp.StatusCode, preview(string(rb), 300))
		}
		var data SearchMessagesResp
		if err := json.Unmarshal(rb, &data); err != nil {
			return nil, err
		}
		if !data.OK {
			return nil, fmt.Errorf("slack error: %s", data.Error)
		}
		for _, m := range data.Messages.Matches {
			out = append(out, SearchMessage{
				Text:      cleanSlackText(m.Text),
				Permalink: m.Permalink,
				Channel:   m.Channel.Name,
				UserID:    m.User,
				Username:  m.Username,
				Ts:        m.Ts,
				Score:     m.Score,
			})
		}
		if data.Messages.Paging.Page >= data.Messages.Paging.Pages || len(data.Messages.Matches) == 0 {
			break
		}
		if len(out) >= 200 {
			break
		}
	}
	// Sort by score descending and dedupe by permalink.
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	seen := map[string]bool{}
	uniq := make([]SearchMessage, 0, len(out))
	for _, m := range out {
		if m.Permalink != "" && seen[m.Permalink] {
			continue
		}
		if m.Permalink != "" {
			seen[m.Permalink] = true
		}
		uniq = append(uniq, m)
		if len(uniq) >= 80 {
			break
		}
	}
	return uniq, nil
}
