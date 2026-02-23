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

// Client wraps the GitHub REST API for repository exploration and file
// retrieval.  It is designed to enrich Jira bug cards with code context
// by navigating the repository structure as a developer would.
type Client struct {
	Token string
	Org   string   // if set, scope searches to "org:Org"
	Repos []string // configured repo list (may include "github.com/" prefix)
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

// NormalizedRepos returns the configured repository list with any
// "github.com/" URL prefix stripped, resulting in "owner/repo" format.
func (c *Client) NormalizedRepos() []string {
	out := make([]string, 0, len(c.Repos))
	for _, r := range c.Repos {
		r = normalizeRepo(r)
		if r != "" {
			out = append(out, r)
		}
	}
	return out
}

// -- internal response types -----------------------------------------------

type treeResponse struct {
	Tree []struct {
		Path string `json:"path"`
		Type string `json:"type"` // "blob" or "tree"
		Size int    `json:"size"`
	} `json:"tree"`
	Truncated bool `json:"truncated"`
}

type fileContentResponse struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
	Size     int    `json:"size"`
}

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

// -- public methods --------------------------------------------------------

// GetRepoTree fetches the complete source-file tree of a repository using the
// Git Trees API.  repoFullName must be in "owner/repo" format (or with the
// optional "github.com/" prefix, which is stripped automatically).
// Only source-code files are returned; common non-code directories
// (node_modules, vendor, dist, …) are excluded.  The result is capped at
// maxFiles entries.
func (c *Client) GetRepoTree(repoFullName string, maxFiles int) ([]string, error) {
	if maxFiles <= 0 {
		maxFiles = 300
	}
	repo := normalizeRepo(repoFullName)
	reqURL := fmt.Sprintf(
		"https://api.github.com/repos/%s/git/trees/HEAD?recursive=1",
		repo,
	)

	resp, err := c.get(reqURL, "application/vnd.github.v3+json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("github tree status=%d repo=%s body=%s", resp.StatusCode, repo, preview(string(b), 300))
	}

	var out treeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}

	var files []string
	for _, item := range out.Tree {
		if item.Type != "blob" {
			continue
		}
		if !isSourceFile(item.Path) {
			continue
		}
		files = append(files, item.Path)
		if len(files) >= maxFiles {
			break
		}
	}
	return files, nil
}

// GetFileContent fetches the content of a single file from a GitHub repository.
// repoFullName must be in "owner/repo" format (github.com/ prefix is stripped).
// The content is decoded from base64 and truncated to maxLines lines (0 = no limit).
func (c *Client) GetFileContent(repoFullName, path string, maxLines int) (string, error) {
	repo := normalizeRepo(repoFullName)
	reqURL := fmt.Sprintf(
		"https://api.github.com/repos/%s/contents/%s",
		repo, url.PathEscape(path),
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

// SearchCode searches GitHub code using the search API.  It is kept as a
// fallback for cases where the exploratory approach is not available.
// repoFullName prefixes are normalized automatically.
func (c *Client) SearchCode(query string, maxResults int) ([]CodeMatch, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, nil
	}
	if maxResults <= 0 {
		maxResults = 5
	}

	var scopeParts []string
	for _, r := range c.NormalizedRepos() {
		scopeParts = append(scopeParts, "repo:"+r)
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

// -- helpers ---------------------------------------------------------------

// normalizeRepo strips any "https://github.com/", "http://github.com/" or
// "github.com/" prefix from a repository identifier, returning "owner/repo".
func normalizeRepo(r string) string {
	r = strings.TrimSpace(r)
	r = strings.TrimPrefix(r, "https://github.com/")
	r = strings.TrimPrefix(r, "http://github.com/")
	r = strings.TrimPrefix(r, "github.com/")
	return strings.TrimSpace(r)
}

// sourceExtensions is the set of file extensions considered source code.
var sourceExtensions = map[string]bool{
	".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true,
	".py": true, ".java": true, ".kt": true, ".swift": true, ".rb": true,
	".php": true, ".cs": true, ".rs": true, ".cpp": true, ".c": true,
	".h": true, ".vue": true, ".dart": true, ".scala": true, ".ex": true,
	".exs": true, ".clj": true,
}

// skipDirSegments lists directory name substrings that indicate generated,
// compiled or dependency content that should not be included in the tree.
var skipDirSegments = []string{
	"/node_modules/", "/vendor/", "/dist/", "/build/", "/.git/",
	"/__pycache__/", "/.next/", "/coverage/", "/target/", "/.cache/",
	"/out/", "/.yarn/",
}

// isSourceFile returns true if the path corresponds to a source-code file
// that is worth including in repository exploration.
func isSourceFile(path string) bool {
	// Extension check
	dot := strings.LastIndex(path, ".")
	if dot < 0 {
		return false
	}
	ext := strings.ToLower(path[dot:])
	if !sourceExtensions[ext] {
		return false
	}
	// Skip non-code directory segments
	lower := "/" + strings.ToLower(path) + "/"
	for _, seg := range skipDirSegments {
		if strings.Contains(lower, seg) {
			return false
		}
	}
	return true
}

func (c *Client) get(reqURL, accept string) (*http.Response, error) {
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	httpClient := &http.Client{Timeout: 20 * time.Second}
	return httpClient.Do(req)
}

func preview(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
