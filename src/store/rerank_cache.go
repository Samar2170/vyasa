package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// LookupRerankCache returns the cached scores for the given pair keys.
// Keys absent from the table are simply missing from the map.
func (s *Store) Lookup(ctx context.Context, keys []string) (map[string]float64, error) {
	rows, err := s.Pool.Query(ctx, `SELECT key, score FROM rerank_score_cache WHERE key = ANY($1)`, keys)
	if err != nil {
		return nil, fmt.Errorf("store: lookup rerank_score_cache: %w", err)
	}
	defer rows.Close()

	out := make(map[string]float64, len(keys))
	for rows.Next() {
		var k string
		var sc float64
		if err := rows.Scan(&k, &sc); err != nil {
			return nil, fmt.Errorf("store: scan rerank_score_cache: %w", err)
		}
		out[k] = sc
	}
	return out, rows.Err()
}

// SaveRerankCache persists rerank scores by pair hash. Existing entries are
// left untouched.
func (s *Store) Store(ctx context.Context, scores map[string]float64) error {
	if len(scores) == 0 {
		return nil
	}
	b := &pgx.Batch{}
	for k, sc := range scores {
		b.Queue(`INSERT INTO rerank_score_cache (key, score)
			VALUES ($1, $2)
			ON CONFLICT (key) DO NOTHING`, k, sc)
	}
	if err := s.Pool.SendBatch(ctx, b).Close(); err != nil {
		return fmt.Errorf("store: save rerank_score_cache: %w", err)
	}
	return nil
}
