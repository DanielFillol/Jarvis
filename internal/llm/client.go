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

// RetrievalDecision encodes the high-level retrieval routing decision
// returned by the LLM. It indicates whether Slack, Jira, and/or Metabase
// context should be fetched and contains any queries supplied by the model.
type RetrievalDecision struct {
	NeedSlack          bool   `json:"need_slack"`
	SlackQuery         string `json:"slack_query"`
	NeedJira           bool   `json:"need_jira"`
	JiraIntent         string `json:"jira_intent"`
	JiraJQL            string `json:"jira_jql"`
	NeedMetabase       bool   `json:"need_metabase"`
	MetabaseDatabaseID int    `json:"metabase_database_id"`
	// ShowSQL is true when the user is asking for the SQL query used in a
	// prior answer.  HandleMessage will attempt to reconstruct and validate
	// the query via runMetabaseQuery (with the usual 3-attempt retry logic)
	// and reply with the SQL directly, bypassing the answer LLM.
	ShowSQL bool `json:"show_sql"`
	// WantsAllRows is true when the user wants the full result set without any
	// row limit (e.g. "todos", "tudo", "sem limite", "lista completa").
	WantsAllRows bool `json:"wants_all_rows"`
	// WantsCSVExport is true when the user explicitly requests a CSV/spreadsheet
	// download OR when the query is expected to return a large dataset (e.g. "todos",
	// "traz tudo", "exportar", "planilha", "csv", "baixar").
	// When true, wants_all_rows is also expected to be true.
	WantsCSVExport bool `json:"wants_csv_export"`
	// NeedOutline is true when the question likely requires documentation from
	// the Outline wiki (processes, how-to guides, product specs, technical docs,
	// onboarding material, runbooks).
	NeedOutline  bool   `json:"need_outline"`
	OutlineQuery string `json:"outline_query"`
}

// DecideRetrieval consults the language model to determine which contexts
// (Slack, Jira, Metabase, Outline) should be retrieved for a given question.
//
// jiraCatalog is the compact project catalog generated at startup
// (e.g. "INV=Faturamento [Bug, Task] | TPTDR=Transporte [Bug, Epic]").
// storedDBID is the Metabase database ID used in a previous turn of this thread.
// outlineEnabled indicates whether the Outline wiki integration is configured.
func (c *Client) DecideRetrieval(question, threadHistory, model string, jiraEnabled bool, jiraCatalog string, senderUserID string, metabaseDatabases []string, storedDBID int, outlineEnabled bool) (RetrievalDecision, error) {
	projectsCtx := ""
	if jiraEnabled && strings.TrimSpace(jiraCatalog) != "" {
		projectsCtx = fmt.Sprintf("\nProjetos Jira disponíveis (formato CHAVE=Nome [tipos de issue]):\n%s\n", jiraCatalog)
	}
	senderCtx := ""
	if strings.TrimSpace(senderUserID) != "" {
		senderCtx = fmt.Sprintf("\nUsuário que está perguntando: <@%s> — quando a pergunta usar \"eu\", \"meu\", \"minha\", \"minhas\", \"me\" refira-se a este usuário.\n", senderUserID)
	}

	now := time.Now()
	// Monday of the current week (ISO: Monday = first day)
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
				"use need_metabase=true e metabase_database_id=%d.\n",
			storedDBID, storedDBID,
		)
	}

	metabaseJSON := ""
	if len(metabaseDatabases) > 0 {
		metabaseJSON = `
  "need_metabase": true/false,
  "metabase_database_id": <ID do banco acima ou 0>`
	}

	// Build Jira-specific prompt sections only when Jira is configured.
	jiraSourceLine := ""
	jiraRules := ""
	jiraIntentBlock := ""
	jiraJQLBlock := ""
	if jiraEnabled {
		jiraSourceLine = "- Jira: tickets, status, roadmap, bugs, histórias, épicos, progresso de tarefas.\n"
		jiraRules = `1. Roadmap, escopo, "o que foi feito", "está no ar", bugs abertos → need_jira=true.
2. "onde falamos", "qual foi a decisão", "me manda o link", "thread do slack" → need_slack=true.
3. Resumo/retrospectiva de processo (sprint, fechamento, entrega) → need_jira=true E need_slack=true.
4. Se need_jira=true para pergunta substantiva (não apenas listagem de tickets), considere need_slack=true também.
5. Perguntas curtas (≤ 2 palavras) ou que já têm resposta no histórico da thread → need_slack=false, need_jira=false.
6. Criar card no Jira → need_slack=false, need_jira=false.`
		jiraIntentBlock = `
Valores de jira_intent:
- "listar_bugs_abertos": perguntas sobre bugs em aberto, falhas, erros.
- "busca_texto": pesquisa de contexto sobre um tema específico no Jira (funcionalidades, épicos, histórias).
- "default": listagem geral ou roadmap.

Regras para jira_jql:
- Se souber exatamente o JQL, preencha. Caso contrário, deixe "" e use jira_intent.
- Use apenas campos padrão do Jira Cloud: project, issuetype, status, statusCategory, text, assignee, priority, labels, sprint, fixVersion, updated, created.
- Para busca por texto: text ~ "termo"
- Para bugs abertos: issuetype = Bug AND statusCategory != Done
- Para busca por sprint: sprint = "Sprint N" ou sprint in openSprints() para sprints ativas.
- SEMPRE agrupe condições OR com parênteses quando combinadas com AND: project = X AND (text ~ "a" OR text ~ "b") — NUNCA escreva: project = X AND text ~ "a" OR text ~ "b"
`
		jiraJQLBlock = `  "need_jira": true/false,
  "jira_intent": "listar_bugs_abertos|busca_texto|default",
  "jira_jql": ""`
	} else {
		// Jira isn't configured: always output false/empty fields, so the JSON is valid.
		jiraJQLBlock = `  "need_jira": false,
  "jira_intent": "",
  "jira_jql": ""`
	}

	outlineSourceLine := ""
	outlineJSONBlock := ""
	outlineRule := ""
	if outlineEnabled {
		outlineSourceLine = "- Outline Wiki: documentação interna, processos, guias, runbooks, especificações de produto, onboarding, políticas.\n"
		outlineJSONBlock = `,
  "need_outline": true/false,
  "outline_query": "..."`
		outlineRule = "12. Quando Outline está configurado, SEMPRE preencha outline_query com os 2–4 termos-chave mais relevantes da pergunta (sem artigos, preposições ou saudações — apenas as palavras que melhor descrevem o assunto para busca na wiki). outline_query NUNCA pode ficar vazio. Defina need_outline=true apenas quando a resposta depende principalmente de documentação interna (processos, guias, runbooks, especificações, políticas, onboarding, benefícios, RH).\n"
	}

	prompt := fmt.Sprintf(`Você é um roteador de contexto de um assistente de Slack.
Decida quais fontes buscar para responder a pergunta.
%s%s%s%s%s%s
Retorne APENAS JSON válido:
{
  "need_slack": true/false,
  "slack_query": "...",
%s%s,
  "show_sql": true/false,
  "wants_all_rows": true/false,
  "wants_csv_export": true/false%s
}

Fontes disponíveis:
%s%s- Slack: discussões, decisões, links de threads, contexto operacional, conversas sobre um tema.
- Metabase (banco de dados): qualquer consulta que necessite de dados estruturados do banco de dados operacional. Isso inclui TANTO agregações/métricas (contagens, totais, médias, KPIs, receita, "quantos") QUANTO listas de registros filtrados (ex: "quero todas as coletas", "me traz coletas do transportador X", "coletas sem rota", "MTRs emitidos na semana passada", "status planned"). Se a pergunta filtra, lista ou consulta entidades operacionais (coletas, MTRs, geradores, transportadores, rotas, pedidos, clientes), use need_metabase=true. NÃO use need_slack para buscar dados estruturados que vivem no banco de dados.

Regras de roteamento:
%s
7. Se o histórico da thread contém blocos de código SQL ou resultados de banco de dados, E o usuário pede para executar/rodar/modificar a consulta ou faz uma pergunta de follow-up sobre os mesmos dados → need_metabase=true. Use o metabase_database_id da linha "Query executada (db=N):" se existir, senão use 0.
8. show_sql=true SOMENTE quando o usuário pede EXPLICITAMENTE para ver o SQL/query/código que o bot usou — palavras como "SQL", "query", "consulta que você rodou", "me mostra o código", "qual foi a query". Pedidos de dados ("me traga", "quero ver", "lista de", "quantas", "quais coletas") → show_sql=false, need_metabase=true. Quando show_sql=true, também defina need_metabase=true com o metabase_database_id da linha "Query executada (db=N):" do histórico (ou 0 se não houver), need_slack=false, need_jira=false.
9. Perguntas de follow-up de dados (pronomes como "dessas", "desses", referindo a entidades já consultadas) → need_metabase=true com o mesmo database_id do turno anterior.
10. wants_all_rows=true quando o usuário quer todos os dados sem limitação de linhas (ex: "todos", "tudo", "sem limite", "lista completa", "traz tudo", "quero todas", "sem limitar").
11. wants_csv_export=true quando o usuário pede exportação explícita ("exportar", "csv", "planilha", "download", "baixar", "excel") OU quando pede todos os dados sem limitação (mesmos casos de wants_all_rows=true). Quando wants_csv_export=true, também defina wants_all_rows=true.
%s%s
Regras para slack_query (IMPORTANTE):
- slack_query NUNCA pode ficar vazio quando need_slack=true — sempre gere uma query útil.
- Se a pergunta mencionar canais (ex: #nome-do-canal), inclua in:#nome-do-canal na query.
- Use apenas 2–4 palavras-chave do tema, sem filtros extras.
- NÃO use has:thread, has:link, has:reaction — reduzem o recall drasticamente.
- Prefira termos sem aspas; use aspas apenas para frases exatas críticas.
- Quando a pergunta é "o que X falou/disse/escreveu/postou", use from:@username. Ex: "o que o @alice falou" → from:@alice
- Se a pergunta menciona um usuário Slack (<@USERID>) em contexto de busca geral, inclua o identificador EXATO: ex. "<@U09FJSKP407>"

Regras para datas na slack_query:
- NÃO inclua expressões como "essa semana" ou "esta semana" como termos de busca — a API do Slack não as interpreta.
- Converta expressões de tempo em filtros de data:
  - "essa semana" / "esta semana" → after:SEGUNDA-DESTA-SEMANA (use a data de segunda calculada acima)
  - "essa semana" como período exato → after:SEGUNDA-DESTA-SEMANA
  - "entre dia X e Y de mês" → after:ANO-MÊS-X before:ANO-MÊS-Y
  - "ontem" → after:DATA-ONTEM before:DATA-HOJE
  - "mês passado" → after:ANO-MÊS-01 before:ANO-MÊS-01 do mês atual
- Exemplo: "o que foi dito essa semana" → slack_query: "termo-relevante after:2026-02-17"

Thread (contexto recente):
%s

Pergunta:
%s
`, projectsCtx, senderCtx, dateCtx, metabaseCtx, metabaseFollowUpCtx, outlineCtxStr, jiraJQLBlock, metabaseJSON, outlineJSONBlock, jiraSourceLine, outlineSourceLine, jiraRules, jiraIntentBlock, outlineRule, clip(threadHistory, 1200), question)

	messages := []OpenAIMessage{{Role: "user", Content: prompt}}
	// Use 4000 tokens: reasoning models (e.g., gpt-5-mini) consume invisible thinking
	// tokens before producing output; 600 was not enough and caused empty responses.
	out, err := c.Chat(messages, model, 0.2, 4000)
	if err != nil {
		return RetrievalDecision{}, err
	}

	out = strings.TrimSpace(stripCodeFences(out))

	var d RetrievalDecision
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		return RetrievalDecision{}, fmt.Errorf("bad decision json: %v raw=%q", err, preview(out, 300))
	}

	d.SlackQuery = strings.TrimSpace(d.SlackQuery)
	d.JiraIntent = strings.TrimSpace(d.JiraIntent)
	d.JiraJQL = strings.TrimSpace(d.JiraJQL)
	d.OutlineQuery = strings.TrimSpace(d.OutlineQuery)

	// Disable Metabase if no databases were provided.
	if len(metabaseDatabases) == 0 {
		d.NeedMetabase = false
		d.MetabaseDatabaseID = 0
	}

	// Enforce that Jira is never routed when not configured.
	if !jiraEnabled {
		d.NeedJira = false
		d.JiraIntent = ""
		d.JiraJQL = ""
	}

	// Enforce that Outline is never routed when not configured.
	if !outlineEnabled {
		d.NeedOutline = false
		d.OutlineQuery = ""
	}

	// Normalize Slack query quirks.
	d.SlackQuery = normalizeSlackQuery(d.SlackQuery)

	return d, nil
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
  "cria uma tarefa no transportador com base nessa thread" → "transportador tarefa fluxo"
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
