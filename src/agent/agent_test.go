package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"vyasa/src/llm"
	"vyasa/src/types"
)

// fakeClient scripts responses deterministically; no network.
type fakeClient struct {
	mu        sync.Mutex
	responses []llm.Response
	requests  []llm.Request
	i         int
	err       error
}

func (f *fakeClient) Name() string                       { return "fake" }
func (f *fakeClient) Capabilities() llm.Capabilities     { return llm.Capabilities{Tools: true} }
func (f *fakeClient) Stream(_ context.Context, _ llm.Request) (llm.Stream, error) {
	return nil, llm.ErrStreamingUnsupported
}

func (f *fakeClient) Complete(_ context.Context, req llm.Request) (*llm.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	if f.err != nil {
		return nil, f.err
	}
	if f.i >= len(f.responses) {
		return nil, errors.New("fakeClient: no more scripted responses")
	}
	r := f.responses[f.i]
	f.i++
	return &r, nil
}

func textResponse(text string) llm.Response {
	return llm.Response{
		ID:           "resp-text",
		Model:        "fake-model",
		Message:      llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{Kind: llm.PartText, Text: text}}},
		FinishReason: llm.FinishStop,
	}
}

func toolResponse(calls ...llm.ToolCall) llm.Response {
	return llm.Response{
		ID:           "resp-tools",
		Model:        "fake-model",
		Message:      llm.Message{Role: llm.RoleAssistant, ToolCalls: calls},
		FinishReason: llm.FinishToolCalls,
	}
}

func call(id, name, args string) llm.ToolCall {
	return llm.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}
}

type stubTool struct {
	name  string
	mu    sync.Mutex
	calls []string
	fn    func(args string) types.ExecutionOutput
}

func (s *stubTool) Name() string            { return s.name }
func (s *stubTool) Description() string     { return "stub " + s.name }
func (s *stubTool) Parameters() json.RawMessage { return nil }
func (s *stubTool) Execute(_ context.Context, args string) types.ExecutionOutput {
	s.mu.Lock()
	s.calls = append(s.calls, args)
	s.mu.Unlock()
	if s.fn != nil {
		return s.fn(args)
	}
	return types.ExecutionOutput{Output: args}
}

type eventCollector struct {
	mu     sync.Mutex
	events []Event
}

func (c *eventCollector) sink(e Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

func TestRunTextResponse(t *testing.T) {
	client := &fakeClient{responses: []llm.Response{textResponse("hello there")}}
	agent := &Agent{Name: "a"}

	res, err := agent.Run(context.Background(), client, "you are a", "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalText != "hello there" {
		t.Errorf("FinalText = %q", res.FinalText)
	}
	if res.Finish != FinishCompleted {
		t.Errorf("Finish = %v", res.Finish)
	}
	if res.Steps != 1 {
		t.Errorf("Steps = %d", res.Steps)
	}
	if len(client.requests) != 1 {
		t.Fatalf("requests = %d", len(client.requests))
	}
	req := client.requests[0]
	if len(req.Messages) != 2 || req.Messages[0].Role != llm.RoleSystem || req.Messages[1].Text() != "hi" {
		t.Errorf("unexpected messages: %+v", req.Messages)
	}
}

func TestRunFactsInSystemPrompt(t *testing.T) {
	client := &fakeClient{responses: []llm.Response{textResponse("ok")}}
	agent := &Agent{Name: "a", Facts: []string{"revenue grew 10%", "Q3 filing done"}}

	if _, err := agent.Run(context.Background(), client, "you are a", "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	sys := client.requests[0].Messages[0].Text()
	if !strings.Contains(sys, "you are a") {
		t.Errorf("system prompt missing base prompt: %q", sys)
	}
	if !strings.Contains(sys, "Facts:\n- revenue grew 10%\n- Q3 filing done") {
		t.Errorf("facts not rendered: %q", sys)
	}
}

func TestRunToolRoundTrip(t *testing.T) {
	echo := &stubTool{name: "echo"}
	registry := types.NewRegistry()
	registry.Register(echo)

	client := &fakeClient{responses: []llm.Response{
		toolResponse(call("tc1", "echo", `{"x":1}`)),
		textResponse("done"),
	}}
	agent := &Agent{Name: "a", Tools: registry}

	res, err := agent.Run(context.Background(), client, "", "use echo")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Finish != FinishCompleted || res.FinalText != "done" {
		t.Errorf("res = %+v", res)
	}
	if len(client.requests) != 2 {
		t.Fatalf("requests = %d", len(client.requests))
	}
	second := client.requests[1]
	found := false
	for _, m := range second.Messages {
		if m.Role == llm.RoleTool {
			found = true
			if m.ToolCallID != "tc1" {
				t.Errorf("ToolCallID = %q", m.ToolCallID)
			}
			if m.Text() != `{"x":1}` {
				t.Errorf("tool message text = %q", m.Text())
			}
		}
	}
	if !found {
		t.Error("no tool message in second request")
	}
}

func TestRunToolErrorFedBack(t *testing.T) {
	boom := &stubTool{name: "boom", fn: func(string) types.ExecutionOutput {
		return types.ExecutionOutput{Error: "disk on fire"}
	}}
	registry := types.NewRegistry()
	registry.Register(boom)

	client := &fakeClient{responses: []llm.Response{
		toolResponse(call("tc1", "boom", `{}`)),
		textResponse("recovered"),
	}}
	agent := &Agent{Name: "a", Tools: registry}

	res, err := agent.Run(context.Background(), client, "", "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Finish != FinishCompleted {
		t.Errorf("Finish = %v", res.Finish)
	}
	second := client.requests[1]
	for _, m := range second.Messages {
		if m.Role == llm.RoleTool && !strings.Contains(m.Text(), "error: disk on fire") {
			t.Errorf("tool error not fed back: %q", m.Text())
		}
	}
}

func TestRunUnknownTool(t *testing.T) {
	client := &fakeClient{responses: []llm.Response{
		toolResponse(call("tc1", "nope", `{}`)),
		textResponse("ok"),
	}}
	agent := &Agent{Name: "a", Tools: types.NewRegistry()}

	if _, err := agent.Run(context.Background(), client, "", "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, m := range client.requests[1].Messages {
		if m.Role == llm.RoleTool && !strings.Contains(m.Text(), "unknown tool") {
			t.Errorf("expected unknown tool message, got %q", m.Text())
		}
	}
}

func TestRunMaxSteps(t *testing.T) {
	always := &stubTool{name: "loop"}
	registry := types.NewRegistry()
	registry.Register(always)

	resp := toolResponse(call("tc1", "loop", `{}`))
	client := &fakeClient{responses: []llm.Response{resp, resp, resp}}
	agent := &Agent{Name: "a", Tools: registry, MaxSteps: 2}

	res, err := agent.Run(context.Background(), client, "", "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Finish != FinishMaxSteps {
		t.Errorf("Finish = %v", res.Finish)
	}
	if res.Steps != 2 {
		t.Errorf("Steps = %d", res.Steps)
	}
}

func TestRunCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	agent := &Agent{Name: "a"}

	res, err := agent.Run(ctx, &fakeClient{}, "", "go")
	if err == nil {
		t.Fatal("expected error")
	}
	if res.Finish != FinishCancelled {
		t.Errorf("Finish = %v", res.Finish)
	}
}

func TestRunUsageAggregation(t *testing.T) {
	// A text response ends the loop, so usage accumulates across steps only
	// when a tool call sits in between.
	r1 := toolResponse(call("tc1", "echo", `{}`))
	r1.Usage = llm.Usage{InputTokens: 10, OutputTokens: 5}
	r2 := textResponse("done")
	r2.Usage = llm.Usage{InputTokens: 20, OutputTokens: 7, CachedTokens: 4}

	echo := &stubTool{name: "echo"}
	registry := types.NewRegistry()
	registry.Register(echo)

	client := &fakeClient{responses: []llm.Response{r1, r2}}
	agent := &Agent{Name: "a", Tools: registry}

	res, err := agent.Run(context.Background(), client, "", "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Usage.InputTokens != 30 || res.Usage.OutputTokens != 12 || res.Usage.CachedTokens != 4 {
		t.Errorf("usage = %+v", res.Usage)
	}
	if res.Steps != 2 {
		t.Errorf("Steps = %d", res.Steps)
	}
}

func TestRunEvents(t *testing.T) {
	echo := &stubTool{name: "echo"}
	registry := types.NewRegistry()
	registry.Register(echo)

	client := &fakeClient{responses: []llm.Response{
		toolResponse(call("tc1", "echo", `{}`)),
		textResponse("done"),
	}}
	agent := &Agent{Name: "a", Tools: registry}

	coll := &eventCollector{}
	if _, err := agent.Run(context.Background(), client, "", "go", WithEvents(coll.sink)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var kinds []EventKind
	runID := ""
	for _, e := range coll.events {
		if runID == "" {
			runID = e.RunID
		}
		if e.RunID != runID {
			t.Errorf("inconsistent RunID: %q vs %q", e.RunID, runID)
		}
		if e.Kind != EventAgentDone {
			kinds = append(kinds, e.Kind)
		}
	}
	want := []EventKind{EventStepStart, EventLLMStart, EventToolCall, EventToolResult, EventStepStart, EventLLMStart}
	if strings.Join(fmtKinds(kinds), ",") != strings.Join(fmtKinds(want), ",") {
		t.Errorf("kinds = %v, want %v", kinds, want)
	}
	last := coll.events[len(coll.events)-1]
	if last.Kind != EventAgentDone || last.Text != "done" {
		t.Errorf("agent_done = %+v", last)
	}
}

func fmtKinds(kinds []EventKind) []string {
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = string(k)
	}
	return out
}

func TestDelegate(t *testing.T) {
	// Script: orchestrator delegates -> sub-agent completes -> orchestrator finishes.
	client := &fakeClient{responses: []llm.Response{
		toolResponse(call("d1", "specialist", `{"task":"do the thing"}`)),
		textResponse("specialist done"),
		textResponse("orchestrator done"),
	}}

	registry := types.NewRegistry()
	sub := &Agent{Name: "specialist", Description: "does the thing"}
	registry.Register(NewDelegateTool(sub.Name, sub.Description, sub, client))

	orch := &Agent{Name: "orch", Tools: registry, MaxSteps: 2}
	coll := &eventCollector{}

	res, err := orch.Run(context.Background(), client, "", "get it done", WithEvents(coll.sink))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalText != "orchestrator done" {
		t.Errorf("FinalText = %q", res.FinalText)
	}

	// Sub-agent saw only its own prompt + task.
	subReq := client.requests[1]
	if len(subReq.Messages) != 2 || subReq.Messages[1].Text() != "do the thing" {
		t.Errorf("sub-agent input = %+v", subReq.Messages)
	}
	for _, m := range client.requests[2].Messages {
		if m.Role == llm.RoleTool && m.Text() != "specialist done" {
			t.Errorf("delegate output = %q", m.Text())
		}
	}
}

func TestDelegateEventTree(t *testing.T) {
	client := &fakeClient{responses: []llm.Response{
		toolResponse(call("d1", "specialist", `{"task":"do it"}`)),
		textResponse("sub done"),
		textResponse("orch done"),
	}}

	registry := types.NewRegistry()
	sub := &Agent{Name: "specialist", Description: "d"}
	registry.Register(NewDelegateTool(sub.Name, sub.Description, sub, client))

	orch := &Agent{Name: "orch", Tools: registry, MaxSteps: 2}
	coll := &eventCollector{}

	if _, err := orch.Run(context.Background(), client, "", "go", WithEvents(coll.sink)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	runIDs := map[string]bool{}
	parentIDs := map[string]bool{}
	for _, e := range coll.events {
		runIDs[e.RunID] = true
		if e.ParentID != "" {
			parentIDs[e.ParentID] = true
		}
	}
	if len(runIDs) != 2 {
		t.Errorf("expected 2 runs (orch + sub), got %d: %v", len(runIDs), runIDs)
	}
	// Sub-run's ParentID must be the orchestrator's RunID.
	for parent := range parentIDs {
		if !runIDs[parent] {
			t.Errorf("ParentID %q is not a known RunID", parent)
		}
	}
	if len(parentIDs) == 0 {
		t.Error("no parent linkage recorded")
	}
}

func TestDelegateInvalidArgs(t *testing.T) {
	sub := &Agent{Name: "s"}
	d := NewDelegateTool("s", "d", sub, &fakeClient{})

	if out := d.Execute(context.Background(), `{not json`); out.Error == "" {
		t.Error("expected error for invalid json")
	}
	if out := d.Execute(context.Background(), `{"task":"  "}`); out.Error == "" {
		t.Error("expected error for empty task")
	}
}

func TestPipeline(t *testing.T) {
	client := &fakeClient{responses: []llm.Response{
		textResponse("first"),
		textResponse("second"),
	}}

	seen := ""
	p := &Pipeline{
		Name: "pipe",
		Steps: []PipelineStep{
			{Agent: &Agent{Name: "step1"}},
			{Agent: &Agent{Name: "step2"}, Map: func(prev *Result) string {
				seen = prev.FinalText
				return "next: " + prev.FinalText
			}},
		},
	}

	res, err := p.Run(context.Background(), client, "", "start")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalText != "second" {
		t.Errorf("FinalText = %q", res.FinalText)
	}
	if res.Steps != 2 {
		t.Errorf("Steps = %d", res.Steps)
	}
	if seen != "first" {
		t.Errorf("Map saw %q", seen)
	}
	if client.requests[1].Messages[1].Text() != "next: first" {
		t.Errorf("step2 input = %q", client.requests[1].Messages[1].Text())
	}
}

func TestPipelineFallbackInput(t *testing.T) {
	client := &fakeClient{responses: []llm.Response{
		textResponse("first"),
		textResponse("second"),
	}}
	p := &Pipeline{
		Name: "pipe",
		Steps: []PipelineStep{
			{Agent: &Agent{Name: "s1"}},
			{Agent: &Agent{Name: "s2"}}, // no Input, no Map -> prev.FinalText
		},
	}
	if _, err := p.Run(context.Background(), client, "", "start"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if client.requests[1].Messages[1].Text() != "first" {
		t.Errorf("step2 input = %q", client.requests[1].Messages[1].Text())
	}
}

func TestPipelineAsDelegate(t *testing.T) {
	client := &fakeClient{responses: []llm.Response{
		toolResponse(call("d1", "pipe", `{"task":"run it"}`)),
		textResponse("pipeline final"),
		textResponse("orch done"),
	}}

	registry := types.NewRegistry()
	p := &Pipeline{Name: "pipe", Steps: []PipelineStep{{Agent: &Agent{Name: "s"}}}}
	registry.Register(p.AsDelegate(client, "a pipeline"))

	orch := &Agent{Name: "orch", Tools: registry, MaxSteps: 2}
	res, err := orch.Run(context.Background(), client, "", "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalText != "orch done" {
		t.Errorf("FinalText = %q", res.FinalText)
	}
	// Pipeline step received the delegation task.
	if client.requests[1].Messages[1].Text() != "run it" {
		t.Errorf("pipeline input = %q", client.requests[1].Messages[1].Text())
	}
}

func TestHub(t *testing.T) {
	h := NewHub()
	if err := h.Add(&Agent{Name: "b", Description: "bravo"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := h.Add(&Agent{Name: "a", Description: "alpha"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := h.Add(&Agent{Name: "b"}); err == nil {
		t.Error("expected duplicate error")
	}
	if err := h.Add(nil); err == nil {
		t.Error("expected nil error")
	}

	names := h.Names()
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Errorf("Names = %v", names)
	}

	roster := h.Roster()
	if !strings.Contains(roster, "- a: alpha") || !strings.Contains(roster, "- b: bravo") {
		t.Errorf("Roster = %q", roster)
	}

	tools := h.DelegateTools(&fakeClient{})
	if len(tools) != 2 || tools[0].Name() != "a" || tools[1].Name() != "b" {
		t.Errorf("DelegateTools = %v", tools)
	}
}

func TestStreamFallback(t *testing.T) {
	client := &fakeClient{responses: []llm.Response{textResponse("hello")}}
	agent := &Agent{Name: "a"}

	res, err := agent.Run(context.Background(), client, "", "hi", WithStream())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalText != "hello" {
		t.Errorf("FinalText = %q", res.FinalText)
	}
}

// streamFakeClient returns a scripted stream, then supports complete fallback.
type streamFakeClient struct {
	fakeClient
	chunks []llm.Chunk
}

func (f *streamFakeClient) Stream(_ context.Context, _ llm.Request) (llm.Stream, error) {
	return &scriptedStream{chunks: f.chunks}, nil
}

type scriptedStream struct {
	chunks []llm.Chunk
	i      int
}

func (s *scriptedStream) Recv() (*llm.Chunk, error) {
	if s.i >= len(s.chunks) {
		return nil, io.EOF
	}
	c := s.chunks[s.i]
	s.i++
	return &c, nil
}
func (s *scriptedStream) Close() error { return nil }

func TestRunWithStream(t *testing.T) {
	client := &streamFakeClient{
		chunks: []llm.Chunk{
			{ID: "r1", Model: "m", Delta: llm.Message{Parts: []llm.Part{{Kind: llm.PartText, Text: "hel"}}}},
			{ID: "r1", Model: "m", Delta: llm.Message{Parts: []llm.Part{{Kind: llm.PartText, Text: "lo"}}}, FinishReason: llm.FinishStop},
			{Usage: &llm.Usage{InputTokens: 3, OutputTokens: 2}},
		},
	}
	agent := &Agent{Name: "a"}
	coll := &eventCollector{}

	res, err := agent.Run(context.Background(), client, "", "hi", WithStream(), WithEvents(coll.sink))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalText != "hello" {
		t.Errorf("FinalText = %q", res.FinalText)
	}
	if res.Finish != FinishCompleted {
		t.Errorf("Finish = %v", res.Finish)
	}
	if res.Usage.InputTokens != 3 || res.Usage.OutputTokens != 2 {
		t.Errorf("usage = %+v", res.Usage)
	}
	deltas := 0
	for _, e := range coll.events {
		if e.Kind == EventTextDelta {
			deltas++
		}
	}
	if deltas != 2 {
		t.Errorf("text deltas = %d", deltas)
	}
}
