// internal/metabase/client.go
package metabase

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// Client wraps the Metabase REST API using API key authentication.
// Set METABASE_API_KEY to a key generated in Metabase ≥ 0.47
// (Admin → Settings → Authentication → API Keys).
type Client struct {
	baseURL     string
	apiKey      string
	httpClient  *http.Client // used for metadata/listing calls (30s)
	queryClient *http.Client // used for /api/dataset — analytical DBs like Redshift can be slow
}

// NewClientAPIKey creates a Client authenticated via the X-Api-Key header.
// queryTimeout controls the deadline for SQL execution via /api/dataset;
// pass 0 to use the default of 5 minutes.
func NewClientAPIKey(baseURL, apiKey string, queryTimeout time.Duration) *Client {
	if queryTimeout <= 0 {
		queryTimeout = 5 * time.Minute
	}
	log.Printf("[METABASE] client initialized baseURL=%q queryTimeout=%s", baseURL, queryTimeout)
	return &Client{
		baseURL:     strings.TrimRight(baseURL, "/"),
		apiKey:      apiKey,
		httpClient:  &http.Client{Timeout: 30 * time.Second},
		queryClient: &http.Client{Timeout: queryTimeout},
	}
}

// get performs an authenticated GET and JSON-decodes the response into out.
func (c *Client) get(path string, out any) error {
	req, err := http.NewRequest("GET", c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("metabase: build GET %s: %w", path, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Api-Key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("metabase: GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("metabase: GET %s status=%d body=%s", path, resp.StatusCode, clip(string(rb), 400))
	}
	if err := json.Unmarshal(rb, out); err != nil {
		return fmt.Errorf("metabase: decode %s: %w", path, err)
	}
	return nil
}

// post performs an authenticated POST using the default httpClient.
func (c *Client) post(path string, payload, out any) error {
	return c.postWith(c.httpClient, path, payload, out)
}

// postWith performs an authenticated POST with a JSON body using the supplied
// http.Client, and JSON-decodes the response into out.
func (c *Client) postWith(hc *http.Client, path string, payload, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("metabase: encode POST %s: %w", path, err)
	}
	req, err := http.NewRequest("POST", c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("metabase: build POST %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Api-Key", c.apiKey)

	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("metabase: POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("metabase: POST %s status=%d body=%s", path, resp.StatusCode, clip(string(rb), 600))
	}
	if err := json.Unmarshal(rb, out); err != nil {
		return fmt.Errorf("metabase: decode POST %s: %w", path, err)
	}
	return nil
}

// ListCards returns all non-archived saved questions visible to the API key.
// Note: the list endpoint typically omits the dataset_query SQL.  Use
// GetCard(id) to retrieve the native SQL for a specific card.
func (c *Client) ListCards() ([]Card, error) {
	var cards []Card
	if err := c.get("/api/card", &cards); err != nil {
		return nil, err
	}
	// Filter out archived cards up-front so callers never see stale entries.
	out := cards[:0]
	for _, card := range cards {
		if !card.Archived {
			out = append(out, card)
		}
	}
	return out, nil
}

// GetCard fetches a single saved question by ID, including its full
// dataset_query (native SQL).
func (c *Client) GetCard(id int) (Card, error) {
	var card Card
	if err := c.get(fmt.Sprintf("/api/card/%d", id), &card); err != nil {
		return Card{}, err
	}
	return card, nil
}

// ListAccessibleSchemas returns the schema names that are actually queryable
// for the given database.  Uses the fast httpClient (30 s).
//
// Redshift exposes externally-shared and late-binding-view schemas only via
// svv_tables (not information_schema.tables), so we try svv_tables first and
// fall back to information_schema.tables for non-Redshift databases.
func (c *Client) ListAccessibleSchemas(databaseID int) ([]string, error) {
	const svvQ = `SELECT DISTINCT table_schema FROM svv_tables
WHERE table_schema NOT IN ('pg_catalog','information_schema','pg_internal','pg_toast','pg_aoseg','pg_automv')
ORDER BY table_schema`

	const stdQ = `SELECT DISTINCT table_schema FROM information_schema.tables
WHERE table_schema NOT IN ('pg_catalog','information_schema','pg_internal','pg_toast','pg_aoseg')
ORDER BY table_schema`

	schemas, err := c.runSchemaDiscovery(databaseID, svvQ)
	if err != nil || len(schemas) == 0 {
		if err != nil {
			log.Printf("[METABASE] svv_tables query failed db=%d (%v) — trying information_schema", databaseID, err)
		}
		schemas, err = c.runSchemaDiscovery(databaseID, stdQ)
	}
	return schemas, err
}

// runSchemaDiscovery executes a single-column schema-listing query using the
// fast httpClient and returns the distinct schema name strings.
func (c *Client) runSchemaDiscovery(databaseID int, q string) ([]string, error) {
	payload := QueryRequest{Database: databaseID, Type: "native", Native: NativeQuery{Query: q}}
	var result QueryResult
	if err := c.postWith(c.httpClient, "/api/dataset", payload, &result); err != nil {
		return nil, err
	}
	if result.Error != "" {
		return nil, fmt.Errorf("%s", clip(result.Error, 200))
	}
	var schemas []string
	for _, row := range result.Data.Rows {
		if len(row) > 0 && row[0] != nil {
			if s := strings.TrimSpace(fmt.Sprintf("%v", row[0])); s != "" {
				schemas = append(schemas, s)
			}
		}
	}
	return schemas, nil
}

// ListDatabases returns all databases visible to the API key.
func (c *Client) ListDatabases() ([]Database, error) {
	var r DatabasesResp
	if err := c.get("/api/database", &r); err != nil {
		return nil, err
	}
	return r.Data, nil
}

// GetDatabaseMetadata fetches detailed metadata for a single database,
// including all its tables and their fields.
func (c *Client) GetDatabaseMetadata(id int) (DatabaseMetadata, error) {
	var m DatabaseMetadata
	path := fmt.Sprintf("/api/database/%d/metadata", id)
	if err := c.get(path, &m); err != nil {
		return DatabaseMetadata{}, err
	}
	return m, nil
}

// RunQuery executes a native SQL query against the given database and returns
// the result.  Only SELECT queries should be passed; the caller is responsible
// for validating the SQL before calling this method.
// Uses the long-timeout queryClient to accommodate analytical databases such
// as Redshift that may take minutes to return results.
func (c *Client) RunQuery(databaseID int, sql string) (QueryResult, error) {
	payload := QueryRequest{
		Database: databaseID,
		Type:     "native",
		Native:   NativeQuery{Query: sql},
	}
	log.Printf("[METABASE] RunQuery db=%d timeout=%s sql_first_line=%s", databaseID, c.queryClient.Timeout, clip(firstSQLLine(sql), 120))
	var result QueryResult
	if err := c.postWith(c.queryClient, "/api/dataset", payload, &result); err != nil {
		return QueryResult{}, err
	}
	if result.Error != "" {
		log.Printf("[METABASE] query error: %s", clip(result.Error, 300))
	}
	return result, nil
}

// clip truncates s to at most n bytes for use in error messages.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// firstSQLLine returns the first non-empty, non-comment line of a SQL string,
// useful for concise logging without printing the full query.
func firstSQLLine(sql string) string {
	for _, line := range strings.Split(sql, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "--") {
			return line
		}
	}
	return sql
}
