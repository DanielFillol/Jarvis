package jira

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Transition represents a possible status transition for a Jira issue.
type Transition struct {
	ID   string
	Name string
}

// JiraUser represents a Jira user returned by the assignable-search endpoint.
type JiraUser struct {
	AccountID    string `json:"accountId"`
	DisplayName  string `json:"displayName"`
	EmailAddress string `json:"emailAddress"`
	Active       bool   `json:"active"`
}

// EditRequest is the structured edit command extracted from user intent.
// Non-empty fields indicate what should be changed. Multiple fields can be
// set to apply combined changes in one message.
type EditRequest struct {
	IssueKey     string   `json:"issue_key"`
	TargetStatus string   `json:"target_status"` // → transition
	AssigneeName string   `json:"assignee_name"` // → assign; "@me" = sender
	ParentKey    string   `json:"parent_key"`    // → set parent
	Summary      string   `json:"summary"`       // → update
	Description  string   `json:"description"`   // → update
	Priority     string   `json:"priority"`      // → update
	Labels       []string `json:"labels"`        // → update
	// TargetSprint describes which sprint to move the issue to.
	// LLM sets one of: "current", "next", or a sprint name/number like "Sprint 5" / "5".
	TargetSprint string `json:"target_sprint"`
	// GenerateDescription signals that the bot should generate description
	// content using LLM rather than copying explicit text from the message.
	GenerateDescription bool `json:"generate_description"`
	// AdditionalIssueKeys holds extra issue keys when the user asks to apply
	// the same edits to multiple cards (e.g. "faça o mesmo para o 509").
	AdditionalIssueKeys []string `json:"additional_issue_keys"`
}

// Sprint represents a Jira Agile sprint.
type Sprint struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"` // "active", "future", "closed"
}

// Board represents a Jira Agile board.
type Board struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// GetBoards returns the Agile boards associated with a project key.
func (c *Client) GetBoards(projectKey string) ([]Board, error) {
	if c.BaseURL == "" || c.Email == "" || c.Token == "" {
		return nil, errors.New("missing Jira credentials or base URL")
	}
	u := fmt.Sprintf("%s/rest/agile/1.0/board?projectKeyOrId=%s&maxResults=10",
		c.BaseURL, url.QueryEscape(projectKey))
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Accept", "application/json")
	cred := base64.StdEncoding.EncodeToString([]byte(c.Email + ":" + c.Token))
	req.Header.Set("Authorization", "Basic "+cred)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("jira get boards status=%d body=%s", resp.StatusCode, preview(string(rb), 400))
	}
	var raw struct {
		Values []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"values"`
	}
	if err := json.Unmarshal(rb, &raw); err != nil {
		return nil, err
	}
	out := make([]Board, 0, len(raw.Values))
	for _, b := range raw.Values {
		out = append(out, Board{ID: b.ID, Name: b.Name})
	}
	return out, nil
}

// GetSprints returns sprints for a board filtered by state ("active", "future", or "").
func (c *Client) GetSprints(boardID int, state string) ([]Sprint, error) {
	if c.BaseURL == "" || c.Email == "" || c.Token == "" {
		return nil, errors.New("missing Jira credentials or base URL")
	}
	u := fmt.Sprintf("%s/rest/agile/1.0/board/%d/sprint?maxResults=25", c.BaseURL, boardID)
	if state != "" {
		u += "&state=" + url.QueryEscape(state)
	}
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Accept", "application/json")
	cred := base64.StdEncoding.EncodeToString([]byte(c.Email + ":" + c.Token))
	req.Header.Set("Authorization", "Basic "+cred)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("jira get sprints status=%d body=%s", resp.StatusCode, preview(string(rb), 400))
	}
	var raw struct {
		Values []Sprint `json:"values"`
	}
	if err := json.Unmarshal(rb, &raw); err != nil {
		return nil, err
	}
	return raw.Values, nil
}

// MoveIssueToSprint moves an issue into the given sprint via the Agile API.
func (c *Client) MoveIssueToSprint(sprintID int, issueKey string) error {
	if c.BaseURL == "" || c.Email == "" || c.Token == "" {
		return errors.New("missing Jira credentials or base URL")
	}
	payload := map[string]any{"issues": []string{issueKey}}
	b, _ := json.Marshal(payload)
	u := fmt.Sprintf("%s/rest/agile/1.0/sprint/%d/issue", c.BaseURL, sprintID)
	req, _ := http.NewRequest("POST", u, bytes.NewReader(b))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	cred := base64.StdEncoding.EncodeToString([]byte(c.Email + ":" + c.Token))
	req.Header.Set("Authorization", "Basic "+cred)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("jira move to sprint status=%d body=%s", resp.StatusCode, preview(string(rb), 400))
	}
	return nil
}

// GetTransitions returns the available status transitions for a Jira issue.
func (c *Client) GetTransitions(issueKey string) ([]Transition, error) {
	if c.BaseURL == "" || c.Email == "" || c.Token == "" {
		return nil, errors.New("missing Jira credentials or base URL")
	}
	u := fmt.Sprintf("%s/rest/api/3/issue/%s/transitions", c.BaseURL, url.PathEscape(issueKey))
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Accept", "application/json")
	cred := base64.StdEncoding.EncodeToString([]byte(c.Email + ":" + c.Token))
	req.Header.Set("Authorization", "Basic "+cred)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("jira get transitions status=%d body=%s", resp.StatusCode, preview(string(rb), 400))
	}
	var raw struct {
		Transitions []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"transitions"`
	}
	if err := json.Unmarshal(rb, &raw); err != nil {
		return nil, err
	}
	out := make([]Transition, 0, len(raw.Transitions))
	for _, t := range raw.Transitions {
		out = append(out, Transition{ID: t.ID, Name: t.Name})
	}
	return out, nil
}

// TransitionIssue moves a Jira issue to the state identified by transitionID.
func (c *Client) TransitionIssue(issueKey, transitionID string) error {
	if c.BaseURL == "" || c.Email == "" || c.Token == "" {
		return errors.New("missing Jira credentials or base URL")
	}
	payload := map[string]any{"transition": map[string]any{"id": transitionID}}
	b, _ := json.Marshal(payload)
	u := fmt.Sprintf("%s/rest/api/3/issue/%s/transitions", c.BaseURL, url.PathEscape(issueKey))
	req, _ := http.NewRequest("POST", u, bytes.NewReader(b))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	cred := base64.StdEncoding.EncodeToString([]byte(c.Email + ":" + c.Token))
	req.Header.Set("Authorization", "Basic "+cred)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("jira transition status=%d body=%s", resp.StatusCode, preview(string(rb), 400))
	}
	return nil
}

// SearchAssignableUsers searches for Jira users that can be assigned to issueKey.
func (c *Client) SearchAssignableUsers(issueKey, query string, maxResults int) ([]JiraUser, error) {
	if c.BaseURL == "" || c.Email == "" || c.Token == "" {
		return nil, errors.New("missing Jira credentials or base URL")
	}
	if maxResults <= 0 {
		maxResults = 5
	}
	u := fmt.Sprintf("%s/rest/api/3/user/assignable/search?issueKey=%s&query=%s&maxResults=%d",
		c.BaseURL, url.QueryEscape(issueKey), url.QueryEscape(query), maxResults)
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Accept", "application/json")
	cred := base64.StdEncoding.EncodeToString([]byte(c.Email + ":" + c.Token))
	req.Header.Set("Authorization", "Basic "+cred)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("jira assignable search status=%d body=%s", resp.StatusCode, preview(string(rb), 400))
	}
	var users []JiraUser
	if err := json.Unmarshal(rb, &users); err != nil {
		return nil, err
	}
	return users, nil
}

// AssignIssue assigns issueKey to the user identified by accountID.
// Pass an empty accountID to unassign.
func (c *Client) AssignIssue(issueKey, accountID string) error {
	if c.BaseURL == "" || c.Email == "" || c.Token == "" {
		return errors.New("missing Jira credentials or base URL")
	}
	var payload map[string]any
	if accountID == "" {
		payload = map[string]any{"accountId": nil}
	} else {
		payload = map[string]any{"accountId": accountID}
	}
	b, _ := json.Marshal(payload)
	u := fmt.Sprintf("%s/rest/api/3/issue/%s/assignee", c.BaseURL, url.PathEscape(issueKey))
	req, _ := http.NewRequest("PUT", u, bytes.NewReader(b))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	cred := base64.StdEncoding.EncodeToString([]byte(c.Email + ":" + c.Token))
	req.Header.Set("Authorization", "Basic "+cred)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("jira assign status=%d body=%s", resp.StatusCode, preview(string(rb), 400))
	}
	return nil
}

// UpdateIssue applies arbitrary field updates to an existing Jira issue.
func (c *Client) UpdateIssue(issueKey string, fields map[string]any) error {
	if c.BaseURL == "" || c.Email == "" || c.Token == "" {
		return errors.New("missing Jira credentials or base URL")
	}
	payload := map[string]any{"fields": fields}
	b, _ := json.Marshal(payload)
	u := fmt.Sprintf("%s/rest/api/3/issue/%s", c.BaseURL, url.PathEscape(issueKey))
	req, _ := http.NewRequest("PUT", u, bytes.NewReader(b))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	cred := base64.StdEncoding.EncodeToString([]byte(c.Email + ":" + c.Token))
	req.Header.Set("Authorization", "Basic "+cred)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("jira update status=%d body=%s", resp.StatusCode, preview(string(rb), 400))
	}
	return nil
}
