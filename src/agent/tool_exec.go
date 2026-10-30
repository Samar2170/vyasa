package agent

import (
	"context"
	"fmt"
	"sync"
	"time"
	"vyasa/src/llm"
	"vyasa/src/types"
)

const maxToolOutputLen = 8 * 1024
const maxParallelToolCalls = 8

func toolMessages(ctx context.Context, registry *types.Registry, calls []llm.ToolCall, toolTimeout time.Duration, emit func(Event), step int) []llm.Message {
	if len(calls) == 0 {
		return nil
	}
	if len(calls) > maxParallelToolCalls {
		calls = calls[:maxParallelToolCalls]
	}

	outputs := make([]types.ExecutionOutput, len(calls))
	var wg sync.WaitGroup
	for i, call := range calls {
		wg.Add(1)
		go func(i int, call llm.ToolCall) {
			defer wg.Done()
			emit(Event{Kind: EventToolCall, Step: step, ToolCall: &call})
			outputs[i] = executeTool(ctx, registry, call, toolTimeout)
			emit(Event{Kind: EventToolResult, Step: step, ToolCall: &call, ToolOutput: &outputs[i]})
		}(i, call)
	}
	wg.Wait()

	msgs := make([]llm.Message, len(calls))
	for i, call := range calls {
		msgs[i] = toolMessage(call, outputs[i])
	}
	return msgs

}

func executeTool(ctx context.Context, registry *types.Registry, call llm.ToolCall, toolTimeout time.Duration) types.ExecutionOutput {
	tool, ok := registry.Get(call.Name)
	if !ok {
		return types.ExecutionOutput{Error: fmt.Sprintf("unknown tool %q", call.Name)}
	}
	toolCtx := ctx
	if toolTimeout > 0 {
		var cancel context.CancelFunc
		toolCtx, cancel = context.WithTimeout(ctx, toolTimeout)
		defer cancel()
	}

	start := time.Now()
	out := tool.Execute(toolCtx, string(call.Arguments))
	if out.Duration == 0 {
		out.Duration = time.Since(start)
	}
	if toolCtx.Err() != nil && out.Error == "" {
		out.Error = fmt.Sprintf("tool %q interrupted: %v", call.Name, toolCtx.Err())
	}
	return out
}

func toolMessage(call llm.ToolCall, out types.ExecutionOutput) llm.Message {
	text := out.Output
	if out.Error != "" {
		text = "error: " + out.Error
	}
	return llm.Message{
		Role:       llm.RoleTool,
		ToolCallID: call.ID,
		Name:       call.Name,
		Parts:      []llm.Part{{Kind: llm.PartText, Text: truncateString(text, maxToolOutputLen)}},
	}
}

func truncateString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("\n...[truncated %d bytes]", len(s)-max)
}
