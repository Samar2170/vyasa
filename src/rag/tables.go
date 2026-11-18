package rag

import (
	"context"
	"fmt"
	"strings"

	"vyasa/src/models"
	"vyasa/src/store"
)

// ExtractedTable is one table mined from a document, ready to be indexed.
// Producers (the ir package) fill it; IndexTables persists it.
type ExtractedTable struct {
	Page    int
	Caption string
	Header  []string
	Rows    [][]string
}

const (
	// tableSplitRows caps the data rows of one indexed table unit. Taller
	// tables split into row groups (header repeated) so no single vector has
	// to carry five years of line items.
	tableSplitRows = 24
	// tableMaxCols rejects pathological "tables" with more columns than any
	// real financial statement has.
	tableMaxCols = 32
)

// Markdown renders the table as a pipe table: caption-qualified header row,
// a separator, then the data rows. Ragged rows are padded so every line
// carries the same number of cells.
func (t ExtractedTable) Markdown() string {
	cols := len(t.Header)
	for _, r := range t.Rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	if cols == 0 {
		return ""
	}
	pad := func(cells []string) string {
		out := make([]string, cols)
		for i := range out {
			out[i] = ""
			if i < len(cells) {
				out[i] = strings.Join(strings.Fields(cells[i]), " ")
			}
		}
		return "| " + strings.Join(out, " | ") + " |"
	}

	var b strings.Builder
	if len(t.Header) > 0 {
		b.WriteString(pad(t.Header))
		b.WriteString("\n")
	}
	sep := make([]string, cols)
	for i := range sep {
		sep[i] = "---"
	}
	b.WriteString("| " + strings.Join(sep, " | ") + " |\n")
	for _, r := range t.Rows {
		b.WriteString(pad(r))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// splitTableRows cuts a tall table into header-carrying row groups of at
// most tableSplitRows data rows each.
func splitTableRows(t ExtractedTable) []ExtractedTable {
	if len(t.Rows) <= tableSplitRows {
		return []ExtractedTable{t}
	}
	var out []ExtractedTable
	for start := 0; start < len(t.Rows); start += tableSplitRows {
		end := start + tableSplitRows
		if end > len(t.Rows) {
			end = len(t.Rows)
		}
		part := t
		part.Rows = t.Rows[start:end]
		out = append(out, part)
	}
	return out
}

// tableCols returns the widest row of a table.
func tableCols(t ExtractedTable) int {
	cols := len(t.Header)
	for _, r := range t.Rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	return cols
}

// IndexTables replaces the stored tables of one (company, segment) with the
// extracted tables, embedded. Idempotent like IndexDocument: every table of
// the segment is deleted first, then re-inserted. One indexed row per table
// (row group for tables taller than tableSplitRows); the embedded text is
// caption plus markdown so retrieval sees what the table is about.
func (ix *Indexer) IndexTables(ctx context.Context, co models.Company, meta DocMeta, tables []ExtractedTable) (int, error) {
	if ix == nil || ix.st == nil {
		return 0, fmt.Errorf("rag: indexer not configured")
	}
	if ix.emb == nil {
		return 0, fmt.Errorf("rag: embedder not configured")
	}
	if err := ix.st.DeleteRAGTables(ctx, co.ID, meta.Segment); err != nil {
		return 0, fmt.Errorf("rag: clear %s/%s tables: %w", co.Symbol, meta.Segment, err)
	}
	if len(tables) == 0 {
		return 0, nil
	}

	var rows []models.RAGTable
	var inputs []string
	ordinal := 0
	for _, t := range tables {
		if cols := tableCols(t); cols < 2 || cols > tableMaxCols || len(t.Rows) == 0 {
			continue
		}
		for _, part := range splitTableRows(t) {
			text := part.Markdown()
			if text == "" {
				continue
			}
			input := text
			if part.Caption != "" {
				input = part.Caption + "\n" + text
			}
			rows = append(rows, models.RAGTable{
				CompanyID:   co.ID,
				Symbol:      co.Symbol,
				Segment:     meta.Segment,
				DocumentURL: meta.DocumentURL,
				Ordinal:     ordinal,
				Page:        part.Page,
				Caption:     part.Caption,
				NRows:       len(t.Rows),
				NCols:       tableCols(part),
				Content:     models.TableGrid{Header: part.Header, Rows: part.Rows},
				Text:        text,
				Model:       ix.model,
				Period:      meta.Period,
				Source:      meta.Source,
				IsQuarterly: meta.IsQuarterly,
			})
			inputs = append(inputs, input)
			ordinal++
		}
	}
	if len(rows) == 0 {
		return 0, nil
	}

	vecs, err := ix.emb.Embed(ctx, inputs)
	if err != nil {
		return 0, fmt.Errorf("rag: embed %s/%s tables: %w", co.Symbol, meta.Segment, err)
	}
	if len(vecs) != len(rows) {
		return 0, fmt.Errorf("rag: embed %s/%s tables: %d vectors for %d tables", co.Symbol, meta.Segment, len(vecs), len(rows))
	}
	for i := range rows {
		rows[i].Embedding = vecs[i]
	}
	if err := ix.st.InsertRAGTables(ctx, rows); err != nil {
		return 0, fmt.Errorf("rag: store %s/%s tables: %w", co.Symbol, meta.Segment, err)
	}
	return len(rows), nil
}

// SearchTables runs the hybrid retrieval over the rag_tables store only:
// tables compete against tables, never against prose chunks. Vector and
// lexical rankings fuse through the same RRF, and the cross-encoder reranks
// the pool when one is attached. Hits carry the caption in Section and the
// source page in Offset.
func (ix *Indexer) SearchTables(ctx context.Context, companyID, segment, query string, topK int) ([]models.RAGHit, error) {
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

	var vecHits, lexHits []models.RAGTable
	if len(vec[0]) > 0 {
		vecHits, err = ix.st.SearchRAGTablesVector(ctx, companyID, segment, store.VectorLiteral(vec[0]), pool)
		if err != nil {
			return nil, fmt.Errorf("store: table vector search: %w", err)
		}
	}
	if strings.TrimSpace(query) != "" {
		lexHits, err = ix.st.SearchRAGTablesLexical(ctx, companyID, segment, query, pool)
		if err != nil {
			return nil, fmt.Errorf("store: table lexical search: %w", err)
		}
	}
	if vecHits == nil && lexHits == nil {
		return nil, fmt.Errorf("rag: table search needs a query")
	}

	hits := store.FuseRRF(pool, tableChunks(vecHits), tableChunks(lexHits))
	if ix.rer == nil || len(hits) <= topK {
		return hits, nil
	}
	return ix.rerank(ctx, query, hits, topK)
}

// tableChunks maps table rows into chunk-shaped candidates so tables fuse
// and rerank alongside prose through the same code paths. Section carries
// the caption and Offset the source page.
func tableChunks(tables []models.RAGTable) []models.RAGChunk {
	out := make([]models.RAGChunk, len(tables))
	for i, t := range tables {
		out[i] = models.RAGChunk{
			BaseModel:   models.BaseModel{ID: t.ID},
			CompanyID:   t.CompanyID,
			Symbol:      t.Symbol,
			Segment:     t.Segment,
			DocumentURL: t.DocumentURL,
			Ordinal:     t.Ordinal,
			Offset:      t.Page,
			Section:     t.Caption,
			Text:        t.Text,
			Period:      t.Period,
			Source:      t.Source,
			IsQuarterly: t.IsQuarterly,
			Kind:        models.HitKindTable,
		}
	}
	return out
}
