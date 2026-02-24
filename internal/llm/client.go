// internal/llm/client.go
package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/DanielFillol/Jarvis/internal/config"
)

// OpenAIMessage defines the role and content for a message in the
// Chat Completions API.  Both system and user/assistant roles are
// supported.
type OpenAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// openAIChatRequest is the request payload for OpenAI's chat
// completions endpoint.  It allows specifying a model, messages and
// optional parameters such as temperature and max tokens.
type openAIChatRequest struct {
	Model               string          `json:"model"`
	Messages            []OpenAIMessage `json:"messages"`
	Temperature         float64         `json:"temperature,omitempty"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
}

// openAIChatResponse models the top-level response from the chat
// completions API.  Only the fields needed by this application are
// defined here.
type openAIChatResponse struct {
	Choices []struct {
		FinishReason string `json:"finish_reason,omitempty"`
		Message      struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Param   string `json:"param"`
		Code    string `json:"code"`
	} `json:"error,omitempty"`
}

// Client encapsulates the OpenAI API key used to authenticate chat
// completions requests.
type Client struct {
	APIKey      string
	JiraBaseURL string
	BotName     string
}

// NewClient constructs a new LLM client from the provided configuration.
func NewClient(cfg config.Config) *Client {
	return &Client{
		APIKey:      cfg.OpenAIAPIKey,
		JiraBaseURL: cfg.JiraBaseURL,
		BotName:     cfg.BotName,
	}
}

// Chat calls the OpenAI chat completions endpoint with the supplied
// messages and model.  Temperature and maxTokens control the
// creativity and length of the response.  The content of the first
// choice is returned on success.  Errors include HTTP failures,
// decoding failures and API-level errors.
func (c *Client) Chat(messages []OpenAIMessage, model string, temperature float64, maxTokens int) (string, error) {
	if c.APIKey == "" {
		return "", errors.New("missing OPENAI_API_KEY")
	}
	if strings.TrimSpace(model) == "" {
		model = "gpt-4o-mini"
	}
	reqBody := openAIChatRequest{
		Model:               model,
		Messages:            messages,
		Temperature:         temperature,
		MaxCompletionTokens: maxTokens,
	}
	b, _ := json.Marshal(reqBody)
	req, _ := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("openai status=%d body=%s", resp.StatusCode, preview(string(rb), 400))
	}
	var out openAIChatResponse
	if err := json.Unmarshal(rb, &out); err != nil {
		return "", err
	}
	if out.Error != nil {
		return "", fmt.Errorf("openai: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", errors.New("openai: no choices")
	}
	content := strings.TrimSpace(out.Choices[0].Message.Content)
	finishReason := out.Choices[0].FinishReason
	// If response truncated due to length and no content, return error
	if finishReason == "length" && content == "" {
		return "", fmt.Errorf("openai: response truncated at max_tokens with no content (model=%s)", model)
	}
	if finishReason == "length" {
		// Append a note if truncated in the middle of a sentence
		if !strings.HasSuffix(content, ".") && !strings.HasSuffix(content, "!") && !strings.HasSuffix(content, "?") {
			content += "\n_(resposta truncada - tente uma pergunta mais especifica)_"
		}
	}
	return content, nil
}

// ConfirmJiraCreateIntent calls the LLM to verify whether the message
// genuinely intends to create a Jira card.  It tries the fallback model first
// (cheaper/faster) and falls back to the primary model on failure.
// Returns true only when the LLM explicitly confirms creation intent.
// On any error, returns false to avoid creating unwanted cards.
func (c *Client) ConfirmJiraCreateIntent(question, fallbackModel, primaryModel string) bool {
	prompt := fmt.Sprintf(`Você é um classificador de intenção. O usuário enviou a mensagem abaixo.
Ele quer criar um novo card/issue/ticket no Jira agora?

Regras:
- Responda APENAS "sim" ou "não".
- "sim" somente se a mensagem pede explicitamente para abrir/criar um card, ticket, issue, história, bug ou épico no Jira.
- "não" se: a mensagem menciona criar um relatório/reporte; contém "não é pra criar"; usa palavras como "criar" ou "épico" apenas como referência de contexto; pede para buscar, resumir ou analisar algo.

Mensagem: %q`, question)

	messages := []OpenAIMessage{{Role: "user", Content: prompt}}

	model := strings.TrimSpace(fallbackModel)
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

// preview is a helper to truncate long strings for error messages.
func preview(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
