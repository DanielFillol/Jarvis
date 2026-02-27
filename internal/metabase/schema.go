// internal/metabase/schema.go
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

// GenerateSchemaDoc fetches metadata from Metabase and writes a Markdown
// documentation file to outputPath.  environment is a free-form label
// (e.g. "production", "staging") included in the file header.
//
// If outputPath is empty it defaults to "./docs/metabase_schema.md".
// The parent directory is created automatically when it does not exist.
func GenerateSchemaDoc(client *Client, outputPath, environment string) error {
	if outputPath == "" {
		outputPath = "./docs/metabase_schema.md"
	}
	if environment == "" {
		environment = "production"
	}

	log.Printf("[METABASE] fetching database list…")
	databases, err := client.ListDatabases()
	if err != nil {
		return fmt.Errorf("metabase: list databases: %w", err)
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

// CompactSchemaPath returns the path of the compact schema file derived from
// the verbose schema path.  Exported so callers can locate the compact file
// without re-implementing the naming convention.
func CompactSchemaPath(schemaPath string) string {
	return compactSchemaPath(schemaPath)
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

// renderMarkdown converts the fetched metadata into a Markdown string.
func renderMarkdown(databases []DatabaseMetadata, environment string) string {
	var sb strings.Builder

	sb.WriteString("# Documentação de Schema — Metabase\n\n")
	sb.WriteString(fmt.Sprintf(
		"> **Gerado em:** %s  \n> **Ambiente:** %s\n\n",
		time.Now().UTC().Format("2006-01-02 15:04:05 UTC"),
		environment,
	))
	sb.WriteString("---\n\n")

	if len(databases) == 0 {
		sb.WriteString("_Nenhum banco de dados encontrado._\n")
		return sb.String()
	}

	for _, db := range databases {
		// Sort tables alphabetically within each database for stable output.
		tables := make([]Table, len(db.Tables))
		copy(tables, db.Tables)
		sort.Slice(tables, func(i, j int) bool {
			return tables[i].Name < tables[j].Name
		})

		engine := db.Engine
		if engine == "" {
			engine = "unknown"
		}
		sb.WriteString(fmt.Sprintf("## Banco: `%s` (%s) · ID %d\n\n", db.Name, engine, db.ID))

		if len(tables) == 0 {
			sb.WriteString("_Sem tabelas disponíveis._\n\n")
			continue
		}

		for _, t := range tables {
			tableHeader := fmt.Sprintf("### Tabela: `%s`", t.Name)
			if t.Schema != "" && t.Schema != "public" {
				tableHeader = fmt.Sprintf("### Tabela: `%s`.`%s`", t.Schema, t.Name)
			}
			if t.DisplayName != "" && !strings.EqualFold(t.DisplayName, t.Name) {
				tableHeader += fmt.Sprintf(" — _%s_", t.DisplayName)
			}
			if t.IsHidden() {
				tableHeader += " ⚠️ **OCULTA**"
			}
			sb.WriteString(tableHeader + "\n\n")

			if t.Description != "" {
				sb.WriteString(fmt.Sprintf("_%s_\n\n", t.Description))
			}

			// Sort fields: PK first, then FK, then alphabetical.
			fields := make([]Field, len(t.Fields))
			copy(fields, t.Fields)
			sort.Slice(fields, func(i, j int) bool {
				pi, pj := fieldSortPriority(fields[i]), fieldSortPriority(fields[j])
				if pi != pj {
					return pi < pj
				}
				return fields[i].Name < fields[j].Name
			})

			sb.WriteString("| Campo | Tipo | Semântico | Chave | Visibilidade | Descrição |\n")
			sb.WriteString("|-------|------|-----------|-------|--------------|----------|\n")

			for _, f := range fields {
				name := fmt.Sprintf("`%s`", f.Name)
				baseType := cleanType(f.BaseType)
				semType := cleanType(f.SemanticType)
				key := keyLabel(f)
				vis := f.VisibilityType
				if vis == "" || vis == "normal" {
					vis = "—"
				}
				desc := f.Description
				if desc == "" {
					desc = "—"
				}
				sb.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s | %s |\n",
					name, baseType, semType, key, vis, escapeMD(desc)))
			}
			sb.WriteString("\n")
		}
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

// keyLabel returns a short label indicating PK, FK, or nothing.
func keyLabel(f Field) string {
	if f.IsPK() {
		return "**PK**"
	}
	if f.IsFK() {
		return "FK"
	}
	return "—"
}

// escapeMD escapes pipe characters to avoid breaking Markdown tables.
func escapeMD(s string) string {
	return strings.ReplaceAll(s, "|", "\\|")
}

// FormatQueryResult converts a QueryResult into a plain-text table suitable
// for use as LLM context.  At most maxRows rows are shown; pass 0 for the
// default limit of 100.
func FormatQueryResult(r QueryResult, maxRows int) string {
	if r.Error != "" {
		return fmt.Sprintf("[Erro na query: %s]", r.Error)
	}
	cols := r.Data.Cols
	rows := r.Data.Rows
	if len(cols) == 0 {
		return "[Resultado vazio — nenhuma coluna retornada]"
	}
	if maxRows <= 0 {
		maxRows = 100
	}

	total := len(rows)
	if total > maxRows {
		rows = rows[:maxRows]
	}

	var sb strings.Builder

	// Header row
	headers := make([]string, len(cols))
	for i, col := range cols {
		h := col.DisplayName
		if h == "" {
			h = col.Name
		}
		headers[i] = h
	}
	sb.WriteString(strings.Join(headers, " | "))
	sb.WriteString("\n")

	// Separator
	seps := make([]string, len(cols))
	for i := range cols {
		seps[i] = "---"
	}
	sb.WriteString(strings.Join(seps, " | "))
	sb.WriteString("\n")

	// Data rows
	for _, row := range rows {
		cells := make([]string, len(cols))
		for i := range cols {
			if i < len(row) && row[i] != nil {
				cells[i] = fmt.Sprintf("%v", row[i])
			} else {
				cells[i] = "NULL"
			}
		}
		sb.WriteString(strings.Join(cells, " | "))
		sb.WriteString("\n")
	}

	if total > maxRows {
		sb.WriteString(fmt.Sprintf("\n_(mostrando %d de %d linhas)_\n", maxRows, total))
	} else {
		sb.WriteString(fmt.Sprintf("\n_%d linha(s)_\n", total))
	}
	return sb.String()
}
