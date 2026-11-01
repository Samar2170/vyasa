package llm

import (
	"context"
	"errors"
	"io"
	"testing"
)

// echoClient returns a fixed response and counts Complete calls.
type echoClient struct {
	resp    Response
	calls   int
	err     error
	lastReq Request
}

func (c *echoClient) Name() string               { return "echo" }
func (c *echoClient) Capabilities() Capabilities { return Capabilities{Tools: true} }
func (c *echoClient) Complete(_ context.Context, req Request) (*Response, error) {
	c.calls++
	c.lastReq = req
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

func TestCompleteJSON(t *testing.T) {
	client := &echoClient{resp: textReply(`{"a": 1}`)}
	var out struct {
		A int `json:"a"`
	}
	if err := CompleteJSON(context.Background(), client, "sys", "usr", &out); err != nil {
		t.Fatalf("CompleteJSON: %v", err)
	}
	if out.A != 1 {
		t.Errorf("out.A = %d", out.A)
	}
	if len(client.lastReq.Messages) != 2 ||
		client.lastReq.Messages[0].Text() != "sys" ||
		client.lastReq.Messages[1].Text() != "usr" {
		t.Errorf("unexpected messages: %+v", client.lastReq.Messages)
	}
}

func TestCompleteJSONStripsFence(t *testing.T) {
	client := &echoClient{resp: textReply("```json\n{\"a\": 2}\n```")}
	var out struct {
		A int `json:"a"`
	}
	if err := CompleteJSON(context.Background(), client, "", "", &out); err != nil {
		t.Fatalf("CompleteJSON: %v", err)
	}
	if out.A != 2 {
		t.Errorf("out.A = %d", out.A)
	}
}

func TestCompleteJSONErrors(t *testing.T) {
	var out struct{}
	if err := CompleteJSON(context.Background(), nil, "", "", &out); err == nil {
		t.Error("expected error for nil client")
	}
	if err := CompleteJSON(context.Background(), &echoClient{err: io.EOF}, "", "", &out); !errors.Is(err, io.EOF) {
		t.Errorf("err = %v, want io.EOF", err)
	}
	if err := CompleteJSON(context.Background(), &echoClient{resp: textReply("not json")}, "", "", &out); err == nil {
		t.Error("expected error for non-JSON reply")
	}
	if err := CompleteJSON(context.Background(), &echoClient{resp: textReply("")}, "", "", &out); err == nil {
		t.Error("expected error for empty reply")
	}
}
