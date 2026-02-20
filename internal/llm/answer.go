// internal/llm/answer.go
package llm

import (
	"errors"
	"github.com/DanielFillol/Jarvis/internal/text"
	"strings"
)

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
