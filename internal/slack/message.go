package slack

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// EventEnvelope represents the outer wrapper for events sent by the
// Slack Events API.  See https://api.slack.com/apis/connections/events-api
// for details.  Only the fields used by this application are defined
// here.
type EventEnvelope struct {
	Type      string          `json:"type"`
	Challenge string          `json:"challenge,omitempty"`
	Event     json.RawMessage `json:"event,omitempty"`
}

// MessageEvent represents a Slack message event.  It omits fields
// that are not currently used by this application.
type MessageEvent struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype,omitempty"`
	Text      string `json:"text"`
	User      string `json:"user,omitempty"`
	BotID     string `json:"bot_id,omitempty"`
	Channel   string `json:"channel"`
	Ts        string `json:"ts"`
	ThreadTs  string `json:"thread_ts,omitempty"`
	DeletedTs string `json:"deleted_ts,omitempty"`
	Files     []File `json:"files,omitempty"`
	// Message is populated for message_changed events (e.g., edits and tombstone deletions in DMs).
	Message *InnerMessage `json:"message,omitempty"`
}

// InnerMessage is the nested "message" object inside message_changed events.
type InnerMessage struct {
	Ts      string `json:"ts"`
	Subtype string `json:"subtype,omitempty"`
	Text    string `json:"text"`
	Hidden  bool   `json:"hidden,omitempty"`
}

// PostMessageRequest encapsulates the body of a chat.postMessage call.
type PostMessageRequest struct {
	Channel  string `json:"channel"`
	Text     string `json:"text"`
	ThreadTs string `json:"thread_ts,omitempty"`
}

// MessageTracker keeps a mapping from originTs (the user's triggering message)
// to botTs (the bot's reply) so that when a user deletes their message, the bot
// can delete its own reply automatically.
type MessageTracker struct {
	mu   sync.RWMutex
	data map[string]string // key: channel+":"+originTs → botTs
}

// NewMessageTracker constructs an empty MessageTracker.
func NewMessageTracker() *MessageTracker {
	return &MessageTracker{data: make(map[string]string)}
}
func key(channel, originTs string) string { return channel + ":" + originTs }

// Get returns the bot reply ts for a given origin message, or "" if not found.
func (t *MessageTracker) Get(channel, originTs string) string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.data[key(channel, originTs)]
}

// Delete removes a tracked entry.
func (t *MessageTracker) Delete(channel, originTs string) {
	t.mu.Lock()
	delete(t.data, key(channel, originTs))
	t.mu.Unlock()
}

// Track records that botTs is the bot reply to the user message at originTs.
func (t *MessageTracker) Track(channel, originTs, botTs string) {
	t.mu.Lock()
	t.data[key(channel, originTs)] = botTs
	t.mu.Unlock()
}

// PostMessage posts a message to Slack.  A non-empty threadTs will cause
// the message to be sent as a reply in the specified thread.  An error
// is returned if the message could not be sent.
func (c *Client) PostMessage(channel, threadTs, text string) error {
	if c.BotToken == "" {
		return errors.New("missing Slack bot token")
	}

	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("no_text")
	}

	payload := PostMessageRequest{
		Channel:  channel,
		Text:     text,
		ThreadTs: threadTs,
	}
	b, _ := json.Marshal(payload)

	req, _ := http.NewRequest("POST", c.APIBaseURL+"/chat.postMessage", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+c.BotToken)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := c.Do(req, 15*time.Second)
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

// DeleteMessage deletes a message the bot posted via chat.delete.
func (c *Client) DeleteMessage(channel, ts string) error {
	if c.BotToken == "" {
		return errors.New("missing Slack bot token")
	}
	payload := map[string]string{"channel": channel, "ts": ts}
	b, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", c.APIBaseURL+"/chat.delete", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+c.BotToken)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := c.Do(req, 10*time.Second)
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

// PostMessageAndGetTS posts a message to Slack and returns the timestamp
// of the posted message.  It is used to get a handle for later updates.
func (c *Client) PostMessageAndGetTS(channel, threadTs, text string) (string, error) {
	if c.BotToken == "" {
		return "", errors.New("missing Slack bot token")
	}

	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("no_text")
	}

	payload := PostMessageRequest{
		Channel:  channel,
		Text:     text,
		ThreadTs: threadTs,
	}
	b, _ := json.Marshal(payload)

	req, _ := http.NewRequest("POST", c.APIBaseURL+"/chat.postMessage", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+c.BotToken)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := c.Do(req, 15*time.Second)
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
// to replace the placeholder with the actual answer.
func (c *Client) UpdateMessage(channel, ts, text string) error {
	if c.BotToken == "" {
		return errors.New("missing Slack bot token")
	}

	text = strings.TrimSpace(text)
	if text == "" || ts == "" {
		return errors.New("missing text or ts")
	}

	payload := map[string]string{
		"channel": channel,
		"ts":      ts,
		"text":    text,
	}
	b, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", c.APIBaseURL+"/chat.update", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+c.BotToken)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := c.Do(req, 15*time.Second)
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
