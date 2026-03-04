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
	"strings"
	"time"
)

// reFromUserID matches "from:USERID" patterns in a Slack search query where
// USERID is a raw Slack user ID (starts with U or W, all caps alphanumeric).
var reFromUserID = regexp.MustCompile(`\bfrom:([UW][A-Z0-9]+)\b`)

// ResolveUserMentions replaces <@USERID> and <@USERID|name> patterns in text
// with @username. When the display name is already embedded after |, it is used
// directly. Otherwise, GetUsernameByID is called (which fast-paths via the cached
// user token owner, avoiding the need for users:read scope in that case).
func (c *Client) ResolveUserMentions(text string) string {
	return reSlackUserMention.ReplaceAllStringFunc(text, func(m string) string {
		sub := reSlackUserMention.FindStringSubmatch(m)
		if len(sub) < 3 {
			return m
		}
		id := sub[1]
		name := strings.TrimSpace(sub[2])
		if name != "" {
			return "@" + name
		}
		username, err := c.GetUsernameByID(id)
		if err != nil {
			return m // keep original if you can't resolve
		}
		return "@" + username
	})
}

// GetUsernameByID calls users.info and returns the Slack "name" (handle)
// E.g. "user.name". It prefers the user token when available.
func (c *Client) GetUsernameByID(userID string) (string, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return "", errors.New("empty user id")
	}

	// Fast path: if this is the user token owner, we already know the username
	// without needing users:read scope.
	if userID == c.UserTokenUserID && c.UserTokenUsername != "" {
		return c.UserTokenUsername, nil
	}

	token := c.UserToken
	if token == "" {
		token = c.BotToken
	}
	if token == "" {
		return "", errors.New("missing slack token")
	}

	u := fmt.Sprintf("%s/users.info?user=%s", c.APIBaseURL, url.QueryEscape(userID))
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.Do(req, 10*time.Second)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("slack status=%d body=%s", resp.StatusCode, preview(string(rb), 300))
	}

	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		User  struct {
			Name string `json:"name"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return "", err
	}
	if !out.OK {
		return "", fmt.Errorf("users.info error: %s", out.Error)
	}
	if strings.TrimSpace(out.User.Name) == "" {
		return "", errors.New("users.info returned empty name")
	}
	return out.User.Name, nil
}

// ResolveUserIDsInQuery replaces from:USERID tokens with from:@username so
// that the Slack search API filters messages by the correct user.
// If the user ID cannot be resolved (e.g., missing users:read scope), the
// from:USERID token is removed from the query — the Slack search API does not
// understand raw user IDs as a from: filter, so leaving it would silently
// break user filtering. The search runs more broadly in that case.
func (c *Client) ResolveUserIDsInQuery(q string) string {
	result := reFromUserID.ReplaceAllStringFunc(q, func(match string) string {
		sub := reFromUserID.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		userID := sub[1]
		name, err := c.GetUsernameByID(userID)
		if err != nil {
			log.Printf("[SLACK] ResolveUserIDsInQuery: could not resolve %s (add users:read scope to fix): %v", userID, err)
			// Return an empty string to remove the broken filter; the Slack search
			// API ignores from:USERID and would return unfiltered results anyway.
			return ""
		}
		log.Printf("[SLACK] resolved from:%s → from:@%s", userID, name)
		return "from:@" + name
	})
	// Collapse any extra spaces left by removals.
	return strings.Join(strings.Fields(result), " ")
}
