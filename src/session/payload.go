package session

import (
	"encoding/json"
	"unicode/utf8"

	"vyasa/src/agent"
)

// maxPayloadText caps tool outputs (and any other long text) stored in the
// payload column; full outputs are already truncated to 8k by the agent's
// tool-message shaping, but events carry the untruncated original.
const maxPayloadText = 16 * 1024

// eventPayload renders the event-specific data as the jsonb document stored
// in the payload column:
//
//	{"text": …, "tool": {"name", "arguments"}, "tool_output": {"output",
//	"error", "duration_ms"}, "usage": {...}, "error": …}
func eventPayload(e agent.Event) map[string]any {
	p := map[string]any{}
	if e.Text != "" {
		p["text"] = truncateText(e.Text, maxPayloadText)
	}
	if e.ToolCall != nil {
		p["tool"] = map[string]any{
			"id":        e.ToolCall.ID,
			"name":      e.ToolCall.Name,
			"arguments": jsonArgs(e.ToolCall.Arguments),
		}
	}
	if e.ToolOutput != nil {
		p["tool_output"] = map[string]any{
			"output":      truncateText(e.ToolOutput.Output, maxPayloadText),
			"error":       e.ToolOutput.Error,
			"code":        e.ToolOutput.Code,
			"duration_ms": e.ToolOutput.Duration.Milliseconds(),
		}
	}
	if e.Usage != nil {
		p["usage"] = UsageSummary(*e.Usage)
	}
	if e.Err != nil {
		p["error"] = e.Err.Error()
	}
	if e.Finish != "" {
		p["finish"] = string(e.Finish)
	}
	return p
}

// jsonArgs keeps valid JSON arguments raw so the payload stays queryable
// (payload->'tool'->'arguments'->>'query'), and falls back to a string for
// malformed arguments.
func jsonArgs(args json.RawMessage) any {
	if len(args) == 0 {
		return nil
	}
	if json.Valid(args) {
		return args
	}
	return string(args)
}

func truncateText(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := s[:max]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "\n…[truncated]"
}
