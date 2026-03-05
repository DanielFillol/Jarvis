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
- "cria uma tarefa no transportador com base nessa thread"
- "abre um card baseado nessa conversa", "crie uma história com base no que foi discutido"
- "com base nessa thread, crie um bug", "cria um card a partir dessa conversa"
- Qualquer combinação de verbo de criação (criar/cria/abre/abrir/gera/gerar) + tipo de issue + "com base em"/"baseado em"/"a partir de"

Responda "não" em todos os outros casos, incluindo:
- Hipótese / cogitação: "estou pensando em abrir", "acho que deveria criar", "talvez valha criar"
- Dúvida / pesquisa primeiro: "não sei se já tem um card", "tem uma thread sobre isso?"
- Menciona criar apenas como contexto ou referência: "criar um serviço dedicado", "criação de demandas"
- Pede APENAS para buscar, resumir, analisar ou verificar — sem verbo de criação de issue
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

// ConfirmJiraEditIntent returns true when the message clearly intends to edit
// an existing Jira issue (transition, assign, update fields, set parent).
// Returns false on any error so no unwanted edit is triggered.
func (c *Client) ConfirmJiraEditIntent(question, threadHistory, lesserModel, primaryModel string) bool {
	threadSection := ""
	if t := strings.TrimSpace(threadHistory); t != "" {
		threadSection = fmt.Sprintf("\nContexto da conversa:\n%s\n", clip(t, 2000))
	}
	prompt := fmt.Sprintf(`Você é um classificador de intenção. O usuário enviou a mensagem abaixo.
Ele quer EDITAR um card/issue/ticket já existente no Jira AGORA? (mudar status, atribuir, atualizar campos, definir pai)

Responda APENAS "sim" ou "não".

Responda "sim" para:
- Transição de status: "fechar", "concluir", "pode concluir", "mover para In Progress", "marcar como feito", "muda status", "colocar em done"
- Atribuição: "atribuir", "assign", "designar", "responsável", "atribui para mim", "atribui ao David"
- Atualização de campos: "mudar prioridade", "alterar summary", "atualizar labels", "mudar título"
- Definir pai: "vincular ao pai", "pai é", "definir pai", "parent é", "set parent"
- Sprint: "mover para sprint", "manda pra sprint atual", "deixa pra próxima sprint", "move para sprint 5", "coloca na sprint corrente"

Responda "não" para:
- Consultas / pesquisas / resumos
- Criação de novos cards
- Hipóteses ou menções sem ação imediata
- Negações: "não quero mudar", "não precisa fechar"
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
		log.Printf("[LLM] confirmJiraEditIntent error: %v — defaulting false", err)
		return false
	}
	confirmed := strings.HasPrefix(strings.ToLower(strings.TrimSpace(out)), "sim")
	log.Printf("[LLM] confirmJiraEditIntent=%t raw=%q", confirmed, preview(out, 40))
	return confirmed
}

// ExtractJiraEditRequest uses the LLM to parse the user message and produce
// a structured EditRequest.  senderName is the Slack display name of the
// requester and is used to resolve "@me" assignments.
func (c *Client) ExtractJiraEditRequest(question, threadHistory, senderName, model string) (jira.EditRequest, error) {
	threadSection := ""
	if t := strings.TrimSpace(threadHistory); t != "" {
		threadSection = fmt.Sprintf("\nContexto da conversa:\n%s\n", clip(t, 2000))
	}
	senderLine := ""
	if senderName != "" {
		senderLine = fmt.Sprintf("\nNome do remetente (use quando o usuário disser 'para mim' / 'a mim'): %s\n", senderName)
	}
	prompt := fmt.Sprintf(`Você é um extrator de comandos de edição de cards Jira.
Analise a mensagem e retorne SOMENTE JSON válido sem markdown fences.
%s%s
Mensagem: %q

Retorne JSON exatamente neste formato (deixe campos vazios/null quando não mencionados):
{
  "issue_key": "",
  "target_status": "",
  "assignee_name": "",
  "parent_key": "",
  "summary": "",
  "description": "",
  "priority": "",
  "labels": [],
  "target_sprint": ""
}

Regras:
- issue_key: chave do card (ex: TPTDR-522). Obrigatório.
- target_sprint: preencha quando o usuário quiser mover para uma sprint.
  Use exatamente um destes valores:
  "current" → sprint atual/corrente/ativa
  "next"    → próxima sprint / sprint seguinte / deixar para depois
  "<nome>"  → sprint específica por nome ou número (ex: "Sprint 5", "5", "Sprint Março")
  Vazio quando nenhuma movimentação de sprint for pedida.
- target_status: status desejado em palavras. SEMPRE preencha quando o usuário usar verbos de transição:
  "concluir"/"fechar"/"pode concluir"/"marcar como feito"/"done"/"concluído" → "Done"
  "iniciar"/"começar"/"em andamento"/"in progress" → "In Progress"
  "reabrir"/"voltar" → "To Do"
  "revisar" → "In Review"
  Para outros verbos de ação de status, use o termo mais próximo em inglês.
  Vazio SOMENTE se nenhuma mudança de status for pedida.
- assignee_name: nome do assignee. Use "@me" quando o usuário disser "para mim", "a mim", "me atribuir". Vazio se não mencionado.
- parent_key: chave do card pai (ex: TPTDR-100). Vazio se não mencionado.
- summary/description/priority/labels: só preencha quando explicitamente pedido.
- priority em português (ex: alta, média, baixa, crítica) deve ser mantida como está — a conversão ocorre depois.
- labels: array de strings, vazio [] quando não mencionado.`, threadSection, senderLine, question)

	messages := []OpenAIMessage{{Role: "user", Content: prompt}}
	out, err := c.Chat(messages, model, 0, 300)
	if err != nil {
		return jira.EditRequest{}, err
	}
	out = strings.TrimSpace(stripCodeFences(out))
	var req jira.EditRequest
	if err := json.Unmarshal([]byte(out), &req); err != nil {
		return jira.EditRequest{}, fmt.Errorf("bad edit request json: %v raw=%q", err, preview(out, 300))
	}
	req.IssueKey = strings.TrimSpace(req.IssueKey)
	req.TargetStatus = strings.TrimSpace(req.TargetStatus)
	req.AssigneeName = strings.TrimSpace(req.AssigneeName)
	req.ParentKey = strings.TrimSpace(req.ParentKey)
	req.Summary = strings.TrimSpace(req.Summary)
	req.Description = strings.TrimSpace(req.Description)
	req.Priority = strings.TrimSpace(req.Priority)
	return req, nil
}

// PickBestSprintByName selects the sprint ID from candidates that best matches
// the user's desired sprint name or number.  Returns 0 when no match is found.
func (c *Client) PickBestSprintByName(sprints []jira.Sprint, desired string, model string) int {
	if len(sprints) == 0 || desired == "" {
		return 0
	}
	var lines []string
	for _, sp := range sprints {
		lines = append(lines, fmt.Sprintf("id=%d name=%q state=%s", sp.ID, sp.Name, sp.State))
	}
	prompt := fmt.Sprintf(`Qual sprint abaixo corresponde melhor ao pedido do usuário?
Retorne SOMENTE o ID numérico da sprint. Se nenhuma corresponder, retorne 0.

Sprint pedida: %q
Sprints disponíveis:
%s`, desired, strings.Join(lines, "\n"))

	messages := []OpenAIMessage{{Role: "user", Content: prompt}}
	out, err := c.Chat(messages, model, 0, 10)
	if err != nil {
		log.Printf("[LLM] pickBestSprintByName error: %v", err)
		return 0
	}
	out = strings.TrimSpace(out)
	var id int
	fmt.Sscanf(out, "%d", &id)
	log.Printf("[LLM] pickBestSprintByName desired=%q → id=%d", desired, id)
	return id
}

// MapStatusName maps a user-provided status name (possibly in a different language
// or informal phrasing) to the best-matching status name from the project's actual
// workflow statuses.  Returns the matched name, or desired unchanged on failure.
func (c *Client) MapStatusName(available []string, desired, model string) string {
	if len(available) == 0 || desired == "" {
		return desired
	}
	// Fast path: exact or case-insensitive match already present.
	for _, s := range available {
		if strings.EqualFold(s, desired) {
			return s
		}
	}
	prompt := fmt.Sprintf(`Qual status abaixo corresponde melhor ao que o usuário pediu?
Retorne SOMENTE o nome exato de um dos status listados, sem alteração.

Status pedido pelo usuário: %q
Status disponíveis no projeto: %s`, desired, strings.Join(available, ", "))

	messages := []OpenAIMessage{{Role: "user", Content: prompt}}
	out, err := c.Chat(messages, model, 0, 30)
	if err != nil {
		log.Printf("[LLM] mapStatusName error: %v", err)
		return desired
	}
	out = strings.TrimSpace(out)
	// Validate the response is actually one of the available statuses.
	for _, s := range available {
		if strings.EqualFold(s, out) {
			log.Printf("[LLM] mapStatusName %q → %q", desired, s)
			return s
		}
	}
	log.Printf("[LLM] mapStatusName %q → no match in available (raw=%q)", desired, out)
	return desired
}

// PickBestTransition selects the transition ID from the available list that
// best moves the issue toward the desired status.  If the desired status is
// directly available it is preferred; otherwise the best intermediate step is
// returned.  Returns "" only when no transition at all makes sense.
func (c *Client) PickBestTransition(transitions []jira.Transition, desired string, model string) string {
	if len(transitions) == 0 || desired == "" {
		return ""
	}
	var names []string
	for _, t := range transitions {
		names = append(names, fmt.Sprintf("%s (id=%s)", t.Name, t.ID))
	}
	prompt := fmt.Sprintf(`Você está ajudando a mover um card Jira para o status desejado.

Status desejado: %q
Transições disponíveis neste momento:
%s

Regras:
1. Se o status desejado estiver disponível diretamente → escolha-o.
2. Se não estiver disponível → escolha o melhor passo intermediário que leva ao destino.
   Exemplo: para chegar em "Done", se só há "Doing", escolha "Doing" (é o próximo passo natural).
3. Retorne SOMENTE o ID numérico da transição escolhida.
4. Retorne vazio APENAS se nenhuma transição fizer sentido algum (lista vazia ou destino já alcançado).`, desired, strings.Join(names, "\n"))

	messages := []OpenAIMessage{{Role: "user", Content: prompt}}
	out, err := c.Chat(messages, model, 0, 20)
	if err != nil {
		log.Printf("[LLM] pickBestTransition error: %v", err)
		return ""
	}
	out = strings.TrimSpace(out)
	log.Printf("[LLM] pickBestTransition desired=%q → id=%q", desired, out)
	return out
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
