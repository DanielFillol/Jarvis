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

	"github.com/DanielFillol/Jarvis/internal/config"
)

// Client wraps the Metabase REST API using API key authentication.
// Set METABASE_API_KEY to a key generated in Metabase ≥ 0.47
// (Admin → Settings → Authentication → API Keys).
type Client struct {
	baseURL     string
	apiKey      string
	httpClient  *http.Client // used for metadata/listing calls
	queryClient *http.Client // used for /api/dataset
	Databases   []Database
	Cards       []Card
	Schemas     map[int][]string
}

// NewClient constructs a new metabase client from the provided configuration.
func NewClient(cfg config.Config) *Client {
	if cfg.MetabaseBaseURL == "" || cfg.MetabaseAPIKey == "" {
		log.Printf("[METABASE] not configured — set METABASE_BASE_URL + METABASE_API_KEY to enable")
		return nil
	}
	c := &Client{
		baseURL:     strings.TrimRight(cfg.MetabaseBaseURL, "/"),
		apiKey:      cfg.MetabaseAPIKey,
		httpClient:  &http.Client{Timeout: 30 * time.Second},
		queryClient: &http.Client{Timeout: cfg.MetabaseQueryTimeout},
		Databases:   nil,
		Cards:       nil,
		Schemas:     nil,
	}
	c = complementClient(c, cfg)
	return c
}

// Get performs an authenticated GET and JSON-decodes the response into out.
func (c *Client) Get(path string, out any) error {
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

// Post performs an authenticated POST with a JSON body using the supplied
// http.Client, and JSON-decodes the response into out.
func (c *Client) Post(hc *http.Client, path string, payload, out any) error {
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

// ListDatabases returns all Databases visible to the API key.
func (c *Client) ListDatabases() ([]Database, error) {
	var r DatabasesResp
	if err := c.Get("/api/database", &r); err != nil {
		return nil, err
	}
	return r.Data, nil
}

// ListAccessibleSchemas returns the schema names that are actually queryable
// for the given database.  Uses the fast httpClient (30 s).
//
// Redshift exposes externally shared and late-binding-view schemas only via
// svv_tables (not information_schema.tables), so we try svv_tables first and
// fall back to information_schema.tables for non-Redshift Databases.
func (c *Client) ListAccessibleSchemas(databaseID int) ([]string, error) {
	const svvQ = `SELECT DISTINCT table_schema FROM svv_tables
WHERE table_schema NOT IN ('pg_catalog','information_schema','pg_internal','pg_toast','pg_aoseg','pg_automv')
ORDER BY table_schema`

	const stdQ = `SELECT DISTINCT table_schema FROM information_schema.tables
WHERE table_schema NOT IN ('pg_catalog','information_schema','pg_internal','pg_toast','pg_aoseg')
ORDER BY table_schema`

	schemas, err := c.RunSchemaDiscovery(databaseID, svvQ)
	if err != nil || len(schemas) == 0 {
		if err != nil {
			log.Printf("[METABASE] svv_tables query failed db=%d (%v) — trying information_schema", databaseID, err)
		}
		schemas, err = c.RunSchemaDiscovery(databaseID, stdQ)
	}
	return schemas, err
}

// RunSchemaDiscovery executes a single-column schema-listing query using the
// fast httpClient and returns the distinct schema name strings.
func (c *Client) RunSchemaDiscovery(databaseID int, q string) ([]string, error) {
	payload := QueryRequest{Database: databaseID, Type: "native", Native: NativeQuery{Query: q}}
	var result QueryResult
	if err := c.Post(c.httpClient, "/api/dataset", payload, &result); err != nil {
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

// ListCards returns all non-archived saved questions visible to the API key.
// Note: the list endpoint typically omits the dataset_query SQL.  Use
// GetCard(id) to retrieve the native SQL for a specific card.
func (c *Client) ListCards() ([]Card, error) {
	var cards []Card
	if err := c.Get("/api/card", &cards); err != nil {
		return nil, err
	}
	// Filter out archived Cards up-front so callers never see stale entries.
	out := cards[:0]
	for _, card := range cards {
		if !card.Archived {
			out = append(out, card)
		}
	}
	return out, nil
}

// ExecuteNativeQuery runs a raw SQL query against the specified Metabase database
// using the queryClient (which carries the configured MetabaseQueryTimeout).
func (c *Client) ExecuteNativeQuery(databaseID int, sql string) (*QueryResult, error) {
	payload := QueryRequest{Database: databaseID, Type: "native", Native: NativeQuery{Query: sql}}
	var result QueryResult
	if err := c.Post(c.queryClient, "/api/dataset", payload, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// GetDatabaseMetadata fetches detailed metadata for a single database,
// including all its tables and their fields.
func (c *Client) GetDatabaseMetadata(id int) (DatabaseMetadata, error) {
	var m DatabaseMetadata
	path := fmt.Sprintf("/api/database/%d/metadata", id)
	if err := c.Get(path, &m); err != nil {
		return DatabaseMetadata{}, err
	}
	return m, nil
}
