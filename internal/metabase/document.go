package metabase

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DatabaseMetadata is returned by GET /api/database/:id/metadata.
// It includes the database tables together with their fields.
type DatabaseMetadata struct {
	ID     int     `json:"id"`
	Name   string  `json:"name"`
	Engine string  `json:"engine"`
	Tables []Table `json:"tables"`
}

// Table represents a Metabase table (i.e., a database table or view).
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

// generateSchemaDoc fetches metadata from Metabase and writes a Markdown
// documentation file to outputPath. Environment is a free-form label
// (e.g. "production", "staging") included in the file header.
//
// If outputPath is empty, it defaults to "./docs/metabase_schema.md".
// The parent directory is created automatically when it does not exist.
func generateSchemaDoc(client *Client, outputPath, environment string) error {
	if outputPath == "" {
		outputPath = "./docs/metabase_schema.md"
	}
	if environment == "" {
		environment = "production"
	}

	log.Printf("[METABASE] fetching database list…")
	databases, err := client.ListDatabases()
	if err != nil {
		return fmt.Errorf("metabase: list Databases: %w", err)
	}
	log.Printf("[METABASE] found %d database(s)", len(databases))

	var metas []DatabaseMetadata
	for _, db := range databases {
		log.Printf("[METABASE] fetching metadata for db id=%d name=%q", db.ID, db.Name)
		meta, err := client.GetDatabaseMetadata(db.ID)
		if err != nil {
			log.Printf("[METABASE] warning: metadata for db %d failed: %v — skipping", db.ID, err)
			continue
		}
		metas = append(metas, meta)
	}

	md := renderMarkdown(metas, environment)

	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return fmt.Errorf("metabase: create output dir: %w", err)
	}
	if err := os.WriteFile(outputPath, []byte(md), 0o644); err != nil {
		return fmt.Errorf("metabase: write schema file: %w", err)
	}
	log.Printf("[METABASE] schema written to %s (%d bytes)", outputPath, len(md))

	// Also write the compact version used for SQL generation.
	compactPath := compactSchemaPath(outputPath)
	compact := renderCompactMarkdown(metas, environment)
	if err := os.WriteFile(compactPath, []byte(compact), 0o644); err != nil {
		log.Printf("[METABASE] warning: could not write compact schema to %s: %v", compactPath, err)
	} else {
		log.Printf("[METABASE] compact schema written to %s (%d bytes)", compactPath, len(compact))
	}

	return nil
}

// compactSchemaPath derives the compact schema file path by inserting "_compact"
// before the file extension (e.g. "./docs/metabase_schema.md" → "./docs/metabase_schema_compact.md").
func compactSchemaPath(schemaPath string) string {
	ext := filepath.Ext(schemaPath)
	if ext == "" {
		return schemaPath + "_compact"
	}
	return schemaPath[:len(schemaPath)-len(ext)] + "_compact" + ext
}

// renderCompactMarkdown produces a concise one-line-per-table schema suitable
// for injecting into SQL generation prompts.  Hidden tables are omitted.
// Format: "schema.table: col1[Type/PK], col2[Type/FK], col3[Type], ..."
func renderCompactMarkdown(databases []DatabaseMetadata, environment string) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# Schema Compacto — Metabase (%s)\n", environment))
	sb.WriteString(fmt.Sprintf("# Gerado: %s\n\n", time.Now().UTC().Format("2006-01-02 15:04:05 UTC")))

	for _, db := range databases {
		tables := make([]Table, len(db.Tables))
		copy(tables, db.Tables)
		sort.Slice(tables, func(i, j int) bool {
			return tables[i].Name < tables[j].Name
		})

		engine := db.Engine
		if engine == "" {
			engine = "unknown"
		}
		sb.WriteString(fmt.Sprintf("## %s (id=%d, engine=%s)\n", db.Name, db.ID, engine))

		for _, t := range tables {
			if t.IsHidden() {
				continue
			}

			tableID := t.Name
			if t.Schema != "" && t.Schema != "public" {
				tableID = t.Schema + "." + t.Name
			}

			fields := make([]Field, len(t.Fields))
			copy(fields, t.Fields)
			sort.Slice(fields, func(i, j int) bool {
				pi, pj := fieldSortPriority(fields[i]), fieldSortPriority(fields[j])
				if pi != pj {
					return pi < pj
				}
				return fields[i].Name < fields[j].Name
			})

			parts := make([]string, 0, len(fields))
			for _, f := range fields {
				baseType := cleanType(f.BaseType)
				suffix := ""
				if f.IsPK() {
					suffix = "/PK"
				} else if f.IsFK() {
					suffix = "/FK"
				}
				parts = append(parts, fmt.Sprintf("%s[%s%s]", f.Name, baseType, suffix))
			}
			sb.WriteString(fmt.Sprintf("- %s: %s\n", tableID, strings.Join(parts, ", ")))
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

// fieldSortPriority returns a numeric sort key so PKs sort first, FKs second.
func fieldSortPriority(f Field) int {
	if f.IsPK() {
		return 0
	}
	if f.IsFK() {
		return 1
	}
	return 2
}

// cleanType strips the "type/" prefix Metabase uses (e.g. "type/Integer" → "Integer").
func cleanType(s string) string {
	s = strings.TrimPrefix(s, "type/")
	if s == "" {
		return "—"
	}
	return s
}
