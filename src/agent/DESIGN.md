# Agent Layer Design

## Status

Draft v1. Builds on `src/llm` (provider-agnostic `ModelClient`, streaming `Accumulator`, tool-call deltas) and `src/types` (`Tool`, `Registry`). No code written yet.

## Goals

- One **core agent loop** that every specialist agent reuses.
- Agents differ only by **configuration**: system prompt, facts, model, tool registry, budgets.
- **Orchestrator is an agent too** — its tools are other agents (hybrid: LLM-router delegation + static pinned steps).
- Every step of every run emits an **event stream** — one mechanism for CLI streaming, persistence, and observability.
- Fresh context per delegation; shared state flows only through the orchestrator (blackboard).

## Non-goals (later phases)

- RAG/indexer integration (RAG tools plug in via `types.Tool` — interface already supports it).
- Shared workspace / file artifacts between agents.
- DB persistence schema (event stream is designed to be persisted as-is later).
- Human-in-the-loop approvals.

---

## 1. Core Agent Loop

### Agent = config, not behavior

```go
package agent

type Agent struct {
    Name         string
    Description  string  // delegation blurb, used by DelegateTool + Hub.Roster
    SystemPrompt string
    Facts        []string
    Model        string
    Temperature  *float64
    MaxTokens    *int
    Tools        *types.Registry
    MaxSteps     int
    StepTimeout  time.Duration
}
```

- `Facts`: static context lines prepended to the system prompt (workflow-specific knowledge — the "Facts" module from prompts.md).
- `Tools`: this agent's registry — regular tools, RAG retriever tools, or delegate tools (see §3).
- `MaxSteps`: hard cap on LLM-call iterations (default 8, orchestrator 16).

### Run

`Run` is the loop over exactly one LLM conversation. It starts **fresh** every time.

```go
type Result struct {
    FinalText string
    Usage     llm.Usage
    Steps     int
    Finish    FinishStatus   // Completed | MaxSteps | Cancelled | Error
    Err       error
}

func (a *Agent) Run(ctx context.Context, client llm.ModelClient, input string, opts ...RunOption) (*Result, error)
```

Loop pseudocode:

```
messages := []llm.Message{
    {Role: system, Parts: [{Text: SystemPrompt + rendered Facts}]},
    {Role: user,   Parts: [{Text: input}]},
}
for step := 0; step < MaxSteps; step++ {
    emit LLMStepStart{step}
    resp := client.Complete(ctx, req)   // or Stream with Accumulator

    if resp.FinishReason != tool_calls:
        emit AgentDone{resp.Message.Text()}
        return Result{...}

    append resp.Message to messages
    results := executeTools(resp.Message.ToolCalls)   // §2
    append tool-result messages to messages
}
return Result{Finish: MaxSteps, FinalText: last text}
```

Decision: **implement `Complete` first**; `Stream` is a `RunOption` (`WithStream()`) that uses `client.Stream` + `Accumulator` and emits `TextDelta` events. The loop logic is identical; only the LLM call differs.

### Message assembly

- System prompt: `SystemPrompt` + `Facts` rendered as a block (`Facts:\n- ...\n- ...`). Facts are part of the system message so they're cached and stable across steps.
- History: the conversation built inside this run only (fresh per delegation). No cross-run memory in v1.
- Tool results: `llm.Message{Role: RoleTool, ToolCallID: <id>, Parts: [{Text: <output>}]}` — matches `src/llm/types.go` message model.

### RunOptions

```go
type RunOption func(*runConfig)

func WithStream() RunOption
func WithEvents(sink EventSink) RunOption      // callback or channel writer
func WithParentID(id string) RunOption          // trace linkage (see §5)
func WithStepLimit(n int) RunOption             // override MaxSteps
```

---

## 2. Tool Execution

Reuses `types.Tool` / `types.Registry` and `llm.ToolSpecs` (src/llm/tools.go).

- Parallel execution: when the model returns multiple tool calls in one response, run them concurrently via `errgroup`, each with `ctx` + per-tool timeout (`StepTimeout`).
- Result shaping: `types.ExecutionOutput` → tool message. If `Error != ""`, the message text is the error string — the LLM sees the failure and can self-correct on the next step. Tool failures are **not** run-fatal by default.
- Truncation: cap tool output length (e.g. 8k chars, configurable) to protect context budget.
- Unknown tool name: return a synthetic error tool message ("unknown tool X") so the model can recover.

### Events

```go
type EventKind string

const (
    EventStepStart EventKind = "step_start"
    EventLLMStart  EventKind = "llm_start"
    EventTextDelta EventKind = "text_delta"
    EventToolCall  EventKind = "tool_call"      // requested by model
    EventToolResult EventKind = "tool_result"
    EventAgentDone EventKind = "agent_done"
)

type Event struct {
    Kind      EventKind
    RunID     string
    ParentID  string          // set for sub-agent runs
    Step      int
    Text      string          // delta or final text
    ToolCall  *llm.ToolCall
    ToolOutput *types.ExecutionOutput
    Usage     *llm.Usage
    Err       error
}

type EventSink func(Event)
```

Event emission is the only side effect of the loop besides tool execution. CLI, persistence, and the orchestrator all consume the same stream.

---

## 3. Orchestrator (hybrid)

### Key insight: the orchestrator is an Agent whose tools are agents

```go
type DelegateTool struct {
    agent  *Agent
    client llm.ModelClient
    hub    *Hub
}

func (d *DelegateTool) Name() string { return d.agent.Name }
func (d *DelegateTool) Description() string { /* written per-agent, says what this agent does and when to use it */ }
func (d *DelegateTool) Parameters() json.RawMessage {
    // {"type":"object","properties":{"task":{"type":"string"}},"required":["task"]}
}
func (d *DelegateTool) Execute(ctx context.Context, args string) types.ExecutionOutput {
    // parse {"task": "..."} -> agent.Run(ctx, client, task)
    // output: final text of sub-agent
}
```

- Delegate tools are registered in the orchestrator's registry like any other tool. Zero new machinery: same loop, same event stream, same budgets.
- **Fresh context per delegation**: the sub-agent sees only its own system prompt + facts + the `task` string. The orchestrator's message text is the delegation payload.
- **Parallel fan-out**: if the orchestrator emits multiple delegate calls in one response, they run concurrently (§2 parallel execution comes free).

### Hybrid: static pinned steps

For deterministic pipelines (ingest → retrieve → analyze), a `Pipeline` runs agents in a fixed sequence without LLM routing:

```go
type Pipeline struct {
    Name  string
    Steps []Step
}

type Step struct {
    Agent  *Agent
    Input  string              // literal, or
    Map    func(prev *Result) string  // transform previous step output into next input
}
```

Pipeline steps are plain `Agent.Run` calls; each can still use tools (including delegates). LLM-router and Pipeline can nest: a pipeline step may be a routing orchestrator, and an orchestrator may delegate to a pipeline (a pipeline gets wrapped in a delegate tool too).

### Hub

```go
type Hub struct {
    agents map[string]*Agent
}
```

- Holds the catalog of specialists by name; provides delegate-tool descriptions so agent definitions and their delegation blurbs live in one place.
- Prevents name collisions; orchestrator's system prompt is auto-augmented with a roster of available delegates.

---

## 4. Context & Token Budgets

- Per-agent: `MaxSteps` + `MaxTokens` per LLM call.
- Tool output truncation (§2) is the main defense against context blowup.
- v1 has no automatic history compaction. If a run approaches the limit: finish with `MaxSteps` status and surface it. Compaction (summarize-then-replace) is a later phase.
- Fresh-context-per-delegation keeps sub-agent contexts small by construction.

---

## 5. Session & Trace

Runs form a tree via `ParentID`:

```
Session
└── orchestrator run (root)
    ├── delegate call → specialist run A
    ├── delegate call → specialist run B (parallel)
    └── delegate call → specialist run A (second round)
```

- Every `Event` carries `RunID`/`ParentID`, so the full trace (LLM reasoning, tool calls + outputs, usage per step) can be persisted as a flat event log and reconstructed as a tree — the "action persistence" module from prompts.md.
- `Result.Usage` aggregates over steps; session usage aggregates over runs (sum, cached tokens included — `llm.Usage` already has `CachedTokens`/`Cost`).

---

## 6. Failure Modes

| Situation | Behavior |
|---|---|
| Tool returns error | Error text fed back to LLM as tool message; loop continues |
| Unknown tool name | Synthetic error tool message; loop continues |
| `MaxSteps` reached | Return `Finish: MaxSteps` with last text; caller decides |
| `ctx` cancelled | Return `Finish: Cancelled`; in-flight tools aborted via ctx |
| LLM provider error | Return `Finish: Error` with wrapped `llm.ProviderError`; retries are the transport's job (already in `Config.MaxRetries`) |
| Delegate sub-agent fails | Tool message with failure summary; orchestrator can retry or reroute |

---

## 7. Package Layout

```
src/agent/
    DESIGN.md
    agent.go        // Agent, Result, Run, FinishStatus
    events.go       // Event, EventKind, EventSink
    runopts.go      // RunOption set, streaming mode
    tools_exec.go   // parallel tool execution, truncation
    delegate.go     // DelegateTool
    hub.go          // Hub
    pipeline.go     // Pipeline, Step
    agent_test.go   // loop tests with fake ModelClient
```

No new dependencies beyond stdlib + existing modules (`golang.org/x/sync/errgroup` if acceptable — otherwise a manual WaitGroup implementation).

---

## 8. Testing Strategy

- **Fake `ModelClient`** implementing `llm.ModelClient`: scripted responses (text-only, tool calls, mixed, stream deltas) — enables deterministic loop tests without network.
- Loop tests: tool-call round-trip, max-steps termination, tool error recovery, parallel tool calls, cancellation mid-run.
- Delegate tests: nested runs, event tree (`ParentID` linkage), parallel delegation.
- Existing transport tests (`src/llm/transport_test.go`) already cover the wire layer; agent tests stay at the fake-client level.

---

## 9. Open Questions

1. **RAG**: retriever tool takes a query string and returns chunks — but does the indexer live per-workflow or per-session? (Decide at indexer phase; interface is unaffected.)
2. **Pipeline transform**: should `Step.Map` get the full event stream of the previous step, or just `Result`? (v1: just `Result`.)
3. **Delegate tool output**: plain text only, or also structured (JSON with usage)? (v1: text; orchestrator asks for structured output in the task if needed.)
4. **Concurrency limit**: cap parallel delegations per run (e.g. 4)? (v1: yes, hardcode; make configurable later.)
