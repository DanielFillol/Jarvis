package outline

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a minimal Outline API client that supports document search.
type Client struct {
	BaseURL    string
	Origin     string // scheme + host only, e.g. "https://musa.getoutline.com"
	APIKey     string
	HTTPClient *http.Client
}

// NewClient constructs an Outline client.  baseURL is the API root, e.g.
// "https://app.getoutline.com/api" (cloud) or "https://wiki.yourcompany.com/api"
// (self-hosted).  apiKey is a personal access token from Outline → Settings → API.
func NewClient(baseURL, apiKey string) *Client {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	origin := baseURL
	if u, err := url.Parse(baseURL); err == nil {
		origin = u.Scheme + "://" + u.Host
	}
	return &Client{
		BaseURL:    baseURL,
		Origin:     origin,
		APIKey:     apiKey,
		HTTPClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// SearchResult holds a single search hit returned by the Outline API.
type SearchResult struct {
	Context  string   // snippet with the matched text
	Document Document // full document metadata + body
	Ranking  float64  // relevance score from Outline
}

// Document holds the Outline document fields used when building LLM context.
type Document struct {
	ID    string
	Title string
	Text  string // full Markdown body
	URL   string
}

type listRequest struct {
	Limit     int    `json:"limit"`
	Sort      string `json:"sort"`
	Direction string `json:"direction"`
}

type listResponse struct {
	Data []searchDocResult `json:"data"`
}

// ListDocuments returns up to limit recently-updated published documents.
// Results are wrapped as SearchResult so FormatContext can be reused.
func (c *Client) ListDocuments(limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 15
	}
	body, _ := json.Marshal(listRequest{
		Limit:     limit,
		Sort:      "updatedAt",
		Direction: "DESC",
	})
	req, err := http.NewRequest("POST", c.BaseURL+"/documents.list", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("outline: list status=%d body=%s", resp.StatusCode, previewStr(string(rb), 200))
	}
	var lr listResponse
	if err := json.Unmarshal(rb, &lr); err != nil {
		return nil, fmt.Errorf("outline: list decode: %w", err)
	}
	results := make([]SearchResult, 0, len(lr.Data))
	for _, d := range lr.Data {
		docURL := d.URL
		if docURL != "" && !strings.HasPrefix(docURL, "http") {
			docURL = c.Origin + docURL
		}
		results = append(results, SearchResult{
			Document: Document{
				ID:    d.ID,
				Title: d.Title,
				Text:  d.Text,
				URL:   docURL,
			},
		})
	}
	return results, nil
}

type searchRequest struct {
	Query        string   `json:"query"`
	Limit        int      `json:"limit"`
	StatusFilter []string `json:"statusFilter"`
}

type searchHit struct {
	Context  string          `json:"context"`
	Ranking  float64         `json:"ranking"`
	Document searchDocResult `json:"document"`
}

type searchDocResult struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Text  string `json:"text"`
	URL   string `json:"url"`
}

type searchResponse struct {
	Data []searchHit `json:"data"`
}

// SearchDocuments queries the Outline search API and returns up to limit results
// ordered by relevance.  Only published documents are included.
func (c *Client) SearchDocuments(query string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 5
	}
	body, _ := json.Marshal(searchRequest{
		Query:        query,
		Limit:        limit,
		StatusFilter: []string{"published"},
	})
	req, err := http.NewRequest("POST", c.BaseURL+"/documents.search", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("outline: status=%d body=%s", resp.StatusCode, previewStr(string(rb), 200))
	}

	var sr searchResponse
	if err := json.Unmarshal(rb, &sr); err != nil {
		return nil, fmt.Errorf("outline: decode: %w", err)
	}

	results := make([]SearchResult, 0, len(sr.Data))
	for _, h := range sr.Data {
		docURL := h.Document.URL
		// The API sometimes returns relative paths (e.g. "/doc/slug-ID").
		// Prepend the origin so callers always get an absolute URL.
		if docURL != "" && !strings.HasPrefix(docURL, "http") {
			docURL = c.Origin + docURL
		}
		results = append(results, SearchResult{
			Context: h.Context,
			Ranking: h.Ranking,
			Document: Document{
				ID:    h.Document.ID,
				Title: h.Document.Title,
				Text:  h.Document.Text,
				URL:   docURL,
			},
		})
	}
	return results, nil
}

// FormatContext formats search results into a compact Markdown block suitable
// for LLM context injection.  maxCharsPerDoc limits how many characters of the
// full document text are included; pass 0 to include all.
func FormatContext(results []SearchResult, maxCharsPerDoc int) string {
	if len(results) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, r := range results {
		if i > 0 {
			sb.WriteString("\n---\n\n")
		}
		sb.WriteString("## ")
		sb.WriteString(r.Document.Title)
		sb.WriteString("\n")
		if r.Document.URL != "" {
			sb.WriteString("URL: ")
			sb.WriteString(r.Document.URL)
			sb.WriteString("\n")
		}
		if r.Context != "" {
			sb.WriteString("Trecho relevante: ")
			sb.WriteString(r.Context)
			sb.WriteString("\n\n")
		}
		text := r.Document.Text
		if maxCharsPerDoc > 0 && len(text) > maxCharsPerDoc {
			text = text[:maxCharsPerDoc] + "...(truncado)"
		}
		if text != "" {
			sb.WriteString(text)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// FormatSources returns a Slack mrkdwn line listing the source documents with
// clickable links.  Returns an empty string when no document has a URL.
func FormatSources(results []SearchResult) string {
	var parts []string
	for _, r := range results {
		if r.Document.URL == "" {
			continue
		}
		title := r.Document.Title
		if title == "" {
			title = "Documento"
		}
		parts = append(parts, fmt.Sprintf("<%s|%s>", r.Document.URL, title))
	}
	if len(parts) == 0 {
		return ""
	}
	return ":books: _Fontes Outline: " + strings.Join(parts, " · ") + "_"
}

func previewStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
