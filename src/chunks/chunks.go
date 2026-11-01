// Package chunks splits parsed research documents (annual reports, investor
// presentations, transcripts, quarterly filings) into searchable slices and
// persists them for the analysis agent's document tools.
//
// Offsets are rune offsets into the source document so a search hit can be
// resolved straight into a read window.
package chunks

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"vyasa/src/models"
	"vyasa/src/store"
)

const (
	// defaultChunkSize is the target chunk length in runes.
	defaultChunkSize = 1500
	// defaultOverlap keeps context that straddles a boundary retrievable.
	defaultOverlap = 200
)

// Chunk is one slice of a document.
type Chunk struct {
	Ordinal int
	Offset  int // rune offset of Text within the source document
	Text    string
}

// Split divides text into overlapping chunks. It returns nil for empty input.
func Split(text string) []Chunk {
	runes := []rune(text)
	n := len(runes)
	if n == 0 {
		return nil
	}
	if n <= defaultChunkSize {
		return []Chunk{{Ordinal: 0, Offset: 0, Text: text}}
	}

	step := defaultChunkSize - defaultOverlap
	var out []Chunk
	for start := 0; start < n; start += step {
		end := start + defaultChunkSize
		if end >= n {
			end = n
		}
		out = append(out, Chunk{
			Ordinal: len(out),
			Offset:  start,
			Text:    string(runes[start:end]),
		})
		if end == n {
			break
		}
	}
	return out
}

// IndexText replaces the stored chunks for one (company, segment) with chunks
// split from text. Indexing is idempotent.
func IndexText(ctx context.Context, st *store.Store, co models.Company, segment, text string) (int, error) {
	if st == nil {
		return 0, fmt.Errorf("chunks: store not configured")
	}
	if _, err := st.Pool.Exec(ctx,
		`DELETE FROM document_chunks WHERE company_id = $1 AND segment = $2`,
		co.ID, segment,
	); err != nil {
		return 0, fmt.Errorf("chunks: clear %s/%s: %w", co.Symbol, segment, err)
	}

	parts := Split(text)
	if len(parts) == 0 {
		return 0, nil
	}

	now := time.Now()
	b := &pgx.Batch{}
	for _, p := range parts {
		b.Queue(`INSERT INTO document_chunks (company_id, symbol, segment, ordinal, "offset", text, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $7)`,
			co.ID, co.Symbol, segment, p.Ordinal, p.Offset, p.Text, now)
	}
	if err := st.Pool.SendBatch(ctx, b).Close(); err != nil {
		return 0, fmt.Errorf("chunks: insert %s/%s: %w", co.Symbol, segment, err)
	}
	return len(parts), nil
}

// CollapseBlankLines trims whitespace and squeezes 3+ newlines to two; shared
// by clients that archive parsed text.
func CollapseBlankLines(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	blank := 0
	for _, line := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		line = strings.TrimRight(line, " \t")
		if line == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}
