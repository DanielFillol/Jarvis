// internal/llm/client.go
package llm

import (
	"bytes"
	"encoding/base64"
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

// ContentPart is a single element in a multi-part message content array,
// used for vision API calls that mix text and images.
type ContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *ImageURLPart `json:"image_url,omitempty"`
}

// ImageURLPart holds the URL (or base64 data URL) and optional detail level
// for an image in a vision message.
type ImageURLPart struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// ImageAttachment holds a raw image downloaded from Slack for vision API calls.
type ImageAttachment struct {
	MimeType string // e.g. "image/jpeg"
	Name     string
	Data     []byte
}

// DataURL returns the base64-encoded data URL for the image.
func (a ImageAttachment) DataURL() string {
	return "data:" + a.MimeType + ";base64," + base64.StdEncoding.EncodeToString(a.Data)
}

// OpenAIMessage defines the role and content for a message in the
// Chat Completions API.  When ContentParts is non-empty (vision messages),
// the content is serialised as an array; otherwise Content is used as a string.
type OpenAIMessage struct {
	Role         string
	Content      string        // used for plain-text messages
	ContentParts []ContentPart // used for vision messages
}

// MarshalJSON serialises the message as either a string or array content.
func (m OpenAIMessage) MarshalJSON() ([]byte, error) {
	if len(m.ContentParts) > 0 {
		return json.Marshal(struct {
			Role    string        `json:"role"`
			Content []ContentPart `json:"content"`
		}{m.Role, m.ContentParts})
	}
	return json.Marshal(struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}{m.Role, m.Content})
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
	return c.chatWithTemperature(messages, model, temperature, maxTokens)
}

// chatWithTemperature performs the actual HTTP call.  If the model rejects
// the requested temperature (e.g. gpt-5-mini only accepts the default),
// it retries once without a custom temperature.
func (c *Client) chatWithTemperature(messages []OpenAIMessage, model string, temperature float64, maxTokens int) (string, error) {
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
		bodyStr := string(rb)
		// Retry without temperature when the model doesn't support a custom value.
		if resp.StatusCode == 400 && strings.Contains(bodyStr, "\"temperature\"") && temperature != 0 {
			log.Printf("[LLM] model %s rejected temperature=%.1f — retrying with default", model, temperature)
			return c.chatWithTemperature(messages, model, 0, maxTokens)
		}
		return "", fmt.Errorf("openai status=%d body=%s", resp.StatusCode, preview(bodyStr, 400))
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
