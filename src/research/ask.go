package research

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"vyasa/src/agent"
	"vyasa/src/llm"
	"vyasa/src/models"
	"vyasa/src/types"
)

const AskToolName = "ask_specialist"

type sessionRecorder struct {
	mu    sync.Mutex
	qa    []models.QAPair
	usage llm.Usage
	steps int
}

func (r *sessionRecorder) AddQA(question, answer string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.qa = append(r.qa, models.QAPair{Question: question, Answer: answer})
}

func (r *sessionRecorder) AddUsage(u llm.Usage, steps int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.usage.InputTokens += u.InputTokens
	r.usage.OutputTokens += u.OutputTokens
	r.usage.CachedTokens += u.CachedTokens
	if u.Cost != nil {
		base := 0.0
		if r.usage.Cost != nil {
			base = *r.usage.Cost
		}
		total := base + *u.Cost
		r.usage.Cost = &total
	}
	r.steps += steps
}

func (r *sessionRecorder) Snapshot() (qa []models.QAPair, u llm.Usage, steps int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]models.QAPair(nil), r.qa...), r.usage, r.steps
}

type AskTool struct {
	verifier *agent.Agent
	client   llm.ModelClient
	recr     *sessionRecorder
}

var _ types.Tool = (*AskTool)(nil)

func NewAskTool(verifier *agent.Agent, client llm.ModelClient, rec *sessionRecorder) *AskTool {
	return &AskTool{verifier: verifier, client: client, recr: rec}
}

func (t *AskTool) Name() string { return AskToolName }

func (t *AskTool) Description() string {
	return "Put one specific, answerable research question to the specialist " +
		"research agent (has financial metrics, filings retrieval and web " +
		"tools). Use when the findings contradict each other, a number is " +
		"missing, or a claim needs verification. Returns the specialist's " +
		"factual answer with sources."
}

func (t *AskTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"question": {"type": "string", "description": "One specific, answerable research question."}
		},
		"required": ["question"]
	}`)
}

func (t *AskTool) Execute(ctx context.Context, args string) types.ExecutionOutput {
	start := time.Now()
	var a struct {
		Question string `json:"question"`
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return types.ExecutionOutput{Error: fmt.Sprintf("decode args: %v", err), Duration: time.Since(start)}
	}
	a.Question = strings.TrimSpace(a.Question)
	if a.Question == "" {
		return types.ExecutionOutput{Error: "question is required", Duration: time.Since(start)}
	}
	res, err := t.verifier.Run(ctx, t.client, "", a.Question)
	if err != nil {
		return types.ExecutionOutput{Error: fmt.Sprintf("specialist failed: %v", err), Duration: time.Since(start)}
	}
	t.recr.AddQA(a.Question, res.FinalText)
	t.recr.AddUsage(res.Usage, res.Steps)

	out := res.FinalText
	switch res.Finish {
	case agent.FinishMaxSteps:
		out += "\n(note: specialist hit its step limit; answer may be incomplete)"
	case agent.FinishCancelled:
		return types.ExecutionOutput{Error: "specialist cancelled", Duration: time.Since(start)}
	case agent.FinishError:
		return types.ExecutionOutput{Error: "specialist errored", Output: res.FinalText, Duration: time.Since(start)}
	}
	return types.ExecutionOutput{Output: out, Duration: time.Since(start)}
}
