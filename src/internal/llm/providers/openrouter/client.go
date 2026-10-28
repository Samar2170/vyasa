package openrouter

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"vyasa/src/internal/llm"
)

const (
	defaultBaseURL = "https://openrouter.ai/api/v1"
	defaultModel   = "z-ai/glm-5.3-flash"
)

type Config struct {
	llm.Config
	SiteURL string
	AppName string
}

type Client struct {
	cfg          Config
	transport    *llm.Transport
	baseURL      string
	defaultModel string
}

var _ llm.ModelClient = (*Client)(nil)

func init() {
	llm.RegisterProvider("openrouter", func(cfg llm.Config) (llm.ModelClient, error) {
		client, err := New(Config{Config: cfg})
		if err != nil {
			return nil, err
		}
		return client, nil
	})
}
func New(cfg Config) (*Client, error) {
	cfg.Config = cfg.Config.WithDefaults()

	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	model := cfg.DefaultModel
	if model == "" {
		model = defaultModel
	}
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("openrouter: API key is required")
	}
	return &Client{
		cfg:          cfg,
		transport:    llm.NewTransport("openrouter", cfg.Config),
		baseURL:      baseURL,
		defaultModel: model,
	}, nil
}

func (c *Client) Name() string {
	return "openrouter"
}

func (c *Client) Capabilities() llm.Capabilities {
	return llm.Capabilities{
		Tools:        true,
		Streaming:    true,
		Vision:       true,
		SystemPrompt: true,
	}
}

func (c *Client) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	resp, err := c.complete(ctx, req)
	if err != nil {
		return nil, err
	}
	return &resp.Response, nil
}

func (c *Client) CompleteDetailed(ctx context.Context, req llm.Request) (*Response, error) {
	return c.complete(ctx, req)
}

func (c *Client) complete(ctx context.Context, req llm.Request) (*Response, error) {
	wireReq, err := toChatRequest(req, c.defaultModel)
	if err != nil {
		return nil, err
	}
	var wireResp chatResponse
	if err := c.transport.DoJSON(ctx, http.MethodPost, c.baseURL+"/chat/completions", c.headers(), wireReq, &wireResp); err != nil {
		return nil, err
	}
	return fromChatResponse(wireResp)
}

func (c *Client) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	wireReq, err := toChatRequest(req, c.defaultModel)
	if err != nil {
		return nil, err
	}

	wireReq.Stream = true
	wireReq.StreamOptions = &streamOptions{IncludeUsage: true}

	headers := c.headers()
	headers["Accept"] = "text/event-stream"

	resp, err := c.transport.DoStream(
		ctx,
		http.MethodPost,
		c.baseURL+"/chat/completions",
		headers,
		wireReq,
	)
	if err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	return &stream{
		body:    resp.Body,
		scanner: scanner,
	}, nil
}

func (c *Client) headers() map[string]string {
	headers := map[string]string{
		"Authorization": "Bearer " + c.cfg.APIKey,
	}
	if c.cfg.SiteURL != "" {
		headers["HTTP-Referer"] = c.cfg.SiteURL
	}
	if c.cfg.AppName != "" {
		headers["X-Title"] = c.cfg.AppName
	}
	return headers
}

type stream struct {
	body    io.ReadCloser
	scanner *bufio.Scanner
	done    bool
	closed  bool
}

func (s *stream) Recv() (*llm.Chunk, error) {
	for {
		if s.done {
			return nil, io.EOF
		}
		if !s.scanner.Scan() {
			if err := s.scanner.Err(); err != nil {
				return nil, fmt.Errorf("openrouter: read stream: %w", err)
			}
			s.done = true
			return nil, io.EOF
		}
		line := strings.TrimSpace(s.scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			s.done = true
			return nil, io.EOF
		}

		var chunk chatResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return nil, fmt.Errorf("openrouter: decode stream chunk: %w", err)
		}
		if chunk.Error != nil {
			return nil, &llm.ProviderError{
				Provider:   "openrouter",
				StatusCode: http.StatusOK,
				Code:       codeString(chunk.Error.Code),
				Message:    chunk.Error.Message,
				Retryable:  false,
			}
		}
		if len(chunk.Choices) == 0 {
			if chunk.Usage != nil {
				return &llm.Chunk{
					ID:    chunk.ID,
					Model: chunk.Model,
					Usage: usagePtr(chunk.Usage),
				}, nil
			}
			continue
		}

		out, err := fromWireStreamChoice(chunk.ID, chunk.Model, chunk.Choices[0])
		if err != nil {
			return nil, err
		}
		if err != nil {
			return nil, err
		}
		if chunk.Usage != nil {
			out.Usage = usagePtr(chunk.Usage)
		}

		if len(out.Delta.Parts) == 0 &&
			len(out.Delta.ToolCalls) == 0 &&
			out.FinishReason == "" &&
			out.Usage == nil {
			continue
		}

		return &out, nil
	}
}

func (s *stream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	s.done = true
	return s.body.Close()
}
