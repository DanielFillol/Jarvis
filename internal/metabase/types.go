// internal/metabase/types.go
package metabase

// SessionResp is the response from POST /api/session.
type SessionResp struct {
	ID string `json:"id"`
}

// DatabasesResp wraps the paginated database list from GET /api/database.
type DatabasesResp struct {
	Data  []Database `json:"data"`
	Total int        `json:"total"`
}

// Database is a Metabase database entry.
type Database struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Engine      string `json:"engine"`
	Description string `json:"description"`
}

// DatabaseMetadata is returned by GET /api/database/:id/metadata.
// It includes the database tables together with their fields.
type DatabaseMetadata struct {
	ID     int     `json:"id"`
	Name   string  `json:"name"`
	Engine string  `json:"engine"`
	Tables []Table `json:"tables"`
}

// Table represents a Metabase table (i.e. a database table or view).
type Table struct {
	ID             int     `json:"id"`
	Name           string  `json:"name"`
	DisplayName    string  `json:"display_name"`
	Schema         string  `json:"schema"`
	Description    string  `json:"description"`
	EntityType     string  `json:"entity_type"`
	VisibilityType string  `json:"visibility_type"`
	Fields         []Field `json:"fields"`
}

// IsHidden reports whether the table is marked as hidden in Metabase.
func (t Table) IsHidden() bool {
	return t.VisibilityType == "hidden" || t.VisibilityType == "technical" || t.VisibilityType == "cruft"
}

// Field represents a column in a Metabase table.
type Field struct {
	ID             int    `json:"id"`
	Name           string `json:"name"`
	DisplayName    string `json:"display_name"`
	BaseType       string `json:"base_type"`
	SemanticType   string `json:"semantic_type"`
	Description    string `json:"description"`
	VisibilityType string `json:"visibility_type"`
}

// IsPK reports whether the field is a primary key.
func (f Field) IsPK() bool { return f.SemanticType == "type/PK" }

// IsFK reports whether the field is a foreign key.
func (f Field) IsFK() bool { return f.SemanticType == "type/FK" }

// IsHidden reports whether the field is hidden from normal Metabase views.
func (f Field) IsHidden() bool {
	return f.VisibilityType == "sensitive" || f.VisibilityType == "retired" || f.VisibilityType == "hidden"
}

// QueryRequest is the payload for POST /api/dataset (native SQL execution).
type QueryRequest struct {
	Database int         `json:"database"`
	Type     string      `json:"type"`
	Native   NativeQuery `json:"native"`
}

// NativeQuery holds the raw SQL string.
type NativeQuery struct {
	Query string `json:"query"`
}

// Card represents a saved question (card) in Metabase.
// Fetched from GET /api/card.
type Card struct {
	ID           int              `json:"id"`
	Name         string           `json:"name"`
	Description  string           `json:"description"`
	DatabaseID   int              `json:"database_id"`
	DatasetQuery CardDatasetQuery `json:"dataset_query"`
	Archived     bool             `json:"archived"`
}

// CardDatasetQuery holds the query definition stored on a Card.
type CardDatasetQuery struct {
	Type   string      `json:"type"`   // "native" or "query" (GUI builder)
	Native NativeQuery `json:"native"` // populated only when Type == "native"
}

// NativeSQL returns the raw SQL for native questions, or "" for GUI questions.
func (c Card) NativeSQL() string {
	if c.DatasetQuery.Type != "native" {
		return ""
	}
	return c.DatasetQuery.Native.Query
}

// QueryResult is the top-level response from POST /api/dataset.
type QueryResult struct {
	Data  QueryData `json:"data"`
	Error string    `json:"error"`
}

// QueryData contains the column definitions and row values.
type QueryData struct {
	Cols []QueryCol `json:"cols"`
	Rows [][]any    `json:"rows"`
}

// QueryCol describes a column returned by a query.
type QueryCol struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}
