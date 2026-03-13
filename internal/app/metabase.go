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

// isAllZeroResult reports whether a successful QueryResult contains only
// numeric-zero values across all rows. Returns false for empty result sets
// or for result sets that contain no numeric columns at all (text-only).
func isAllZeroResult(qr *metabase.QueryResult) bool {
	if qr == nil || len(qr.Data.Rows) == 0 {
		return false
	}
	foundNumeric := false
	for _, row := range qr.Data.Rows {
		for _, cell := range row {
			switch v := cell.(type) {
			case float64:
				foundNumeric = true
				if v != 0 {
					return false
				}
			case int:
				foundNumeric = true
				if v != 0 {
					return false
				}
			case int64:
				foundNumeric = true
				if v != 0 {
					return false
				}
			}
		}
	}
	return foundNumeric
}

// metabaseQueryResult holds the outcome of runMetabaseQuery.
type metabaseQueryResult struct {
	DBCtx       string
	QueryResult *metabase.QueryResult
	ExecutedSQL string
}

// runMetabaseQuery generates a SQL query via the LLM, executes it against the
// specified Metabase database, and returns a metabaseQueryResult containing the
// formatted context string, the raw QueryResult, and the executed SQL.
// It retries up to three times on failure.
//
// When the LLM requests clarification before generating SQL, DBCtx is prefixed
// with llm.ClarificationPrefix and QueryResult/ExecutedSQL are nil/"".
func (s *Service) runMetabaseQuery(question, threadHist string, dbID int, baseSQL string, wantsAllRows bool) metabaseQueryResult {
	if s.Metabase == nil {
		return metabaseQueryResult{}
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

	hintsCtx := loadDBHints(s.Cfg.SQLHintsDir, dbID)

	const maxAttempts = 3
	lastSQL := strings.TrimSpace(baseSQL)
	lastErr := ""
	var zeroResult *metabase.QueryResult // non-nil when phase 1 produced an all-zero result
	var zeroSQL string                   // the SQL that produced the zero result
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		sql, err := s.LLM.GenerateSQL(question, threadHist, schema, lastSQL, lastErr, dbEngine, hintsCtx, wantsAllRows, s.Cfg.OpenAIModel)
		if err != nil {
			log.Printf("[METABASE] GenerateSQL attempt %d failed: %v", attempt, err)
			continue
		}
		if strings.HasPrefix(sql, llm.ClarificationPrefix) {
			log.Printf("[METABASE] LLM requested clarification (attempt %d)", attempt)
			return metabaseQueryResult{DBCtx: sql}
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
		if !isAllZeroResult(qr) {
			ctx := fmt.Sprintf("Query executada (db=%d):\n```sql\n%s\n```\n\nResultado:\n%s",
				dbID, sql, metabase.FormatQueryResult(*qr, 100))
			return metabaseQueryResult{DBCtx: ctx, QueryResult: qr, ExecutedSQL: sql}
		}
		// All-zero — save and break; Phase 2 will verify.
		log.Printf("[METABASE] zero result detected at attempt %d — entering zero-result retry phase", attempt)
		zeroResult = qr
		zeroSQL = sql
		lastSQL = sql
		break
	}

	if zeroResult != nil {
		const maxZeroRetries = 3
		const zeroHint = "A query anterior retornou apenas zeros em todas as colunas numéricas. " +
			"Verifique se os filtros, JOINs e nomes de tabelas/colunas estão corretos. " +
			"Tente uma abordagem alternativa para confirmar se o resultado é realmente zero ou se há um erro na query."

		for zeroAttempt := 1; zeroAttempt <= maxZeroRetries; zeroAttempt++ {
			log.Printf("[METABASE] zero-result retry %d/%d for db=%d", zeroAttempt, maxZeroRetries, dbID)
			sql, err := s.LLM.GenerateSQL(question, threadHist, schema, lastSQL, zeroHint, dbEngine, hintsCtx, wantsAllRows, s.Cfg.OpenAIModel)
			if err != nil {
				log.Printf("[METABASE] zero-retry GenerateSQL attempt %d failed: %v", zeroAttempt, err)
				continue
			}
			if strings.HasPrefix(sql, llm.ClarificationPrefix) {
				log.Printf("[METABASE] LLM requested clarification during zero-retry (attempt %d)", zeroAttempt)
				return metabaseQueryResult{DBCtx: sql}
			}
			log.Printf("[METABASE] zero-retry %d sql: %s", zeroAttempt, clip(sql, 400))
			qr, err := s.Metabase.ExecuteNativeQuery(dbID, sql)
			if err != nil {
				log.Printf("[METABASE] zero-retry ExecuteNativeQuery attempt %d failed: %v", zeroAttempt, err)
				lastSQL = sql
				continue
			}
			if qr.Error != "" {
				log.Printf("[METABASE] zero-retry query error attempt %d: %s", zeroAttempt, clip(qr.Error, 200))
				lastSQL = sql
				continue
			}
			log.Printf("[METABASE] zero-retry %d succeeded rows=%d", zeroAttempt, len(qr.Data.Rows))
			if !isAllZeroResult(qr) {
				log.Printf("[METABASE] zero-result overridden by non-zero result on retry %d", zeroAttempt)
				ctx := fmt.Sprintf("Query executada (db=%d):\n```sql\n%s\n```\n\nResultado:\n%s",
					dbID, sql, metabase.FormatQueryResult(*qr, 100))
				return metabaseQueryResult{DBCtx: ctx, QueryResult: qr, ExecutedSQL: sql}
			}
			log.Printf("[METABASE] zero-retry %d also returned all-zeros", zeroAttempt)
			lastSQL = sql
		}

		// All retries confirmed zero — return original zero result.
		log.Printf("[METABASE] zero result confirmed after %d retries for db=%d", maxZeroRetries, dbID)
		zeroCtx := fmt.Sprintf(
			"Query executada (db=%d):\n```sql\n%s\n```\n\nResultado (resultado confirmado: 0):\n%s",
			dbID, zeroSQL, metabase.FormatQueryResult(*zeroResult, 100),
		)
		return metabaseQueryResult{DBCtx: zeroCtx, QueryResult: zeroResult, ExecutedSQL: zeroSQL}
	}

	log.Printf("[METABASE] all %d attempts failed for db=%d", maxAttempts, dbID)
	return metabaseQueryResult{}
}

// loadDBHints reads the hint file for the given database ID from dir.
// Returns "" when the file is absent or empty — callers treat that as "no hints".
func loadDBHints(dir string, dbID int) string {
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	path := filepath.Join(dir, fmt.Sprintf("db_%d.md", dbID))
	data, err := os.ReadFile(path)
	if err != nil {
		return "" // file absent is normal — not an error
	}
	return strings.TrimSpace(string(data))
}
