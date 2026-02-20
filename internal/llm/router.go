// internal/llm/router.go
package llm

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

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
// recent thread history and the user's question.  The returned JSON
// is unmarshalled into a RetrievalDecision.  If the JSON is invalid
// or the API call fails, a non-nil error is returned.
func (c *Client) DecideRetrieval(question, threadHistory, model string) (RetrievalDecision, error) {
	prompt := fmt.Sprintf(`Você é um roteador de contexto do Jarvis (Slack + Jira).
Decida quais fontes buscar para responder a pergunta.

Retorne APENAS JSON válido:
{
  "need_slack": true/false,
  "slack_query": "...",
  "need_jira": true/false,
  "jira_intent": "listar_bugs_abertos|explicar_v2|default",
  "jira_jql": ""
}

Fontes disponíveis:
- Jira: tickets, status, roadmap, bugs, histórias, épicos, progresso de tarefas.
- Slack: discussões, decisões, links de threads, contexto operacional, conversas sobre um tema.

Regras de roteamento:
1. Roadmap, escopo, "o que foi feito", "está no ar", bugs abertos → need_jira=true.
2. "onde falamos", "qual foi a decisão", "me manda o link", "thread do slack" → need_slack=true.
3. Resumo/retrospectiva de processo (faturamento, sprint, fechamento) → need_jira=true E need_slack=true.
4. Se need_jira=true para pergunta substantiva (não apenas listagem de tickets), considere need_slack=true também — discussões no Slack enriquecem a resposta com contexto que o Jira não tem.
5. Perguntas curtas (≤ 2 palavras) ou que já têm resposta no histórico da thread → need_slack=false, need_jira=false.
6. Criar card no Jira → need_slack=false, need_jira=false.

Regras para slack_query (IMPORTANTE):
- Use apenas 2–4 palavras-chave do tema, sem filtros extras.
- NÃO use has:thread, has:link, has:reaction — reduzem o recall drasticamente.
- Só use in:#canal se o usuário mencionar explicitamente um canal.
- Prefira termos sem aspas; use aspas apenas para frases exatas críticas.
- Exemplo ruim: "faturamento janeiro in:#faturamento has:thread"
- Exemplo bom: "faturamento janeiro"

jira_jql: se não tiver certeza, deixe vazio e use jira_intent="default".

Thread (contexto recente):
%s

Pergunta:
%s
`, clip(threadHistory, 1200), question)

	messages := []OpenAIMessage{{Role: "user", Content: prompt}}
	out, err := c.Chat(messages, model, 0.2, 600)
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

	// 1. Jira creation intent → no external retrieval needed.
	if parse.LooksLikeJiraCreateIntent(question) ||
		strings.Contains(qLower, "crie um card") ||
		strings.Contains(qLower, "criar um card") {
		d.NeedSlack = false
		d.SlackQuery = ""
		d.NeedJira = false
		d.JiraIntent = "default"
		d.JiraJQL = ""
		return
	}

	// 2. Very short questions (≤ 2 words) are likely answered by thread history alone.
	if len(strings.Fields(qLower)) <= 2 {
		d.NeedSlack = false
		d.SlackQuery = ""
		d.NeedJira = false
		d.JiraJQL = ""
		return
	}

	// 3. Roadmap + explicit project → Jira only (structured listing).
	if strings.Contains(qLower, "roadmap") && !strings.Contains(qLower, "v2") {
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
// (3) Example fix: from:@U02EC... -> from:U02EC...
// Also normalizes from:<@U...> -> from:U...
func normalizeSlackQuery(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return q
	}

	q = strings.ReplaceAll(q, "to:@", "to:")

	reFrom := regexp.MustCompile(`\bfrom:\s*(?:<@)?@?(U[A-Z0-9]+)(?:\|[^>]+)?>?`)
	q = reFrom.ReplaceAllString(q, "from:$1")

	reTo := regexp.MustCompile(`\bto:\s*(?:<@)?@?(U[A-Z0-9]+)(?:\|[^>]+)?>?`)
	q = reTo.ReplaceAllString(q, "to:$1")

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
// around a JSON payload.  It trims whitespace on both ends.  This
// replicates the behavior from the monolithic helper.
func stripCodeFences(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}
