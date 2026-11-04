package rag

import (
	"context"
	"fmt"
	"vyasa/src/chunks"
	"vyasa/src/models"
	"vyasa/src/store"
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
// retrieval.
type Indexer struct {
	st    *store.Store
	emb   Embedder
	model string
	dim   int
}

// NewIndexer builds an indexer. model and dim are recorded on every row so
// vectors from different embedding models never mix.
func NewIndexer(st *store.Store, emb Embedder, model string, dim int) *Indexer {
	return &Indexer{st: st, emb: emb, model: model, dim: dim}
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

// Search embeds the query and runs the hybrid retrieval.
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
	return ix.st.SearchRAGHybrid(ctx, companyID, segment, query, vec[0], topK)
}

// embedInput is the text embedded for a chunk: the chunker's context line
// (section, slide) prefixed to the chunk body so retrieval sees it.
func embedInput(c chunks.Chunk) string {
	if c.Context == "" {
		return c.Text
	}
	return c.Context + "\n" + c.Text
}
