package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
	"vyasa/src/models"

	"github.com/jackc/pgx/v5"
)

// DeleteRAGTables removes every stored table of one (company, segment).
// The resource table keys documents by segment, so clearing the segment —
// like DeleteRAGChunks — keeps re-indexing idempotent and orphans nothing
// when a segment's document is replaced by one with a different URL.
func (s *Store) DeleteRAGTables(ctx context.Context, companyID, segment string) error {
	if _, err := s.Pool.Exec(ctx,
		`DELETE FROM rag_tables WHERE company_id = $1 AND segment = $2`,
		companyID, segment); err != nil {
		return fmt.Errorf("store: delete rag_tables %s/%s: %w", companyID, segment, err)
	}
	return nil
}

func (s *Store) InsertRAGTables(ctx context.Context, tables []models.RAGTable) error {
	if len(tables) == 0 {
		return nil
	}
	now := time.Now()
	b := &pgx.Batch{}
	for _, t := range tables {
		content, err := json.Marshal(t.Content)
		if err != nil {
			return fmt.Errorf("store: encode rag_tables content %s/%s#%d: %w", t.Symbol, t.Segment, t.Ordinal, err)
		}
		b.Queue(`INSERT INTO rag_tables
				(company_id, symbol, segment, document_url, ordinal, page, caption, n_rows, n_cols, content, text, embedding, model, period, source, is_quarterly, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11, $12::vector, $13, $14, $15, $16, $17, $17)`,
			t.CompanyID, t.Symbol, t.Segment, t.DocumentURL, t.Ordinal, t.Page,
			t.Caption, t.NRows, t.NCols, string(content), t.Text,
			VectorLiteral(t.Embedding), t.Model,
			t.Period, t.Source, t.IsQuarterly, now)
	}
	if err := s.Pool.SendBatch(ctx, b).Close(); err != nil {
		return fmt.Errorf("store: insert rag_tables: %w", err)
	}
	return nil
}

const ragTableColumns = `id, company_id, symbol, segment, document_url, ordinal, page, caption, n_rows, n_cols, content, text, model, period, source, is_quarterly`

func scanRAGTable(rows pgx.Rows, extra ...any) (models.RAGTable, error) {
	var t models.RAGTable
	var content []byte
	dests := []any{&t.ID, &t.CompanyID, &t.Symbol, &t.Segment, &t.DocumentURL,
		&t.Ordinal, &t.Page, &t.Caption, &t.NRows, &t.NCols, &content,
		&t.Text, &t.Model, &t.Period, &t.Source, &t.IsQuarterly}
	dests = append(dests, extra...)
	if err := rows.Scan(dests...); err != nil {
		return t, err
	}
	if len(content) > 0 {
		if err := json.Unmarshal(content, &t.Content); err != nil {
			return t, fmt.Errorf("store: decode rag_tables content %s: %w", t.ID, err)
		}
	}
	return t, nil
}

func collectRAGTables(rows pgx.Rows) ([]models.RAGTable, error) {
	var out []models.RAGTable
	for rows.Next() {
		t, err := scanRAGTable(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan rag table: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) SearchRAGTablesVector(ctx context.Context, companyID, segment, qvec string, topK int) ([]models.RAGTable, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+ragTableColumns+`
	FROM rag_tables
	WHERE ($1 = '' OR company_id = $1) AND ($2 = '' OR segment = $2)
	ORDER BY embedding <=> $3::vector
	LIMIT $4`, companyID, segment, qvec, topK)
	if err != nil {
		return nil, fmt.Errorf("store: table vector search: %w", err)
	}
	defer rows.Close()
	return collectRAGTables(rows)
}

func (s *Store) SearchRAGTablesLexical(ctx context.Context, companyID, segment, query string, topK int) ([]models.RAGTable, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+ragTableColumns+`, ts_rank_cd(rag_tables.tsv, query) AS score
	FROM rag_tables, websearch_to_tsquery('english', $3) AS query
	WHERE rag_tables.tsv @@ query
	  AND ($1 = '' OR company_id = $1) AND ($2 = '' OR segment = $2)
	ORDER BY score DESC, id
	LIMIT $4`, companyID, segment, query, topK)
	if err != nil {
		return nil, fmt.Errorf("store: table lexical search: %w", err)
	}
	defer rows.Close()

	var out []models.RAGTable
	for rows.Next() {
		var rank float64
		t, err := scanRAGTable(rows, &rank)
		if err != nil {
			return nil, fmt.Errorf("store: scan rag table: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
