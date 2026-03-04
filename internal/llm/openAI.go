package llm

import "encoding/json"

// OpenAIMessage defines the role and content for a message in the
// Chat Completions API.  When ContentParts are non-empty (vision messages),
// the content is serialized as an array; otherwise Content is used as a string.
type OpenAIMessage struct {
	Role         string
	Content      string        // used for plain-text messages
	ContentParts []ContentPart // used for vision messages
}

// MarshalJSON serializes the message with lowercase field names as required by
// the OpenAI API. Content is emitted as a string when ContentParts is empty,
// or as an array of content parts for vision messages.
func (m OpenAIMessage) MarshalJSON() ([]byte, error) {
	if len(m.ContentParts) > 0 {
		return json.Marshal(struct {
			Role    string        `json:"role"`
			Content []ContentPart `json:"content"`
		}{Role: m.Role, Content: m.ContentParts})
	}
	return json.Marshal(struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}{Role: m.Role, Content: m.Content})
}

// ContentPart is a single element in a multipart message content array,
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
