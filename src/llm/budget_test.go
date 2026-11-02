package llm

import (
	"context"
	"errors"
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
