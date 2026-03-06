package jira

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ProjectMeta holds the metadata fetched from Jira for a single project.
type ProjectMeta struct {
	Key         string
	Name        string
	Description string
	IssueTypes  []string
	// StatusesByType maps issue type name → ordered list of status names.
	// Populated by GetProjectStatuses.
	StatusesByType map[string][]string
}

// GetProjectStatuses fetches the available statuses for each issue type in a
// project via GET /rest/api/3/project/{key}/statuses.
func (c *Client) GetProjectStatuses(key string) (map[string][]string, error) {
	if c.BaseURL == "" || c.Email == "" || c.Token == "" {
		return nil, nil
	}
	u := fmt.Sprintf("%s/rest/api/3/project/%s/statuses", c.BaseURL, key)
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
		return nil, fmt.Errorf("jira project statuses status=%d body=%s", resp.StatusCode, preview(string(rb), 300))
	}
	var raw []struct {
		Name     string `json:"name"`
		Subtask  bool   `json:"subtask"`
		Statuses []struct {
			Name string `json:"name"`
		} `json:"statuses"`
	}
	if err := json.Unmarshal(rb, &raw); err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(raw))
	for _, it := range raw {
		if it.Subtask || it.Name == "" {
			continue
		}
		var statuses []string
		seen := make(map[string]bool)
		for _, s := range it.Statuses {
			if s.Name != "" && !seen[s.Name] {
				seen[s.Name] = true
				statuses = append(statuses, s.Name)
			}
		}
		if len(statuses) > 0 {
			out[it.Name] = statuses
		}
	}
	return out, nil
}

// GetProjectMeta fetches a project's name, description, and available issue
// types from the Jira REST API (GET /rest/api/3/project/{key}).
func (c *Client) GetProjectMeta(key string) (ProjectMeta, error) {
	if c.BaseURL == "" || c.Email == "" || c.Token == "" {
		return ProjectMeta{Key: key}, nil
	}
	u := fmt.Sprintf("%s/rest/api/3/project/%s", c.BaseURL, key)
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Accept", "application/json")
	cred := base64.StdEncoding.EncodeToString([]byte(c.Email + ":" + c.Token))
	req.Header.Set("Authorization", "Basic "+cred)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return ProjectMeta{Key: key}, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return ProjectMeta{Key: key}, fmt.Errorf("jira project meta status=%d body=%s", resp.StatusCode, preview(string(rb), 300))
	}
	var raw struct {
		Key         string `json:"key"`
		Name        string `json:"name"`
		Description string `json:"description"`
		IssueTypes  []struct {
			Name    string `json:"name"`
			Subtask bool   `json:"subtask"`
		} `json:"issueTypes"`
	}
	if err := json.Unmarshal(rb, &raw); err != nil {
		return ProjectMeta{Key: key}, err
	}
	var types []string
	for _, it := range raw.IssueTypes {
		if it.Name != "" && !it.Subtask {
			types = append(types, it.Name)
		}
	}
	return ProjectMeta{
		Key:         raw.Key,
		Name:        raw.Name,
		Description: strings.TrimSpace(raw.Description),
		IssueTypes:  types,
	}, nil
}

// GenerateCatalog fetches metadata for all configured project keys, writes a
// Markdown catalog to filePath (if non-empty), and returns a compact
// one-line summary string suitable for use in LLM prompts.
// On any per-project error the key is still included with whatever was fetched.
func (c *Client) GenerateCatalog(filePath string) string {
	if c.BaseURL == "" || len(c.Projects) == 0 {
		return strings.Join(c.Projects, ", ")
	}
	workflowStatuses := make(map[string][]string)
	var projects []ProjectMeta
	for _, key := range c.Projects {
		meta, err := c.GetProjectMeta(key)
		if err != nil {
			log.Printf("[JIRA] GetProjectMeta %s failed: %v", key, err)
		}
		if meta.Key == "" {
			meta.Key = key
		}
		statuses, err := c.GetProjectStatuses(key)
		if err != nil {
			log.Printf("[JIRA] GetProjectStatuses %s failed: %v", key, err)
		}
		meta.StatusesByType = statuses
		// Build deduplicated status list for this project and store on client.
		if len(statuses) > 0 {
			seen := make(map[string]bool)
			var all []string
			for _, ss := range statuses {
				for _, s := range ss {
					if !seen[s] {
						seen[s] = true
						all = append(all, s)
					}
				}
			}
			workflowStatuses[key] = all
		}
		projects = append(projects, meta)
	}
	c.WorkflowStatuses = workflowStatuses
	compact := formatProjectsCompact(projects)
	if filePath != "" {
		dir := filepath.Dir(filePath)
		if err := os.MkdirAll(dir, 0o755); err == nil {
			if err := os.WriteFile(filePath, []byte(renderProjectsMarkdown(projects)), 0o644); err == nil {
				log.Printf("[JIRA] catalog written to %s", filePath)
			}
		}
	}
	log.Printf("[JIRA] catalog: %s", compact)
	return compact
}

// formatProjectsCompact produces a pipe-separated summary of projects:
// "INV=Faturamento [Bug, Task] statuses:[To Do,Doing,Done] | ..."
func formatProjectsCompact(projects []ProjectMeta) string {
	parts := make([]string, 0, len(projects))
	for _, p := range projects {
		if p.Name == "" {
			parts = append(parts, p.Key)
			continue
		}
		s := fmt.Sprintf("%s=%s", p.Key, p.Name)
		if len(p.IssueTypes) > 0 {
			s += fmt.Sprintf(" [%s]", strings.Join(p.IssueTypes, ", "))
		}
		// Include a deduplicated status list (from any issue type) so the LLM
		// knows what statuses exist in the project without repeating per type.
		if len(p.StatusesByType) > 0 {
			seen := make(map[string]bool)
			var allStatuses []string
			for _, statuses := range p.StatusesByType {
				for _, st := range statuses {
					if !seen[st] {
						seen[st] = true
						allStatuses = append(allStatuses, st)
					}
				}
			}
			if len(allStatuses) > 0 {
				s += fmt.Sprintf(" statuses:[%s]", strings.Join(allStatuses, ","))
			}
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " | ")
}

// renderProjectsMarkdown generates a full Markdown document describing all
// projects, their descriptions, issue types, and workflow statuses.
func renderProjectsMarkdown(projects []ProjectMeta) string {
	var sb strings.Builder
	sb.WriteString("# Documentação de Projetos Jira\n\n")
	sb.WriteString(fmt.Sprintf("> Gerado em: %s\n\n", time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString("---\n\n")
	for _, p := range projects {
		title := p.Name
		if title == "" {
			title = p.Key
		}
		sb.WriteString(fmt.Sprintf("## %s — `%s`\n\n", title, p.Key))
		if p.Description != "" {
			sb.WriteString(fmt.Sprintf("_%s_\n\n", p.Description))
		}
		if len(p.IssueTypes) > 0 {
			sb.WriteString("**Tipos de issue disponíveis:** " + strings.Join(p.IssueTypes, ", ") + "\n\n")
		}
		if len(p.StatusesByType) > 0 {
			sb.WriteString("**Workflow de status por tipo de issue:**\n\n")
			for _, issueType := range p.IssueTypes {
				statuses, ok := p.StatusesByType[issueType]
				if !ok || len(statuses) == 0 {
					continue
				}
				sb.WriteString(fmt.Sprintf("- **%s:** %s\n", issueType, strings.Join(statuses, " → ")))
			}
			sb.WriteString("\n")
		}
	}
	return sb.String()
}
