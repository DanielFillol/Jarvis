package slack

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ListChannels returns the public channels the bot is a member of (up to 200).
// It tries the bot token first; if that fails with missing_scope, retries with
// the user token which typically has broader channel access.
func (c *Client) ListChannels() ([]ChannelInfo, error) {
	var tokens []string
	if c.BotToken != "" {
		tokens = append(tokens, c.BotToken)
	}
	if c.UserToken != "" {
		tokens = append(tokens, c.UserToken)
	}
	if len(tokens) == 0 {
		return nil, errors.New("missing Slack token")
	}

	type chanResp struct {
		OK       bool   `json:"ok"`
		Error    string `json:"error"`
		Channels []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			IsMember bool   `json:"is_member"`
		} `json:"channels"`
	}

	u := fmt.Sprintf("%s/conversations.list?types=public_channel&exclude_archived=true&limit=200", c.APIBaseURL)

	for _, token := range tokens {
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := c.Do(req, 10*time.Second)
		if err != nil {
			return nil, err
		}

		rb, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var data chanResp
		if err := json.Unmarshal(rb, &data); err != nil {
			return nil, err
		}

		if !data.OK {
			// missing_scope: try next token
			if data.Error == "missing_scope" {
				log.Printf("[SLACK] ListChannels: token missing_scope, trying next token")
				continue
			}
			return nil, fmt.Errorf("conversations.list error: %s", data.Error)
		}

		var out []ChannelInfo
		for _, ch := range data.Channels {
			if ch.IsMember && ch.Name != "" {
				out = append(out, ChannelInfo{ID: ch.ID, Name: ch.Name})
			}
		}
		return out, nil
	}
	return nil, errors.New("conversations.list: no token with required scope")
}

// ResolveChannelMentions replaces <#CHANID> and <#CHANID|name> patterns
// in text with #channel-name, fetching the name from the Slack API when
// the name is not embedded in the mention.  This allows the router LLM
// to generate correct in:#channel-name search filters.
func (c *Client) ResolveChannelMentions(text string) string {
	return reSlackChannelMention.ReplaceAllStringFunc(text, func(m string) string {
		sub := reSlackChannelMention.FindStringSubmatch(m)
		if len(sub) < 3 {
			return m
		}
		id := sub[1]
		name := strings.TrimSpace(sub[2])
		if name != "" {
			return "#" + name
		}
		if resolved := c.GetChannelName(id); resolved != "" {
			log.Printf("[SLACK] resolved channel %s → #%s", id, resolved)
			return "#" + resolved
		}
		log.Printf("[SLACK] could not resolve channel %s — keeping raw ID for query stripping", id)
		return "<#" + id + ">"
	})
}

// GetChannelName resolves a Slack channel ID to its display name via
// conversations.info.  Returns an empty string on failure so callers
// can fall back gracefully.
func (c *Client) GetChannelName(channelID string) string {
	channelID = strings.TrimSpace(channelID)
	if channelID == "" {
		return ""
	}
	// Prefer user token: broader channel access (bot may not be a member).
	// Fall back to bot token if the user token is unavailable.
	var tokens []string
	if c.UserToken != "" {
		tokens = append(tokens, c.UserToken)
	}
	if c.BotToken != "" {
		tokens = append(tokens, c.BotToken)
	}
	if len(tokens) == 0 {
		return ""
	}
	u := fmt.Sprintf("%s/conversations.info?channel=%s", c.APIBaseURL, url.QueryEscape(channelID))
	var out struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
		Channel struct {
			Name string `json:"name"`
		} `json:"channel"`
	}
	for _, token := range tokens {
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := c.Do(req, 10*time.Second)
		if err != nil {
			continue
		}
		rb, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		out = struct {
			OK      bool   `json:"ok"`
			Error   string `json:"error"`
			Channel struct {
				Name string `json:"name"`
			} `json:"channel"`
		}{}
		if err := json.Unmarshal(rb, &out); err != nil {
			log.Printf("[SLACK] GetChannelName %s: json unmarshal error: %v", channelID, err)
			continue
		}
		if out.OK && out.Channel.Name != "" {
			return out.Channel.Name
		}
		log.Printf("[SLACK] GetChannelName %s: api error=%q", channelID, out.Error)
		// channel_not_found is definitive — no token will help.
		// not_in_channel only means this token's user isn't in the channel; try the next token.
		if out.Error == "channel_not_found" {
			break
		}
	}
	return ""
}

// GetChannelHistoryForPeriod fetches up to limit messages from a channel
// between oldest and latest.  It requires channels:history (or groups:history)
// scope.  The user token is tried first (broader public-channel access), then
// the bot token.  The channel name in the returned messages is set to channelID
// since we may not have channels:read to resolve it.
func (c *Client) GetChannelHistoryForPeriod(channelID string, oldest, latest time.Time, limit int) ([]SearchMessage, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 200 {
		limit = 200
	}
	var tokens []string
	if c.UserToken != "" {
		tokens = append(tokens, c.UserToken)
	}
	if c.BotToken != "" {
		tokens = append(tokens, c.BotToken)
	}
	if len(tokens) == 0 {
		return nil, errors.New("missing Slack token")
	}

	oldestTs := fmt.Sprintf("%.6f", float64(oldest.UnixNano())/1e9)
	latestTs := fmt.Sprintf("%.6f", float64(latest.UnixNano())/1e9)
	u := fmt.Sprintf("%s/conversations.history?channel=%s&oldest=%s&latest=%s&limit=%d",
		c.APIBaseURL, url.QueryEscape(channelID), oldestTs, latestTs, limit)

	for _, token := range tokens {
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := c.Do(req, 20*time.Second)
		if err != nil {
			continue
		}
		rb, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var data struct {
			OK       bool   `json:"ok"`
			Error    string `json:"error"`
			Messages []struct {
				Type    string `json:"type"`
				Subtype string `json:"subtype,omitempty"`
				User    string `json:"user,omitempty"`
				Text    string `json:"text"`
				Ts      string `json:"ts"`
				BotID   string `json:"bot_id,omitempty"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(rb, &data); err != nil {
			continue
		}
		if !data.OK {
			log.Printf("[SLACK] GetChannelHistoryForPeriod %s: api error=%q", channelID, data.Error)
			if data.Error == "missing_scope" || data.Error == "not_in_channel" {
				continue // try next token
			}
			return nil, fmt.Errorf("conversations.history error: %s", data.Error)
		}

		var out []SearchMessage
		for _, m := range data.Messages {
			if m.BotID != "" || m.Subtype != "" {
				continue
			}
			txt := cleanSlackText(m.Text)
			if txt == "" {
				continue
			}
			out = append(out, SearchMessage{
				Text:     txt,
				Channel:  channelID,
				Username: m.User,
				Ts:       m.Ts,
			})
		}
		return out, nil
	}
	return nil, fmt.Errorf("GetChannelHistoryForPeriod %s: no token succeeded (missing channels:history scope?)", channelID)
}
