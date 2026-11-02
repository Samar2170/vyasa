package ir

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"vyasa/src/llm"
)

// defaultChatBaseURL serves when the LLM config carries no base URL (the
// default openrouter provider is OpenAI-compatible).
const defaultChatBaseURL = "https://openrouter.ai/api/v1"

// llmChat is the chat-completions endpoint backing the link-resolution and
// classification prompts.
type llmChat struct {
	transport *llm.Transport
	url       string
	model     string
	apiKey    string
}

// newLLMChat builds the chat handle from the LLM config, or nil when no API
// key is configured.
func newLLMChat(provider string, cfg llm.Config) *llmChat {
	if cfg.APIKey == "" {
		return nil
	}
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = defaultChatBaseURL
	}
	return &llmChat{
		transport: llm.NewTransport(provider, cfg),
		url:       base + "/chat/completions",
		model:     cfg.DefaultModel,
		apiKey:    cfg.APIKey,
	}
}

// chatRequest is the minimal chat-completions payload the ir prompts need.
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}

// chatMessage is one system/user/assistant turn.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatResponse keeps the reply text of the first choice plus the in-body
// error some providers return with a 200 status.
type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Error *chatError `json:"error"`
}

// chatError is a provider error embedded in an otherwise successful reply.
type chatError struct {
	Message string `json:"message"`
	Code    any    `json:"code"`
}

// chatJSON sends a two-message (system, user) chat request through the llm
// transport and decodes the model's JSON reply into out. Fenced code blocks
// are stripped before decoding so a ```json wrapper does not break parsing.
func (c *Client) chatJSON(ctx context.Context, system, user string, out any) error {
	if c.llm == nil {
		return fmt.Errorf("ir: llm not configured")
	}
	req := chatRequest{Model: c.llm.model, Messages: []chatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}}
	var resp chatResponse
	err := c.llm.transport.DoJSON(ctx, http.MethodPost, c.llm.url,
		map[string]string{"Authorization": "Bearer " + c.llm.apiKey}, req, &resp)
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("ir: llm error %v: %s", resp.Error.Code, resp.Error.Message)
	}
	if len(resp.Choices) == 0 {
		return fmt.Errorf("ir: empty llm reply")
	}
	content := stripCodeFence(resp.Choices[0].Message.Content)
	if content == "" {
		return fmt.Errorf("ir: empty llm reply")
	}
	if err := json.Unmarshal([]byte(content), out); err != nil {
		return fmt.Errorf("ir: decode llm reply: %w", err)
	}
	return nil
}

// stripCodeFence removes a leading markdown code fence so JSON-only replies
// wrapped by the model still decode.
func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```")
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	return strings.TrimSpace(s)
}
