package slack

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// GetThreadHistory fetches up to limit messages from a Slack thread and
// returns a concatenated text representation.  Messages authored by
// bots include the bot ID instead of a user ID.
func (c *Client) GetThreadHistory(channel, threadTs string, limit int) (string, error) {
	if c.BotToken == "" {
		return "", errors.New("missing Slack bot token")
	}
	if limit <= 0 {
		limit = 20
	}
	u := fmt.Sprintf("%s/conversations.replies?channel=%s&ts=%s&limit=%d", c.APIBaseURL, url.QueryEscape(channel), url.QueryEscape(threadTs), limit)

	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+c.BotToken)
	resp, err := c.Do(req, 15*time.Second)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	rb, _ := io.ReadAll(resp.Body)
	var data struct {
		OK       bool `json:"ok"`
		Messages []struct {
			User  string `json:"user"`
			Text  string `json:"text"`
			BotID string `json:"bot_id"`
		} `json:"messages"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rb, &data); err != nil {
		return "", err
	}

	if !data.OK {
		return "", fmt.Errorf("slack error: %s", data.Error)
	}
	var b strings.Builder
	for _, m := range data.Messages {
		txt := strings.TrimSpace(m.Text)
		if txt == "" {
			continue
		}
		author := m.User
		if author == "" && m.BotID != "" {
			author = "BOT"
		}
		b.WriteString(author + ": " + cleanSlackText(txt) + "\n")
	}
	return b.String(), nil
}

// GetThreadHistoryFull fetches the full message history for a Slack thread via
// conversations.replies with cursor-based pagination. It returns a concatenated
// text representation (root and replies) ordered by ts, with basic subtype
// filtering and guardrails for maxMessages and maxChars.
//
// It is intended for "permalink mode" where the user provides an explicit Slack
// thread link, and we want high-fidelity context.
func (c *Client) GetThreadHistoryFull(channel, threadTs string, maxMessages int, maxChars int) (string, error) {
	if c.BotToken == "" {
		return "", errors.New("missing Slack bot token")
	}

	channel = strings.TrimSpace(channel)
	threadTs = strings.TrimSpace(threadTs)
	if channel == "" || threadTs == "" {
		return "", errors.New("missing channel or thread ts")
	}

	if maxMessages <= 0 {
		maxMessages = 400
	}
	if maxChars <= 0 {
		maxChars = 40000
	}

	type replyMsg struct {
		Type     string `json:"type,omitempty"`
		Subtype  string `json:"subtype,omitempty"`
		User     string `json:"user,omitempty"`
		Text     string `json:"text,omitempty"`
		BotID    string `json:"bot_id,omitempty"`
		Ts       string `json:"ts,omitempty"`
		ThreadTs string `json:"thread_ts,omitempty"`
	}
	type repliesResp struct {
		OK               bool       `json:"ok"`
		Error            string     `json:"error,omitempty"`
		Messages         []replyMsg `json:"messages"`
		HasMore          bool       `json:"has_more,omitempty"`
		ResponseMetadata struct {
			NextCursor string `json:"next_cursor,omitempty"`
		} `json:"response_metadata,omitempty"`
	}

	pageSize := 100
	rootTs := threadTs
	cursor := ""
	redirectedToRoot := false

	var all []replyMsg
	useUserToken := false
	for {
		u := fmt.Sprintf("%s/conversations.replies?channel=%s&ts=%s&limit=%d", c.APIBaseURL, url.QueryEscape(channel), url.QueryEscape(rootTs), pageSize)
		if strings.TrimSpace(cursor) != "" {
			u += "&cursor=" + url.QueryEscape(cursor)
		}
		req, _ := http.NewRequest("GET", u, nil)
		token := c.BotToken
		if useUserToken && c.UserToken != "" {
			token = c.UserToken
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := c.Do(req, 20*time.Second)
		if err != nil {
			return "", err
		}
		rb, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 429 {
			return "", fmt.Errorf("rate_limited retry_after=%s", resp.Header.Get("Retry-After"))
		}
		if resp.StatusCode >= 300 {
			return "", fmt.Errorf("slack status=%d body=%s", resp.StatusCode, preview(string(rb), 400))
		}

		var data repliesResp
		if err := json.Unmarshal(rb, &data); err != nil {
			return "", err
		}
		if !data.OK {
			// If bot token failed with not_in_channel, and we have user token, retry with user token
			if !useUserToken && data.Error == "not_in_channel" && c.UserToken != "" {
				useUserToken = true
				cursor = ""
				all = nil
				redirectedToRoot = false
				continue
			}
			return "", fmt.Errorf("slack error: %s", data.Error)
		}

		// If the provided ts is a reply, Slack may indicate the thread root via thread_ts.
		// When detected, restart pagination from the root thread_ts.
		if !redirectedToRoot && cursor == "" && len(data.Messages) > 0 {
			if rt := strings.TrimSpace(data.Messages[0].ThreadTs); rt != "" && rt != rootTs {
				rootTs = rt
				cursor = ""
				all = nil
				redirectedToRoot = true
				continue
			}
		}

		for _, m := range data.Messages {
			if shouldIncludeThreadMessage(m.Subtype, m.Text) {
				all = append(all, m)
				if len(all) >= maxMessages {
					break
				}
			}
		}
		if len(all) >= maxMessages {
			break
		}

		next := strings.TrimSpace(data.ResponseMetadata.NextCursor)
		if !data.HasMore || next == "" {
			break
		}
		cursor = next
	}

	// Ensure deterministic ordering even with edits/subtypes in large threads.
	sort.SliceStable(all, func(i, j int) bool {
		aiSec, aiMicro := parseSlackTs(all[i].Ts)
		ajSec, ajMicro := parseSlackTs(all[j].Ts)
		if aiSec != ajSec {
			return aiSec < ajSec
		}
		return aiMicro < ajMicro
	})

	var b strings.Builder
	for _, m := range all {
		txt := cleanSlackTextMax(m.Text, 800)
		if strings.TrimSpace(txt) == "" {
			continue
		}
		author := strings.TrimSpace(m.User)
		if author == "" && strings.TrimSpace(m.BotID) != "" {
			author = "BOT"
		}
		if author == "" {
			author = "UNKNOWN"
		}
		line := fmt.Sprintf("ts=%s %s: %s\n", strings.TrimSpace(m.Ts), author, txt)
		if b.Len()+len(line) > maxChars {
			b.WriteString("... [truncado: thread muito longa]\n")
			break
		}
		b.WriteString(line)
	}
	return b.String(), nil
}

// GetPermalink returns a permalink for a given message timestamp in a
// channel.  An empty string and error are returned if the call fails.
func (c *Client) GetPermalink(channel, messageTs string) (string, error) {
	if c.BotToken == "" {
		return "", errors.New("missing Slack bot token")
	}

	u := fmt.Sprintf("%s/chat.getPermalink?channel=%s&message_ts=%s", c.APIBaseURL, url.QueryEscape(channel), url.QueryEscape(messageTs))
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+c.BotToken)
	resp, err := c.Do(req, 10*time.Second)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	rb, _ := io.ReadAll(resp.Body)
	var out struct {
		OK        bool   `json:"ok"`
		Error     string `json:"error,omitempty"`
		Permalink string `json:"permalink,omitempty"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return "", err
	}

	if !out.OK {
		return "", fmt.Errorf("chat.getPermalink error: %s", out.Error)
	}
	return out.Permalink, nil
}
