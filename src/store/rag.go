package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"vyasa/src/models"

	"github.com/jackc/pgx/v5"
)

const rrfK = 60

func vectorLiteral(vec []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range vec {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%g", v)
	}
	b.WriteByte(']')
	return b.String()
}

// DeleteRAGChunks removes every stored chunk of one (company, segment).
// The resource table keys documents by segment, so clearing the segment
// keeps re-indexing idempotent.
func (s *Store) DeleteRAGChunks(ctx context.Context, companyID, segment string) error {
	if _, err := s.Pool.Exec(ctx,
		`DELETE FROM rag_chunks WHERE company_id = $1 AND segment = $2`,
		companyID, segment); err != nil {
		return fmt.Errorf("store: delete rag_chunks %s/%s: %w", companyID, segment, err)
	}
	return nil
}

func (s *Store) InsertRAGChunks(ctx context.Context, chunks []models.RAGChunk) error {
	if len(chunks) == 0 {
		return nil
	}
	now := time.Now()
	b := &pgx.Batch{}
	for _, c := range chunks {
		b.Queue(`INSERT INTO rag_chunks
				(company_id, symbol, segment, document_url, ordinal, "offset", section, text, embedding, model, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::vector, $10, $11, $11)`,
			c.CompanyID, c.Symbol, c.Segment, c.DocumentURL, c.Ordinal, c.Offset,
			c.Section, c.Text, vectorLiteral(c.Embedding), c.Model, now)
	}
	if err := s.Pool.SendBatch(ctx, b).Close(); err != nil {
		return fmt.Errorf("store: insert rag_chunks: %w", err)
	}
	return nil
}

const ragChunkColumns = `id, company_id, symbol, segment, document_url, ordinal, "offset", section, text, model`

func scanRAGChunk(rows pgx.Rows) (models.RAGChunk, error) {
	var c models.RAGChunk
	err := rows.Scan(&c.ID, &c.CompanyID, &c.Symbol, &c.Segment, &c.DocumentURL,
		&c.Ordinal, &c.Offset, &c.Section, &c.Text, &c.Model)
	return c, err
}

func (s *Store) SearchRAGVector(ctx context.Context, companyID, segment, qvec string, topK int) ([]models.RAGChunk, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+ragChunkColumns+`
	FROM rag_chunks
	WHERE ($1 = '' OR company_id = $1) AND ($2 = '' OR segment = $2)
	ORDER BY embedding <=> $3::vector
	LIMIT $4`, companyID, segment, qvec, topK)
	if err != nil {
		return nil, fmt.Errorf("store: vector search: %w", err)
	}
	defer rows.Close()
	return collectRAGChunks(rows)
}
func (s *Store) SearchRAGLexical(ctx context.Context, companyID, segment, query string, topK int) ([]models.RAGChunk, error) {
	rows, err := s.Pool.Query(ctx, `SELECT`+ragChunkColumns+`ts_rank_cd(rag_chunks.tsv,
	query) AS score
	FROM rag_chunks, websearch_to_tsquery('english', $3) AS query
	WHERE rag_chunks.tsv @@ query
	 AND ($1 = '' OR company_id = $1) AND ($2 = '' OR segment = $2)
	ORDER BY score DESC
	LIMIT $4`, companyID, segment, query, topK)
	if err != nil {
		return nil, fmt.Errorf("store: lexical search: %w", err)
	}
	defer rows.Close()
	return collectRAGChunks(rows)
}

func collectRAGChunks(rows pgx.Rows) ([]models.RAGChunk, error) {
	var out []models.RAGChunk
	for rows.Next() {
		c, err := scanRAGChunk(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan rag chunk: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SearchRAGHybrid runs semantic vector search and lexical full-text search,
// fuses both rankings with Reciprocal Rank Fusion and returns the top-k
// chunks. Either signal alone degrades gracefully.
func (s *Store) SearchRAGHybrid(ctx context.Context, companyID, segment, query string, qvec []float32, topK int) ([]models.RAGHit, error) {
	if topK <= 0 {
		topK = 8
	}
	candidates := topK * 5
	if candidates < 50 {
		candidates = 50
	}

	var vecHits, lexHits []models.RAGChunk
	if len(qvec) > 0 {
		hits, err := s.SearchRAGVector(ctx, companyID, segment, vectorLiteral(qvec), candidates)
		if err != nil {
			return nil, fmt.Errorf("store: hybrid vector search: %w", err)
		}
		vecHits = hits
	}
	if strings.TrimSpace(query) != "" {
		hits, err := s.SearchRAGLexical(ctx, companyID, segment, query, candidates)
		if err != nil {
			return nil, fmt.Errorf("store: hybrid lexical search: %w", err)
		}
		lexHits = hits
	}
	if vecHits == nil && lexHits == nil {
		return nil, fmt.Errorf("store: hybrid search needs a query and/or a query vector")
	}
	return FuseRRF(vecHits, lexHits, topK), nil
}

// FuseRRF merges the vector and lexical rankings with Reciprocal Rank
// Fusion, best first.
func FuseRRF(vecHits, lexHits []models.RAGChunk, topK int) []models.RAGHit {
	scores := make(map[string]float64, len(vecHits)+len(lexHits))
	byID := make(map[string]models.RAGChunk, len(vecHits)+len(lexHits))
	accumulate := func(hits []models.RAGChunk) {
		for i, h := range hits {
			scores[h.ID] = 1.0 / float64(rrfK+i+1)
			if _, ok := byID[h.ID]; !ok {
				byID[h.ID] = h
			}
		}
	}
	accumulate(vecHits)
	accumulate(lexHits)

	out := make([]models.RAGHit, 0, len(scores))
	for id, sc := range scores {
		out = append(out, models.RAGHit{RAGChunk: byID[id], Score: sc})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > topK {
		out = out[:topK]
	}
	return out

}

func (s *Store) MarkResourceIndexed(ctx context.Context, companyID, segment string, indexed bool) error {
	now := time.Now()
	_, err := s.Pool.Exec(ctx, `UPDATE company_research_resources
		SET indexed = $3, updated_at = $4
		WHERE company_id = $1 AND analysis_segment = $2`,
		companyID, segment, indexed, now)
	if err != nil {
		return fmt.Errorf("store: mark resource indexed %s/%s: %w", companyID, segment, err)
	}
	return nil
}

// PendingResource is one unindexed company_research_resources row with the
// fields the rag-index backfill needs.
type PendingResource struct {
	CompanyID       string
	Symbol          string
	AnalysisSegment string
	DocumentURL     string // research_resource_link
	TextPath        string // extracted_data.text_path
}

// UnindexedResources returns every archived filing row not yet indexed,
// oldest company first.
func (s *Store) UnindexedResources(ctx context.Context) ([]PendingResource, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT r.company_id, c.symbol, r.analysis_segment, r.research_resource_link,
		       COALESCE(r.extracted_data->>'text_path', '')
		FROM company_research_resources r
		JOIN companies c ON c.id = r.company_id
		WHERE r.indexed = false AND COALESCE(r.extracted_data->>'text_path', '') <> ''
		ORDER BY c.symbol, r.analysis_segment`)
	if err != nil {
		return nil, fmt.Errorf("store: unindexed resources: %w", err)
	}
	defer rows.Close()

	var out []PendingResource
	for rows.Next() {
		var p PendingResource
		if err := rows.Scan(&p.CompanyID, &p.Symbol, &p.AnalysisSegment,
			&p.DocumentURL, &p.TextPath); err != nil {
			return nil, fmt.Errorf("store: scan pending resource: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
