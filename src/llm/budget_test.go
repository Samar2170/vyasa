package llm

import (
	"context"
	"errors"
	"io"
	"testing"
)

// echoClient returns a fixed response and counts Complete calls.
type echoClient struct {
	resp  Response
	calls int
	err   error
}

func (c *echoClient) Name() string               { return "echo" }
func (c *echoClient) Capabilities() Capabilities { return Capabilities{Tools: true} }
func (c *echoClient) Complete(_ context.Context, req Request) (*Response, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	return &c.resp, nil
}
func (c *echoClient) Stream(_ context.Context, _ Request) (Stream, error) {
	return nil, ErrStreamingUnsupported
}

func TestBudgetEnforcement(t *testing.T) {
	client := &echoClient{resp: textReply("hi")}
	b := NewBudget(1, 0)
	ctx := WithBudget(context.Background(), b)
	wrapped := BudgetClient{ModelClient: client}

	if _, err := wrapped.Complete(ctx, Request{}); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := wrapped.Complete(ctx, Request{}); !errors.Is(err, ErrBudgetExhausted) {
		t.Errorf("second call err = %v, want ErrBudgetExhausted", err)
	}
	if !Exhausted(ctx) {
		t.Error("Exhausted(ctx) = false, want true")
	}
	if requests, _ := b.Snapshot(); requests != 1 {
		t.Errorf("requests = %d, want 1", requests)
	}
}

func TestBudgetTokenLimit(t *testing.T) {
	resp := textReply("hi")
	resp.Usage = Usage{InputTokens: 10, OutputTokens: 5}
	client := &echoClient{resp: resp}
	b := NewBudget(0, 20)
	ctx := WithBudget(context.Background(), b)

	wrapped := BudgetClient{ModelClient: client}
	if _, err := wrapped.Complete(ctx, Request{}); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// 15 tokens recorded; limit 20 not reached yet.
	if _, err := wrapped.Complete(ctx, Request{}); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if _, tokens := b.Snapshot(); tokens != 30 {
		t.Errorf("tokens = %d, want 30", tokens)
	}
	if _, err := wrapped.Complete(ctx, Request{}); !errors.Is(err, ErrBudgetExhausted) {
		t.Errorf("third call err = %v, want ErrBudgetExhausted", err)
	}
}

func TestBudgetNilUnlimited(t *testing.T) {
	client := &echoClient{resp: textReply("hi")}
	ctx := context.Background() // no budget attached
	for i := 0; i < 3; i++ {
		if _, err := client.Complete(ctx, Request{}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if Exhausted(ctx) {
		t.Error("Exhausted(ctx) with no budget = true")
	}
}

func textReply(s string) Response {
	return Response{Message: Message{Role: RoleAssistant, Parts: []Part{{Kind: PartText, Text: s}}}}
}

// --- cost budget --------------------------------------------------------

func costReply(cost float64) Response {
	resp := textReply("hi")
	resp.Usage = Usage{InputTokens: 10, OutputTokens: 5, Cost: &cost}
	return resp
}

func TestCostBudgetExhaustion(t *testing.T) {
	b := NewCostBudget(1.00)
	if b.Exhausted() {
		t.Fatal("fresh budget reports exhausted")
	}
	b.Add(0.40)
	if b.Exhausted() {
		t.Error("exhausted at $0.40 of $1.00")
	}
	b.Add(0.60)
	if !b.Exhausted() {
		t.Error("not exhausted at $1.00 of $1.00")
	}
	spent, limit := b.Snapshot()
	if spent != 1.00 || limit != 1.00 {
		t.Errorf("snapshot = (%.4f, %.4f), want (1, 1)", spent, limit)
	}
	// Recording beyond the limit keeps accruing (in-flight calls land).
	b.Add(0.10)
	if s, _ := b.Snapshot(); s != 1.10 {
		t.Errorf("spent = %.4f, want 1.10", s)
	}
	// A limit <= 0 is unlimited.
	un := NewCostBudget(0)
	for i := 0; i < 3; i++ {
		un.Add(1.0)
	}
	if un.Exhausted() {
		t.Error("unlimited budget reports exhausted")
	}
	// Nil budget is a no-op everywhere.
	var nilBudget *CostBudget
	nilBudget.Add(5)
	if nilBudget.Exhausted() {
		t.Error("nil budget reports exhausted")
	}
}

func TestCostBudgetClientComplete(t *testing.T) {
	// 0.25 is exact in float64, so four calls sum to exactly 1.00.
	client := &echoClient{resp: costReply(0.25)}
	b := NewCostBudget(1.00)
	ctx := WithCostBudget(context.Background(), b)
	wrapped := NewCostBudgetClient(client)

	for i := 0; i < 4; i++ {
		if _, err := wrapped.Complete(ctx, Request{}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if spent, _ := b.Snapshot(); spent != 1.00 {
		t.Errorf("spent = %.4f, want 1.00", spent)
	}
	if _, err := wrapped.Complete(ctx, Request{}); !errors.Is(err, ErrCostBudgetExhausted) {
		t.Errorf("fifth call err = %v, want ErrCostBudgetExhausted", err)
	}
	// The refused call never reached the provider.
	if client.calls != 4 {
		t.Errorf("provider calls = %d, want 4", client.calls)
	}
}

func TestCostBudgetClientNilBudgetUnlimited(t *testing.T) {
	client := &echoClient{resp: costReply(1.0)}
	wrapped := NewCostBudgetClient(client)
	ctx := context.Background() // no budget attached
	for i := 0; i < 3; i++ {
		if _, err := wrapped.Complete(ctx, Request{}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}

// usageStream replays canned chunks and records the last Recv'd chunk.
type usageStream struct {
	chunks []*Chunk
	i      int
}

func (s *usageStream) Recv() (*Chunk, error) {
	if s.i >= len(s.chunks) {
		return nil, io.EOF
	}
	c := s.chunks[s.i]
	s.i++
	return c, nil
}
func (s *usageStream) Close() error { return nil }

func TestCostBudgetClientStream(t *testing.T) {
	cost := 0.25
	inner := &usageStream{chunks: []*Chunk{
		{Delta: Message{Role: RoleAssistant, Parts: []Part{{Kind: PartText, Text: "he"}}}},
		{FinishReason: FinishStop, Usage: &Usage{InputTokens: 10, OutputTokens: 5, Cost: &cost}},
	}}
	client := streamClient{stream: inner}
	b := NewCostBudget(1.00)
	ctx := WithCostBudget(context.Background(), b)
	wrapped := NewCostBudgetClient(client)

	s, err := wrapped.Stream(ctx, Request{})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	for {
		if _, err := s.Recv(); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("recv: %v", err)
		}
	}
	if spent, _ := b.Snapshot(); spent != 0.25 {
		t.Errorf("spent = %.4f, want 0.25 (recorded once)", spent)
	}
	_ = s.Close()
}

func TestCostBudgetClientStreamExhausted(t *testing.T) {
	client := streamClient{}
	b := NewCostBudget(0.10)
	b.Add(0.10)
	ctx := WithCostBudget(context.Background(), b)
	if _, err := NewCostBudgetClient(client).Stream(ctx, Request{}); !errors.Is(err, ErrCostBudgetExhausted) {
		t.Errorf("stream err = %v, want ErrCostBudgetExhausted", err)
	}
}

// streamClient adapts a canned Stream into a ModelClient.
type streamClient struct {
	stream Stream
}

func (c streamClient) Name() string               { return "stream-echo" }
func (c streamClient) Capabilities() Capabilities { return Capabilities{Streaming: true} }
func (c streamClient) Complete(_ context.Context, _ Request) (*Response, error) {
	return nil, ErrStreamingUnsupported
}
func (c streamClient) Stream(_ context.Context, _ Request) (Stream, error) {
	return c.stream, nil
}
