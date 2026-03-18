package slack

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// File represents a file attached to a Slack message.
type File struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Mimetype           string `json:"mimetype"`
	Filetype           string `json:"filetype"`
	Size               int64  `json:"size"`
	URLPrivateDownload string `json:"url_private_download"`
	ExternalURL        string `json:"external_url"`
}

// GetThreadFiles fetches all file attachments from the messages of a Slack
// thread and returns them deduplicated by file ID.
func (c *Client) GetThreadFiles(channel, threadTs string) ([]File, error) {
	if c.BotToken == "" {
		return nil, errors.New("missing Slack bot token")
	}

	u := fmt.Sprintf("%s/conversations.replies?channel=%s&ts=%s&limit=200", c.APIBaseURL, url.QueryEscape(channel), url.QueryEscape(threadTs))
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+c.BotToken)
	resp, err := c.Do(req, 15*time.Second)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	rb, _ := io.ReadAll(resp.Body)
	var data struct {
		OK       bool   `json:"ok"`
		Error    string `json:"error"`
		Messages []struct {
			Files []File `json:"files,omitempty"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rb, &data); err != nil {
		return nil, err
	}

	if !data.OK {
		return nil, fmt.Errorf("slack error: %s", data.Error)
	}
	var files []File
	seen := make(map[string]bool)
	for _, m := range data.Messages {
		for _, f := range m.Files {
			if seen[f.ID] {
				continue
			}
			seen[f.ID] = true
			files = append(files, f)
		}
	}
	return files, nil
}

// DownloadFile fetches a private Slack file into memory.
// It tries the user token first (broader file access), then falls back to
// the bot token. If the response is HTML (Slack login redirect), the token
// lacks files:read scope, and an error is returned.
// The caller is responsible for enforcing size limits.
func (c *Client) DownloadFile(urlPrivate string) ([]byte, error) {
	urlPrivate = strings.TrimSpace(urlPrivate)
	if urlPrivate == "" {
		return nil, errors.New("empty file URL")
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

	var lastErr error
	for _, token := range tokens {
		req, err := http.NewRequest("GET", urlPrivate, nil)
		if err != nil {
			return nil, fmt.Errorf("build download request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := c.Do(req, 30*time.Second)
		if err != nil {
			lastErr = fmt.Errorf("download file: %w", err)
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 20*1024*1024))
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("read file body: %w", err)
			continue
		}
		if resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("slack file download status=%d body=%s", resp.StatusCode, preview(string(body), 300))
			continue
		}
		// If Slack returned an HTML page, the token lacks files:read scope.
		ct := resp.Header.Get("Content-Type")
		if strings.Contains(ct, "text/html") || (len(body) > 0 && body[0] == '<') {
			lastErr = fmt.Errorf("received HTML instead of file content (token missing files:read scope?)")
			continue
		}
		return body, nil
	}
	return nil, lastErr
}
