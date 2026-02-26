// internal/llm/router.go
package llm

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/DanielFillol/Jarvis/internal/parse"
)

// RetrievalDecision encodes the high-level retrieval routing decision
// returned by the LLM. It indicates whether Slack and/or Jira
// context should be fetched and contains any queries supplied by the
// model.
type RetrievalDecision struct {
	NeedSlack  bool   `json:"need_slack"`
	SlackQuery string `json:"slack_query"`
	NeedJira   bool   `json:"need_jira"`
	JiraIntent string `json:"jira_intent"`
	JiraJQL    string `json:"jira_jql"`
}

// DecideRetrieval consults the language model to determine which
// contexts (Slack, Jira) should be retrieved for a given question.
// It passes a prompt describing the routing task along with the
// recent thread history, the user's question and the list of
// configured Jira project keys.  The returned JSON is unmarshalled
// into a RetrievalDecision.  If the JSON is invalid or the API call
// fails, a non-nil error is returned.
func (c *Client) DecideRetrieval(question, threadHistory, model string, projectKeys []string, senderUserID string) (RetrievalDecision, error) {
	projectsCtx := ""
	if len(projectKeys) > 0 {
		projectsCtx = fmt.Sprintf("\nProjetos Jira configurados: %s\n", strings.Join(projectKeys, ", "))
	}
	senderCtx := ""
	if strings.TrimSpace(senderUserID) != "" {
		senderCtx = fmt.Sprintf("\nUsuário que está perguntando: <@%s> — quando a pergunta usar \"eu\", \"meu\", \"minha\", \"minhas\", \"me\" refira-se a este usuário.\n", senderUserID)
	}

	now := time.Now()
	// Monday of current week (ISO: Monday = first day)
	weekday := int(now.Weekday())
	if weekday == 0 {
		weekday = 7
	}
	monday := now.AddDate(0, 0, -(weekday - 1))
	dateCtx := fmt.Sprintf("\nData atual: %s (segunda-feira desta semana: %s)\n",
		now.Format("2006-01-02"),
		monday.Format("2006-01-02"),
	)

	prompt := fmt.Sprintf(`Você é um roteador de contexto de um assistente Slack+Jira.
Decida quais fontes buscar para responder a pergunta.
%s%s%s
Retorne APENAS JSON válido:
{
  "need_slack": true/false,
  "slack_query": "...",
  "need_jira": true/false,
  "jira_intent": "listar_bugs_abertos|busca_texto|default",
  "jira_jql": ""
}

Fontes disponíveis:
- Jira: tickets, status, roadmap, bugs, histórias, épicos, progresso de tarefas.
- Slack: discussões, decisões, links de threads, contexto operacional, conversas sobre um tema.

Regras de roteamento:
1. Roadmap, escopo, "o que foi feito", "está no ar", bugs abertos → need_jira=true.
2. "onde falamos", "qual foi a decisão", "me manda o link", "thread do slack" → need_slack=true.
3. Resumo/retrospectiva de processo (sprint, fechamento, entrega) → need_jira=true E need_slack=true.
4. Se need_jira=true para pergunta substantiva (não apenas listagem de tickets), considere need_slack=true também — discussões no Slack enriquecem a resposta com contexto que o Jira não tem.
5. Perguntas curtas (≤ 2 palavras) ou que já têm resposta no histórico da thread → need_slack=false, need_jira=false.
6. Criar card no Jira → need_slack=false, need_jira=false.

Valores de jira_intent:
- "listar_bugs_abertos": perguntas sobre bugs em aberto, falhas, erros.
- "busca_texto": pesquisa de contexto sobre um tema específico no Jira (funcionalidades, épicos, histórias).
- "default": listagem geral ou roadmap.

Regras para jira_jql:
- Se souber exatamente o JQL, preencha. Caso contrário, deixe "" e use jira_intent.
- Use apenas campos padrão do Jira Cloud: project, issuetype, status, statusCategory, text, assignee, priority, labels, sprint, fixVersion, updated, created.
- Para busca por texto: text ~ "termo"
- Para bugs abertos: issuetype = Bug AND statusCategory != Done
- Para busca por sprint: sprint = "Sprint N" (ex: sprint = "Sprint 7") ou sprint in openSprints() para sprints ativas
- SEMPRE agrupe condições OR com parênteses quando combinadas com AND: project = X AND (text ~ "a" OR text ~ "b") — NUNCA escreva: project = X AND text ~ "a" OR text ~ "b"

Regras para slack_query (IMPORTANTE):
- slack_query NUNCA pode ficar vazio quando need_slack=true — sempre gere uma query útil.
- Se a pergunta mencionar canais (ex: #prod-coletas, #prod-musa-hub), inclua in:#nome-do-canal na query.
- Use apenas 2–4 palavras-chave do tema, sem filtros extras.
- NÃO use has:thread, has:link, has:reaction — reduzem o recall drasticamente.
- Prefira termos sem aspas; use aspas apenas para frases exatas críticas.
- Quando a pergunta é "o que X falou/disse/escreveu/postou", use from:@username (NÃO apenas a menção @username). Ex: "o que o @fillol falou" → from:@fillol
- Se a pergunta menciona um usuário Slack (<@USERID>) em contexto de busca geral (não "o que falou"), inclua o identificador EXATO: ex. "<@U09FJSKP407>"

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
`, projectsCtx, senderCtx, dateCtx, clip(threadHistory, 1200), question)

	messages := []OpenAIMessage{{Role: "user", Content: prompt}}
	// Use 4000 tokens: reasoning models (e.g. gpt-5-mini) consume invisible thinking
	// tokens before producing output; 600 was insufficient and caused empty responses.
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

	// (3) normalize Slack query quirks
	d.SlackQuery = normalizeSlackQuery(d.SlackQuery)

	// (2) deterministic overrides
	applyDeterministicOverrides(question, &d)

	return d, nil
}

// applyDeterministicOverrides enforces deterministic routing rules that should
// not depend on the LLM being perfect.
func applyDeterministicOverrides(question string, d *RetrievalDecision) {
	qLower := strings.ToLower(strings.TrimSpace(question))
	if qLower == "" {
		return
	}

	// Note: Jira creation intent suppression was removed from here.
	// The LLM prompt already contains rule 6 ("criar card → need_slack=false,
	// need_jira=false"). Using a heuristic override here caused context
	// suppression on Q&A fallbacks when the create intent was rejected by
	// ConfirmJiraCreateIntent in the create flow.

	// 2. Very short questions (≤ 2 words) are likely answered by thread history alone.
	if len(strings.Fields(qLower)) <= 2 {
		d.NeedSlack = false
		d.SlackQuery = ""
		d.NeedJira = false
		d.JiraJQL = ""
		return
	}

	// 3. Roadmap + explicit project → Jira only (structured listing).
	if strings.Contains(qLower, "roadmap") {
		if proj := parse.ParseProjectKeyFromText(question); proj != "" {
			d.NeedSlack = false
			d.SlackQuery = ""
			d.NeedJira = true
			d.JiraIntent = "default"
			d.JiraJQL = fmt.Sprintf("project = %s ORDER BY updated DESC", proj)
			return
		}
	}

	// 4. Open bugs + explicit project → Jira only.
	if (strings.Contains(qLower, "bugs") || strings.Contains(qLower, "bug")) &&
		(strings.Contains(qLower, "aberto") || strings.Contains(qLower, "em aberto")) {
		if proj := parse.ParseProjectKeyFromText(question); proj != "" {
			d.NeedSlack = false
			d.SlackQuery = ""
			d.NeedJira = true
			d.JiraIntent = "default"
			d.JiraJQL = fmt.Sprintf("project = %s AND issuetype = Bug AND statusCategory != Done ORDER BY updated DESC", proj)
			return
		}
	}

	// 5. If Jira was selected for a substantive question, enrich with Slack context.
	// Discussions in Slack often contain context that Jira tickets don't have.
	if d.NeedJira && !d.NeedSlack {
		topic := topicQuery(question)
		if topic != "" {
			d.NeedSlack = true
			d.SlackQuery = topic
		}
	}
}

// ptStopWords is the set of Portuguese stopwords and intent verbs to strip
// when building a short topic query for Slack search.
var ptStopWords = map[string]bool{
	"a": true, "o": true, "as": true, "os": true, "um": true, "uma": true,
	"de": true, "do": true, "da": true, "dos": true, "das": true,
	"em": true, "no": true, "na": true, "nos": true, "nas": true,
	"para": true, "por": true, "com": true, "e": true, "é": true,
	"me": true, "te": true, "se": true, "que": true, "já": true,
	"qual": true, "quais": true, "quando": true, "como": true,
	"está": true, "estão": true, "tem": true, "foi": true, "ser": true,
	"nessa": true, "esse": true, "essa": true, "isso": true, "isto": true,
	"manda": true, "preciso": true, "quero": true, "gostaria": true,
	// intent verbs
	"crie": true, "criar": true, "cria": true, "abra": true, "abre": true,
	"mostre": true, "mostra": true, "liste": true, "listar": true,
	"resume": true, "resumo": true, "busca": true, "buscar": true,
	"explica": true, "explicar": true, "fala": true,
}

// topicQuery extracts 2–4 meaningful words from a question to use as a
// focused Slack search query. It strips stopwords and intent verbs.
func topicQuery(question string) string {
	words := strings.Fields(strings.ToLower(question))
	var kept []string
	for _, w := range words {
		w = strings.Trim(w, ".,!?;:\"'()[]{}")
		if w == "" || ptStopWords[w] {
			continue
		}
		kept = append(kept, w)
		if len(kept) == 4 {
			break
		}
	}
	return strings.Join(kept, " ")
}

// normalizeSlackQuery fixes common formatting mistakes for Slack search.
// It converts LLM-generated "menção USERID" patterns to <@USERID> and
// normalizes from:/to: user ID filters.
func normalizeSlackQuery(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return q
	}

	// Convert "menção/mencao/mencionado USERID" (LLM hallucination) to <@USERID>
	reMentionWord := regexp.MustCompile(`(?i)\b(?:menção|mencao|mencionado|mencionada|mentioned)\s+((U|W)[A-Z0-9]+)\b`)
	q = reMentionWord.ReplaceAllString(q, "<@$1>")

	// Strip leftover mention words when a <@USERID> is already present.
	// e.g. "<@U09FJSKP407> menção" → "<@U09FJSKP407>"
	if strings.Contains(q, "<@") {
		reMentionLeftover := regexp.MustCompile(`(?i)\s*\b(?:menção|mencao|mencionado|mencionada|mentioned)\b\s*`)
		q = strings.TrimSpace(reMentionLeftover.ReplaceAllString(q, " "))
	}

	q = strings.ReplaceAll(q, "to:@", "to:")

	reFrom := regexp.MustCompile(`\bfrom:\s*(?:<@)?@?(U[A-Z0-9]+)(?:\|[^>]+)?>?`)
	q = reFrom.ReplaceAllString(q, "from:$1")

	reTo := regexp.MustCompile(`\bto:\s*(?:<@)?@?(U[A-Z0-9]+)(?:\|[^>]+)?>?`)
	q = reTo.ReplaceAllString(q, "to:$1")

	// Strip unresolved channel ID filters: in:#C09H8S8A0VD
	// Raw Slack channel IDs are not supported in the in: search filter;
	// keeping them returns 0 results. Better to search without channel filter.
	reRawChannelFilter := regexp.MustCompile(`\bin:#[CG][A-Z0-9]{8,}\b`)
	q = strings.TrimSpace(reRawChannelFilter.ReplaceAllString(q, ""))
	// Also strip leftover <#CHANID> tokens the LLM might have included verbatim.
	reRawChannelMention := regexp.MustCompile(`<#[CG][A-Z0-9]{8,}(?:\|[^>]*)?>`)
	q = strings.TrimSpace(reRawChannelMention.ReplaceAllString(q, ""))
	// Collapse multiple spaces left by removals.
	q = strings.Join(strings.Fields(q), " ")

	return strings.TrimSpace(q)
}

// clip truncates a string to a maximum number of runes.  It is
// replicated here to avoid a dependency on internal/text.
func clip(s string, n int) string {
	if n <= 0 {
		return ""
	}
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// stripCodeFences removes optional backtick fences (``` or ```json)
// around a JSON payload and trims surrounding whitespace.
func stripCodeFences(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}
