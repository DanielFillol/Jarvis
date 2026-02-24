// internal/llm/extract_issue.go
package llm

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/DanielFillol/Jarvis/internal/jira"
)

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

// ExtractMultipleIssuesFromThread uses the LLM to extract several Jira issue
// drafts at once from a Slack thread.  userInstruction must describe all cards
// to be created (e.g. "crie dois cards: um bug sobre X e uma história sobre Y").
// The function returns one IssueDraft per card mentioned in the instruction.
func (c *Client) ExtractMultipleIssuesFromThread(threadHistory, userInstruction, model string, projectNameMap map[string]string) ([]jira.IssueDraft, error) {
	system := `Você é um Product Manager sênior especializado em escrever issues Jira de alta qualidade.
Sua tarefa é extrair MÚLTIPLOS rascunhos de issues a partir de uma conversa no Slack.
Retorne SOMENTE um array JSON válido, sem markdown fences.`

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

	user := fmt.Sprintf(`
Instrução do usuário (respeite SEMPRE os campos informados explicitamente — projeto, tipo, título, prioridade, labels):
%s
%s
Thread do Slack:
%s

Retorne um array JSON com um objeto por card solicitado, exatamente neste formato:
[
  {
    "project": "",
    "issue_type": "",
    "summary": "…",
    "description": "…",
    "priority": "",
    "labels": []
  }
]

Regras CRÍTICAS:
- Crie exatamente o número de cards que o usuário pediu, na ordem mencionada.
- Se o usuário informou project/issue_type/summary/priority/labels para um card → copie EXATAMENTE, não altere.
- Se o usuário não informou um campo → deixe vazio (""), o sistema pedirá depois.
- summary <= 110 chars, direto ao ponto, sem prefixos como "[Bug]".
- NÃO invente fatos. Se faltar informação escreva "A confirmar:" seguido de bullets.

Estrutura da description por tipo:
- Bug: ## Contexto\n## Evidências\n## Ambiente / Onde ocorreu\n## Passos para reproduzir\n## Resultado atual\n## Resultado esperado\n## Impacto
- História (Story): ## Contexto\n## Objetivo\n## Escopo (MVP)\n## Critérios de aceitação (lista "- [ ] ...")
- Epic: ## Contexto\n## Objetivo\n## Escopo (MVP)\n## Fora de escopo\n## KPIs\n## Fluxos-chave\n## Requisitos não-funcionais\n## Riscos\n## DoD
- Tipo desconhecido: ## Contexto\n## Problema\n## Impacto\n## Critérios de aceite\n## Links da thread
`, userInstruction, projectMapBlock, clip(threadHistory, 4500))

	messages := []OpenAIMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}
	out, err := c.Chat(messages, model, 0.2, 4000)
	if err != nil {
		return nil, err
	}
	out = strings.TrimSpace(stripCodeFences(out))
	var drafts []jira.IssueDraft
	if err := json.Unmarshal([]byte(out), &drafts); err != nil {
		return nil, fmt.Errorf("bad issues json: %v raw=%q", err, preview(out, 300))
	}
	for i := range drafts {
		drafts[i].Project = strings.TrimSpace(drafts[i].Project)
		drafts[i].IssueType = strings.TrimSpace(drafts[i].IssueType)
		drafts[i].Summary = strings.TrimSpace(drafts[i].Summary)
		drafts[i].Description = strings.TrimSpace(drafts[i].Description)
		drafts[i].Priority = strings.TrimSpace(drafts[i].Priority)
		if drafts[i].Summary == "" {
			drafts[i].Summary = fmt.Sprintf("Card %d gerado a partir de thread", i+1)
		}
		if drafts[i].Description == "" {
			drafts[i].Description = "(sem descrição)"
		}
	}
	return drafts, nil
}
