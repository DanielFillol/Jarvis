// internal/github/client.go
package github

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/DanielFillol/Jarvis/internal/config"
)

// CodeMatch represents a single result from a GitHub code search.
type CodeMatch struct {
	Repo      string   // full name: "owner/repo"
	Path      string   // file path within the repo
	HTMLURL   string   // link to the file on github.com
	Fragments []string // text-match snippets returned by the API
}

// Client wraps the GitHub REST API for code search and file retrieval.
// It is designed to be used exclusively for enriching Jira bug cards
// with code context.  All operations require a valid PAT stored in Token.
type Client struct {
	Token string
	Org   string   // if set, scope searches to "org:Org"
	Repos []string // if set, scope searches to "repo:owner/repo" for each entry
}

// NewClient constructs a GitHub client from the application configuration.
func NewClient(cfg config.Config) *Client {
	return &Client{
		Token: cfg.GitHubToken,
		Org:   cfg.GitHubOrg,
		Repos: cfg.GitHubRepos,
	}
}

// Enabled returns true when a GitHub token is configured.
func (c *Client) Enabled() bool {
	return strings.TrimSpace(c.Token) != ""
}

// -- internal response types -----------------------------------------------

type codeSearchResponse struct {
	TotalCount int `json:"total_count"`
	Items      []struct {
		Path       string `json:"path"`
		HTMLURL    string `json:"html_url"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		TextMatches []struct {
			Fragment string `json:"fragment"`
		} `json:"text_matches"`
	} `json:"items"`
}

type fileContentResponse struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
	Size     int    `json:"size"`
}

// -- public methods --------------------------------------------------------

// SearchCode searches for files matching the given query in the configured
// scope (repos / org).  maxResults caps the number of items returned.
// It requests text-match fragments so callers can show relevant snippets
// without fetching the full file.
func (c *Client) SearchCode(query string, maxResults int) ([]CodeMatch, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, nil
	}
	if maxResults <= 0 {
		maxResults = 5
	}

	// Build scope: prefer explicit repo list, fall back to org.
	var scopeParts []string
	for _, r := range c.Repos {
		r = strings.TrimSpace(r)
		if r != "" {
			scopeParts = append(scopeParts, "repo:"+r)
		}
	}
	if len(scopeParts) == 0 && strings.TrimSpace(c.Org) != "" {
		scopeParts = append(scopeParts, "org:"+strings.TrimSpace(c.Org))
	}
	if len(scopeParts) > 0 {
		q = q + " " + strings.Join(scopeParts, " ")
	}

	reqURL := fmt.Sprintf(
		"https://api.github.com/search/code?q=%s&per_page=%d",
		url.QueryEscape(q), maxResults,
	)

	resp, err := c.get(reqURL, "application/vnd.github.text-match+json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("github search status=%d body=%s", resp.StatusCode, preview(string(b), 300))
	}

	var out codeSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}

	var matches []CodeMatch
	for _, item := range out.Items {
		m := CodeMatch{
			Repo:    item.Repository.FullName,
			Path:    item.Path,
			HTMLURL: item.HTMLURL,
		}
		for _, tm := range item.TextMatches {
			if f := strings.TrimSpace(tm.Fragment); f != "" {
				m.Fragments = append(m.Fragments, f)
			}
		}
		matches = append(matches, m)
	}
	return matches, nil
}

// GetFileContent fetches the content of a file from a GitHub repository.
// repoFullName must be in "owner/repo" format.  The content is decoded from
// base64 and truncated to maxLines lines (0 means no limit).
func (c *Client) GetFileContent(repoFullName, path string, maxLines int) (string, error) {
	reqURL := fmt.Sprintf(
		"https://api.github.com/repos/%s/contents/%s",
		repoFullName, url.PathEscape(path),
	)

	resp, err := c.get(reqURL, "application/vnd.github.v3+json")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("github contents status=%d path=%s body=%s", resp.StatusCode, path, preview(string(b), 200))
	}

	var out fileContentResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Encoding != "base64" {
		return "", fmt.Errorf("unexpected encoding: %s", out.Encoding)
	}

	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
	if err != nil {
		return "", err
	}

	content := string(decoded)
	if maxLines > 0 {
		lines := strings.Split(content, "\n")
		if len(lines) > maxLines {
			lines = lines[:maxLines]
			content = strings.Join(lines, "\n") + "\n… (truncado)"
		}
	}
	return content, nil
}

// -- helpers ---------------------------------------------------------------

func (c *Client) get(reqURL, accept string) (*http.Response, error) {
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	httpClient := &http.Client{Timeout: 15 * time.Second}
	return httpClient.Do(req)
}

func preview(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
