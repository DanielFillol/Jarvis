package jira

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/DanielFillol/Jarvis/internal/config"
	"github.com/DanielFillol/Jarvis/internal/parse"
)

// Client wraps the Jira REST API used by the application.  It
// encapsulates authentication details and base URL.
type Client struct {
	BaseURL  string
	Email    string
	Token    string
	Store    time.Duration
	Projects []string
	// CatalogCompact is a one-line summary of all configured projects
	// (e.g. "INV=Faturamento [Bug, Task] | TPTDR=Transporte [Bug, Epic]").
	// Pre-populated with raw keys at construction; overwritten by GenerateCatalog.
	CatalogCompact string
}

// NewClient constructs a Jira client from the supplied configuration.  If
// JiraBaseURL is empty, then API methods will return errors.
func NewClient(cfg config.Config) *Client {
	// Create a pending Store with 2-hour TTL
	store := NewStore(2 * time.Hour)

	// Register project name→key mapping for natural language parsing
	parse.SetProjectNameMap(cfg.JiraProjectNameMap)

	return &Client{
		BaseURL:        strings.TrimRight(cfg.JiraBaseURL, "/"),
		Email:          cfg.JiraEmail,
		Token:          cfg.JiraAPIToken,
		Store:          store.ttl,
		Projects:       cfg.JiraProjectKeys,
		CatalogCompact: strings.Join(cfg.JiraProjectKeys, ", "),
	}
}

// IssueDraft represents a draft of a Jira issue used when the user
// requests creation of a new card.  Optional fields may be left blank
// and later filled in by the user.
type IssueDraft struct {
	Project     string   `json:"project"`
	IssueType   string   `json:"issue_type"`
	Summary     string   `json:"summary"`
	Description string   `json:"description"`
	Priority    string   `json:"priority"`
	Labels      []string `json:"labels"`
}

// ProjectInfo holds a minimal project summary returned by ListProjects.
type ProjectInfo struct {
	Key  string
	Name string
}

// SearchJQLRespIssue is an internal flattened representation of a
// Jira issue used by higher-level code to build context for the LLM.
type SearchJQLRespIssue struct {
	Key      string
	Project  string
	Type     string
	Status   string
	Priority string
	Assignee string
	Summary  string
	Updated  string
	Sprint   string
}

// IssueResp models the response from Jira's GET issue endpoint.  It
// contains both rendered and raw fields.  Only fields accessed in
//
//	the current code are defined here.
type IssueResp struct {
	Key            string `json:"key"`
	RenderedFields struct {
		Description string `json:"description"`
	} `json:"renderedFields"`
	Fields struct {
		Summary string `json:"summary"`
		Status  struct {
			Name string `json:"name"`
		} `json:"status"`
		IssueType struct {
			Name string `json:"name"`
		} `json:"issuetype"`
		Priority struct {
			Name string `json:"name"`
		} `json:"priority"`
		Assignee *struct {
			DisplayName string `json:"displayName"`
		} `json:"assignee"`
		Description any `json:"description"`
		Parent      *struct {
			Key    string `json:"key"`
			Fields struct {
				Summary string `json:"summary"`
			} `json:"fields"`
		} `json:"parent"`
		Subtasks []struct {
			Key    string `json:"key"`
			Fields struct {
				Summary string `json:"summary"`
				Status  struct {
					Name string `json:"name"`
				} `json:"status"`
				IssueType struct {
					Name string `json:"name"`
				} `json:"issuetype"`
			} `json:"fields"`
		} `json:"subtasks"`
	} `json:"fields"`
}

// SearchJQLResp models the response from the /rest/api/3/search/jql
// endpoint.  Only a subset of fields is defined.
type SearchJQLResp struct {
	Issues []struct {
		Key    string `json:"key"`
		Fields struct {
			Summary string `json:"summary"`
			Updated string `json:"updated"`
			Status  struct {
				Name string `json:"name"`
			} `json:"status"`
			IssueType struct {
				Name string `json:"name"`
			} `json:"issuetype"`
			Priority struct {
				Name string `json:"name"`
			} `json:"priority"`
			Assignee *struct {
				DisplayName string `json:"displayName"`
			} `json:"assignee"`
			Project struct {
				Key string `json:"key"`
			} `json:"project"`
			// Custom-field_10020 is the standard Jira Cloud sprint field.
			// It is an array; the last active (or most recent) entry is used.
			Sprint []struct {
				Name  string `json:"name"`
				State string `json:"state"`
			} `json:"customfield_10020"`
		} `json:"fields"`
	} `json:"issues"`
}

// SearchJQLReq is the request payload for the /rest/api/3/search/jql
// endpoint.  The JQL string specifies the search query, and optional
// parameters control paging and the fields returned.
type SearchJQLReq struct {
	JQL        string   `json:"jql"`
	StartAt    int      `json:"startAt,omitempty"`
	MaxResults int      `json:"maxResults,omitempty"`
	Fields     []string `json:"fields,omitempty"`
}

// CreateIssueResp represents the response from Jira's creation issue
// API.  Only a subset of fields is defined here.
type CreateIssueResp struct {
	ID   string `json:"id"`
	Key  string `json:"key"`
	Self string `json:"self"`
}

// ListProjects fetches all accessible Jira projects and returns up to 50,
// each as a ProjectInfo with key and name.
func (c *Client) ListProjects() ([]ProjectInfo, error) {
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

	out := make([]ProjectInfo, 0, len(raw))
	for _, p := range raw {
		if p.Key != "" {
			out = append(out, ProjectInfo{Key: p.Key, Name: p.Name})
		}
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

// FetchAll performs a full JQL search, paginating through results up to
// maxTotal issues.  It flattens each issue into a SearchJQLRespIssue
// for convenient use elsewhere.  If maxTotal <= 0, a default of 200 is
// used.
func (c *Client) FetchAll(jql string, maxTotal int) ([]SearchJQLRespIssue, error) {
	if maxTotal <= 0 {
		maxTotal = 200
	}
	var all []SearchJQLRespIssue
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
				sprint = sp.Name // keep overwriting; the last entry is the most recent
			}
			all = append(all, SearchJQLRespIssue{
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
func (c *Client) GetIssue(key string) (IssueResp, error) {
	if c.BaseURL == "" {
		return IssueResp{}, errors.New("missing Jira base URL")
	}
	if c.Email == "" || c.Token == "" {
		return IssueResp{}, errors.New("missing Jira credentials")
	}
	u := fmt.Sprintf("%s/rest/api/3/issue/%s?expand=renderedFields&fields=summary,description,status,issuetype,priority,assignee,subtasks,parent", c.BaseURL, url.PathEscape(key))
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Accept", "application/json")
	cred := base64.StdEncoding.EncodeToString([]byte(c.Email + ":" + c.Token))
	req.Header.Set("Authorization", "Basic "+cred)
	client := &http.Client{Timeout: 25 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return IssueResp{}, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return IssueResp{}, fmt.Errorf("jira get issue status=%d body=%s", resp.StatusCode, preview(string(rb), 600))
	}
	var out IssueResp
	if err := json.Unmarshal(rb, &out); err != nil {
		return IssueResp{}, err
	}
	return out, nil
}

// SearchJQL performs a Jira JQL search.  It returns a SearchJQLResp
// containing issues.  JQL syntax is not validated by this method.
func (c *Client) SearchJQL(jql string, startAt, maxResults int, fields []string) (SearchJQLResp, error) {
	if c.BaseURL == "" {
		return SearchJQLResp{}, errors.New("missing Jira base URL")
	}
	if c.Email == "" || c.Token == "" {
		return SearchJQLResp{}, errors.New("missing Jira credentials")
	}
	reqBody := SearchJQLReq{
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
		return SearchJQLResp{}, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return SearchJQLResp{}, fmt.Errorf("jira status=%d body=%s", resp.StatusCode, preview(string(rb), 600))
	}
	var out SearchJQLResp
	if err := json.Unmarshal(rb, &out); err != nil {
		return SearchJQLResp{}, err
	}
	return out, nil
}

// CreateIssue creates a new issue in Jira from a draft.  The draft
// fields must include Project and IssueType; Summary and Description
// must also be populated.  On success, the new issue-key and ID are returned.
func (c *Client) CreateIssue(d IssueDraft) (CreateIssueResp, error) {
	d.Project = strings.TrimSpace(d.Project)
	d.IssueType = strings.TrimSpace(d.IssueType)
	d.Summary = strings.TrimSpace(d.Summary)
	d.Description = strings.TrimSpace(d.Description)
	if d.Project == "" || d.IssueType == "" {
		return CreateIssueResp{}, errors.New("project and issueType are required")
	}
	if c.BaseURL == "" {
		return CreateIssueResp{}, errors.New("missing Jira base URL")
	}
	if c.Email == "" || c.Token == "" {
		return CreateIssueResp{}, errors.New("missing Jira credentials")
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
		return CreateIssueResp{}, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return CreateIssueResp{}, fmt.Errorf("jira create status=%d body=%s", resp.StatusCode, preview(string(rb), 800))
	}
	var out CreateIssueResp
	if err := json.Unmarshal(rb, &out); err != nil {
		return CreateIssueResp{}, err
	}
	if out.Key == "" {
		return CreateIssueResp{}, errors.New("jira create: empty key in response")
	}
	return out, nil
}

// AttachFileToIssue uploads a file as an attachment to an existing Jira issue.
// The Jira attachment API requires the X-Atlassian-Token: no-check header to
// bypass XSRF verification.
func (c *Client) AttachFileToIssue(issueKey, filename string, data []byte) error {
	if c.BaseURL == "" || c.Email == "" || c.Token == "" {
		return errors.New("missing Jira credentials or base URL")
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		return fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return fmt.Errorf("write file data: %w", err)
	}
	w.Close()
	u := fmt.Sprintf("%s/rest/api/3/issue/%s/attachments", c.BaseURL, url.PathEscape(issueKey))
	req, _ := http.NewRequest("POST", u, &buf)
	req.Header.Set("X-Atlassian-Token", "no-check")
	req.Header.Set("Content-Type", w.FormDataContentType())
	cred := base64.StdEncoding.EncodeToString([]byte(c.Email + ":" + c.Token))
	req.Header.Set("Authorization", "Basic "+cred)
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("jira attach: %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("jira attach status=%d body=%s", resp.StatusCode, preview(string(rb), 400))
	}
	return nil
}
