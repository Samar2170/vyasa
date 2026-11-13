package llm

import (
	"context"
	"encoding/json"
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
