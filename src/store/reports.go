package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"vyasa/src/models"
)

// CompanyLinks returns the cached official-website / investor-relations
// links for a company id, or nil when none were recorded.
func (s *Store) CompanyLinks(ctx context.Context, companyID string) (*models.CompanyLinks, error) {
	var l models.CompanyLinks
	err := s.Pool.QueryRow(ctx, `SELECT id, company_id, official_website, investor_relations_link, created_at, updated_at
		FROM company_links WHERE company_id = $1 LIMIT 1`, companyID).
		Scan(&l.ID, &l.CompanyID, &l.OfficialWebsite, &l.InvestorRelationsLink, &l.CreatedAt, &l.UpdatedAt)
	if IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: company links %s: %w", companyID, err)
	}
	return &l, nil
}

// InsertResearchReport stores one synthesized report and fills its
// generated id and timestamps back into r.
func (s *Store) InsertResearchReport(ctx context.Context, r *models.ResearchReport) error {
	now := time.Now()
	questions := marshalQAPairs(r.Questions)
	usage := "{}"
	if len(r.Usage) > 0 {
		if buf, err := json.Marshal(r.Usage); err == nil {
			usage = string(buf)
		}
	}
	return s.Pool.QueryRow(ctx, `INSERT INTO research_reports
		(company_id, symbol, title, report_md, questions, usage, archived_path, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6::jsonb, $7, $8, $8)
		RETURNING id, created_at, updated_at`,
		r.CompanyID, r.Symbol, r.Title, r.ReportMD, questions, usage, r.ArchivedPath, now).
		Scan(&r.ID, &r.CreatedAt, &r.UpdatedAt)
}

// ListResearchReports returns a symbol's stored reports, newest first,
// without the markdown bodies.
func (s *Store) ListResearchReports(ctx context.Context, symbol string) ([]models.ResearchReport, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, company_id, symbol, title, questions, usage, archived_path, created_at, updated_at
		FROM research_reports WHERE symbol = $1 ORDER BY created_at DESC`, symbol)
	if err != nil {
		return nil, fmt.Errorf("store: list research reports %s: %w", symbol, err)
	}
	defer rows.Close()

	var out []models.ResearchReport
	for rows.Next() {
		var r models.ResearchReport
		var questions, usage []byte
		if err := rows.Scan(&r.ID, &r.CompanyID, &r.Symbol, &r.Title,
			&questions, &usage, &r.ArchivedPath, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan research report: %w", err)
		}
		_ = json.Unmarshal(questions, &r.Questions)
		_ = json.Unmarshal(usage, &r.Usage)
		out = append(out, r)
	}
	return out, rows.Err()
}

// marshalQAPairs encodes the Q&A list as a jsonb payload; nil stays an
// empty array rather than SQL null.
func marshalQAPairs(qa []models.QAPair) string {
	if len(qa) == 0 {
		return "[]"
	}
	buf, err := json.Marshal(qa)
	if err != nil {
		return "[]"
	}
	return string(buf)
}
