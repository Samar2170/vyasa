package agent

import (
	"vyasa/src/llm"
	"vyasa/src/types"
)

type EventKind string

const (
	EventRunStart   EventKind = "run_start"
	EventStepStart  EventKind = "step_start"
	EventLLMStart   EventKind = "llm_start"
	EventTextDelta  EventKind = "text_delta"
	EventToolCall   EventKind = "tool_call"
	EventToolResult EventKind = "tool_result"
	EventAgentDone  EventKind = "agent_done"
)

type Event struct {
	Kind       EventKind
	RunID      string
	ParentID   string // set for sub-agent runs (delegation tree)
	Agent      string // name of the agent that produced the event
	Step       int
	Text       string // run input, text delta or final text
	ToolCall   *llm.ToolCall
	ToolOutput *types.ExecutionOutput
	Usage      *llm.Usage
	Finish     FinishStatus // set on agent_done
	Err        error
}

type EventSink func(Event)

func noopSink(Event) {}
