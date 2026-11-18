package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"vyasa/src/agent"
)

// TerminalSink renders the agent event stream for the terminal: run
// boundaries, steps, tool calls with their arguments, tool results with
// durations and errors, streamed text deltas inline and per-run usage.
// Events come from concurrent agent runs, so a mutex keeps the interleaved
// output intact and every line names the agent that produced it.
func TerminalSink() agent.EventSink {
	var mu sync.Mutex
	return func(e agent.Event) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Print(render(e))
	}
}

// render turns one event into its terminal text ("").
func render(e agent.Event) string {
	switch e.Kind {
	case agent.EventRunStart:
		var b strings.Builder
		fmt.Fprintf(&b, "\n▶ %s", orDash(e.Agent))
		if e.ParentID != "" {
			fmt.Fprintf(&b, " · sub-run of %s", shortID(e.ParentID))
		}
		if e.Text != "" {
			fmt.Fprintf(&b, " — %s", firstLine(e.Text, 160))
		}
		b.WriteString("\n")
		return b.String()
	case agent.EventStepStart:
		return fmt.Sprintf("· %s step %d\n", orDash(e.Agent), e.Step)
	case agent.EventLLMStart:
		return ""
	case agent.EventTextDelta:
		return e.Text
	case agent.EventToolCall:
		if e.ToolCall == nil {
			return ""
		}
		return fmt.Sprintf("→ %s: %s %s\n", orDash(e.Agent), e.ToolCall.Name, renderArgs(e.ToolCall.Arguments))
	case agent.EventToolResult:
		if e.ToolOutput == nil {
			return ""
		}
		status, detail := "ok", ""
		if e.ToolOutput.Error != "" {
			status, detail = "error", firstLine(e.ToolOutput.Error, 160)
		}
		if e.ToolOutput.Output != "" && status == "ok" {
			detail = fmt.Sprintf("%d chars", len([]rune(e.ToolOutput.Output)))
		}
		return fmt.Sprintf("← %s: %s %s %s (%.1fs)\n",
			orDash(e.Agent), e.ToolCall.Name, status, detail, e.ToolOutput.Duration.Seconds())
	case agent.EventAgentDone:
		return renderDone(e)
	}
	return ""
}

func renderDone(e agent.Event) string {
	var b strings.Builder
	mark := "✔"
	if e.Finish != agent.FinishCompleted {
		mark = "⚠"
	}
	fmt.Fprintf(&b, "\n%s %s %s — %d step(s)", mark, orDash(e.Agent), e.Finish, e.Step)
	if e.Usage != nil {
		fmt.Fprintf(&b, " · in %d tok, out %d tok, cached %d tok",
			e.Usage.InputTokens, e.Usage.OutputTokens, e.Usage.CachedTokens)
		if e.Usage.Cost != nil {
			fmt.Fprintf(&b, " · $%.4f", *e.Usage.Cost)
		}
	}
	if e.Err != nil {
		fmt.Fprintf(&b, " · error: %v", e.Err)
	}
	b.WriteString("\n")
	return b.String()
}

// renderArgs compacts tool arguments to a short single-line form.
func renderArgs(args json.RawMessage) string {
	if len(args) == 0 {
		return "{}"
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, args); err != nil {
		return truncateRunes(string(args), 120)
	}
	return truncateRunes(buf.String(), 200)
}

func firstLine(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncateRunes(strings.TrimSpace(s), max)
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

func orDash(s string) string {
	if s == "" {
		return "agent"
	}
	return s
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
