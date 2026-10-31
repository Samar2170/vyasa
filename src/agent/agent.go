package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"time"
	"vyasa/src/llm"
	"vyasa/src/types"
)

type FinishStatus string

const (
	FinishCompleted FinishStatus = "completed"
	FinishMaxSteps  FinishStatus = "max_steps"
	FinishCancelled FinishStatus = "cancelled"
	FinishError     FinishStatus = "error"
)

const DefaultMaxSteps = 50

// Runner is anything runnable as a unit of work. Agent and Pipeline both
// implement it, which is what lets orchestrators delegate to either.
type Runner interface {
	Run(ctx context.Context, client llm.ModelClient, systemPrompt, input string, opts ...RunOption) (*Result, error)
}

type Agent struct {
	Name         string
	Description  string
	SystemPrompt string
	Facts        []string
	Model        string
	Temperature  *float64
	MaxTokens    *int
	Tools        *types.Registry
	MaxSteps     int
	StepTimeout  time.Duration
}

var _ Runner = (*Agent)(nil)

type Result struct {
	FinalText string
	Usage     llm.Usage
	Steps     int
	Finish    FinishStatus
	Err       error
}

func (a *Agent) Run(ctx context.Context, client llm.ModelClient, systemPrompt string, input string, opts ...RunOption) (*Result, error) {
	cfg := inheritedRunConfig(ctx)
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.sink == nil {
		cfg.sink = noopSink
	}
	cfg.toolTimeout = a.StepTimeout

	runID := newRunID()
	maxSteps := a.MaxSteps
	if maxSteps <= 0 {
		maxSteps = DefaultMaxSteps
	}
	if cfg.stepLimit > 0 {
		maxSteps = cfg.stepLimit
	}

	emit := func(e Event) {
		e.RunID = runID
		e.ParentID = cfg.parentID
		cfg.sink(e)
	}
	result := Result{Finish: FinishMaxSteps}
	defer func() {
		emit(Event{Kind: EventAgentDone, Step: result.Steps, Text: result.FinalText, Usage: &result.Usage, Err: result.Err})
	}()

	// The run context carries run identity so delegate tools can link
	// sub-runs into the trace tree and forward events. The parent for
	// child runs is this run's ID.
	runCtx := context.WithValue(ctx, ctxKeyRunID, runID)
	runCtx = context.WithValue(runCtx, ctxKeySink, cfg.sink)
	runCtx = context.WithValue(runCtx, ctxKeyParentID, runID)

	messages := []llm.Message{
		//TODO: if facts is nil Change the string
		{
			Role: llm.RoleSystem,
			Parts: []llm.Part{
				{Kind: llm.PartText, Text: renderSystemPrompt(systemPrompt, a.Facts)},
			},
		},
		{Role: llm.RoleUser, Parts: []llm.Part{{Kind: llm.PartText, Text: input}}},
	}

	for step := 0; step < maxSteps; step++ {
		if err := ctx.Err(); err != nil {
			result.Finish = FinishCancelled
			result.Err = err
			return &result, err
		}
		emit(Event{Kind: EventStepStart, Step: step})
		emit(Event{Kind: EventLLMStart, Step: step})

		resp, err := a.step(runCtx, client, messages, cfg, emit, step)
		if err != nil {
			result.Finish = FinishError
			result.Err = err
			return &result, err
		}
		accumulateUsage(&result.Usage, resp.Usage)
		result.Steps = step + 1

		if resp.FinishReason == llm.FinishToolCalls {
			messages = append(messages, resp.Message)
			for _, msg := range toolMessages(runCtx, a.Tools, resp.Message.ToolCalls, cfg.toolTimeout, emit, step) {
				messages = append(messages, msg)
			}
			continue
		}

		result.FinalText = resp.Message.Text()
		result.Finish = FinishCompleted
		return &result, nil
	}

	// Step budget exhausted without a natural stop: surface the last
	// assistant text so the caller can still use partial work.
	if last := lastAssistantText(messages); last != "" {
		result.FinalText = last
	}
	return &result, nil

}

func renderSystemPrompt(prompt string, facts []string) string {
	if len(facts) == 0 {
		return prompt
	}
	var b strings.Builder
	if prompt != "" {
		b.WriteString(prompt)
		b.WriteString("\n\n")
	}
	b.WriteString("Facts:\n")
	for _, f := range facts {
		b.WriteString("- " + f + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (a *Agent) step(
	ctx context.Context,
	client llm.ModelClient,
	messages []llm.Message,
	cfg runConfig,
	emit func(Event),
	step int,
) (*llm.Response, error) {
	req := llm.Request{
		Model:       a.Model,
		Messages:    messages,
		Tools:       llm.ToolSpecs(a.Tools),
		Temperature: a.Temperature,
		MaxTokens:   a.MaxTokens,
	}
	if cfg.stream {
		return streamComplete(ctx, client, req, emit, step)
	}
	return client.Complete(ctx, req)

}

func streamComplete(ctx context.Context, client llm.ModelClient, req llm.Request, emit func(Event), step int) (*llm.Response, error) {
	stream, err := client.Stream(ctx, req)
	if err != nil {
		if errors.Is(err, llm.ErrStreamingUnsupported) {
			return client.Complete(ctx, req)
		}
		return nil, err
	}
	defer func() { _ = stream.Close() }()

	acc := llm.NewAccumulator()
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if chunk == nil {
			continue
		}
		acc.Add(chunk)
		for _, p := range chunk.Delta.Parts {
			if p.Kind == llm.PartText && p.Text != "" {
				emit(Event{Kind: EventTextDelta, Step: step, Text: p.Text})
			}
		}
	}
	resp := acc.Result()
	return &resp, nil
}

func lastAssistantText(messages []llm.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == llm.RoleAssistant {
			return messages[i].Text()
		}
	}
	return ""
}

func accumulateUsage(dst *llm.Usage, src llm.Usage) {
	dst.InputTokens += src.InputTokens
	dst.OutputTokens += src.OutputTokens
	dst.CachedTokens += src.CachedTokens
	if src.Cost != nil {
		var base float64
		if dst.Cost != nil {
			base = *dst.Cost
		}
		total := base + *src.Cost
		dst.Cost = &total
	}
}

func newRunID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "run-unknown"
	}
	return hex.EncodeToString(b[:])
}
