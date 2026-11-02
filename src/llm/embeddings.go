package llm

import (
	"context"
	"fmt"
	"net/http"
)

const (
	defaultEmbedBaseURL = "https://openrouter.ai/api/v1"
	// embedBatchSize caps how many inputs go into one /embeddings request.
	embedBatchSize = 64
)

type Embedder struct {
	transport *Transport
	baseURL   string
	model     string
	dim       int
}

func NewEmbedder(cfg Config, model string, dim int) *Embedder {
	cfg = cfg.WithDefaults()
	base := cfg.BaseURL
	if base == "" {
		base = defaultEmbedBaseURL
	}
	return &Embedder{
		transport: NewTransport("embeddings", cfg),
		baseURL:   base,
		model:     model,
		dim:       dim,
	}
}
func (e *Embedder) Model() string { return e.model }

// Dim returns the expected embedding dimension.
func (e *Embedder) Dim() int { return e.dim }

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedData struct {
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

type embedResponse struct {
	Model string      `json:"model"`
	Data  []embedData `json:"data"`
}

func (e *Embedder) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	out := make([][]float32, 0, len(inputs))
	for start := 0; start < len(inputs); start += embedBatchSize {
		end := start + embedBatchSize
		if end > len(inputs) {
			end = len(inputs)
		}
		vecs, err := e.embedBatch(ctx, inputs[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	return out, nil
}

func (e *Embedder) embedBatch(ctx context.Context, batch []string) ([][]float32, error) {
	headers := map[string]string{"Authorization": "Bearer " + e.transport.cfg.APIKey}
	var resp embedResponse
	if err := e.transport.DoJSON(ctx, http.MethodPost, e.baseURL+"/embeddings", headers,
		embedRequest{Model: e.model, Input: batch}, &resp); err != nil {
		return nil, fmt.Errorf("llm: embed %s: %w", e.model, err)
	}

	if len(resp.Data) != len(batch) {
		return nil, fmt.Errorf("llm: embed %s: got %d vectors for %d inputs", e.model, len(resp.Data), len(batch))
	}
	out := make([][]float32, len(batch))
	for _, d := range resp.Data {
		if d.Index < 0 || d.Index >= len(batch) {
			return nil, fmt.Errorf("llm: embed %s: index %d out of range", e.model, d.Index)
		}
		if e.dim > 0 && len(d.Embedding) != e.dim {
			return nil, fmt.Errorf("llm: embed %s: got %d dims, want %d", e.model, len(d.Embedding), e.dim)
		}
		out[d.Index] = d.Embedding
	}
	for i, v := range out {
		if len(v) == 0 {
			return nil, fmt.Errorf("llm: embed %s: missing vector for input %d", e.model, i)
		}
	}
	return out, nil
}
