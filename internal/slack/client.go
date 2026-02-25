// internal/slack/client.go
package slack

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/DanielFillol/Jarvis/internal/config"
)

// Client provides a thin wrapper around Slack's HTTP APIs used by
// Jarvis.  It encapsulates configuration details such as tokens and
// signing secrets.
type Client struct {
	BotToken       string
	UserToken      string
	SigningSecret  string
	SearchMaxPages int
	BotUserID      string
	APIBaseURL     string
	HTTPClient     *http.Client
}

// NewClient constructs a Slack client from the supplied configuration.  The
// client may later be updated with the BotUserID returned from auth.test.
func NewClient(cfg config.Config) *Client {
	return &Client{
		BotToken:       cfg.SlackBotToken,
		UserToken:      cfg.SlackUserToken,
		SigningSecret:  cfg.SlackSigningSecret,
		SearchMaxPages: cfg.SlackSearchMaxPages,
		APIBaseURL:     "https://slack.com/api",
	}
}

func (c *Client) apiBase() string {
	b := strings.TrimSpace(c.APIBaseURL)
	if b == "" {
		b = "https://slack.com/api"
	}
	return strings.TrimRight(b, "/")
}

func (c *Client) do(req *http.Request, timeout time.Duration) (*http.Response, error) {
	if c.HTTPClient != nil {
		return c.HTTPClient.Do(req)
	}
	client := &http.Client{Timeout: timeout}
	return client.Do(req)
}

// AuthTest calls Slack's auth.test API to retrieve the bot user ID. On
// success the BotUserID field of the client is updated, and the ID is
// returned.
func (c *Client) AuthTest() (string, error) {
	if c.BotToken == "" {
		return "", errors.New("missing Slack bot token")
	}
	req, _ := http.NewRequest("POST", c.apiBase()+"/auth.test", nil)
	req.Header.Set("Authorization", "Bearer "+c.BotToken)
	resp, err := c.do(req, 10*time.Second)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out struct {
		OK     bool   `json:"ok"`
		UserID string `json:"user_id"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return "", err
	}
	if !out.OK {
		return "", fmt.Errorf("auth.test error: %s", out.Error)
	}
	c.BotUserID = out.UserID
	return out.UserID, nil
}

// PostMessage posts a message to Slack.  A non-empty threadTs will cause
// the message to be sent as a reply in the specified thread.  An error
// is returned if the message could not be sent.
func (c *Client) PostMessage(channel, threadTs, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("no_text")
	}
	if c.BotToken == "" {
		return errors.New("missing Slack bot token")
	}
	payload := SlackPostMessageRequest{
		Channel:  channel,
		Text:     text,
		ThreadTs: threadTs,
	}
	b, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", c.apiBase()+"/chat.postMessage", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+c.BotToken)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := c.do(req, 15*time.Second)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("slack status=%d body=%s", resp.StatusCode, preview(string(rb), 400))
	}
	var slackResp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	_ = json.Unmarshal(rb, &slackResp)
	if !slackResp.OK {
		return fmt.Errorf("slack api error: %s", slackResp.Error)
	}
	return nil
}

// SearchMessagesAll performs a Slack search across messages.  It
// iterates through pages up to the configured maximum and returns a
// slice of flattened results.  If no user token is configured, an
// error is returned.
//
// Queries containing " OR " (top-level, outside quoted strings) are
// split into independent sub-searches and their results merged.  This
// works around the Slack search.messages API not reliably supporting
// complex boolean expressions that combine quoted phrases with OR —
// a combination that works in the Slack UI but returns 0 results via
// the API.
func (c *Client) SearchMessagesAll(query string) ([]SlackSearchMessage, error) {
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
	var merged []SlackSearchMessage
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

// searchMessagesPages executes a single Slack search query with
// cursor-based page iteration and returns up to 200 raw matches.
func (c *Client) searchMessagesPages(query string) ([]SlackSearchMessage, error) {
	maxPages := c.SearchMaxPages
	if maxPages < 1 {
		maxPages = 10
	}
	if maxPages > 50 {
		maxPages = 50
	}
	var out []SlackSearchMessage
	for page := 1; page <= maxPages; page++ {
		u := fmt.Sprintf("%s/search.messages?query=%s&count=20&page=%d", c.apiBase(), url.QueryEscape(query), page)
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("Authorization", "Bearer "+c.UserToken)
		resp, err := c.do(req, 20*time.Second)
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
		var data SlackSearchMessagesResp
		if err := json.Unmarshal(rb, &data); err != nil {
			return nil, err
		}
		if !data.OK {
			return nil, fmt.Errorf("slack error: %s", data.Error)
		}
		for _, m := range data.Messages.Matches {
			out = append(out, SlackSearchMessage{
				Text:      cleanSlackText(m.Text),
				Permalink: m.Permalink,
				Channel:   m.Channel.Name,
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
	uniq := make([]SlackSearchMessage, 0, len(out))
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

// splitTopLevelOR splits a Slack search query on " OR " tokens that
// appear outside of double-quoted strings.  It returns a slice with
// the individual clauses.  If no top-level OR is found, a single-
// element slice containing the original query is returned.
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
	u := fmt.Sprintf("%s/conversations.replies?channel=%s&ts=%s&limit=%d", c.apiBase(), url.QueryEscape(channel), url.QueryEscape(threadTs), limit)
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+c.BotToken)
	resp, err := c.do(req, 15*time.Second)
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
// text representation (root + replies) ordered by ts, with basic subtype
// filtering and guardrails for maxMessages and maxChars.
//
// It is intended for "permalink mode" where the user provides an explicit Slack
// thread link and we want high-fidelity context.
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
		u := fmt.Sprintf("%s/conversations.replies?channel=%s&ts=%s&limit=%d", c.apiBase(), url.QueryEscape(channel), url.QueryEscape(rootTs), pageSize)
		if strings.TrimSpace(cursor) != "" {
			u += "&cursor=" + url.QueryEscape(cursor)
		}
		req, _ := http.NewRequest("GET", u, nil)
		token := c.BotToken
		if useUserToken && c.UserToken != "" {
			token = c.UserToken
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := c.do(req, 20*time.Second)
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
			// If bot token failed with not_in_channel and we have user token, retry with user token
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

// ThreadHasBot determines whether the bot has participated in the
// specified thread.  It returns true if any message in the thread was
// authored by the bot or another bot (via bot_id), otherwise false.
func (c *Client) ThreadHasBot(channel, threadTs string) (bool, error) {
	if c.BotToken == "" {
		return false, errors.New("missing Slack bot token")
	}
	if c.BotUserID == "" {
		return false, nil
	}
	u := fmt.Sprintf("%s/conversations.replies?channel=%s&ts=%s&limit=30", c.apiBase(), url.QueryEscape(channel), url.QueryEscape(threadTs))
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+c.BotToken)
	resp, err := c.do(req, 10*time.Second)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	var data struct {
		OK       bool `json:"ok"`
		Messages []struct {
			User  string `json:"user"`
			BotID string `json:"bot_id"`
		} `json:"messages"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rb, &data); err != nil {
		return false, err
	}
	if !data.OK {
		return false, fmt.Errorf("slack error: %s", data.Error)
	}
	for _, m := range data.Messages {
		if m.User == c.BotUserID || m.BotID != "" {
			return true, nil
		}
	}
	return false, nil
}

// GetPermalink returns a permalink for a given message timestamp in a
// channel.  An empty string and error are returned if the call fails.
func (c *Client) GetPermalink(channel, messageTs string) (string, error) {
	if c.BotToken == "" {
		return "", errors.New("missing Slack bot token")
	}
	u := fmt.Sprintf("%s/chat.getPermalink?channel=%s&message_ts=%s", c.apiBase(), url.QueryEscape(channel), url.QueryEscape(messageTs))
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+c.BotToken)
	resp, err := c.do(req, 10*time.Second)
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

// PostMessageAndGetTS posts a message to Slack and returns the timestamp
// of the posted message.  It is used to obtain a handle for later updates.
func (c *Client) PostMessageAndGetTS(channel, threadTs, text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("no_text")
	}
	if c.BotToken == "" {
		return "", errors.New("missing Slack bot token")
	}
	payload := SlackPostMessageRequest{
		Channel:  channel,
		Text:     text,
		ThreadTs: threadTs,
	}
	b, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", c.apiBase()+"/chat.postMessage", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+c.BotToken)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := c.do(req, 15*time.Second)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	var slackResp struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error,omitempty"`
		Ts      string `json:"ts"`
		Channel string `json:"channel"`
	}
	_ = json.Unmarshal(rb, &slackResp)
	if !slackResp.OK {
		return "", fmt.Errorf("slack api error: %s", slackResp.Error)
	}
	return slackResp.Ts, nil
}

// UpdateMessage updates an existing Slack message in-place.  It is used
// to replace a "buscando..." placeholder with the actual answer.
func (c *Client) UpdateMessage(channel, ts, text string) error {
	text = strings.TrimSpace(text)
	if text == "" || ts == "" {
		return errors.New("missing text or ts")
	}
	if c.BotToken == "" {
		return errors.New("missing Slack bot token")
	}
	payload := map[string]string{
		"channel": channel,
		"ts":      ts,
		"text":    text,
	}
	b, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", c.apiBase()+"/chat.update", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+c.BotToken)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := c.do(req, 15*time.Second)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	var slackResp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	_ = json.Unmarshal(rb, &slackResp)
	if !slackResp.OK {
		return fmt.Errorf("slack update error: %s", slackResp.Error)
	}
	return nil
}

// DeleteMessage deletes a message the bot posted via chat.delete.
func (c *Client) DeleteMessage(channel, ts string) error {
	if c.BotToken == "" {
		return errors.New("missing Slack bot token")
	}
	payload := map[string]string{"channel": channel, "ts": ts}
	b, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", c.apiBase()+"/chat.delete", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+c.BotToken)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := c.do(req, 10*time.Second)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	var slackResp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	_ = json.Unmarshal(rb, &slackResp)
	if !slackResp.OK {
		return fmt.Errorf("slack delete error: %s", slackResp.Error)
	}
	return nil
}

// VerifySignature verifies the Slack signing signature on the incoming
// request.  It returns an error if the signature is invalid or the
// timestamp is stale.  The raw body bytes must be the exact bytes used
// to compute the signature.
func (c *Client) VerifySignature(r *http.Request, body []byte) error {
	if c.SigningSecret == "" {
		return errors.New("missing Slack signing secret")
	}
	ts := r.Header.Get("X-Slack-Request-Timestamp")
	sig := r.Header.Get("X-Slack-Signature")
	if ts == "" || sig == "" {
		return errors.New("missing slack signature headers")
	}
	tsInt, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return errors.New("invalid timestamp")
	}
	if time.Since(time.Unix(tsInt, 0)) > 5*time.Minute {
		return errors.New("stale timestamp")
	}
	base := "v0:" + ts + ":" + string(body)
	mac := hmac.New(sha256.New, []byte(c.SigningSecret))
	_, _ = mac.Write([]byte(base))
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return errors.New("signature mismatch")
	}
	return nil
}

// Helper preview returns a shortened version of a string for logging.
func preview(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// cleanSlackText normalizes Slack message text by collapsing whitespace
// and truncating long strings.
func cleanSlackText(s string) string {
	return cleanSlackTextMax(s, 420)
}

var (
	reSlackUserMention    = regexp.MustCompile(`<@([UW][A-Z0-9]+)(?:\|([^>]+))?>`)
	reSlackChannelMention = regexp.MustCompile(`<#([CG][A-Z0-9]+)(?:\|([^>]+))?>`)
	reSlackUsergroup      = regexp.MustCompile(`<!subteam\^([A-Z0-9]+)(?:\|([^>]+))?>`)
	reSlackSpecial        = regexp.MustCompile(`<!(here|channel|everyone)>`)
	reSlackLink           = regexp.MustCompile(`<((?:https?://|mailto:)[^>|]+)(?:\|([^>]+))?>`)
)

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
	s = reSlackUsergroup.ReplaceAllStringFunc(s, func(m string) string {
		sub := reSlackUsergroup.FindStringSubmatch(m)
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

// Sort is needed for sorting search results; import it here.
// Without explicitly referencing sort.Sort, the compiler would
// complain about unused import if sort is not otherwise used.
// We alias it here to make the dependency explicit.
var _ = sort.Strings

// GetChannelName resolves a Slack channel ID to its display name via
// conversations.info.  Returns an empty string on failure so callers
// can fall back gracefully.
func (c *Client) GetChannelName(channelID string) string {
	channelID = strings.TrimSpace(channelID)
	if channelID == "" {
		return ""
	}
	// Prefer user token: broader channel access (bot may not be a member).
	// Fall back to bot token if user token is unavailable.
	tokens := []string{}
	if c.UserToken != "" {
		tokens = append(tokens, c.UserToken)
	}
	if c.BotToken != "" {
		tokens = append(tokens, c.BotToken)
	}
	if len(tokens) == 0 {
		return ""
	}
	u := fmt.Sprintf("%s/conversations.info?channel=%s", c.apiBase(), url.QueryEscape(channelID))
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
		resp, err := c.do(req, 10*time.Second)
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
		// not_in_channel only means this token's user isn't in the channel; try next token.
		if out.Error == "channel_not_found" {
			break
		}
	}
	return ""
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

// GetUsernameByID calls users.info and returns the Slack "name" (handle)
// e.g. "daniel.fillol". It prefers the user token when available.
func (c *Client) GetUsernameByID(userID string) (string, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return "", errors.New("empty user id")
	}

	token := c.UserToken
	if token == "" {
		token = c.BotToken
	}
	if token == "" {
		return "", errors.New("missing slack token")
	}

	u := fmt.Sprintf("%s/users.info?user=%s", c.apiBase(), url.QueryEscape(userID))
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.do(req, 10*time.Second)
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

// ListChannels returns the public channels the bot is a member of (up to 200).
func (c *Client) ListChannels() ([]SlackChannelInfo, error) {
	if c.BotToken == "" {
		return nil, errors.New("missing Slack bot token")
	}
	u := fmt.Sprintf("%s/conversations.list?types=public_channel&exclude_archived=true&limit=200", c.apiBase())
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+c.BotToken)
	resp, err := c.do(req, 10*time.Second)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	var data struct {
		OK       bool   `json:"ok"`
		Error    string `json:"error"`
		Channels []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			IsMember bool   `json:"is_member"`
		} `json:"channels"`
	}
	if err := json.Unmarshal(rb, &data); err != nil {
		return nil, err
	}
	if !data.OK {
		return nil, fmt.Errorf("conversations.list error: %s", data.Error)
	}
	var out []SlackChannelInfo
	for _, ch := range data.Channels {
		if ch.IsMember && ch.Name != "" {
			out = append(out, SlackChannelInfo{ID: ch.ID, Name: ch.Name})
		}
	}
	return out, nil
}

// reFromUserID matches "from:USERID" patterns in a Slack search query where
// USERID is a raw Slack user ID (starts with U or W, all caps alphanumeric).
var reFromUserID = regexp.MustCompile(`\bfrom:([UW][A-Z0-9]+)\b`)

// ResolveUserIDsInQuery replaces from:USERID tokens with from:@username so
// that the Slack search API filters messages by the correct user.
// Unresolvable IDs are left unchanged so the search still runs.
func (c *Client) ResolveUserIDsInQuery(q string) string {
	return reFromUserID.ReplaceAllStringFunc(q, func(match string) string {
		sub := reFromUserID.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		userID := sub[1]
		name, err := c.GetUsernameByID(userID)
		if err != nil {
			log.Printf("[SLACK] ResolveUserIDsInQuery: could not resolve %s: %v", userID, err)
			return match
		}
		log.Printf("[SLACK] resolved from:%s → from:@%s", userID, name)
		return "from:@" + name
	})
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
		full := m[0]   // full matched segment
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
			// safe fallback: remove the invalid filter to avoid zeroing the search
			out = strings.ReplaceAll(out, r.full, "")
		}
	}

	// Also resolve bare <@USERID> and <@USERID|name> mentions to @username
	reBare := regexp.MustCompile(`<@((U|W)[A-Z0-9]+)(?:\|[^>]+)?>`)
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
