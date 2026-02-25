// internal/jira/client.go
package jira

import (
	"bytes"
	"encoding/base64"
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

	"github.com/DanielFillol/Jarvis/internal/config"
)

// Client wraps the Jira REST API used by the application.  It
// encapsulates authentication details and base URL.
type Client struct {
	BaseURL  string
	Email    string
	Token    string
	Projects []string
}

// NewClient constructs a Jira client from the supplied configuration.  If
// JiraBaseURL is empty, then API methods will return errors.
func NewClient(cfg config.Config) *Client {
	return &Client{
		BaseURL:  strings.TrimRight(cfg.JiraBaseURL, "/"),
		Email:    cfg.JiraEmail,
		Token:    cfg.JiraAPIToken,
		Projects: cfg.JiraProjectKeys,
	}
}

// SearchJQL performs a Jira JQL search.  It returns a JiraSearchJQLResp
// containing issues.  JQL syntax is not validated by this method.
func (c *Client) SearchJQL(jql string, startAt, maxResults int, fields []string) (JiraSearchJQLResp, error) {
	if c.BaseURL == "" {
		return JiraSearchJQLResp{}, errors.New("missing Jira base URL")
	}
	if c.Email == "" || c.Token == "" {
		return JiraSearchJQLResp{}, errors.New("missing Jira credentials")
	}
	reqBody := JiraSearchJQLReq{
		JQL:        jql,
		StartAt:    startAt,
		MaxResults: maxResults,
		Fields:     fields,
	}
	b, _ := json.Marshal(reqBody)
	u := c.BaseURL + "/rest/api/3/search/jql"
	req, _ := http.NewRequest("POST", u, bytes.NewReader(b))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	cred := base64.StdEncoding.EncodeToString([]byte(c.Email + ":" + c.Token))
	req.Header.Set("Authorization", "Basic "+cred)
	client := &http.Client{Timeout: 25 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return JiraSearchJQLResp{}, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return JiraSearchJQLResp{}, fmt.Errorf("jira status=%d body=%s", resp.StatusCode, preview(string(rb), 600))
	}
	var out JiraSearchJQLResp
	if err := json.Unmarshal(rb, &out); err != nil {
		return JiraSearchJQLResp{}, err
	}
	return out, nil
}

// FetchAll performs a full JQL search, paginating through results up to
// maxTotal issues.  It flattens each issue into a JiraSearchJQLRespIssue
// for convenient use elsewhere.  If maxTotal <= 0, a default of 200 is
// used.
func (c *Client) FetchAll(jql string, maxTotal int) ([]JiraSearchJQLRespIssue, error) {
	if maxTotal <= 0 {
		maxTotal = 200
	}
	var all []JiraSearchJQLRespIssue
	startAt := 0
	pageSize := 50
	for {
		resp, err := c.SearchJQL(jql, startAt, pageSize, []string{"summary", "status", "issuetype", "updated", "project", "priority", "assignee", "customfield_10020"})
		if err != nil {
			if startAt > 0 {
				// If a further page fails, return what we've accumulated so far
				return all, nil
			}
			return nil, err
		}
		for _, it := range resp.Issues {
			assignee := "Unassigned"
			if it.Fields.Assignee != nil && it.Fields.Assignee.DisplayName != "" {
				assignee = it.Fields.Assignee.DisplayName
			}
			// Pick the active sprint if available, otherwise the last one.
			sprint := ""
			for _, sp := range it.Fields.Sprint {
				if sp.State == "active" {
					sprint = sp.Name
					break
				}
				sprint = sp.Name // keep overwriting; last entry is most recent
			}
			all = append(all, JiraSearchJQLRespIssue{
				Key:      it.Key,
				Project:  it.Fields.Project.Key,
				Type:     it.Fields.IssueType.Name,
				Status:   it.Fields.Status.Name,
				Priority: it.Fields.Priority.Name,
				Assignee: assignee,
				Summary:  it.Fields.Summary,
				Updated:  it.Fields.Updated,
				Sprint:   sprint,
			})
			if len(all) >= maxTotal {
				return all, nil
			}
		}
		if len(resp.Issues) < pageSize {
			return all, nil
		}
		startAt += pageSize
	}
}

// GetIssue fetches a single Jira issue by key.  The renderedFields are
// requested along with selected fields.  An error is returned if the
// request fails.
func (c *Client) GetIssue(key string) (JiraIssueResp, error) {
	if c.BaseURL == "" {
		return JiraIssueResp{}, errors.New("missing Jira base URL")
	}
	if c.Email == "" || c.Token == "" {
		return JiraIssueResp{}, errors.New("missing Jira credentials")
	}
	u := fmt.Sprintf("%s/rest/api/3/issue/%s?expand=renderedFields&fields=summary,description,status,issuetype,priority,assignee,subtasks,parent", c.BaseURL, url.PathEscape(key))
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Accept", "application/json")
	cred := base64.StdEncoding.EncodeToString([]byte(c.Email + ":" + c.Token))
	req.Header.Set("Authorization", "Basic "+cred)
	client := &http.Client{Timeout: 25 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return JiraIssueResp{}, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return JiraIssueResp{}, fmt.Errorf("jira get issue status=%d body=%s", resp.StatusCode, preview(string(rb), 600))
	}
	var out JiraIssueResp
	if err := json.Unmarshal(rb, &out); err != nil {
		return JiraIssueResp{}, err
	}
	return out, nil
}

// CreateIssue creates a new issue in Jira from a draft.  The draft
// fields must include Project and IssueType; Summary and Description
// must also be populated.  On success, the new issue key and ID are returned.
func (c *Client) CreateIssue(d IssueDraft) (JiraCreateIssueResp, error) {
	d.Project = strings.TrimSpace(d.Project)
	d.IssueType = strings.TrimSpace(d.IssueType)
	d.Summary = strings.TrimSpace(d.Summary)
	d.Description = strings.TrimSpace(d.Description)
	if d.Project == "" || d.IssueType == "" {
		return JiraCreateIssueResp{}, errors.New("project and issueType are required")
	}
	if c.BaseURL == "" {
		return JiraCreateIssueResp{}, errors.New("missing Jira base URL")
	}
	if c.Email == "" || c.Token == "" {
		return JiraCreateIssueResp{}, errors.New("missing Jira credentials")
	}
	fields := map[string]any{
		"project":     map[string]any{"key": d.Project},
		"summary":     d.Summary,
		"issuetype":   map[string]any{"name": d.IssueType},
		"description": TextToADF(d.Description),
	}
	if strings.TrimSpace(d.Priority) != "" {
		fields["priority"] = map[string]any{"name": d.Priority}
	}
	if len(d.Labels) > 0 {
		fields["labels"] = d.Labels
	}
	payload := map[string]any{"fields": fields}
	b, _ := json.Marshal(payload)
	// Log payload for debugging (preview only, don't log full description)
	payloadPreview := map[string]any{
		"fields": map[string]any{
			"project":   fields["project"],
			"issuetype": fields["issuetype"],
			"summary":   fields["summary"],
		},
	}
	previewBytes, _ := json.Marshal(payloadPreview)
	log.Printf("[JIRA] create issue payload preview: %s", string(previewBytes))
	u := c.BaseURL + "/rest/api/3/issue"
	req, _ := http.NewRequest("POST", u, bytes.NewReader(b))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	cred := base64.StdEncoding.EncodeToString([]byte(c.Email + ":" + c.Token))
	req.Header.Set("Authorization", "Basic "+cred)
	client := &http.Client{Timeout: 35 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return JiraCreateIssueResp{}, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return JiraCreateIssueResp{}, fmt.Errorf("jira create status=%d body=%s", resp.StatusCode, preview(string(rb), 800))
	}
	var out JiraCreateIssueResp
	if err := json.Unmarshal(rb, &out); err != nil {
		return JiraCreateIssueResp{}, err
	}
	if out.Key == "" {
		return JiraCreateIssueResp{}, errors.New("jira create: empty key in response")
	}
	return out, nil
}

// FetchExampleIssues fetches up to 'limit' recent issues from the same project and type in Jira,
// returning each one as a formatted string (Key + Summary + Description) for use as
// inspiration examples for the LLM.
//
// Errors from individual issues (e.g., GetIssue fails for a key) are silently ignored;
// only errors from the initial JQL search are propagated.
func (c *Client) FetchExampleIssues(project, issueType string, limit int) ([]string, error) {
	if strings.TrimSpace(project) == "" || strings.TrimSpace(issueType) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 3
	}

	jql := fmt.Sprintf(
		`project = %s AND issuetype = "%s" AND description is not EMPTY ORDER BY updated DESC`,
		project, issueType,
	)

	issues, err := c.FetchAll(jql, limit)
	if err != nil {
		return nil, fmt.Errorf("fetchExampleIssues jql failed: %w", err)
	}

	var examples []string
	for _, it := range issues {
		full, err := c.GetIssue(it.Key)
		if err != nil {
			log.Printf("[JIRA] fetchExampleIssues: GetIssue %s failed: %v", it.Key, err)
			continue
		}

		// Prefer the rendered version (HTML → clean text); fallback to raw field
		desc := strings.TrimSpace(full.RenderedFields.Description)
		if desc == "" {
			if full.Fields.Description != nil {
				raw, _ := json.Marshal(full.Fields.Description)
				desc = string(raw)
			}
		} else {
			desc = jiraStripHTML(desc)
		}

		if strings.TrimSpace(desc) == "" {
			continue // skip issues without a useful description
		}

		example := fmt.Sprintf(
			"Key: %s\nTipo: %s\nSummary: %s\nDescription:\n%s",
			it.Key,
			it.Type,
			it.Summary,
			jiraClip(desc, 700),
		)
		examples = append(examples, example)
	}

	return examples, nil
}

// ListProjects fetches all accessible Jira projects and returns up to 50,
// each as a JiraProjectInfo with key and name.
func (c *Client) ListProjects() ([]JiraProjectInfo, error) {
	if c.BaseURL == "" {
		return nil, errors.New("missing Jira base URL")
	}
	if c.Email == "" || c.Token == "" {
		return nil, errors.New("missing Jira credentials")
	}
	u := c.BaseURL + "/rest/api/3/project?maxResults=50"
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Accept", "application/json")
	cred := base64.StdEncoding.EncodeToString([]byte(c.Email + ":" + c.Token))
	req.Header.Set("Authorization", "Basic "+cred)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("jira projects status=%d body=%s", resp.StatusCode, preview(string(rb), 400))
	}
	var raw []struct {
		Key  string `json:"key"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(rb, &raw); err != nil {
		return nil, err
	}
	out := make([]JiraProjectInfo, 0, len(raw))
	for _, p := range raw {
		if p.Key != "" {
			out = append(out, JiraProjectInfo{Key: p.Key, Name: p.Name})
		}
	}
	return out, nil
}

var jiraReHTML = regexp.MustCompile(`<[^>]+>`)
var jiraReSpaces = regexp.MustCompile(`\s+`)

// jiraStripHTML removes HTML tags and normalizes spaces/line breaks.
func jiraStripHTML(s string) string {
	s = strings.ReplaceAll(s, "<br>", "\n")
	s = strings.ReplaceAll(s, "<br/>", "\n")
	s = strings.ReplaceAll(s, "<br />", "\n")
	s = strings.ReplaceAll(s, "</p>", "\n\n")
	s = jiraReHTML.ReplaceAllString(s, " ")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = jiraReSpaces.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// jiraClip trunca a string em n bytes, preservando o conteúdo útil.
func jiraClip(s string, n int) string {
	if n <= 0 {
		return ""
	}
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// preview trunca strings longas para mensagens de erro.
func preview(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
