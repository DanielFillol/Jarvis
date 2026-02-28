// internal/llm/sql.go
package llm

import (
	"fmt"
	"log"
	"strings"
)

// GenerateSQL asks the LLM to produce a safe SELECT query that answers
// question using the provided Metabase schema documentation.
//
// threadHistory provides prior conversation context so the LLM can resolve
// pronouns and references to entities mentioned in earlier turns
// (e.g. "ela" → "Multilixo" when established in a previous message).
//
// baseSQL is the last SQL query executed in the current thread (if any).
// When non-empty it is injected as a "QUERY BASE" so the LLM can use it as
// a starting point for follow-up questions, preserving existing filters
// (especially date filters) instead of rewriting from scratch.
//
// engineType is the Metabase database engine string (e.g. "redshift",
// "postgres", "mysql", "bigquery").  It is used to inject dialect-specific
// SQL rules into the prompt so the generated SQL is always compatible with
// the target database.  Pass an empty string when the engine is unknown.
//
// Returns an empty string (and nil error) when the LLM determines that the
// question cannot be answered from the available schema.  Returns an error
// only when the API call itself fails.
func (c *Client) GenerateSQL(question, threadHistory, schemaDoc, examples, baseSQL string, databaseID int, engineType string, model string) (string, error) {
	if strings.TrimSpace(schemaDoc) == "" {
		return "", nil
	}

	threadSection := ""
	if t := strings.TrimSpace(threadHistory); t != "" {
		threadSection = fmt.Sprintf(`
CONTEXTO DA CONVERSA (use para resolver pronomes e referências como "ela", "aquela empresa", "o mesmo produto"):
%s

`, clip(t, 3000))
	}

	examplesSection := ""
	if e := strings.TrimSpace(examples); e != "" {
		examplesSection = "\n" + e + "\n"
	}

	baseSQLSection := ""
	if b := strings.TrimSpace(baseSQL); b != "" {
		baseSQLSection = fmt.Sprintf("\nQUERY BASE (última query executada nesta conversa — veja regra 10):\n```sql\n%s\n```\n", b)
	}

	engineRules := engineSpecificRules(engineType)
	prompt := fmt.Sprintf(`Você é um especialista em SQL. Com base no schema do banco de dados e nos exemplos abaixo, escreva uma query SQL para responder à pergunta do usuário.

REGRAS OBRIGATÓRIAS:
1. Use apenas SELECT (ou WITH ... SELECT). NUNCA use INSERT, UPDATE, DELETE, DROP, CREATE, ALTER, TRUNCATE ou qualquer instrução que modifique dados.
2. Use apenas tabelas e colunas que existem no schema fornecido.
3. Se não for possível responder à pergunta com os dados disponíveis, responda exatamente com: IMPOSSIBLE
4. Retorne APENAS o SQL puro — sem explicações, sem blocos de código, sem comentários, sem markdown.
5. Use LIMIT 100 salvo quando a pergunta exigir uma agregação total (SUM, COUNT, AVG, etc.).
6. Prefira nomes de coluna descritivos no SELECT (use aliases se necessário).
7. Quando existirem perguntas salvas similares, prefira usar as mesmas tabelas e estrutura delas como base.
8. Para filtrar por nome de empresa/cliente, use comparação case-insensitive (ILIKE, LOWER(), etc. conforme o dialeto) em vez de comparação exata (=), para tolerar diferenças de capitalização e codificação.
9. Ao detalhar resultados de uma query anterior (drill-down), use o mesmo campo que foi utilizado no GROUP BY / SELECT da query original. Ex: se a query anterior agrupou por fantasy_name, filtre por fantasy_name — não por legal_name ou outro campo similar.
10. Se houver uma QUERY BASE abaixo, use-a como ponto de partida para perguntas de follow-up (refinamentos, filtros adicionais, agrupamentos diferentes sobre os mesmos dados). NUNCA remova filtros já existentes na QUERY BASE — especialmente filtros de data e de entidade principal. Se a pergunta for sobre um tema completamente diferente, ignore a QUERY BASE e escreva do zero.%s
%s%s%s
SCHEMA DO BANCO (ID: %d):
%s

PERGUNTA DO USUÁRIO:
%s

SQL:`, engineRules, threadSection, examplesSection, baseSQLSection, databaseID, clip(schemaDoc, 120000), question)

	messages := []OpenAIMessage{{Role: "user", Content: prompt}}
	out, err := c.Chat(messages, model, 0.1, 2000)
	if err != nil {
		return "", err
	}

	out = strings.TrimSpace(stripCodeFences(out))

	if strings.EqualFold(out, "IMPOSSIBLE") || out == "" {
		log.Printf("[LLM] GenerateSQL: model replied IMPOSSIBLE or empty for question=%q", clip(question, 120))
		return "", nil
	}

	// Safety guard: only allow SELECT / WITH … SELECT queries.
	upper := strings.ToUpper(strings.TrimSpace(out))
	if !strings.HasPrefix(upper, "SELECT") && !strings.HasPrefix(upper, "WITH") {
		log.Printf("[LLM] GenerateSQL: rejected non-SELECT query: %s", clip(out, 200))
		return "", nil
	}

	log.Printf("[LLM] GenerateSQL: generated sql_first_line=%s", clip(firstSQLLine(out), 120))
	return out, nil
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

// GenerateCorrectedSQL asks the LLM to fix a failed SQL query.
// It receives the original question, schema, the query that produced an error,
// and the error message returned by Metabase, and returns a corrected query.
// Returns an empty string (and nil error) when the LLM cannot fix the query.
func (c *Client) GenerateCorrectedSQL(question, schemaDoc, failedSQL, errorMsg, engineType string, databaseID int, model string) (string, error) {
	if strings.TrimSpace(schemaDoc) == "" {
		return "", nil
	}

	engineRules := engineSpecificRules(engineType)
	prompt := fmt.Sprintf(`Você é um especialista em SQL. A query abaixo foi executada no banco de dados (ID: %d) e retornou um erro. Corrija-a para responder à pergunta original.

PERGUNTA ORIGINAL:
%s

QUERY COM ERRO:
%s

ERRO RETORNADO:
%s

REGRAS OBRIGATÓRIAS:
1. Use apenas SELECT (ou WITH ... SELECT). NUNCA use INSERT, UPDATE, DELETE, DROP, CREATE, ALTER, TRUNCATE.
2. Use apenas tabelas e colunas que existem no schema fornecido.
3. Se não for possível corrigir, responda exatamente com: IMPOSSIBLE
4. Retorne APENAS o SQL puro — sem explicações, sem blocos de código, sem comentários, sem markdown.
5. Use LIMIT 100 salvo quando a pergunta exigir agregação total (SUM, COUNT, AVG, etc.).
6. Para filtrar por nome de empresa/cliente, use ILIKE ou LOWER() em vez de comparação exata.%s

SCHEMA DO BANCO (ID: %d):
%s

SQL CORRIGIDO:`,
		databaseID, question, failedSQL, clip(errorMsg, 600), engineRules, databaseID, clip(schemaDoc, 100000))

	messages := []OpenAIMessage{{Role: "user", Content: prompt}}
	out, err := c.Chat(messages, model, 0.1, 2000)
	if err != nil {
		return "", err
	}

	out = strings.TrimSpace(stripCodeFences(out))

	if strings.EqualFold(out, "IMPOSSIBLE") || out == "" {
		log.Printf("[LLM] GenerateCorrectedSQL: model replied IMPOSSIBLE or empty")
		return "", nil
	}

	upper := strings.ToUpper(strings.TrimSpace(out))
	if !strings.HasPrefix(upper, "SELECT") && !strings.HasPrefix(upper, "WITH") {
		log.Printf("[LLM] GenerateCorrectedSQL: rejected non-SELECT query: %s", clip(out, 200))
		return "", nil
	}

	log.Printf("[LLM] GenerateCorrectedSQL: corrected sql_first_line=%s", clip(firstSQLLine(out), 120))
	return out, nil
}

// engineSpecificRules returns a block of numbered SQL rules tailored to the
// given Metabase engine string (e.g. "redshift", "postgres", "mysql",
// "bigquery", "snowflake").  An empty string is returned when the engine is
// unknown so the generic rules still apply.  The block starts with a newline
// so it slots directly into the prompt rules section.
func engineSpecificRules(engine string) string {
	e := strings.ToLower(strings.TrimSpace(engine))
	switch {
	case strings.Contains(e, "redshift"):
		return `
10. O banco usa Amazon Redshift — NÃO use a cláusula FILTER(WHERE ...). Substitua por CASE WHEN: COUNT(CASE WHEN condição THEN 1 END) ou SUM(CASE WHEN condição THEN valor END).
11. Redshift NÃO suporta RETURNING, LATERAL JOIN ou funções exclusivas do PostgreSQL como generate_series. Use apenas constructs padrão SQL compatíveis com Redshift.`
	case strings.Contains(e, "postgres"), strings.Contains(e, "postgresql"):
		return `
10. O banco usa PostgreSQL — pode usar FILTER(WHERE ...): COUNT(*) FILTER (WHERE condição).
11. Use ILIKE para comparações de texto case-insensitive.`
	case strings.Contains(e, "mysql"), strings.Contains(e, "mariadb"):
		return `
10. O banco usa MySQL/MariaDB — use backticks para identificadores reservados, IF() para contagens condicionais: SUM(IF(condição, 1, 0)).
11. Funções de data: NOW(), DATE(), DATE_FORMAT(), DATEDIFF(). NÃO use ILIKE — use LIKE (MySQL é case-insensitive por padrão para strings).`
	case strings.Contains(e, "bigquery"):
		return `
10. O banco usa Google BigQuery — use backticks para nomes de tabela: ` + "`project.dataset.table`" + `.
11. Funções de data: CURRENT_DATE(), DATE_TRUNC(), FORMAT_DATE(), DATE_DIFF(). Use LIKE para padrões de texto.`
	case strings.Contains(e, "snowflake"):
		return `
10. O banco usa Snowflake — use ILIKE para comparações case-insensitive.
11. Funções de data: CURRENT_DATE(), DATE_TRUNC(), DATEADD(), DATEDIFF(). Agregações condicionais: COUNT(CASE WHEN cond THEN 1 END).`
	case strings.Contains(e, "sqlite"):
		return `
10. O banco usa SQLite — não suporta RIGHT JOIN nem FULL OUTER JOIN; reescreva com LEFT JOIN quando necessário.
11. Funções de data: date('now'), strftime(). Não existe tipo BOOLEAN nativo — use 0/1.`
	default:
		return ""
	}
}
