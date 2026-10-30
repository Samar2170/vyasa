package agent

import (
	"context"
	"time"
)

type runConfig struct {
	stream      bool
	sink        EventSink
	parentID    string
	stepLimit   int
	toolTimeout time.Duration
}

// RunOption tunes a single Run invocation.
type RunOption func(*runConfig)

// WithStream makes the agent use client.Stream with the Accumulator and
// emit TextDelta events; falls back to Complete if streaming is unsupported.
func WithStream() RunOption {
	return func(c *runConfig) { c.stream = true }
}

// WithEvents installs an event sink. If omitted, the sink is inherited from
// the run context (i.e. the orchestrator's sink), so sub-agent events bubble
// up automatically.
func WithEvents(sink EventSink) RunOption {
	return func(c *runConfig) {
		if sink != nil {
			c.sink = sink
		}
	}
}

// WithParentID links this run's events to a parent run (trace tree).
// If omitted, the parent ID is inherited from the run context.
func WithParentID(id string) RunOption {
	return func(c *runConfig) { c.parentID = id }
}

// WithStepLimit overrides the agent's MaxSteps for this run.
func WithStepLimit(n int) RunOption {
	return func(c *runConfig) { c.stepLimit = n }
}

// Context keys carrying run identity across the tool-call boundary, so
// delegate tools can link sub-runs and forward events without extra plumbing.
type ctxKey int

const (
	ctxKeyRunID ctxKey = iota
	ctxKeySink
	ctxKeyParentID
)

func valueFromRunContext[T any](ctx context.Context, key ctxKey) (T, bool) {
	v, ok := ctx.Value(key).(T)
	return v, ok
}

func inheritedRunConfig(ctx context.Context) runConfig {
	var cfg runConfig
	if sink, ok := valueFromRunContext[EventSink](ctx, ctxKeySink); ok {
		cfg.sink = sink
	}
	if parentID, ok := valueFromRunContext[string](ctx, ctxKeyParentID); ok {
		cfg.parentID = parentID
	}
	return cfg
}
