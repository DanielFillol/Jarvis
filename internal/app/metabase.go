package app

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/DanielFillol/Jarvis/internal/llm"
	"github.com/DanielFillol/Jarvis/internal/metabase"
)

// formattedMetabaseDatabases returns the available Metabase databases formatted
// as ["1: Production DB (postgres)", ...] for injection into the router prompt.
// Returns nil when Metabase is not configured.
func (s *Service) formattedMetabaseDatabases() []string {
	if s.Metabase == nil || len(s.Metabase.Databases) == 0 {
		return nil
	}
	out := make([]string, 0, len(s.Metabase.Cards))
	for _, db := range s.Metabase.Databases {
		engine := db.Engine
		if engine == "" {
			engine = "unknown"
		}
		out = append(out, fmt.Sprintf("%d: %s (%s)", db.ID, db.Name, engine))
	}
	return out
}

// loadThreadDBID returns the last Metabase database ID used in a thread,
// or (0, false) when none has been stored.
func (s *Service) loadThreadDBID(channel, threadTs string) (int, bool) {
	v, ok := s.threadLastDBID.Load(channel + ":" + threadTs)
	if !ok {
		return 0, false
	}
	id, ok := v.(int)
	return id, ok && id > 0
}

// storeThreadDBID persists the Metabase database ID used in a thread so that
// follow-up messages can be routed correctly without hardcoded keyword matching.
func (s *Service) storeThreadDBID(channel, threadTs string, dbID int) {
	s.threadLastDBID.Store(channel+":"+threadTs, dbID)
}

// loadMetabaseSchema reads the compact schema documentation from disk.
// It tries the compact variant first (faster for the LLM), then falls back to
// the full schema.  Returns an empty string when no schema file is found.
func (s *Service) loadMetabaseSchema() string {
	base := strings.TrimSpace(s.Cfg.MetabaseSchemaPath)
	if base == "" {
		return ""
	}
	ext := filepath.Ext(base)
	compactPath := base[:len(base)-len(ext)] + "_compact" + ext
	data, err := os.ReadFile(compactPath)
	if err != nil {
		data, err = os.ReadFile(base)
		if err != nil {
			log.Printf("[METABASE] schema not found (tried %s and %s): %v", compactPath, base, err)
			return ""
		}
	}
	return string(data)
}

// filterSchemaForDatabase extracts only the schema section for the given
// Metabase database ID from the full compact schema document.
// Section headers look like: ## Name (id=N, engine=X)
// If no matching section is found, the full schema is returned unchanged.
func filterSchemaForDatabase(fullSchema string, dbID int) string {
	if dbID <= 0 || fullSchema == "" {
		return fullSchema
	}
	target := fmt.Sprintf("(id=%d,", dbID)
	lines := strings.Split(fullSchema, "\n")
	startIdx := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") && strings.Contains(trimmed, target) {
			startIdx = i
			break
		}
	}
	if startIdx == -1 {
		return fullSchema
	}
	var sb strings.Builder
	for i := startIdx; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if i > startIdx && strings.HasPrefix(trimmed, "## ") {
			break
		}
		sb.WriteString(lines[i])
		sb.WriteByte('\n')
	}
	return strings.TrimSpace(sb.String())
}

// runMetabaseQuery generates a SQL query via the LLM, executes it against the
// specified Metabase database, and returns a formatted context string, the raw
// QueryResult, and the executed SQL.  It retries up to three times on failure.
//
// When the LLM requests clarification before generating SQL, dbCtx is prefixed
// with llm.ClarificationPrefix and result/sql are nil/"".
func (s *Service) runMetabaseQuery(question, threadHist string, dbID int, baseSQL string) (dbCtx string, result *metabase.QueryResult, executedSQL string) {
	if s.Metabase == nil {
		return "", nil, ""
	}
	fullSchema := s.loadMetabaseSchema()
	schema := filterSchemaForDatabase(fullSchema, dbID)
	log.Printf("[METABASE] schema for db=%d: %d chars (full=%d)", dbID, len(schema), len(fullSchema))

	dbEngine := ""
	for _, db := range s.Metabase.Databases {
		if db.ID == dbID {
			dbEngine = db.Engine
			break
		}
	}

	const maxAttempts = 3
	lastSQL := strings.TrimSpace(baseSQL)
	lastErr := ""
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		sql, err := s.LLM.GenerateSQL(question, threadHist, schema, lastSQL, lastErr, dbEngine, s.Cfg.OpenAIModel)
		if err != nil {
			log.Printf("[METABASE] GenerateSQL attempt %d failed: %v", attempt, err)
			continue
		}
		if strings.HasPrefix(sql, llm.ClarificationPrefix) {
			log.Printf("[METABASE] LLM requested clarification (attempt %d)", attempt)
			return sql, nil, ""
		}
		log.Printf("[METABASE] attempt %d sql: %s", attempt, clip(sql, 400))
		qr, err := s.Metabase.ExecuteNativeQuery(dbID, sql)
		if err != nil {
			log.Printf("[METABASE] ExecuteNativeQuery attempt %d failed: %v", attempt, err)
			lastSQL = sql
			lastErr = err.Error()
			continue
		}
		if qr.Error != "" {
			log.Printf("[METABASE] query error attempt %d: %s", attempt, clip(qr.Error, 200))
			lastSQL = sql
			lastErr = qr.Error
			continue
		}
		log.Printf("[METABASE] query succeeded attempt %d rows=%d", attempt, len(qr.Data.Rows))
		ctx := fmt.Sprintf("Query executada (db=%d):\n```sql\n%s\n```\n\nResultado:\n%s",
			dbID, sql, metabase.FormatQueryResult(*qr, 100))
		return ctx, qr, sql
	}
	log.Printf("[METABASE] all %d attempts failed for db=%d", maxAttempts, dbID)
	return "", nil, ""
}
