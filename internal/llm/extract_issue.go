// internal/llm/extract_issue.go
package llm

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/DanielFillol/Jarvis/internal/jira"
)

// GenerateGitHubSearchQuery uses the LLM to produce 2-3 focused technical
// search terms suitable for the GitHub Code Search API.  The terms are
// derived from the bug summary and description so they target function names,
// class names, identifiers and error messages rather than generic words.
// The returned string is a space-separated list of terms (no operators).
func (c *Client) GenerateGitHubSearchQuery(summary, description, model string) (string, error) {
	prompt := fmt.Sprintf(`Você é um engenheiro de software. Dado um bug report, gere 2-3 termos técnicos de busca para encontrar o código relevante no GitHub Code Search.

Regras:
- Retorne APENAS os termos separados por espaço, sem pontuação extra, sem aspas, sem operadores.
- Prefira nomes de funções, classes, identificadores, mensagens de erro exatas.
- Evite palavras genéricas como "erro", "bug", "problema", "sistema".
- Máximo 4 palavras no total.

Bug summary: %s
Bug description (início): %s

Responda com os termos apenas, ex: processPayment timeout RetryHandler`, summary, clip(description, 400))

	msgs := []OpenAIMessage{{Role: "user", Content: prompt}}
	out, err := c.Chat(msgs, model, 0.1, 80)
	if err != nil {
		return "", err
	}
	// Strip any accidental quotes/operators the model might add
	out = strings.TrimSpace(out)
	out = strings.Trim(out, `"'`+"`")
	return out, nil
}

// EnhanceBugWithCodeContext takes an initial Jira bug draft and code context
// fetched from GitHub and returns an enriched draft whose description includes
// three additional sections: likely code location, root cause hypothesis and
// suggested fix.  The original description content is always preserved.
func (c *Client) EnhanceBugWithCodeContext(draft jira.IssueDraft, codeContext, model string) (jira.IssueDraft, error) {
	draftJSON, _ := json.Marshal(draft)

	prompt := fmt.Sprintf(`Você é um engenheiro de software sênior especializado em análise de bugs.

Recebeu um rascunho inicial de bug report e trechos de código relevantes encontrados no GitHub.
Sua tarefa é enriquecer a descrição do bug com análise técnica baseada EXCLUSIVAMENTE no código fornecido.

Rascunho atual (JSON):
%s

Trechos de código do GitHub:
%s

Retorne o rascunho completo em JSON NO MESMO FORMATO, com a description enriquecida.
Adicione ao final da description (preservando TUDO que já está nela) estas seções em Markdown:

## Localização provável no código
- Liste os arquivos e funções/métodos que provavelmente contêm o bug, com os URLs do GitHub.
- Se não for possível determinar com certeza, escreva "A confirmar com o time de engenharia".

## Hipótese de causa raiz
- Descreva em 2-4 linhas o que provavelmente está causando o bug, com base no código visto.
- Se o código não for suficiente, escreva "A confirmar: [o que precisa ser investigado]".

## Sugestão de correção
- Proponha uma abordagem técnica objetiva para corrigir o bug.
- Pode incluir pseudo-código, nomes de funções a modificar, ou abordagens arquiteturais.
- Se incerto, escreva "A confirmar com o time de engenharia".

Regras CRÍTICAS:
- NÃO invente código nem comportamentos que não estão nos trechos fornecidos.
- Preserve TODOS os outros campos do JSON exatamente como estão (project, issue_type, summary, priority, labels).
- Retorne APENAS JSON válido, sem markdown fences.
`, string(draftJSON), codeContext)

	msgs := []OpenAIMessage{{Role: "user", Content: prompt}}
	out, err := c.Chat(msgs, model, 0.2, 2500)
	if err != nil {
		return draft, err
	}
	out = strings.TrimSpace(stripCodeFences(out))

	var enriched jira.IssueDraft
	if err := json.Unmarshal([]byte(out), &enriched); err != nil {
		return draft, fmt.Errorf("bad enriched bug json: %v raw=%q", err, preview(out, 300))
	}
	// Guarantee original critical fields are not lost
	if strings.TrimSpace(enriched.Project) == "" {
		enriched.Project = draft.Project
	}
	if strings.TrimSpace(enriched.IssueType) == "" {
		enriched.IssueType = draft.IssueType
	}
	if strings.TrimSpace(enriched.Summary) == "" {
		enriched.Summary = draft.Summary
	}
	if strings.TrimSpace(enriched.Priority) == "" && draft.Priority != "" {
		enriched.Priority = draft.Priority
	}
	if len(enriched.Labels) == 0 && len(draft.Labels) > 0 {
		enriched.Labels = draft.Labels
	}
	return enriched, nil
}

// ExtractIssueFromThread uses the LLM to parse a Slack thread and
// produce a Jira issue draft.  The userInstruction is the free-form
// command from the user (e.g. "crie um card no jira…").  The
// threadHistory contains recent messages in the thread.  The model
// parameter allows specifying the LLM model; callers typically pass
// the primary model from configuration.  If the call or JSON parse
// fails, an error is returned.
func (c *Client) ExtractIssueFromThread(threadHistory, userInstruction, model string, exampleIssues []string) (jira.IssueDraft, error) {
	system := `Você é um Product Manager sênior especializado em escrever issues Jira de alta qualidade.
Sua tarefa é extrair um rascunho de issue a partir de uma conversa no Slack.
Retorne SOMENTE JSON válido, sem markdown fences.`

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
%s
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
`, userInstruction, examplesBlock, clip(threadHistory, 4500))

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
func (c *Client) ExtractMultipleIssuesFromThread(threadHistory, userInstruction, model string) ([]jira.IssueDraft, error) {
	system := `Você é um Product Manager sênior especializado em escrever issues Jira de alta qualidade.
Sua tarefa é extrair MÚLTIPLOS rascunhos de issues a partir de uma conversa no Slack.
Retorne SOMENTE um array JSON válido, sem markdown fences.`

	user := fmt.Sprintf(`
Instrução do usuário (respeite SEMPRE os campos informados explicitamente — projeto, tipo, título, prioridade, labels):
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
`, userInstruction, clip(threadHistory, 4500))

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
