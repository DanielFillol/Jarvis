package llm

import (
	"fmt"
	"log"
	"strings"
)

// EnhancePrompt rewrites the user's question to make the intent and target data
// source explicit before the question is passed to DecideActions.  This reduces
// routing errors caused by word-order variations and implicit references.
//
// The function always returns a non-empty string: on any error or empty LLM
// response it returns the original question unchanged so the pipeline continues
// normally.
func (c *Client) EnhancePrompt(question, threadHistory, availableSources, model string) string {
	if strings.TrimSpace(question) == "" {
		return question
	}
	if strings.TrimSpace(model) == "" {
		model = "gpt-4o-mini"
	}

	histSnippet := clip(threadHistory, 800)
	histSection := ""
	if strings.TrimSpace(histSnippet) != "" {
		histSection = fmt.Sprintf("\nHistórico recente da conversa (para contexto):\n%s\n", histSnippet)
	}

	sourcesSection := ""
	if strings.TrimSpace(availableSources) != "" {
		sourcesSection = fmt.Sprintf("\nFontes de dados disponíveis:\n%s\n", availableSources)
	}

	prompt := fmt.Sprintf(`Você é um especialista em reformulação de perguntas para um assistente corporativo.
%s%s
Tarefa: Reescreva a pergunta abaixo para que fique mais precisa, sem ambiguidade e com a fonte de dados correta explícita quando mencionada implicitamente.

Regras:
- Retorne APENAS a pergunta reescrita, sem explicações adicionais
- Se a fonte de dados está implícita (ex: "do hubspot", "no crm"), deixe-a explícita no início ou final
- Corrija ordem de palavras que possa causar confusão
- Preserve o significado original completamente
- Se a pergunta já está clara, retorne-a como está

Pergunta original: %s`, sourcesSection, histSection, question)

	msgs := []OpenAIMessage{{Role: "user", Content: prompt}}
	out, err := c.Chat(msgs, model, 0.2, 400)
	if err != nil {
		log.Printf("[LLM][enhance] failed: %v — using original question", err)
		return question
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return question
	}
	return out
}
