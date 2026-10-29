package llm

import (
	"context"
	"errors"
	"sort"
	"strings"
)

var ErrStreamingUnsupported = errors.New("llm: streaming unsupported")

type Capabilities struct {
	Tools        bool
	Streaming    bool
	Vision       bool
	SystemPrompt bool
	JSONMode     bool
}

type ModelClient interface {
	Name() string
	Capabilities() Capabilities
	Complete(ctx context.Context, req Request) (*Response, error)
	Stream(ctx context.Context, req Request) (Stream, error)
}

type Stream interface {
	Recv() (*Chunk, error)
	Close() error
}

type Accumulator struct {
	id        string
	model     string
	text      strings.Builder
	toolCalls map[int]*ToolCall
	toolOrder []int
	finish    FinishReason
	usage     *Usage
}

func NewAccumulator() *Accumulator {
	return &Accumulator{toolCalls: make(map[int]*ToolCall)}
}

func (a *Accumulator) Add(c *Chunk) {
	if c == nil {
		return
	}
	if a.toolCalls == nil {
		a.toolCalls = make(map[int]*ToolCall)
	}
	if a.id == "" {
		a.id = c.ID
	}
	if c.Model != "" {
		a.model = c.Model
	}
	for _, p := range c.Delta.Parts {
		if p.Kind == PartText {
			a.text.WriteString(p.Text)
		}
	}
	for _, tc := range c.Delta.ToolCalls {
		cur, ok := a.toolCalls[tc.Index]
		if !ok {
			cur = &ToolCall{Index: tc.Index}
			a.toolCalls[tc.Index] = cur
			a.toolOrder = append(a.toolOrder, tc.Index)
		}
		if tc.ID != "" {
			cur.ID = tc.ID
		}
		if tc.Name != "" {
			cur.Name = tc.Name
		}
		if len(tc.Arguments) > 0 {
			cur.Arguments = append(cur.Arguments, tc.Arguments...)
		}
	}
	if c.FinishReason != "" {
		a.finish = c.FinishReason
	}
	if c.Usage != nil {
		u := *c.Usage
		a.usage = &u
	}
}

func (a *Accumulator) Result() Response {
	tcs := make([]ToolCall, 0, len(a.toolCalls))
	for _, idx := range a.toolOrder {
		if tc, ok := a.toolCalls[idx]; ok {
			tcs = append(tcs, *tc)
		}
	}
	sort.Slice(tcs, func(i, j int) bool { return tcs[i].Index < tcs[j].Index })
	msg := Message{Role: RoleAssistant, ToolCalls: tcs}
	if a.text.Len() > 0 {
		msg.Parts = []Part{{Kind: PartText, Text: a.text.String()}}
	}
	finish := a.finish
	if finish == "" {
		finish = FinishUnknown
	}
	var usage Usage
	if a.usage != nil {
		usage = *a.usage
	}
	return Response{
		ID:           a.id,
		Model:        a.model,
		Message:      msg,
		FinishReason: finish,
		Usage:        usage,
	}
}
