package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/DanielFillol/Jarvis/internal/config"
)

// ClarificationPrefix is prepended to the return value of GenerateSQL when the
// LLM needs additional information from the user before generating a query.
const ClarificationPrefix = "CLARIFICATION_NEEDED:"

// Client encapsulates credentials and capability flags used across all LLM calls.
type Client struct {
	APIKey      string
	JiraBaseURL string
	BotName     string
	// JiraEnabled, MetabaseEnabled, and OutlineEnabled tell the answer LLM which
	// integrations are active so it can respond accurately when asked.
	JiraEnabled     bool
	MetabaseEnabled bool
	OutlineEnabled  bool
}

// NewClient constructs a new LLM client from the provided configuration.
func NewClient(cfg config.Config) *Client {
	return &Client{
		APIKey:          cfg.OpenAIAPIKey,
		JiraBaseURL:     cfg.JiraBaseURL,
		BotName:         cfg.BotName,
		JiraEnabled:     cfg.JiraEnabled(),
		MetabaseEnabled: cfg.MetabaseEnabled(),
		OutlineEnabled:  cfg.OutlineEnabled(),
	}
}

// Chat calls the OpenAI chat completions endpoint with the supplied
// messages and model.  Temperature and maxTokens control the
// creativity and length of the response.  The content of the first
// choice is returned on success.  Errors include HTTP failures,
// decoding failures and API-level errors.
func (c *Client) Chat(messages []OpenAIMessage, model string, temperature float64, maxTokens int) (string, error) {
	if c.APIKey == "" {
		return "", errors.New("missing OPENAI_API_KEY")
	}
	if strings.TrimSpace(model) == "" {
		model = "gpt-4o-mini"
	}
	return c.chatWithTemperature(messages, model, temperature, maxTokens)
}

// chatWithTemperature performs the actual HTTP call.  If the model rejects
// the requested temperature (e.g., gpt-5-mini only accepts the default),
// it retries once without a custom temperature.
func (c *Client) chatWithTemperature(messages []OpenAIMessage, model string, temperature float64, maxTokens int) (string, error) {
	reqBody := openAIChatRequest{
		Model:               model,
		Messages:            messages,
		Temperature:         temperature,
		MaxCompletionTokens: maxTokens,
	}
	b, _ := json.Marshal(reqBody)
	req, _ := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		bodyStr := string(rb)
		// Retry without temperature when the model doesn't support a custom value.
		if resp.StatusCode == 400 && strings.Contains(bodyStr, "\"temperature\"") && temperature != 0 {
			log.Printf("[LLM] model %s rejected temperature=%.1f — retrying with default", model, temperature)
			return c.chatWithTemperature(messages, model, 0, maxTokens)
		}
		return "", fmt.Errorf("openai status=%d body=%s", resp.StatusCode, preview(bodyStr, 400))
	}
	var out openAIChatResponse
	if err := json.Unmarshal(rb, &out); err != nil {
		return "", err
	}
	if out.Error != nil {
		return "", fmt.Errorf("openai: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", errors.New("openai: no choices")
	}
	content := strings.TrimSpace(out.Choices[0].Message.Content)
	finishReason := out.Choices[0].FinishReason
	// If response truncated due to length and no content, return error
	if finishReason == "length" && content == "" {
		return "", fmt.Errorf("openai: response truncated at max_tokens with no content (model=%s)", model)
	}
	if finishReason == "length" {
		// Append a note if truncated in the middle of a sentence
		if !strings.HasSuffix(content, ".") && !strings.HasSuffix(content, "!") && !strings.HasSuffix(content, "?") {
			content += "\n_(report truncada - tente uma pergunta mais especifica)_"
		}
	}
	return content, nil
}

// ActionDescriptor is a single skill the bot must execute for a given message.
// Fields are optional and only populated for the kinds that need them.
type ActionDescriptor struct {
	Kind string `json:"kind"` // one of the Action* constants below

	// slack_search, outline_search
	Query string `json:"query,omitempty"`

	// jira_search
	JQL        string `json:"jql,omitempty"`
	JiraIntent string `json:"jira_intent,omitempty"`

	// metabase_query, show_sql
	MetabaseDatabaseID int  `json:"database_id,omitempty"`
	WantsAllRows       bool `json:"wants_all_rows,omitempty"`
	WantsCSVExport     bool `json:"wants_csv_export,omitempty"`
}

const (
	ActionJiraCreate    = "jira_create"
	ActionJiraEdit      = "jira_edit"
	ActionJiraSearch    = "jira_search"
	ActionSlackSearch   = "slack_search"
	ActionMetabaseQuery = "metabase_query"
	ActionShowSQL       = "show_sql"
	ActionOutlineSearch = "outline_search"
)

// DecideActions performs a single LLM call to determine all actions the bot must
// execute for the given message. It replaces the prior two-call approach of
// DetectJiraActions + DecideRetrieval.
//
// The returned slice is ordered — execution order matters (jira_create before
// jira_edit). An empty slice means no external actions are needed. On error,
// callers should use fallbackActions.
func (c *Client) DecideActions(
	question, threadHistory, model string,
	jiraEnabled bool,
	jiraCatalog string,
	senderUserID string,
	metabaseDatabases []string,
	storedDBID int,
	outlineEnabled bool,
) ([]ActionDescriptor, error) {
	projectsCtx := ""
	if jiraEnabled && strings.TrimSpace(jiraCatalog) != "" {
		projectsCtx = fmt.Sprintf("\nProjetos Jira disponíveis (formato CHAVE=Nome [tipos de issue]):\n%s\n", jiraCatalog)
	}
	senderCtx := ""
	if strings.TrimSpace(senderUserID) != "" {
		senderCtx = fmt.Sprintf("\nUsuário que está perguntando: <@%s> — quando a pergunta usar \"eu\", \"meu\", \"minha\", \"minhas\", \"me\" refira-se a este usuário.\n", senderUserID)
	}

	now := time.Now()
	weekday := int(now.Weekday())
	if weekday == 0 {
		weekday = 7
	}
	monday := now.AddDate(0, 0, -(weekday - 1))
	dateCtx := fmt.Sprintf("\nData atual: %s (segunda-feira desta semana: %s)\n",
		now.Format("2006-01-02"),
		monday.Format("2006-01-02"),
	)

	metabaseCtx := ""
	if len(metabaseDatabases) > 0 {
		metabaseCtx = fmt.Sprintf("\nBancos de dados Metabase disponíveis:\n- %s\n", strings.Join(metabaseDatabases, "\n- "))
	}
	outlineCtxStr := ""
	if outlineEnabled {
		outlineCtxStr = "\nOutline Wiki está configurado e disponível para busca de documentação.\n"
	}
	metabaseFollowUpCtx := ""
	if storedDBID > 0 {
		metabaseFollowUpCtx = fmt.Sprintf(
			"\nEste thread já executou uma consulta no banco de dados Metabase ID=%d. "+
				"Se a pergunta atual for um follow-up, refinamento ou pedido de execução da consulta anterior, "+
				"inclua {\"kind\":\"metabase_query\",\"database_id\":%d} no array.\n",
			storedDBID, storedDBID,
		)
	}

	// Build example items for the output format block (only enabled integrations).
	var exampleItems []string
	if jiraEnabled {
		exampleItems = append(exampleItems, `  {"kind": "jira_create"}`)
		exampleItems = append(exampleItems, `  {"kind": "jira_edit"}`)
		exampleItems = append(exampleItems, `  {"kind": "jira_search", "jql": "project = X AND sprint in openSprints()", "jira_intent": "default"}`)
	}
	exampleItems = append(exampleItems, `  {"kind": "slack_search", "query": "deploy produção after:2026-03-01"}`)
	if len(metabaseDatabases) > 0 {
		exampleItems = append(exampleItems, `  {"kind": "metabase_query", "database_id": 1, "wants_all_rows": false, "wants_csv_export": false}`)
		exampleItems = append(exampleItems, `  {"kind": "show_sql", "database_id": 1}`)
	}
	if outlineEnabled {
		exampleItems = append(exampleItems, `  {"kind": "outline_search", "query": "processo deploy produção"}`)
	}

	// Jira-specific routing rules.
	jiraSourceLine := ""
	jiraRules := ""
	jiraIntentBlock := ""
	if jiraEnabled {
		jiraSourceLine = "- Jira: tickets, status, roadmap, bugs, histórias, épicos, progresso de tarefas.\n"
		jiraRules = `1. Roadmap, escopo, "o que foi feito", "está no ar", bugs abertos → jira_search.
2. "onde falamos", "qual foi a decisão", "me manda o link", "thread do slack" → slack_search.
3. Resumo/retrospectiva de processo (sprint, fechamento, entrega) → jira_search E slack_search.
4. Se jira_search para pergunta substantiva (não apenas listagem de tickets), considere slack_search também.
5. Perguntas curtas (≤ 2 palavras) ou com resposta no histórico → sem jira_search, sem slack_search.
6. Criar card no Jira → sem slack_search, sem jira_search.`
		jiraIntentBlock = `
Valores de jira_intent (campo de jira_search):
- "listar_bugs_abertos": perguntas sobre bugs em aberto, falhas, erros.
- "busca_texto": pesquisa de contexto sobre um tema específico no Jira.
- "default": listagem geral ou roadmap.

Regras para jql (campo de jira_search):
- Se souber exatamente o JQL, preencha. Caso contrário, deixe "" e use jira_intent.
- Use apenas campos padrão do Jira Cloud: project, issuetype, status, statusCategory, text, assignee, priority, labels, sprint, fixVersion, updated, created.
- Para busca por texto: text ~ "termo"
- Para bugs abertos: issuetype = Bug AND statusCategory != Done
- Para busca por sprint: sprint = "Sprint N" ou sprint in openSprints() para sprints ativas.
- SEMPRE agrupe condições OR com parênteses quando combinadas com AND: project = X AND (text ~ "a" OR text ~ "b") — NUNCA escreva: project = X AND text ~ "a" OR text ~ "b"
`
	} else {
		jiraRules = `1. "onde falamos", "qual foi a decisão", "me manda o link" → slack_search.
2. Perguntas curtas (≤ 2 palavras) ou com resposta no histórico → sem slack_search.`
	}

	outlineSourceLine := ""
	outlineRule := ""
	if outlineEnabled {
		outlineSourceLine = "- Outline Wiki: documentação interna, processos, guias, runbooks, especificações de produto, onboarding, políticas.\n"
		outlineRule = "12. Quando Outline está configurado: inclua outline_search quando a resposta depende principalmente de documentação interna (processos, guias, runbooks, políticas, onboarding, RH, benefícios). Preencha query com 2–4 termos-chave do assunto, sem artigos ou preposições.\n"
	}

	prompt := fmt.Sprintf(`Você é um roteador de ações de um assistente de Slack.
Analise a mensagem e retorne um JSON array com TODAS as ações necessárias, na ordem certa.
%s%s%s%s%s%s
Retorne APENAS um JSON array válido, sem markdown fences. Retorne [] quando nenhuma ação for necessária.

Exemplo de array (inclua apenas as ações necessárias):
[
%s
]

Fontes disponíveis:
%s%s- Slack: discussões, decisões, links de threads, contexto operacional.
- Metabase (banco de dados): dados estruturados do banco operacional. Se a pergunta filtra, lista ou consulta entidades (registros, entidades operacionais, transações, pedidos), use metabase_query. NÃO use slack_search para dados que vivem no banco.

Regras para jira_create e jira_edit:
- "jira_create": verbo de criação EXPLÍCITO (criar/cria/abre/abrir/gera/gerar) + tipo de issue, pedido AGORA
- "jira_edit": mudar status, atribuir, alterar campos, mover para sprint, "adicione para", "atribuir", "assign"
- Hipóteses ("estou pensando em criar") → sem jira_create
- Negações ("não quero criar") → sem jira_create
- Criação + atribuição na mesma mensagem → jira_create ANTES de jira_edit no array
- Criar um card SEM pedido explícito de dados externos → sem jira_search, sem slack_search
- jira_create/jira_edit PODEM coexistir com metabase_query, slack_search ou jira_search quando o usuário pede EXPLICITAMENTE dados externos para incluir no card ou na resposta (ex: "escreva nesse card o total de X", "adicione as threads onde X foi mencionada", "busque o total de registros")
%s
Regras de roteamento de contexto:
%s
7. Se o histórico contém SQL ou resultados de banco E o usuário faz follow-up → metabase_query. Use database_id da linha "Query executada (db=N):" do histórico.
8. show_sql SOMENTE quando usuário pede EXPLICITAMENTE o SQL/query/código usado ("SQL", "query", "consulta que você rodou", "me mostra o código"). Pedidos de dados → metabase_query, não show_sql. Inclua database_id do "Query executada (db=N):" do histórico (ou 0).
9. Follow-ups com pronomes ("dessas", "desses") referindo entidades já consultadas → metabase_query com mesmo database_id do turno anterior.
10. wants_all_rows=true: usuário quer todos os dados ("todos", "tudo", "sem limite", "lista completa", "traz tudo").
11. wants_csv_export=true: exportação explícita ("exportar", "csv", "planilha", "download", "baixar", "excel") OU pedido de todos os dados. Quando true, também wants_all_rows=true.
%s
Regras para query em slack_search (IMPORTANTE):
- query NUNCA vazio quando kind="slack_search" — sempre gere uma query útil.
- Se mencionar canais (#nome), inclua in:#nome-do-canal.
- Use 2–4 palavras-chave, sem has:thread, has:link, has:reaction.
- "o que X falou/disse" → from:@username.
- Usuário Slack (<@USERID>) em busca → inclua o identificador EXATO: ex. "<@U09FJSKP407>".

Regras para datas na query slack_search:
- NÃO use "essa semana" como termo — a API não interpreta.
- Converta expressões de tempo:
  - "essa semana" → after:SEGUNDA-DESTA-SEMANA (use a data calculada acima)
  - "ontem" → after:DATA-ONTEM before:DATA-HOJE
  - "mês passado" → after:ANO-MÊS-01 before:ANO-MÊS-01 do mês atual
- Exemplo: "o que foi dito essa semana" → query: "termo-relevante after:2026-02-17"

Thread (contexto recente):
%s

Pergunta:
%s
`, dateCtx, senderCtx, projectsCtx, metabaseCtx, metabaseFollowUpCtx, outlineCtxStr,
		strings.Join(exampleItems, ",\n"),
		jiraSourceLine, outlineSourceLine,
		jiraIntentBlock,
		jiraRules,
		outlineRule,
		clip(threadHistory, 1200), question)

	messages := []OpenAIMessage{{Role: "user", Content: prompt}}
	out, err := c.Chat(messages, model, 0.2, 4000)
	if err != nil {
		return nil, err
	}
	out = strings.TrimSpace(stripCodeFences(out))

	var actions []ActionDescriptor
	if err := json.Unmarshal([]byte(out), &actions); err != nil {
		return nil, fmt.Errorf("bad actions json: %v raw=%q", err, preview(out, 300))
	}

	// Post-process: normalize fields and enforce integration constraints.
	var result []ActionDescriptor
	for _, a := range actions {
		switch a.Kind {
		case ActionJiraCreate, ActionJiraEdit, ActionJiraSearch:
			if !jiraEnabled {
				continue
			}
			a.JiraIntent = strings.TrimSpace(a.JiraIntent)
			a.JQL = strings.TrimSpace(a.JQL)
		case ActionSlackSearch:
			a.Query = normalizeSlackQuery(strings.TrimSpace(a.Query))
		case ActionOutlineSearch:
			if !outlineEnabled {
				continue
			}
			a.Query = strings.TrimSpace(a.Query)
		case ActionMetabaseQuery, ActionShowSQL:
			if len(metabaseDatabases) == 0 {
				continue
			}
		default:
			continue // unknown kind — skip
		}
		result = append(result, a)
	}

	var kinds []string
	for _, a := range result {
		kinds = append(kinds, a.Kind)
	}
	log.Printf("[LLM] decideActions=%v raw=%q", kinds, preview(out, 120))
	return result, nil
}

// GenerateOutlineQuery uses the LLM to produce a concise, effective search
// query (2–4 keywords) for the Outline wiki from the user's question.
// It extracts the core subject, stripping greetings, articles, prepositions,
// and conversational filler.  Returns an empty string on error.
func (c *Client) GenerateOutlineQuery(question, model string) string {
	if strings.TrimSpace(model) == "" {
		model = "gpt-4o-mini"
	}
	prompt := fmt.Sprintf(`Gere uma query de busca para uma wiki interna com 2 a 4 termos-chave em português.

RETORNE APENAS os termos, sem pontuação, sem aspas, sem explicações.

Regras:
- Extraia apenas substantivos e adjetivos centrais do assunto.
- Omita saudações, artigos, preposições, verbos auxiliares e pronomes.
- Exemplos:
  "quanto eu ganho de vale alimentação por mês?" → "vale alimentação benefício"
  "como funciona o processo de deploy em produção?" → "deploy produção processo"
  "quais são as políticas de férias da empresa?" → "férias políticas empresa"
  "quanto eu ganho de flash por mes?" → "benefícios flash"
  "cria uma tarefa com base nessa thread" → "tarefa fluxo processo"
  "me explica o onboarding de novos engenheiros" → "onboarding engenheiros"

Pergunta: %s`, question)

	msgs := []OpenAIMessage{{Role: "user", Content: prompt}}
	out, err := c.Chat(msgs, model, 0, 50)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// GenerateSQL uses the LLM to produce a native SQL query for the given question.
// schemaCtx should contain the compact schema documentation for the target database.
// baseSQL may be a prior query to use as a starting point for follow-up questions.
// lastErr, if non-empty, is the database error from the previous attempt — the LLM
// will use it to correct the query rather than regenerating from scratch.
// Returns the SQL string, or a string prefixed with ClarificationPrefix when the
// LLM needs more information from the user before it can generate a valid query.
func (c *Client) GenerateSQL(question, threadHist, schemaCtx, baseSQL, lastErr, dbEngine, model string) (string, error) {
	if strings.TrimSpace(model) == "" {
		model = "gpt-4o-mini"
	}
	baseCtx := ""
	if strings.TrimSpace(baseSQL) != "" {
		if strings.TrimSpace(lastErr) != "" {
			baseCtx = fmt.Sprintf(
				"\n\nQuery anterior que falhou com erro — CORRIJA o erro abaixo antes de responder:\n```sql\n%s\n```\nErro retornado pelo banco:\n%s\n",
				strings.TrimSpace(baseSQL), strings.TrimSpace(lastErr),
			)
		} else {
			baseCtx = fmt.Sprintf(
				"\n\nQuery anterior (use como base para follow-ups, preservando todos os filtros existentes):\n```sql\n%s\n```\n",
				strings.TrimSpace(baseSQL),
			)
		}
	}

	now := time.Now()
	weekday := int(now.Weekday())
	if weekday == 0 {
		weekday = 7
	}
	thisMonday := now.AddDate(0, 0, -(weekday - 1))
	lastMonday := thisMonday.AddDate(0, 0, -7)
	lastSunday := thisMonday.AddDate(0, 0, -1)
	dateCtx := fmt.Sprintf(
		"\nReferências de data (use estas ao interpretar expressões relativas):\n"+
			"- Hoje: %s\n"+
			"- Segunda desta semana: %s\n"+
			"- Semana passada: %s a %s (inclusive)\n",
		now.Format("2006-01-02"),
		thisMonday.Format("2006-01-02"),
		lastMonday.Format("2006-01-02"),
		lastSunday.Format("2006-01-02"),
	)

	engineCtx := strings.TrimSpace(dbEngine)
	if engineCtx == "" {
		engineCtx = "desconhecido"
	}
	prompt := fmt.Sprintf(`Você é um especialista em SQL que gera queries nativas para o Metabase.
Engine do banco de dados: %s
%s
Schema do banco de dados:
%s
%s
Histórico da conversa (para contexto):
%s

Pergunta do usuário: %s

INSTRUÇÕES:
- Responda APENAS com a query SQL pura, sem texto extra, sem explicações, sem blocos de código markdown.
- Se a pergunta for ambígua ou carecer de informação essencial que não pode ser inferida do histórico (ex: período de tempo obrigatório não especificado), responda APENAS com: %s<pergunta de esclarecimento em português>
- Use somente tabelas e colunas que existem no schema acima.
- Inclua ORDER BY quando relevante para a pergunta.
- Limite a 1000 linhas por padrão, a menos que o usuário tenha pedido todos os dados.

REGRAS DE SQL:
- Buscas textuais: use ILIKE '%%termo%%'. Redshift ILIKE é insensível a maiúsculas mas NÃO a acentos — use substrings curtas e sem caracteres acentuados para maior abrangência.
- Atributos de entidades referenciadas por ID estão em tabelas separadas — sempre use JOIN, nunca assuma que o valor existe como coluna direta.
- Prefira tabelas base/normalizadas em vez de views ou tabelas de staging.
- Datas: use os tipos corretos da coluna (Date vs DateTime) e filtre com >= / < ou BETWEEN conforme a sintaxe do engine acima.
- Use as referências de data acima para expressões relativas como "semana passada", "hoje", "mês passado".`,
		engineCtx, dateCtx, clip(schemaCtx, 120000), baseCtx, clip(threadHist, 800), question, ClarificationPrefix)

	msgs := []OpenAIMessage{{Role: "user", Content: prompt}}
	out, err := c.Chat(msgs, model, 0.1, 2000)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(stripCodeFences(out)), nil
}
