package rag

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"

	"vyasa/src/chunks"
	"vyasa/src/llm"
	"vyasa/src/models"
	"vyasa/src/store"
)

const (
	// rerankDocRunes caps how much of each chunk is sent to the reranker.
	// The /rerank endpoint bills tokens with the instruction+query template
	// repeated for every document, and cross-encoder relevance is decided
	// mostly by the head of the passage, so long tails are trimmed first.
	rerankDocRunes = 800
)

type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// DocMeta describes the source document every chunk of an indexing run
// belongs to. It is stored beside the chunks so retrieval hits carry their
// fiscal context.
type DocMeta struct {
	Segment     string
	DocumentURL string
	Period      string // e.g. "FY2025-26" or "Q1 FY2026-27"; "" when unknown
	Source      string // provenance, e.g. "investor_relations"
	IsQuarterly bool
}

// Indexer chunks documents, embeds the chunks and stores them for hybrid
// retrieval. With a reranker attached, Search fetches a deep candidate pool
// from the hybrid retrieval, reorders it with the cross-encoder and keeps
// the top-K.
type Indexer struct {
	st    *store.Store
	emb   Embedder
	model string
	dim   int
	rer   *llm.Reranker
	pool  int

	budgetLogged bool // the budget-exhausted note is logged once per indexer
}

// NewIndexer builds an indexer. model and dim are recorded on every row so
// vectors from different embedding models never mix.
func NewIndexer(st *store.Store, emb Embedder, model string, dim int) *Indexer {
	return &Indexer{st: st, emb: emb, model: model, dim: dim}
}

// WithReranker attaches a cross-encoder reranker: Search fetches pool
// candidates (pool >= topK), reranks them and keeps the best topK.
func (ix *Indexer) WithReranker(r *llm.Reranker, pool int) *Indexer {
	if ix != nil {
		ix.rer = r
		ix.pool = pool
	}
	return ix
}

// IndexDocument replaces the stored chunks of one (company, segment) with
// chunks of text, embedded. Indexing is idempotent.
func (ix *Indexer) IndexDocument(ctx context.Context, co models.Company, meta DocMeta, text string) (int, error) {
	if ix == nil || ix.st == nil {
		return 0, fmt.Errorf("rag: indexer not configured")
	}
	if ix.emb == nil {
		return 0, fmt.Errorf("rag: embedder not configured")
	}

	parts := chunks.ForSegment(meta.Segment).Chunk(text)
	if err := ix.st.DeleteRAGChunks(ctx, co.ID, meta.Segment); err != nil {
		return 0, fmt.Errorf("rag: clear %s/%s: %w", co.Symbol, meta.Segment, err)
	}
	if len(parts) == 0 {
		return 0, nil
	}

	inputs := make([]string, len(parts))
	for i, p := range parts {
		inputs[i] = embedInput(p)
	}
	vecs, err := ix.emb.Embed(ctx, inputs)
	if err != nil {
		return 0, fmt.Errorf("rag: embed %s/%s: %w", co.Symbol, meta.Segment, err)
	}
	if len(vecs) != len(parts) {
		return 0, fmt.Errorf("rag: embed %s/%s: %d vectors for %d chunks", co.Symbol, meta.Segment, len(vecs), len(parts))
	}

	rows := make([]models.RAGChunk, len(parts))
	for i, p := range parts {
		rows[i] = models.RAGChunk{
			CompanyID:   co.ID,
			Symbol:      co.Symbol,
			Segment:     meta.Segment,
			DocumentURL: meta.DocumentURL,
			Ordinal:     p.Ordinal,
			Offset:      p.Offset,
			Section:     p.Context,
			Text:        p.Text,
			Embedding:   vecs[i],
			Model:       ix.model,
			Period:      meta.Period,
			Source:      meta.Source,
			IsQuarterly: meta.IsQuarterly,
		}
	}
	if err := ix.st.InsertRAGChunks(ctx, rows); err != nil {
		return 0, fmt.Errorf("rag: store %s/%s: %w", co.Symbol, meta.Segment, err)
	}
	return len(rows), nil
}

// Search embeds the query and runs the hybrid retrieval, reranked by the
// cross-encoder when one is attached.
func (ix *Indexer) Search(ctx context.Context, companyID, segment, query string, topK int) ([]models.RAGHit, error) {
	if ix == nil || ix.st == nil {
		return nil, fmt.Errorf("rag: indexer not configured")
	}
	if ix.emb == nil {
		return nil, fmt.Errorf("rag: embedder not configured")
	}
	if topK <= 0 {
		topK = 8
	}
	vec, err := ix.emb.Embed(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("rag: embed query: %w", err)
	}
	if len(vec) != 1 {
		return nil, fmt.Errorf("rag: embed query: %d vectors for one query", len(vec))
	}
	pool := topK
	if ix.rer != nil && ix.pool > pool {
		pool = ix.pool
	}
	hits, err := ix.st.SearchRAGHybrid(ctx, companyID, segment, query, vec[0], topK, pool)
	if err != nil {
		return nil, err
	}
	if ix.rer == nil || len(hits) <= topK {
		return hits, nil
	}
	return ix.rerank(ctx, query, hits, topK)
}

// rerank scores the hybrid pool with the cross-encoder and returns the best
// topK by relevance score. On rerank failure the RRF order is kept.
func (ix *Indexer) rerank(ctx context.Context, query string, hits []models.RAGHit, topK int) ([]models.RAGHit, error) {
	docs := make([]string, len(hits))
	for i, h := range hits {
		docs[i] = truncateRunes(h.Text, rerankDocRunes)
	}
	scores, err := ix.rer.Rerank(ctx, query, docs)
	if err != nil {
		if errors.Is(err, llm.ErrRerankBudget) {
			if !ix.budgetLogged {
				ix.budgetLogged = true
				log.Printf("rag: rerank budget exhausted, keeping RRF order")
			}
			return hits[:topK], nil
		}
		log.Printf("rag: rerank failed, keeping RRF order: %v", err)
		return hits[:topK], nil
	}
	for i, sc := range scores {
		hits[i].RerankScore = sc.Score
		hits[i].Reranked = true
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].RerankScore > hits[j].RerankScore })
	return hits[:topK], nil
}

// truncateRunes cuts s to at most n runes, backing off to the last word
// boundary when the cut lands mid-word.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	cut := string(r[:n])
	if i := strings.LastIndexAny(cut, " \n\t"); i > n/2 {
		return strings.TrimRight(cut[:i], " \n\t")
	}
	return cut
}

// embedInput is the text embedded for a chunk: the chunker's context line
// (section, slide) prefixed to the chunk body so retrieval sees it.
func embedInput(c chunks.Chunk) string {
	if c.Context == "" {
		return c.Text
	}
	return c.Context + "\n" + c.Text
}
