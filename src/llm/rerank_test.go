package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func serveRerank(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		var req rerankRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		// reversed relevance order exercises the positional index mapping
		results := make([]rerankResult, len(req.Documents))
		for i := range req.Documents {
			results[len(req.Documents)-1-i] = rerankResult{Index: i, RelevanceScore: 0.5 + float64(i)/100}
		}
		json.NewEncoder(w).Encode(rerankResponse{Results: results})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func calls(c *int32) int32 { return atomic.LoadInt32(c) }

func TestRerankNoRetriesAndPositionalScores(t *testing.T) {
	srv, n := serveRerank(t)

	rr := NewReranker(Config{BaseURL: srv.URL}, "qwen/qwen3-reranker-8b")
	if *rr.transport.cfg.MaxRetries != 0 {
		t.Fatalf("reranker must not retry, retries = %d", *rr.transport.cfg.MaxRetries)
	}

	docs := []string{"alpha", "beta", "gamma"}
	hits, err := rr.Rerank(context.Background(), "query", docs)
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if len(hits) != 3 || hits[0].Index != 0 || hits[1].Index != 1 || hits[2].Index != 2 {
		t.Fatalf("positional indices wrong: %+v", hits)
	}
	if hits[0].Score == hits[1].Score {
		t.Errorf("expected distinct positional scores: %+v", hits)
	}
	if calls(n) != 1 {
		t.Fatalf("first rerank must call the API once, calls = %d", calls(n))
	}
}

func TestRerankPairCache(t *testing.T) {
	srv, n := serveRerank(t)
	rr := NewReranker(Config{BaseURL: srv.URL}, "qwen/qwen3-reranker-8b")
	ctx := context.Background()
	docs := []string{"alpha", "beta", "gamma"}

	if _, err := rr.Rerank(ctx, "query", docs); err != nil {
		t.Fatalf("rerank: %v", err)
	}

	// identical call: every pair cached, zero API calls
	if _, err := rr.Rerank(ctx, "query", docs); err != nil {
		t.Fatalf("cached rerank: %v", err)
	}
	if calls(n) != 1 {
		t.Fatalf("identical call must not hit the API, calls = %d", calls(n))
	}

	// subset of a cached pool: all pairs cached, zero API calls
	if _, err := rr.Rerank(ctx, "query", docs[:2]); err != nil {
		t.Fatalf("subset rerank: %v", err)
	}
	if calls(n) != 1 {
		t.Fatalf("cached subset must not hit the API, calls = %d", calls(n))
	}

	// reordered cached pool: zero API calls
	if _, err := rr.Rerank(ctx, "query", []string{"gamma", "alpha"}); err != nil {
		t.Fatalf("reordered rerank: %v", err)
	}
	if calls(n) != 1 {
		t.Fatalf("reordered cached pool must not hit the API, calls = %d", calls(n))
	}

	// different query: same docs, new pairs, one API call
	if _, err := rr.Rerank(ctx, "other", docs); err != nil {
		t.Fatalf("other-query rerank: %v", err)
	}
	if calls(n) != 2 {
		t.Fatalf("new query must hit the API once, calls = %d", calls(n))
	}

	// one new doc mixed into a cached pool: one API call scoring only the new doc
	if _, err := rr.Rerank(ctx, "query", []string{"alpha", "delta"}); err != nil {
		t.Fatalf("partial rerank: %v", err)
	}
	if calls(n) != 3 {
		t.Fatalf("new doc must hit the API once, calls = %d", calls(n))
	}
}

type memCache struct {
	data map[string]float64
}

func (m *memCache) Lookup(ctx context.Context, keys []string) (map[string]float64, error) {
	out := make(map[string]float64)
	for _, k := range keys {
		if sc, ok := m.data[k]; ok {
			out[k] = sc
		}
	}
	return out, nil
}

func (m *memCache) Store(ctx context.Context, scores map[string]float64) error {
	for k, v := range scores {
		m.data[k] = v
	}
	return nil
}

func TestRerankDiskCache(t *testing.T) {
	srv, n := serveRerank(t)
	ctx := context.Background()
	docs := []string{"alpha", "beta"}
	disk := &memCache{data: make(map[string]float64)}

	rr := NewReranker(Config{BaseURL: srv.URL}, "qwen/qwen3-reranker-8b").WithCache(disk)
	if _, err := rr.Rerank(ctx, "query", docs); err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if len(disk.data) != 2 {
		t.Fatalf("disk cache must hold both pairs, has %d", len(disk.data))
	}

	// a fresh process (empty in-memory cache) must be served from disk
	rr2 := NewReranker(Config{BaseURL: srv.URL}, "qwen/qwen3-reranker-8b").WithCache(disk)
	hits, err := rr2.Rerank(ctx, "query", docs)
	if err != nil {
		t.Fatalf("warm rerank: %v", err)
	}
	if calls(n) != 1 {
		t.Fatalf("warm rerank must not hit the API, calls = %d", calls(n))
	}
	if len(hits) != 2 || hits[0].Index != 0 || hits[1].Index != 1 {
		t.Fatalf("positional indices wrong: %+v", hits)
	}
}

func TestRerankBudgetRefusesUncachedPool(t *testing.T) {
	srv, n := serveRerank(t)
	ctx := context.Background()

	rr := NewReranker(Config{BaseURL: srv.URL}, "qwen/qwen3-reranker-8b").
		WithBudget(NewRerankBudget(2))

	_, err := rr.Rerank(ctx, "query", []string{"alpha", "beta", "gamma"})
	if !errors.Is(err, ErrRerankBudget) {
		t.Fatalf("over-budget pool must fail with ErrRerankBudget, got %v", err)
	}
	if calls(n) != 0 {
		t.Fatalf("refused pool must not hit the API, calls = %d", calls(n))
	}
	b := rr.budget
	if b.Spent() != 0 || b.Skipped() != 1 || b.Left() != 2 {
		t.Fatalf("budget state after refusal: spent %d skipped %d left %d", b.Spent(), b.Skipped(), b.Left())
	}

	// a pool that fits is scored normally
	if _, err := rr.Rerank(ctx, "query", []string{"alpha", "beta"}); err != nil {
		t.Fatalf("within-budget pool: %v", err)
	}
	if calls(n) != 1 || b.Spent() != 2 || b.Left() != 0 {
		t.Fatalf("within-budget pool: calls %d spent %d left %d", calls(n), b.Spent(), b.Left())
	}

	// everything cached now: a repeat costs no budget
	if _, err := rr.Rerank(ctx, "query", []string{"alpha", "beta"}); err != nil {
		t.Fatalf("cached pool: %v", err)
	}
	if calls(n) != 1 || b.Spent() != 2 {
		t.Fatalf("cached pool must not spend budget: calls %d spent %d", calls(n), b.Spent())
	}
}

func TestRerankBudgetSkipsOnlyUncachedPairs(t *testing.T) {
	srv, n := serveRerank(t)
	ctx := context.Background()
	disk := &memCache{data: make(map[string]float64)}

	rr := NewReranker(Config{BaseURL: srv.URL}, "qwen/qwen3-reranker-8b").
		WithCache(disk).WithBudget(NewRerankBudget(3))
	if _, err := rr.Rerank(ctx, "query", []string{"alpha", "beta", "gamma"}); err != nil {
		t.Fatalf("first pool: %v", err)
	}
	if rr.budget.Spent() != 3 || calls(n) != 1 {
		t.Fatalf("first pool must spend 3 pairs: spent %d calls %d", rr.budget.Spent(), calls(n))
	}

	// budget exhausted: one new doc mixed into a cached pool is refused
	// whole-pool even though a single pair would fit it
	_, err := rr.Rerank(ctx, "query", []string{"alpha", "delta"})
	if !errors.Is(err, ErrRerankBudget) {
		t.Fatalf("one uncached pair with no budget left must be refused, got %v", err)
	}
	if calls(n) != 1 || rr.budget.Skipped() != 1 {
		t.Fatalf("refusal must not hit the API: calls %d skipped %d", calls(n), rr.budget.Skipped())
	}

	// a fresh process sharing the disk cache, with a one-pair budget,
	// admits that same mixed pool: only the uncached pair is charged
	rr2 := NewReranker(Config{BaseURL: srv.URL}, "qwen/qwen3-reranker-8b").
		WithCache(disk).WithBudget(NewRerankBudget(1))
	if _, err := rr2.Rerank(ctx, "query", []string{"alpha", "delta"}); err != nil {
		t.Fatalf("mixed pool within budget: %v", err)
	}
	if rr2.budget.Spent() != 1 || calls(n) != 2 {
		t.Fatalf("mixed pool must charge only the uncached pair: spent %d calls %d", rr2.budget.Spent(), calls(n))
	}
}

func TestRerankBudgetRefundsOnAPIError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, "upstream down", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	rr := NewReranker(Config{BaseURL: srv.URL}, "qwen/qwen3-reranker-8b").
		WithBudget(NewRerankBudget(5))
	if _, err := rr.Rerank(context.Background(), "query", []string{"alpha", "beta"}); err == nil {
		t.Fatal("expected API error")
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("failed call must still hit the API once, calls = %d", calls)
	}
	if rr.budget.Spent() != 0 || rr.budget.Left() != 5 {
		t.Fatalf("failed call must be refunded: spent %d left %d", rr.budget.Spent(), rr.budget.Left())
	}
}
