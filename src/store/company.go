// Company queries: the seed command creates missing companies from the NSE
// equity master list, the collect command resolves companies by symbol.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"vyasa/src/models"
)

// GetCompany returns the company with the given symbol, or nil when absent.
func (s *Store) GetCompany(ctx context.Context, symbol string) (*models.Company, error) {
	var co models.Company
	err := s.Pool.QueryRow(ctx, `SELECT id, symbol, exchange, name, industry, series, isin_code, created_at, updated_at
		FROM companies WHERE symbol = $1 LIMIT 1`, symbol).
		Scan(&co.ID, &co.Symbol, &co.Exchange, &co.Name, &co.Industry, &co.Series, &co.ISINCode, &co.CreatedAt, &co.UpdatedAt)
	if IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get company %s: %w", symbol, err)
	}
	return &co, nil
}

// CreateMissingCompanies inserts the companies whose symbol is not already
// present in the companies table and leaves existing documents untouched.
// It returns the number inserted and the number skipped because their symbol
// already existed. The unique (symbol, exchange) index also guards against
// concurrent inserts, so conflict-nothing is counted as skipped.
func (s *Store) CreateMissingCompanies(ctx context.Context, cos []models.Company) (created int, skipped int, err error) {
	rows, err := s.Pool.Query(ctx, `SELECT DISTINCT symbol FROM companies`)
	if err != nil {
		return 0, 0, fmt.Errorf("store: list symbols: %w", err)
	}
	existing := make(map[string]struct{})
	for rows.Next() {
		var sym string
		if err := rows.Scan(&sym); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("store: list symbols: %w", err)
		}
		existing[sym] = struct{}{}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("store: list symbols: %w", err)
	}

	now := time.Now()
	b := &pgx.Batch{}
	for _, co := range cos {
		if co.Symbol == "" {
			continue
		}
		if _, ok := existing[co.Symbol]; ok {
			skipped++
			continue
		}
		existing[co.Symbol] = struct{}{}
		b.Queue(`INSERT INTO companies (symbol, exchange, name, industry, series, isin_code, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
			ON CONFLICT (symbol, exchange) DO NOTHING`,
			co.Symbol, co.Exchange, co.Name, co.Industry, co.Series, co.ISINCode, now)
		created++
	}
	if err := s.Pool.SendBatch(ctx, b).Close(); err != nil {
		return created, skipped, fmt.Errorf("store: insert companies: %w", err)
	}
	return created, skipped, nil
}
