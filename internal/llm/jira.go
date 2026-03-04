package llm

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/DanielFillol/Jarvis/internal/jira"
)

// ConfirmJiraCreateIntent calls the LLM to verify whether the message
// genuinely intends to create a Jira issue right now.  threadHistory provides
// prior conversation context so the LLM can distinguish immediate commands
// from hypothetical or contextual mentions of creation.
// The lesserModel is tried first (cheaper/faster); the primaryModel is used on
// failure.  Returns false on any error to avoid creating unwanted issues.
func (c *Client) ConfirmJiraCreateIntent(question, threadHistory, lesserModel, primaryModel string) bool {
	threadSection := ""
	if t := strings.TrimSpace(threadHistory); t != "" {
		threadSection = fmt.Sprintf("\nContexto da conversa (use para entender se o pedido é imediato ou hipotético):\n%s\n", clip(t, 2000))
	}
	prompt := fmt.Sprintf(`Você é um classificador de intenção. O usuário enviou a mensagem abaixo.
Ele quer criar um novo card/issue/ticket no Jira AGORA, neste momento?

Responda APENAS "sim" ou "não".

Responda "sim" somente quando a mensagem for um pedido direto e imediato de criação:
- "crie um card", "abre um bug", "cria uma história", "criar um ticket no Jira"

Responda "não" em todos os outros casos, incluindo:
- Hipótese / cogitação: "estou pensando em abrir", "acho que deveria criar", "talvez valha criar"
- Dúvida / pesquisa primeiro: "não sei se já tem um card", "tem uma thread sobre isso?"
- Menciona criar apenas como contexto ou referência: "criar um serviço dedicado", "criação de demandas"
- Pede para buscar, resumir, analisar ou verificar algo
- Menciona criar um relatório, reporte ou documento
- Contém negação: "não é pra criar", "não quero criar"
%s
Mensagem: %q`, threadSection, question)

	messages := []OpenAIMessage{{Role: "user", Content: prompt}}

	model := strings.TrimSpace(lesserModel)
	if model == "" {
		model = primaryModel
	}
	out, err := c.Chat(messages, model, 0, 10)
	if err != nil && model != primaryModel {
		out, err = c.Chat(messages, primaryModel, 0, 10)
	}
	if err != nil {
		log.Printf("[LLM] confirmJiraCreateIntent error: %v — defaulting false", err)
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(out))
	confirmed := strings.HasPrefix(answer, "sim")
	log.Printf("[LLM] confirmJiraCreateIntent=%t raw=%q", confirmed, preview(out, 40))
	return confirmed
}

// ExtractIssueFromThread uses the LLM to parse a Slack thread and
// produce a Jira issue draft.  The userInstruction is the free-form
// command from the user (e.g. "crie um card no jira…").  The
// threadHistory contains recent messages in the thread.  The model
// parameter allows specifying the LLM model; callers typically pass
// the primary model from configuration.  If the call or JSON parse
// fails, an error is returned.
func (c *Client) ExtractIssueFromThread(threadHistory, userInstruction, model string, exampleIssues []string, projectNameMap map[string]string) (jira.IssueDraft, error) {
	system := `Você é um Product Manager sênior especializado em escrever issues Jira de alta qualidade.
Sua tarefa é extrair um rascunho de issue a partir de uma conversa no Slack.
Retorne SOMENTE JSON válido, sem markdown fences.`

	// Build project name→key mapping block
	projectMapBlock := ""
	if len(projectNameMap) > 0 {
		var lines []string
		for name, key := range projectNameMap {
			lines = append(lines, fmt.Sprintf("- %s → %s", name, key))
		}
		projectMapBlock = fmt.Sprintf(`
Mapeamento de nomes de projeto para chaves Jira (use para identificar o campo "project"):
%s
`, strings.Join(lines, "\n"))
	}

	// Build real Jira examples for inspiration
	examplesBlock := ""
	if len(exampleIssues) > 0 {
		examplesBlock = fmt.Sprintf(`
Exemplos de cards bem escritos do mesmo projeto (use como referência de estilo e profundidade):
%s
`, strings.Join(exampleIssues, "\n---\n"))
	}

	user := fmt.Sprintf(`
Instrução do usuário (respeite SEMPRE os campos informados explicitamente — projeto, tipo, título, prioridade, labels):
%s
%s%s
Thread do Slack:
%s

Retorne JSON exatamente neste formato:
{
  "project": "",
  "issue_type": "",
  "summary": "…",
  "description": "…",
  "priority": "",
  "labels": []
}

Regras CRÍTICAS:
- Se o usuário informou project/issue_type/summary/priority/labels na instrução → copie EXATAMENTE, não altere.
- Se o usuário não informou um campo → deixe vazio (""), o sistema pedirá depois.
- summary <= 110 chars, direto ao ponto, sem prefixos como "[Bug]".
- NÃO invente fatos. Se faltar informação escreva "A confirmar:" seguido de bullets.

Estrutura da description por tipo:
- Bug: ## Contexto\n## Evidências\n## Ambiente / Onde ocorreu\n## Passos para reproduzir\n## Resultado atual\n## Resultado esperado\n## Impacto
- História (Story): ## Contexto\n## Objetivo\n## Escopo (MVP)\n## Critérios de aceitação (lista "- [ ] ...")
- Epic: ## Contexto\n## Objetivo\n## Escopo (MVP)\n## Fora de escopo\n## KPIs\n## Fluxos-chave\n## Requisitos não-funcionais\n## Riscos\n## DoD
- Tipo desconhecido: ## Contexto\n## Problema\n## Impacto\n## Critérios de aceite\n## Links da thread
`, userInstruction, projectMapBlock, examplesBlock, clip(threadHistory, 4500))

	messages := []OpenAIMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}
	out, err := c.Chat(messages, model, 0.2, 2000)
	if err != nil {
		return jira.IssueDraft{}, err
	}
	out = strings.TrimSpace(stripCodeFences(out))
	var d jira.IssueDraft
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		return jira.IssueDraft{}, fmt.Errorf("bad issue json: %v raw=%q", err, preview(out, 300))
	}
	d.Project = strings.TrimSpace(d.Project)
	d.IssueType = strings.TrimSpace(d.IssueType)
	d.Summary = strings.TrimSpace(d.Summary)
	d.Description = strings.TrimSpace(d.Description)
	d.Priority = strings.TrimSpace(d.Priority)
	if d.Summary == "" {
		d.Summary = "Card gerado a partir de thread"
	}
	if d.Description == "" {
		d.Description = "(sem descrição)"
	}
	return d, nil
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
