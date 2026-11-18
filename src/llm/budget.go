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

// --- cost budget --------------------------------------------------------

// ErrCostBudgetExhausted signals that the aggregate USD cost budget is
// spent and further LLM calls are refused.
var ErrCostBudgetExhausted = errors.New("llm cost budget exhausted")

// CostBudget caps the aggregate dollar cost of the LLM calls issued under
// it (the provider-reported usage.cost of every call). A limit <= 0 means
// unlimited. Safe for concurrent use.
type CostBudget struct {
	mu    sync.Mutex
	limit float64
	spent float64
}

// NewCostBudget builds a cost budget capped at limitUSD; values <= 0 are
// unlimited.
func NewCostBudget(limitUSD float64) *CostBudget {
	return &CostBudget{limit: limitUSD}
}

// Add records the reported cost of one LLM call.
func (b *CostBudget) Add(cost float64) {
	if b == nil || cost <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.spent += cost
}

// Exhausted reports whether further LLM calls should be refused.
func (b *CostBudget) Exhausted() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.limit > 0 && b.spent >= b.limit
}

// Snapshot reports the spend so far and the limit (<= 0 when unlimited).
func (b *CostBudget) Snapshot() (spent, limit float64) {
	if b == nil {
		return 0, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spent, b.limit
}

type costBudgetCtxKey struct{}

// WithCostBudget attaches a cost budget to ctx; every LLM call issued with
// the returned context enforces it. A nil budget is unlimited.
func WithCostBudget(ctx context.Context, b *CostBudget) context.Context {
	return context.WithValue(ctx, costBudgetCtxKey{}, b)
}

func costBudgetFrom(ctx context.Context) *CostBudget {
	b, _ := ctx.Value(costBudgetCtxKey{}).(*CostBudget)
	return b
}

// refuseErr builds the budget-exhausted error with the current spend.
func refuseErr(b *CostBudget) error {
	spent, limit := b.Snapshot()
	return fmt.Errorf("%w: $%.4f spent of $%.4f limit", ErrCostBudgetExhausted, spent, limit)
}

// CostBudgetClient wraps a ModelClient and enforces the CostBudget attached
// to the call context (see WithCostBudget). Calls are refused once the
// accumulated provider-reported cost reaches the limit; the cost of calls
// already in flight when the limit is crossed still lands (the pre-check
// only gates new calls).
type CostBudgetClient struct {
	ModelClient
}

// NewCostBudgetClient returns a cost-budget-enforcing view of client.
func NewCostBudgetClient(client ModelClient) CostBudgetClient {
	return CostBudgetClient{ModelClient: client}
}

// Complete refuses the call when the budget is exhausted, then records the
// response's reported cost.
func (c CostBudgetClient) Complete(ctx context.Context, req Request) (*Response, error) {
	if b := costBudgetFrom(ctx); b.Exhausted() {
		return nil, refuseErr(b)
	}
	resp, err := c.ModelClient.Complete(ctx, req)
	if err != nil {
		return resp, err
	}
	if b := costBudgetFrom(ctx); b != nil && resp.Usage.Cost != nil {
		b.Add(*resp.Usage.Cost)
	}
	return resp, nil
}

// Stream refuses the call when the budget is exhausted, then meters the
// stream: the first usage-carrying chunk records its cost (OpenRouter
// reports usage once, in the final IncludeUsage chunk).
func (c CostBudgetClient) Stream(ctx context.Context, req Request) (Stream, error) {
	if b := costBudgetFrom(ctx); b.Exhausted() {
		return nil, refuseErr(b)
	}
	s, err := c.ModelClient.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	return &costStream{Stream: s, budget: costBudgetFrom(ctx)}, nil
}

type costStream struct {
	Stream
	budget   *CostBudget
	recorded bool
}

func (s *costStream) Recv() (*Chunk, error) {
	chunk, err := s.Stream.Recv()
	if err == nil && chunk != nil && !s.recorded && s.budget != nil &&
		chunk.Usage != nil && chunk.Usage.Cost != nil {
		s.budget.Add(*chunk.Usage.Cost)
		s.recorded = true
	}
	return chunk, err
}
