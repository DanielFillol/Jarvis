// internal/llm/answer.go
package llm

import (
	"errors"
	"fmt"
	"strings"

	"github.com/DanielFillol/Jarvis/internal/text"
)

// IntroContext holds all runtime data used to generate the bot introduction.
type IntroContext struct {
	BotName           string
	PrimaryModel      string
	FallbackModel     string
	JiraBaseURL       string
	JiraProjects      []string // formatted as "KEY — Nome"
	SlackChannels     []string // formatted as "#canal"
	JiraCreateEnabled bool
}

// GenerateIntroduction calls the LLM to produce a rich, contextual
// self-introduction in Slack mrkdwn format using real configuration data.
func (c *Client) GenerateIntroduction(ctx IntroContext, model, fallbackModel string) (string, error) {
	botName := ctx.BotName
	if strings.TrimSpace(botName) == "" {
		botName = "Jarvis"
	}

	projText := "(nenhum projeto configurado)"
	if len(ctx.JiraProjects) > 0 {
		projText = strings.Join(ctx.JiraProjects, ", ")
	}

	chanText := "(nenhum canal listado)"
	if len(ctx.SlackChannels) > 0 {
		chanText = strings.Join(ctx.SlackChannels, ", ")
	}

	createStatus := "desabilitada"
	if ctx.JiraCreateEnabled {
		createStatus = "habilitada"
	}

	// Pick sample values to anchor examples
	sampleKey := "PROJ"
	if len(ctx.JiraProjects) > 0 {
		sampleKey = strings.SplitN(ctx.JiraProjects[0], " — ", 2)[0]
	}
	sampleChan := "geral"
	if len(ctx.SlackChannels) > 0 {
		sampleChan = strings.TrimPrefix(ctx.SlackChannels[0], "#")
	}
	sampleChan2 := "tech"
	if len(ctx.SlackChannels) > 1 {
		sampleChan2 = strings.TrimPrefix(ctx.SlackChannels[1], "#")
	}

	prompt := fmt.Sprintf(`Você é o %s, assistente operacional do Slack, integrado com Jira e IA.
Escreva uma apresentação das suas funcionalidades para a equipe, em português brasileiro.

Configuração real atual:
- Modelo de IA principal: %s
- Modelo de IA secundário (fallback): %s
- Projetos Jira disponíveis: %s
- Canais Slack onde estou presente: %s
- Criação de cards no Jira: %s

Escreva a apresentação com 4 seções distintas de casos de uso, usando exemplos REAIS baseados na configuração acima.
Use "%s" como exemplo de projeto Jira e "#%s" / "#%s" como exemplos de canais Slack.

As 4 seções são:
1. *Consultas no Jira* — roadmap, bugs abertos, issues por status/sprint/assignee, detalhes de cards específicos (use chaves reais dos projetos nos exemplos)
2. *Busca no Slack* — encontrar threads, decisões passadas, mensagens de usuários específicos, filtros por canal e data (use nomes de canais reais nos exemplos)
3. *Criação de cards no Jira* — se habilitada: linguagem natural, baseado em thread, multi-card, formato explícito; se desabilitada: mencione brevemente que está desabilitada
4. *Contexto de conversa e perguntas gerais* — o bot entende o histórico da thread, responde perguntas técnicas, ajuda a redigir textos, explica conceitos, conversa sobre qualquer assunto

Formatação OBRIGATÓRIA — Slack mrkdwn:
- *negrito* com asterisco simples (NUNCA **negrito**)
- _itálico_
- Listas com • ou -
- NUNCA use # ## ### — use *Título:* em vez disso
- Exemplos de comandos em _itálico_
- Máximo de 3000 caracteres no total

Comece com uma saudação curta e simpática mencionando o nome %s. Seja direto e mostre exemplos concretos que a equipe possa usar agora.`,
		botName,
		ctx.PrimaryModel, ctx.FallbackModel,
		projText, chanText, createStatus,
		sampleKey, sampleChan, sampleChan2,
		botName,
	)

	messages := []OpenAIMessage{{Role: "user", Content: prompt}}
	out, err := c.Chat(messages, model, 0.7, 2000)
	if err != nil && strings.TrimSpace(fallbackModel) != "" && fallbackModel != model {
		out, err = c.Chat(messages, fallbackModel, 0.7, 2000)
	}
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", errors.New("empty intro from llm")
	}
	return out, nil
}

// Compat aliases to keep parity with the monolith.
// They allow other packages (or future refactors) to refer to the same
// types names that existed before, without changing the new design.

type OpenAIChatRequest = openAIChatRequest
type OpenAIChatResponse = openAIChatResponse

// Answer generates an answer to the user's question using the language
// model.  It passes the thread history and any additional Slack or
// Jira context to the model as part of the prompt.  primaryModel
// designates the preferred model; fallbackModel is used if the
// primary fails or returns an empty response.  The returned answer
// uses Slack Markdown formatting.
func (c *Client) Answer(question, threadHistory, slackCtx, jiraCtx, primaryModel, fallbackModel string) (string, error) {
	out, err := c.answerWithModel(question, threadHistory, slackCtx, jiraCtx, primaryModel)
	if err == nil && strings.TrimSpace(out) != "" {
		return out, nil
	}
	if fallbackModel != "" && fallbackModel != primaryModel {
		out2, err2 := c.answerWithModel(question, threadHistory, slackCtx, jiraCtx, fallbackModel)
		if err2 == nil && strings.TrimSpace(out2) != "" {
			return out2, nil
		}
		if err2 != nil {
			return "", err2
		}
		return "", errors.New("empty content from openai (fallback)")
	}
	if err != nil {
		return "", err
	}
	return "", errors.New("empty content from openai")
}

// answerWithModel assembles the prompt and calls the Chat API with the
// specified model.  It converts Markdown into Slack Markdown before
// returning the result.
func (c *Client) answerWithModel(question, threadHistory, slackCtx, jiraCtx, model string) (string, error) {
	botName := c.BotName
	if strings.TrimSpace(botName) == "" {
		botName = "Jarvis"
	}
	systemParts := []string{
		"Você é o " + botName + ", assistente do Slack.",
		"Responda em português brasileiro, direto, sem enrolação, usando o contexto quando existir.",
		"Se o contexto não for suficiente, diga o que falta e sugira como achar (JQL/links).",
		"Não invente fatos.",
		"FORMATAÇÃO: Use APENAS Slack mrkdwn. Regras:",
		"- Negrito: *texto* (NÃO use **texto**)",
		"- Itálico: _texto_",
		"- Listas: comece com • ou - (sem markdown de heading)",
		"- Links de issue Jira: mencione a KEY diretamente, ex: PROJ-123",
		"- NÃO use # ## ### para títulos, use *Título:* em vez disso",
	}
	if strings.TrimSpace(c.JiraBaseURL) != "" {
		baseURL := strings.TrimRight(strings.TrimSpace(c.JiraBaseURL), "/")
		systemParts = append(systemParts, "",
			"IMPORTANTE - Links do Jira:",
			"- Quando precisar gerar um link completo de uma issue Jira, use SEMPRE este base URL: "+baseURL,
			"- Formato: "+baseURL+"/browse/KEY (ex: "+baseURL+"/browse/PROJ-123)",
			"- NUNCA use outros domínios além do base URL fornecido acima.")
	}
	system := strings.Join(systemParts, "\n")
	var u strings.Builder
	if threadHistory != "" {
		u.WriteString("CONTEXTO DO THREAD:\n")
		u.WriteString(threadHistory)
		u.WriteString("\n\n")
	}
	if slackCtx != "" {
		u.WriteString("CONTEXTO DO SLACK (busca):\n")
		u.WriteString(slackCtx)
		u.WriteString("\n\n")
	}
	if jiraCtx != "" {
		u.WriteString("CONTEXTO DO JIRA:\n")
		u.WriteString(jiraCtx)
		u.WriteString("\n\n")
	}
	u.WriteString("PERGUNTA:\n")
	u.WriteString(question)
	u.WriteString("\n\n")
	u.WriteString("Responda com tópicos curtos. Quando citar issue, inclua a KEY (ex: PROJ-123).")
	msgs := []OpenAIMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: u.String()},
	}
	out, err := c.Chat(msgs, model, 0.7, 2500)
	if err != nil {
		return "", err
	}
	// Convert Markdown to Slack Markdown
	out = text.MarkdownToMarkdown(strings.TrimSpace(out))
	return out, nil
}
