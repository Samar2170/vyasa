package session

import (
	"strings"
	"testing"
	"time"

	"vyasa/src/agent"
	"vyasa/src/llm"
	"vyasa/src/types"
)

func TestRenderRunStart(t *testing.T) {
	got := render(agent.Event{Kind: agent.EventRunStart, Agent: "financial_analyst", Text: "Company under review: RELIANCE.\nProduce findings."})
	for _, want := range []string{"▶ financial_analyst", "Company under review: RELIANCE."} {
		if !strings.Contains(got, want) {
			t.Errorf("render = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "Produce findings") {
		t.Errorf("render printed past the first line: %q", got)
	}
}

func TestRenderSubRunShowsParent(t *testing.T) {
	got := render(agent.Event{Kind: agent.EventRunStart, Agent: "research_verifier", ParentID: "abcdef1234567890"})
	if !strings.Contains(got, "sub-run of abcdef12") {
		t.Errorf("render = %q", got)
	}
}

func TestRenderToolCallCompactsArgs(t *testing.T) {
	got := render(agent.Event{
		Kind:     agent.EventToolCall,
		Agent:    "annual_report_analyst",
		ToolCall: toolCall("t", "rag_search", `{ "query" : "capex guidance" }`),
	})
	if !strings.Contains(got, "→ annual_report_analyst: rag_search") {
		t.Errorf("render = %q", got)
	}
	if strings.ContainsAny(got, "\n") && !strings.HasSuffix(got, "\n") {
		t.Errorf("render = %q, want single trailing newline", got)
	}
	if strings.Contains(got, " ") && strings.Contains(got, `" : "`) {
		t.Errorf("args not compacted: %q", got)
	}
}

func TestRenderToolResult(t *testing.T) {
	ok := render(agent.Event{
		Kind:       agent.EventToolResult,
		Agent:      "synthesizer",
		ToolCall:   toolCall("t", "metrics_snapshot", "{}"),
		ToolOutput: &types.ExecutionOutput{Output: strings.Repeat("x", 4200), Duration: 1300 * time.Millisecond},
	})
	if !strings.Contains(ok, "← synthesizer: metrics_snapshot ok 4200 chars (1.3s)") {
		t.Errorf("ok render = %q", ok)
	}

	err := render(agent.Event{
		Kind:       agent.EventToolResult,
		Agent:      "synthesizer",
		ToolCall:   toolCall("t", "ask_specialist", "{}"),
		ToolOutput: &types.ExecutionOutput{Error: "specialist failed: budget\nmore detail"},
	})
	if !strings.Contains(err, "error specialist failed: budget") || strings.Contains(err, "more detail") {
		t.Errorf("error render = %q", err)
	}
}

func TestRenderDoneLine(t *testing.T) {
	cost := 0.0132
	got := render(agent.Event{
		Kind:   agent.EventAgentDone,
		Agent:  "financial_analyst",
		Step:   6,
		Usage:  &llm.Usage{InputTokens: 12345, OutputTokens: 1234, CachedTokens: 456, Cost: &cost},
		Finish: agent.FinishCompleted,
	})
	for _, want := range []string{"✔ financial_analyst completed", "6 step(s)", "in 12345 tok", "out 1234 tok", "cached 456 tok", "$0.0132"} {
		if !strings.Contains(got, want) {
			t.Errorf("done render %q missing %q", got, want)
		}
	}

	bad := render(agent.Event{Kind: agent.EventAgentDone, Agent: "x", Finish: agent.FinishMaxSteps})
	if !strings.Contains(bad, "⚠ x max_steps") {
		t.Errorf("max-steps render = %q", bad)
	}
}

func TestRenderTextDeltaPassthrough(t *testing.T) {
	if got := render(agent.Event{Kind: agent.EventTextDelta, Text: "chunk"}); got != "chunk" {
		t.Errorf("delta render = %q", got)
	}
}
