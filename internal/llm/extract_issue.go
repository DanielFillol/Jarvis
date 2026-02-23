// internal/llm/extract_issue.go
package llm

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/DanielFillol/Jarvis/internal/jira"
)

// GenerateGitHubSearchQuery uses the LLM to produce 3-4 focused technical
// search terms for the GitHub Code Search API.  Instead of extracting words
// from the user's description (which is often non-technical), the model
// reasons about what code patterns, function names and class names would
// typically implement the broken functionality.
// The returned string is a space-separated list of terms (no operators).
func (c *Client) GenerateGitHubSearchQuery(summary, threadHistory, model string) (string, error) {
	prompt := fmt.Sprintf(`Você é um engenheiro de software sênior analisando um bug reportado de forma simplória por um usuário.

Sua tarefa é identificar 3-4 termos técnicos de código para buscar no GitHub Code Search e encontrar onde esse bug provavelmente está implementado.

Bug reportado:
%s

Contexto da thread:
%s

Raciocine sobre o comportamento com defeito:
1. Que tipo de funcionalidade está quebrada? (busca, filtro, validação, formatação, criação, autenticação…)
2. Que nomes de funções, classes ou serviços em inglês implementariam isso tipicamente?
3. Há nomes técnicos mencionados na thread? (componentes, endpoints, campos de banco, serviços)

Retorne APENAS os termos separados por espaço, sem pontuação, sem aspas.
IGNORE: valores de dados (CPFs, nomes de clientes, datas, números), palavras genéricas em português.
FOQUE: identificadores de código em inglês, nomes de funções/classes/serviços, comportamentos técnicos.

Exemplos:
- Bug "CPF não encontrado na busca de motorista" → searchDriver cpfValidation findByDocument
- Bug "botão de pagamento não responde" → processPayment PaymentController handleSubmit
- Bug "email de confirmação não chegou" → sendConfirmationEmail EmailService triggerNotification`, summary, clip(threadHistory, 600))

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

// EnhanceBugWithCodeContext rewrites the bug description as a proper technical
// bug report.  It always produces structured output — even when codeContext is
// empty — by reasoning about what the broken behaviour likely involves.
// When codeContext contains real GitHub snippets the output becomes more
// specific: exact files, functions and a concrete fix proposal.
func (c *Client) EnhanceBugWithCodeContext(draft jira.IssueDraft, codeContext, model string) (jira.IssueDraft, error) {
	draftJSON, _ := json.Marshal(draft)

	codeSection := "(nenhum trecho de código encontrado no GitHub)"
	if strings.TrimSpace(codeContext) != "" {
		codeSection = codeContext
	}

	prompt := fmt.Sprintf(`Você é um engenheiro de software sênior e tech lead especializado em análise e documentação de bugs.

Recebeu um rascunho simples de bug report criado por um usuário não-técnico.
Sua tarefa é reescrever a description como um bug report técnico de alta qualidade, útil para o time de engenharia investigar e corrigir.

Rascunho atual (JSON):
%s

Trechos de código do GitHub relacionados ao bug:
%s

Retorne o rascunho completo em JSON NO MESMO FORMATO, com a description completamente reescrita.
A description deve conter exatamente estas seções em Markdown:

## Contexto
Descreva o comportamento com defeito de forma clara e objetiva, com terminologia técnica.

## Evidências
Liste sintomas, mensagens de erro, condições de reprodução. Se não há evidências concretas: "A confirmar: [o que precisa ser coletado]".

## Ambiente / Onde ocorre
Informe plataforma, versão, ambiente (prod/staging/local). Se desconhecido: "A confirmar".

## Passos para reproduzir
Liste os passos numerados. Se não foi descrito claramente: escreva os passos mais prováveis baseando-se no comportamento.

## Resultado atual
O que acontece de errado.

## Resultado esperado
O que deveria acontecer.

## Impacto
Descreva o impacto no usuário e no negócio.

## Localização provável no código
%s

## Hipótese de causa raiz
%s

## Sugestão de correção
%s

Regras CRÍTICAS:
- Reescreva a description completamente — o rascunho original era simplório e não serve para o time técnico.
- Use linguagem técnica e objetiva. Nunca use frases vagas como "o sistema não funciona".
- Para "Localização", "Hipótese" e "Sugestão": se tiver trechos de código, baseie-se EXCLUSIVAMENTE neles. Se não tiver, raciocine sobre o que tipicamente implementa esse tipo de funcionalidade e escreva sua hipótese, sempre prefixando com "Hipótese: ".
- NÃO invente dados concretos (nomes de usuários, CPFs, IDs). Use "[valor informado]" como placeholder.
- Preserve TODOS os outros campos do JSON exatamente como estão (project, issue_type, summary, priority, labels).
- Retorne APENAS JSON válido, sem markdown fences.
`,
		string(draftJSON),
		codeSection,
		codeLocationInstruction(codeContext),
		causeHypothesisInstruction(codeContext),
		fixSuggestionInstruction(codeContext),
	)

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

// codeLocationInstruction returns the instruction for the "Localização provável"
// section depending on whether real code context is available.
func codeLocationInstruction(codeContext string) string {
	if strings.TrimSpace(codeContext) != "" {
		return "Com base nos trechos de código acima, liste os arquivos e funções/métodos que provavelmente contêm o bug, incluindo os URLs do GitHub."
	}
	return "Hipótese: descreva os módulos, serviços ou camadas da aplicação que tipicamente implementam esse tipo de funcionalidade. Prefixe com \"Hipótese: \" e indique o que o time deve investigar primeiro."
}

// causeHypothesisInstruction returns the instruction for the "Hipótese de causa raiz"
// section depending on whether real code context is available.
func causeHypothesisInstruction(codeContext string) string {
	if strings.TrimSpace(codeContext) != "" {
		return "Com base no código visto, descreva em 2-4 linhas o que provavelmente está causando o bug."
	}
	return "Hipótese: com base no comportamento descrito, raciocine sobre as causas mais comuns desse tipo de bug (ex: comparação de string sem normalização, tratamento de caracteres especiais, filtro de query mal construído, cache desatualizado). Prefixe com \"Hipótese: \"."
}

// fixSuggestionInstruction returns the instruction for the "Sugestão de correção"
// section depending on whether real code context is available.
func fixSuggestionInstruction(codeContext string) string {
	if strings.TrimSpace(codeContext) != "" {
		return "Com base no código visto, proponha uma abordagem técnica concreta para corrigir o bug. Pode incluir pseudo-código ou nomes de funções a modificar."
	}
	return "Hipótese: sugira as verificações e correções mais prováveis para esse tipo de bug, sem inventar código específico. Indique o que o desenvolvedor deve checar primeiro. Prefixe com \"Hipótese: \"."
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
