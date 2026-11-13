package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
)

const (
	defaultRerankBaseURL = "https://openrouter.ai/api/v1"
	// rerankBatchSize caps how many documents go into one /rerank request.
	rerankBatchSize = 100
	// rerankCacheLimit caps cached pairs before the in-memory cache resets.
	// A full eval sweep is items x pool pairs (~2000), so the limit leaves
	// headroom while bounding memory.
	rerankCacheLimit = 65536
)

// rerankNoRetries is assigned to Config.MaxRetries: see NewReranker.
var rerankNoRetries = 0

// ErrRerankBudget is returned by Rerank when the attached RerankBudget has
// no room left for a pool's uncached pairs. Callers treat it exactly like
// any rerank failure: keep the retrieval order.
var ErrRerankBudget = errors.New("llm: rerank budget exhausted")

// RerankBudget caps how many uncached (query, document) pairs one process
// may bill through a Reranker. Cached pairs are free and never consume the
// budget; a pool is charged atomically — when its uncached pairs do not fit
// what is left, the whole call is refused (ErrRerankBudget) and the pool
// keeps retrieval order. One pair is roughly 285 API-billed tokens (a
// document trimmed to 800 runes plus query plus the instruction template),
// so pair budgets map directly onto the endpoint's token bill.
type RerankBudget struct {
	mu      sync.Mutex
	left    int
	spent   int
	skipped int // pools refused for lack of budget
}

// NewRerankBudget builds a budget of pairs pair-units.
func NewRerankBudget(pairs int) *RerankBudget {
	return &RerankBudget{left: pairs}
}

// trySpend charges n pair-units, reporting whether they fit. Callers refund
// when the API call that consumed them fails.
func (b *RerankBudget) trySpend(n int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > b.left {
		return false
	}
	b.left -= n
	b.spent += n
	return true
}

// refund returns n charged pair-units to the budget.
func (b *RerankBudget) refund(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.left += n
	b.spent -= n
}

// bumpSkipped counts one pool refused for lack of budget.
func (b *RerankBudget) bumpSkipped() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.skipped++
}

// Spent returns the pair-units charged so far.
func (b *RerankBudget) Spent() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spent
}

// Left returns the unspent pair-units.
func (b *RerankBudget) Left() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.left
}

// Skipped returns how many pools were refused for lack of budget.
func (b *RerankBudget) Skipped() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.skipped
}

// Reranker scores query-document pairs with a cross-encoder rerank model
// (the OpenRouter /rerank endpoint) so a deep candidate pool can be
// reordered by relevance before truncation. Scores are cached per
// (model, query, document) pair — a cross-encoder scores every pair
// independently, so a pair's score is a pure function of the pair. Pairs
// cached once are never billed again, no matter how the surrounding pool
// is reordered or re-fetched.
type Reranker struct {
	transport *Transport
	baseURL   string
	model     string
	disk      RerankCache
	budget    *RerankBudget

	mu    sync.Mutex
	cache map[string]float64
}

// RerankCache persists rerank scores keyed by a (query, document) pair hash
// so scores survive process restarts (e.g. repeated evaluation runs over the
// same pool cost zero API requests).
type RerankCache interface {
	Lookup(ctx context.Context, keys []string) (map[string]float64, error)
	Store(ctx context.Context, scores map[string]float64) error
}

// NewReranker builds a rerank client for the given model (e.g.
// "qwen/qwen3-reranker-8b"). Retries are disabled: the endpoint
// rate-limits aggressively and each retry is a billable request, so
// failures surface immediately and callers fall back to retrieval order.
func NewReranker(cfg Config, model string) *Reranker {
	cfg = cfg.WithDefaults()
	cfg.MaxRetries = &rerankNoRetries
	base := cfg.BaseURL
	if base == "" {
		base = defaultRerankBaseURL
	}
	return &Reranker{
		transport: NewTransport("rerank", cfg),
		baseURL:   base,
		model:     model,
		cache:     make(map[string]float64),
	}
}

func (r *Reranker) Model() string { return r.model }

type rerankRequest struct {
	Model     string   `json:"model"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
}

type rerankResult struct {
	Index          int     `json:"index"`
	RelevanceScore float64 `json:"relevance_score"`
}

type rerankResponse struct {
	Model   string         `json:"model"`
	Results []rerankResult `json:"results"`
}

// RerankHit is one document's relevance score for the query.
type RerankHit struct {
	Index int     // position of the document in the input
	Score float64 // relevance score, higher is better
}

// WithCache attaches a persistent cache (e.g. the Postgres-backed store
// implementation) behind the in-memory one.
func (r *Reranker) WithCache(c RerankCache) *Reranker {
	if r != nil {
		r.disk = c
	}
	return r
}

// WithBudget attaches a pair budget: Rerank charges it for the pool's
// uncached pairs and fails with ErrRerankBudget when they do not fit. Nil
// (default) means uncapped.
func (r *Reranker) WithBudget(b *RerankBudget) *Reranker {
	if r != nil {
		r.budget = b
	}
	return r
}

// Rerank scores every document against the query. Every input document gets
// exactly one score at its input index. Scores are served from the
// in-memory pair cache, then the persistent cache, then the API — a pair
// scored once is never billed again, so reordering or subsetting the same
// pool costs nothing. With a budget attached, only the uncached pairs are
// charged, and a pool whose uncached pairs exceed the remaining budget is
// refused wholesale (ErrRerankBudget) instead of being partially scored.
func (r *Reranker) Rerank(ctx context.Context, query string, docs []string) ([]RerankHit, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	out := make([]RerankHit, len(docs))
	for i := range docs {
		out[i].Index = i
	}

	keys := make([]string, len(docs))
	for i, d := range docs {
		keys[i] = r.pairKey(query, d)
	}

	var missing []int
	for i, k := range keys {
		if sc, ok := r.cached(k); ok {
			out[i].Score = sc
			continue
		}
		missing = append(missing, i)
	}

	if len(missing) > 0 && r.disk != nil {
		diskKeys := make([]string, len(missing))
		for j, i := range missing {
			diskKeys[j] = keys[i]
		}
		found, err := r.disk.Lookup(ctx, diskKeys)
		if err != nil {
			// cache reads never break search
			log.Printf("llm: rerank cache lookup: %v", err)
			found = nil
		}
		still := missing[:0]
		for _, i := range missing {
			if sc, ok := found[keys[i]]; ok {
				out[i].Score = sc
				r.storeCache(keys[i], sc)
				continue
			}
			still = append(still, i)
		}
		missing = still
	}

	if r.budget != nil && len(missing) > 0 && !r.budget.trySpend(len(missing)) {
		r.budget.bumpSkipped()
		return nil, ErrRerankBudget
	}

	for start := 0; start < len(missing); start += rerankBatchSize {
		end := start + rerankBatchSize
		if end > len(missing) {
			end = len(missing)
		}
		batch := missing[start:end]
		batchDocs := make([]string, len(batch))
		for j, i := range batch {
			batchDocs[j] = docs[i]
		}
		scores, err := r.rerankBatch(ctx, query, batchDocs)
		if err != nil {
			if r.budget != nil {
				r.budget.refund(len(batch))
			}
			return nil, err
		}
		for j, sc := range scores {
			i := batch[j]
			out[i].Score = sc
			r.storeCache(keys[i], sc)
		}
		if r.disk != nil {
			stored := make(map[string]float64, len(batch))
			for j, sc := range scores {
				stored[keys[batch[j]]] = sc
			}
			if derr := r.disk.Store(ctx, stored); derr != nil {
				// a full cache must never break search
				log.Printf("llm: rerank cache store: %v", derr)
			}
		}
	}
	return out, nil
}

// pairKey hashes the model, query and one document. The cross-encoder scores
// every pair independently, so the pair hash is the natural cache unit: it
// survives pool reordering and pool membership changes.
func (r *Reranker) pairKey(query, doc string) string {
	h := sha256.New()
	h.Write([]byte(r.model))
	h.Write([]byte{0})
	h.Write([]byte(query))
	h.Write([]byte{0})
	h.Write([]byte(doc))
	return hex.EncodeToString(h.Sum(nil))
}

func (r *Reranker) cached(key string) (float64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sc, ok := r.cache[key]
	return sc, ok
}

func (r *Reranker) storeCache(key string, score float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.cache) >= rerankCacheLimit {
		r.cache = make(map[string]float64) // simple reset keeps memory bounded
	}
	r.cache[key] = score
}

func (r *Reranker) rerankBatch(ctx context.Context, query string, batch []string) ([]float64, error) {
	headers := map[string]string{"Authorization": "Bearer " + r.transport.cfg.APIKey}
	var resp rerankResponse
	if err := r.transport.DoJSON(ctx, http.MethodPost, r.baseURL+"/rerank", headers,
		rerankRequest{Model: r.model, Query: query, Documents: batch}, &resp); err != nil {
		return nil, fmt.Errorf("llm: rerank %s: %w", r.model, err)
	}

	// The API returns results in relevance order; rerankResult.Index carries
	// the document's input position. Rebuild a positional slice so callers
	// can zip scores back onto their documents.
	out := make([]float64, len(batch))
	for _, res := range resp.Results {
		if res.Index < 0 || res.Index >= len(batch) {
			return nil, fmt.Errorf("llm: rerank %s: index %d out of range", r.model, res.Index)
		}
		out[res.Index] = res.RelevanceScore
	}
	return out, nil
}
