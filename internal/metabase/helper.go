package metabase

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/DanielFillol/Jarvis/internal/config"
)

// Database is a Metabase database entry.
type Database struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Engine      string `json:"engine"`
	Description string `json:"description"`
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

// NativeQuery holds the raw SQL string.
type NativeQuery struct {
	Query string `json:"query"`
}

// DatabasesResp wraps the paginated database list from GET /api/database.
type DatabasesResp struct {
	Data  []Database `json:"data"`
	Total int        `json:"total"`
}

// QueryRequest is the payload for POST /api/dataset (native SQL execution).
type QueryRequest struct {
	Database int         `json:"database"`
	Type     string      `json:"type"`
	Native   NativeQuery `json:"native"`
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

// clip truncates s to at most n bytes for use in error messages.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func complementClient(c *Client, cfg config.Config) *Client {
	dbs, err := c.ListDatabases()
	if err != nil {
		log.Printf("[METABASE] ListDatabases failed: %v — Metabase integration disabled", err)
	}
	for _, db := range dbs {
		log.Printf("[METABASE] database id=%d name=%q engine=%s", db.ID, db.Name, db.Engine)
	}

	// Discover which schemas are actually queryable per database so we can
	// filter the compact schema and prevent the LLM from using phantom schemas.
	accessibleSchemas := make(map[int][]string)
	for _, db := range dbs {
		schemas, err := c.ListAccessibleSchemas(db.ID)
		if err != nil {
			log.Printf("[METABASE] ListAccessibleSchemas db=%d failed: %v — schema filtering disabled for this db", db.ID, err)
			continue
		}
		accessibleSchemas[db.ID] = schemas
		log.Printf("[METABASE] db=%d accessible schemas: %v", db.ID, schemas)
	}

	// Load saved questions (Cards) for use as SQL examples during query generation.
	// The list endpoint returns names/IDs only; individual card SQL is fetched on demand.
	cards, err := c.ListCards()
	if err != nil {
		log.Printf("[METABASE] ListCards failed: %v — saved questions will not be used as examples", err)
	} else {
		log.Printf("[METABASE] loaded %d saved questions for keyword matching (SQL fetched on demand)", len(cards))
	}

	// Generate schema documentation asynchronously so the startup is not blocked.
	go func() {
		if err := generateSchemaDoc(c, cfg.MetabaseSchemaPath, cfg.MetabaseEnv); err != nil {
			log.Printf("[METABASE] schema generation failed: %v", err)
		}
	}()

	c.Databases = dbs
	c.Cards = cards
	c.Schemas = accessibleSchemas
	return c
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
