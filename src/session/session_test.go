package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"vyasa/src/agent"
	"vyasa/src/llm"
	"vyasa/src/models"
	"vyasa/src/types"
)

// fakeStore collects every insert call so tests can assert on the full
// persisted stream after Close.
type fakeStore struct {
	mu   sync.Mutex
	rows []models.SessionEvent
}

func (f *fakeStore) InsertSessionEvents(_ context.Context, events []models.SessionEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, events...)
	return nil
}

func (f *fakeStore) all() []models.SessionEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]models.SessionEvent(nil), f.rows...)
}

func (f *fakeStore) kinds() []string {
	rows := f.all()
	kinds := make([]string, len(rows))
	for i, r := range rows {
		kinds[i] = r.Kind
	}
	return kinds
}

func toolCall(id, name, args string) *llm.ToolCall {
	return &llm.ToolCall{ID: id, Name: name, Arguments: []byte(args)}
}

func TestEventPayloadShapes(t *testing.T) {
	cost := 0.0132
	e := agent.Event{
		Kind:       agent.EventToolResult,
		ToolCall:   toolCall("tc1", "rag_search", `{"query":"capex"}`),
		ToolOutput: &types.ExecutionOutput{Output: "hits", Error: "boom", Duration: 1500 * time.Millisecond},
		Usage:      &llm.Usage{InputTokens: 10, OutputTokens: 2, CachedTokens: 1, Cost: &cost},
		Err:        fmt.Errorf("some failure"),
		Finish:     agent.FinishCompleted,
	}
	p := eventPayload(e)

	tool, ok := p["tool"].(map[string]any)
	if !ok || tool["name"] != "rag_search" {
		t.Fatalf("tool = %#v, want name rag_search", p["tool"])
	}
	// Valid arguments stay raw JSON so jsonb queries can reach into them.
	if args, ok := tool["arguments"].(json.RawMessage); !ok || string(args) != `{"query":"capex"}` {
		t.Errorf("arguments = %#v, want raw json.RawMessage", tool["arguments"])
	}
	out, ok := p["tool_output"].(map[string]any)
	if !ok || out["error"] != "boom" || out["duration_ms"] != int64(1500) {
		t.Errorf("tool_output = %#v", out)
	}
	usage, ok := p["usage"].(map[string]any)
	if !ok || usage["input_tokens"] != 10 || usage["cost"] != 0.0132 {
		t.Errorf("usage = %#v", usage)
	}
	if p["error"] != "some failure" || p["finish"] != "completed" {
		t.Errorf("error/finish = %v/%v", p["error"], p["finish"])
	}

	// Malformed arguments degrade to a string instead of breaking marshal.
	bad := agent.Event{Kind: agent.EventToolCall, ToolCall: toolCall("t", "x", `{invalid`)}
	if args, ok := eventPayload(bad)["tool"].(map[string]any)["arguments"].(string); !ok || args != `{invalid` {
		t.Errorf("malformed arguments = %#v", args)
	}
}

func TestTruncateTextKeepsRuneBoundary(t *testing.T) {
	s := strings.Repeat("é", 10) // 20 bytes
	got := truncateText(s, 15)
	if want := strings.Repeat("é", 7) + "\n…[truncated]"; got != want {
		t.Errorf("truncateText = %q, want %q", got, want)
	}
	if got := truncateText("short", 100); got != "short" {
		t.Errorf("short text = %q", got)
	}
}

func TestLoggerCoalescesDeltasAndOrdersSeq(t *testing.T) {
	st := &fakeStore{}
	l := newLogger(context.Background(), st, map[string]any{"command": "test"})
	runA, runB := "aaaa1111", "bbbb2222"

	sink := l.Sink()
	// Two streamed deltas for run A step 0, a tool event, then deltas for
	// step 1 — plus a parallel run's delta in between.
	sink(agent.Event{Kind: agent.EventRunStart, RunID: runA, Agent: "analyst", Text: "do it"})
	sink(agent.Event{Kind: agent.EventTextDelta, RunID: runA, Agent: "analyst", Step: 0, Text: "hel"})
	sink(agent.Event{Kind: agent.EventTextDelta, RunID: runA, Agent: "analyst", Step: 0, Text: "lo "})
	sink(agent.Event{Kind: agent.EventTextDelta, RunID: runB, Agent: "verifier", Step: 0, Text: "sub "})
	sink(agent.Event{Kind: agent.EventTextDelta, RunID: runB, Agent: "verifier", Step: 0, Text: "delta"})
	sink(agent.Event{Kind: agent.EventToolCall, RunID: runA, Agent: "analyst", Step: 1, ToolCall: toolCall("t1", "echo", "{}")})
	sink(agent.Event{Kind: agent.EventTextDelta, RunID: runA, Agent: "analyst", Step: 1, Text: "next "})
	sink(agent.Event{Kind: agent.EventTextDelta, RunID: runA, Agent: "analyst", Step: 1, Text: "step"})
	sink(agent.Event{Kind: agent.EventLLMStart, RunID: runA, Agent: "analyst", Step: 2})
	l.Close(map[string]any{"status": "completed"})

	rows := st.all()
	if rows[0].Kind != "session_start" {
		t.Fatalf("first row = %s, want session_start", rows[0].Kind)
	}
	wantKinds := []string{
		"session_start", // written synchronously by New
		"run_start",     // run A start
		"text",          // "hello " (run A step 0)
		"text",          // "sub delta" (run B step 0)
		"tool_call",     // run A step 1
		"text",          // "next step" (run A step 1)
		"session_end",
	}
	if got := kindsOf(rows); strings.Join(got, ",") != strings.Join(wantKinds, ",") {
		t.Errorf("kinds = %v, want %v", got, wantKinds)
	}

	var texts []string
	for _, r := range rows {
		if r.Kind == "text" {
			texts = append(texts, r.Payload["text"].(string))
			if r.Agent == "" {
				t.Errorf("text row %d missing agent", r.Seq)
			}
		}
	}
	if strings.Join(texts, "|") != "hello |sub delta|next step" {
		t.Errorf("coalesced texts = %q", texts)
	}

	// seq must be strictly increasing from 0 across all rows.
	last := -1
	for _, r := range rows {
		if r.Seq <= last {
			t.Errorf("seq not increasing: %d after %d", r.Seq, last)
		}
		last = r.Seq
	}
	if rows[0].Seq != 0 {
		t.Errorf("session_start seq = %d, want 0", rows[0].Seq)
	}
	// session_end carries the summary.
	end := rows[len(rows)-1]
	if end.Payload["status"] != "completed" {
		t.Errorf("session_end summary = %#v", end.Payload)
	}

	// Events after Close are ignored, not lost-panicked.
	l.Sink()(agent.Event{Kind: agent.EventTextDelta, Text: "late"})
	if got := len(st.all()); got != len(rows) {
		t.Errorf("post-close event persisted: %d rows, want %d", got, len(rows))
	}
}

func TestLoggerEmptyClose(t *testing.T) {
	st := &fakeStore{}
	l := newLogger(context.Background(), st, nil)
	l.Close(nil)
	rows := st.all()
	if len(rows) != 2 || rows[0].Kind != "session_start" || rows[1].Kind != "session_end" {
		t.Fatalf("rows = %v", kindsOf(rows))
	}
	// A second Close must not duplicate the end row.
	l.Close(nil)
	if len(st.all()) != 2 {
		t.Errorf("second Close duplicated rows: %v", st.kinds())
	}
}

func kindsOf(rows []models.SessionEvent) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Kind
	}
	return out
}
