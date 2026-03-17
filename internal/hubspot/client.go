package hubspot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/DanielFillol/Jarvis/internal/config"
)

const defaultBaseURL = "https://api.hubapi.com"

// SearchResult is a single CRM record returned by the HubSpot search API.
type SearchResult struct {
	ObjectType string // "contacts", "companies", "deals", "tickets"
	ID         string
	Name       string            // display name derived from object-specific properties
	Properties map[string]string // key fields from HubSpot response
	WebURL     string            // HubSpot record URL (if portal ID is available)
}

// Client wraps the HubSpot CRM v3 search API (read-only).
type Client struct {
	baseURL     string
	apiKey      string
	searchLimit int
	http        *http.Client
	// CatalogCompact is a compact one-liner of all deal/ticket pipelines and
	// their stages.  Populated asynchronously at startup by GenerateCatalog.
	CatalogCompact string
}

// NewClient creates a new HubSpot client from config.
// Returns nil when HubSpot is not configured (HUBSPOT_API_KEY missing).
func NewClient(cfg config.Config) *Client {
	if !cfg.HubSpotEnabled() {
		return nil
	}
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.HubSpotBaseURL), "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	limit := cfg.HubSpotSearchLimit
	if limit <= 0 {
		limit = 10
	}
	log.Printf("[BOOT] HubSpot enabled base_url=%q search_limit=%d", baseURL, limit)
	c := &Client{
		baseURL:     baseURL,
		apiKey:      cfg.HubSpotAPIKey,
		searchLimit: limit,
		http:        &http.Client{Timeout: 30 * time.Second},
	}
	catalogPath := cfg.HubSpotCatalogPath
	go func() {
		catalog := c.GenerateCatalog(catalogPath)
		c.CatalogCompact = catalog
		log.Printf("[HUBSPOT] catalog: %s", catalog)
	}()
	return c
}

// objectProperties maps each CRM object type to the properties to fetch.
var objectProperties = map[string][]string{
	"contacts":  {"firstname", "lastname", "email", "phone", "company", "jobtitle", "hs_lead_status", "lifecyclestage"},
	"companies": {"name", "domain", "industry", "city", "country", "annualrevenue", "numberofemployees", "lifecyclestage"},
	"deals":     {"dealname", "dealstage", "amount", "closedate", "pipeline", "hubspot_owner_id"},
	"tickets":   {"subject", "content", "hs_pipeline_stage", "hs_ticket_priority", "hubspot_owner_id", "createdate"},
}

// allObjectTypes is the ordered list of CRM object types to search when no type is specified.
var allObjectTypes = []string{"contacts", "companies", "deals", "tickets"}

// deriveName returns a display name for a result based on its object type and properties.
func deriveName(objectType string, props map[string]string) string {
	switch objectType {
	case "contacts":
		first := strings.TrimSpace(props["firstname"])
		last := strings.TrimSpace(props["lastname"])
		full := strings.TrimSpace(first + " " + last)
		if full == "" {
			full = props["email"]
		}
		return full
	case "companies":
		return props["name"]
	case "deals":
		return props["dealname"]
	case "tickets":
		return props["subject"]
	}
	return ""
}

// isoToMillis converts an ISO date string (YYYY-MM-DD) to Unix milliseconds.
func isoToMillis(date string) (int64, error) {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return 0, err
	}
	return t.UnixMilli(), nil
}

// search performs POST /crm/v3/objects/{objectType}/search and returns parsed results.
// after and before are optional ISO YYYY-MM-DD strings to filter by hs_lastmodifieddate.
func (c *Client) search(objectType, query, after, before string) ([]*SearchResult, error) {
	props, ok := objectProperties[objectType]
	if !ok {
		return nil, fmt.Errorf("unknown hubspot object type: %s", objectType)
	}

	bodyMap := map[string]interface{}{
		"query":      query,
		"properties": props,
		"limit":      c.searchLimit,
	}
	var filters []map[string]interface{}
	if after != "" {
		if ms, err := isoToMillis(after); err == nil {
			filters = append(filters, map[string]interface{}{
				"propertyName": "hs_lastmodifieddate", "operator": "GTE", "value": fmt.Sprintf("%d", ms),
			})
		}
	}
	if before != "" {
		if ms, err := isoToMillis(before); err == nil {
			filters = append(filters, map[string]interface{}{
				"propertyName": "hs_lastmodifieddate", "operator": "LTE", "value": fmt.Sprintf("%d", ms),
			})
		}
	}
	if len(filters) > 0 {
		bodyMap["filterGroups"] = []map[string]interface{}{{"filters": filters}}
	}
	body, _ := json.Marshal(bodyMap)

	url := fmt.Sprintf("%s/crm/v3/objects/%s/search", c.baseURL, objectType)
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("hubspot status=%d body=%s", resp.StatusCode, preview(string(rb), 300))
	}

	var out struct {
		Results []struct {
			ID         string            `json:"id"`
			Properties map[string]string `json:"properties"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return nil, fmt.Errorf("hubspot decode: %w", err)
	}

	results := make([]*SearchResult, 0, len(out.Results))
	for _, r := range out.Results {
		props := r.Properties
		if props == nil {
			props = map[string]string{}
		}
		sr := &SearchResult{
			ObjectType: objectType,
			ID:         r.ID,
			Properties: props,
			Name:       deriveName(objectType, props),
		}
		results = append(results, sr)
	}
	return results, nil
}

// Search searches a specific object type. When objectType is empty, searches all types.
// after and before are optional ISO YYYY-MM-DD date bounds (hs_lastmodifieddate filter).
func (c *Client) Search(objectType, query, after, before string) ([]*SearchResult, error) {
	if strings.TrimSpace(objectType) == "" {
		return c.searchAllTypes(query, after, before)
	}
	return c.search(objectType, query, after, before)
}

// searchAllTypes runs Search across all object types and merges results.
func (c *Client) searchAllTypes(query, after, before string) ([]*SearchResult, error) {
	var all []*SearchResult
	var lastErr error
	for _, ot := range allObjectTypes {
		res, err := c.search(ot, query, after, before)
		if err != nil {
			lastErr = err
			continue
		}
		all = append(all, res...)
	}
	if len(all) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return all, nil
}

// FormatContext builds a compact Markdown string to inject into the LLM prompt.
// maxCharsPerResult caps the total character contribution of each record.
func FormatContext(results []*SearchResult, maxCharsPerResult int) string {
	if len(results) == 0 {
		return ""
	}
	if maxCharsPerResult <= 0 {
		maxCharsPerResult = 4000
	}
	var sb strings.Builder
	for _, r := range results {
		var parts []string
		parts = append(parts, fmt.Sprintf("[%s ID=%s]", strings.ToUpper(r.ObjectType), r.ID))
		if r.Name != "" {
			parts = append(parts, "Nome: "+r.Name)
		}
		for k, v := range r.Properties {
			if v == "" {
				continue
			}
			parts = append(parts, fmt.Sprintf("%s: %s", k, v))
		}
		entry := strings.Join(parts, " | ")
		if len(entry) > maxCharsPerResult {
			entry = entry[:maxCharsPerResult]
		}
		sb.WriteString(entry)
		sb.WriteString("\n")
	}
	return strings.TrimSpace(sb.String())
}

// FormatSources produces a short footer listing each record with its type and name.
func FormatSources(results []*SearchResult) string {
	if len(results) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("*Fontes HubSpot CRM:*\n")
	for _, r := range results {
		label := r.Name
		if label == "" {
			label = r.ID
		}
		sb.WriteString(fmt.Sprintf("• %s: %s (ID: %s)\n", strings.Title(r.ObjectType), label, r.ID))
	}
	return strings.TrimSpace(sb.String())
}

// preview returns the first n characters of s with a "…" suffix when truncated.
func preview(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
