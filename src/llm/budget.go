package llm

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrBudgetExhausted signals that a session budget is spent and further
// LLM calls are refused.
var ErrBudgetExhausted = errors.New("llm budget exhausted")

// Budget caps LLM usage of a single session. A zero limit means unlimited.
// It is safe for concurrent use.
type Budget struct {
	mu          sync.Mutex
	maxRequests int
	maxTokens   int
	requests    int
	tokens      int
}

func NewBudget(maxRequests, maxTokens int) *Budget {
	return &Budget{maxRequests: maxRequests, maxTokens: maxTokens}
}

// reserve consumes one request slot, refusing the call once either limit
// is spent.
func (b *Budget) reserve() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.maxRequests > 0 && b.requests >= b.maxRequests {
		return fmt.Errorf("%w: request limit %d reached, %d tokens used", ErrBudgetExhausted, b.maxRequests, b.tokens)
	}
	if b.maxTokens > 0 && b.tokens >= b.maxTokens {
		return fmt.Errorf("%w: token limit %d reached after %d requests", ErrBudgetExhausted, b.maxTokens, b.requests)
	}
	b.requests++
	return nil
}

func (b *Budget) record(tokens int) {
	if b == nil || tokens <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens += tokens
}

func (b *Budget) exhausted() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return (b.maxRequests > 0 && b.requests >= b.maxRequests) ||
		(b.maxTokens > 0 && b.tokens >= b.maxTokens)
}

// Snapshot reports the request and token usage so far.
func (b *Budget) Snapshot() (requests, tokens int) {
	if b == nil {
		return 0, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.requests, b.tokens
}

type budgetCtxKey struct{}

// WithBudget attaches a session budget to ctx; every LLM call issued with
// the returned context enforces it. A nil budget means unlimited.
func WithBudget(ctx context.Context, b *Budget) context.Context {
	return context.WithValue(ctx, budgetCtxKey{}, b)
}

func budgetFrom(ctx context.Context) *Budget {
	b, _ := ctx.Value(budgetCtxKey{}).(*Budget)
	return b
}

// Exhausted reports whether further LLM calls under ctx would be refused.
func Exhausted(ctx context.Context) bool {
	return budgetFrom(ctx).exhausted()
}

// BudgetClient wraps a ModelClient and enforces the session Budget attached
// to the call context (see WithBudget). A nil budget is unlimited. Wrap the
// client once where it is handed to agents or clients that call the LLM.
type BudgetClient struct {
	ModelClient
}

// NewBudgetClient returns a budget-enforcing view of client.
func NewBudgetClient(client ModelClient) BudgetClient {
	return BudgetClient{ModelClient: client}
}

// Complete reserves a request slot and records the token usage of the
// response before returning it.
func (c BudgetClient) Complete(ctx context.Context, req Request) (*Response, error) {
	b := budgetFrom(ctx)
	if err := b.reserve(); err != nil {
		return nil, err
	}
	resp, err := c.ModelClient.Complete(ctx, req)
	if err != nil {
		return resp, err
	}
	b.record(resp.Usage.InputTokens + resp.Usage.OutputTokens)
	return resp, nil
}

// Stream reserves a request slot; streamed token usage is not metered, so
// only the request counts against the budget.
func (c BudgetClient) Stream(ctx context.Context, req Request) (Stream, error) {
	b := budgetFrom(ctx)
	if err := b.reserve(); err != nil {
		return nil, err
	}
	return c.ModelClient.Stream(ctx, req)
}
